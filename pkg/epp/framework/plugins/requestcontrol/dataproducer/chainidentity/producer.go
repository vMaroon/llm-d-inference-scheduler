/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package chainidentity provides a DataProducer that derives session
// identity from request content as consistent chained hashes: one hash per
// message block, each folding the previous one, so hash-prefix equality
// holds exactly when content prefixes are byte-identical — the condition
// for KV interchangeability. Any router with the same configuration
// recomputes the chain from a resent prefix; the lineage index is a cache,
// never a source of truth.
//
// The machinery is deliberately shared with the prefix-cache substrate:
// blocks are packed with the tokenizer estimate backend's PackBytes (no
// tokenizer in the router) and chained with the approximate prefix-cache
// producer's ChainSeed/ChainHash algebra, so identity ids and block chains
// agree on what "same prefix" means. What identity adds is framing: blocks
// are length-prefixed per message part, so different message splits can
// never yield the same byte stream (the estimate backend's delimiter-free
// concatenation is fine for counting, ambiguous for identity).
//
// Lineage maintenance:
//   - continuation: a request whose chain extends a known head keeps the
//     lineage id; the head advances.
//   - fork discovery: a request diverging from a mid-chain append becomes a
//     new lineage with a ForkParent attribute — no declaration needed. This
//     covers undeclared sub-agents and template-sharing sessions alike.
//   - structural rollover: a rewritten history shares no known append, so it
//     simply becomes a new lineage.
//   - delta continuation: a declared client id (header or in-band dialect)
//     aliases its lineage, bridging requests that carry no prefix to rehash.
//
// The derived id is published under the shared SessionID attribute (with
// this producer's name), the fork parent under ForkParentDataKey, and a
// StructuralIdentity marker tells consumers to disable heuristic rollover.
// The session-coverage scorer consumes all three.
package chainidentity

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrsession "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/session"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/approximateprefix"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/chainidentity/constants"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/tokenizer"
)

// ChainIdentityProducerType is the plugin type registered with the framework.
const ChainIdentityProducerType = constants.ChainIdentityProducerType

const (
	// chainVersion seeds every chain; bumping it is a deliberate fleet-wide
	// identity rollover (a normalization change re-keys everything).
	chainVersion = "llm-d/chain-identity/v1"

	defaultHeaderName  = "x-session-id"
	defaultLineageTTL  = 30 * time.Minute
	defaultMaxLineages = 100_000

	sweepInterval = time.Minute
	capEvictBatch = 128
)

// Parameters configures the chain-identity producer.
type Parameters struct {
	// TenantSalt seeds every chain so tenants sharing a router fleet derive
	// disjoint identities for identical content. Operator-configured; all
	// routers of a tenant must agree or ids diverge.
	TenantSalt string `json:"tenantSalt"`
	// HeaderName is the request header carrying a declared client session id,
	// recorded as an alias of the derived lineage (bridges delta
	// continuations). Defaults to x-session-id. The in-band session dialect
	// is read as a fallback alias source.
	HeaderName string `json:"headerName"`
	// LineageTTLSeconds is the idle time after which a lineage is dropped
	// from the index. Defaults to 1800 (30 minutes).
	LineageTTLSeconds int `json:"lineageTTLSeconds"`
	// MaxLineages caps the number of tracked lineages. Defaults to 100000.
	MaxLineages int `json:"maxLineages"`
}

var _ requestcontrol.DataProducer = &Producer{}

// Producer derives chain-hash session identity for each request and
// publishes it on the request attribute store.
type Producer struct {
	typedName     fwkplugin.TypedName
	sessionKey    string
	forkKey       string
	structuralKey string
	tenantSalt    string
	headerName    string
	index         *lineageIndex
}

// Factory builds a Producer from raw plugin parameters.
func Factory(name string, rawParameters *json.Decoder, handle fwkplugin.Handle) (fwkplugin.Plugin, error) {
	params := Parameters{}
	if rawParameters != nil {
		if err := rawParameters.Decode(&params); err != nil {
			return nil, fmt.Errorf("failed to parse the parameters of the '%s' producer: %w", ChainIdentityProducerType, err)
		}
	}

	ctx := context.Background()
	if handle != nil {
		ctx = handle.Context()
	}
	return New(ctx, name, params), nil
}

// New returns a chain-identity producer. Zero-valued parameters fall back to
// their defaults. The provided context bounds the background sweeper.
func New(ctx context.Context, name string, params Parameters) *Producer {
	headerName := strings.ToLower(strings.TrimSpace(params.HeaderName))
	if headerName == "" {
		headerName = defaultHeaderName
	}
	ttl := defaultLineageTTL
	if params.LineageTTLSeconds > 0 {
		ttl = time.Duration(params.LineageTTLSeconds) * time.Second
	}
	maxLineages := params.MaxLineages
	if maxLineages <= 0 {
		maxLineages = defaultMaxLineages
	}

	p := &Producer{
		typedName: fwkplugin.TypedName{Type: ChainIdentityProducerType, Name: name},
		// Attribute keys are built with the producer TYPE, not the instance
		// name, so readers need not guess how the instance was named.
		sessionKey:    attrsession.SessionIDDataKey.WithNonEmptyProducerName(ChainIdentityProducerType).String(),
		forkKey:       attrsession.ForkParentDataKey.String(),
		structuralKey: attrsession.StructuralIdentityDataKey.String(),
		tenantSalt:    params.TenantSalt,
		headerName:    headerName,
		index:         newLineageIndex(ttl, maxLineages),
	}
	if ctx != nil {
		go p.sweep(ctx)
	}
	return p
}

// TypedName returns the type and name of the plugin.
func (p *Producer) TypedName() fwkplugin.TypedName {
	return p.typedName
}

// Produces declares the attributes written by this producer.
func (p *Producer) Produces() map[fwkplugin.DataKey]any {
	return map[fwkplugin.DataKey]any{
		attrsession.SessionIDDataKey.WithNonEmptyProducerName(ChainIdentityProducerType): attrsession.SessionID(""),
		attrsession.ForkParentDataKey:         attrsession.SessionID(""),
		attrsession.StructuralIdentityDataKey: true,
	}
}

// Produce derives the request's chain identity and publishes it. Requests
// whose bodies yield no content blocks are left untouched; consumers fall
// back to declared identity.
func (p *Producer) Produce(_ context.Context, request *fwksched.InferenceRequest, _ []fwksched.Endpoint) error {
	if request == nil || request.Body == nil {
		return nil
	}
	blocks := contentBlocks(request.Body)
	if len(blocks) == 0 {
		return nil
	}
	chain := p.chain(request, blocks)
	res := p.index.resolve(chain, p.declaredID(request))
	if res.LineageID == "" {
		return nil
	}
	request.PutAttribute(p.sessionKey, attrsession.SessionID(res.LineageID))
	if res.ForkParent != "" {
		request.PutAttribute(p.forkKey, attrsession.SessionID(res.ForkParent))
	}
	request.PutAttribute(p.structuralKey, true)
	return nil
}

// chain folds the framed content blocks into append ids: the reused
// prefix-cache chain algebra over the reused pseudo-token packing, seeded by
// identity version, tenant salt, and model (KV is model-specific, so
// identical content on different models must not share identity).
func (p *Producer) chain(request *fwksched.InferenceRequest, blocks [][]byte) []uint64 {
	prev := approximateprefix.ChainSeed(chainVersion, p.tenantSalt, request.TargetModel)
	ids := make([]uint64, len(blocks))
	for i, b := range blocks {
		prev = approximateprefix.ChainHash(prev, approximateprefix.HashBlock{Tokens: tokenizer.PackBytes(b)})
		ids[i] = prev
	}
	return ids
}

// declaredID returns the client-declared session id, if any: the configured
// header first, then the in-band session dialect carried in the body (the
// same shape the session-coverage scorer reads as its own fallback).
func (p *Producer) declaredID(request *fwksched.InferenceRequest) string {
	if request.Headers != nil {
		if id := strings.TrimSpace(request.Headers[p.headerName]); id != "" {
			return id
		}
	}
	if request.Body == nil || request.Body.Payload == nil {
		return ""
	}
	payload, ok := request.Body.Payload.AsMap()
	if !ok {
		return ""
	}
	nvext, ok := payload["nvext"].(map[string]any)
	if !ok {
		return ""
	}
	sessionControl, ok := nvext["session_control"].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := sessionControl["session_id"].(string)
	return strings.TrimSpace(id)
}

func (p *Producer) sweep(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.index.removeExpired()
		}
	}
}

// contentBlocks flattens a request body into framed per-message content
// blocks — the unit of identity chaining. Every part is length-prefixed
// (uvarint), so distinct message splits always yield distinct byte streams;
// content is carried verbatim (content that affects the KV changes identity,
// which is exactly as durable as the KV it names).
func contentBlocks(body *fwkrh.InferenceRequestBody) [][]byte {
	switch {
	case body.ChatCompletions != nil:
		chat := body.ChatCompletions
		var blocks [][]byte
		if len(chat.Tools) > 0 {
			if raw, err := json.Marshal(chat.Tools); err == nil {
				blocks = append(blocks, frame(frameString(nil, "tools"), raw))
			}
		}
		for _, msg := range chat.Messages {
			blocks = append(blocks, messageBlock(msg))
		}
		return blocks

	case body.Messages != nil:
		req := body.Messages
		var blocks [][]byte
		if len(req.Tools) > 0 {
			if raw, err := json.Marshal(req.Tools); err == nil {
				blocks = append(blocks, frame(frameString(nil, "tools"), raw))
			}
		}
		if system := anthropicSystemBlock(req.System); system != nil {
			blocks = append(blocks, system)
		}
		for _, msg := range req.Messages {
			blocks = append(blocks, anthropicMessageBlock(msg))
		}
		return blocks

	case body.Completions != nil:
		return rawPromptBlocks("prompt", body.Completions.Prompt.PlainText())

	default:
		return nil
	}
}

// rawBlockBytes is the chunk size for unstructured (completions-style)
// prompts, which carry no message boundaries to anchor on: ~256 estimated
// tokens at 4 bytes/token, the only boundary raw text offers.
const rawBlockBytes = 1024

// rawPromptBlocks chunks an unstructured prompt into fixed-size framed
// blocks. The trailing partial chunk is EXCLUDED from identity: it is still
// growing, and dropping it keeps turn N's chain a strict prefix of turn
// N+1's — a conversation replayed as raw text extends its own lineage
// instead of forking off it every turn. Prompts shorter than one chunk
// derive no identity (consumers fall back to declared ids).
func rawPromptBlocks(label, text string) [][]byte {
	if len(text) < rawBlockBytes {
		return nil
	}
	data := []byte(text)
	var blocks [][]byte
	for len(data) >= rawBlockBytes {
		b := frameString(nil, label)
		b = frame(b, data[:rawBlockBytes])
		blocks = append(blocks, b)
		data = data[rawBlockBytes:]
	}
	return blocks
}

// messageBlock frames one message: role, then each content part as
// (type, payload). Non-text parts serialize as their JSON block so content
// that affects the KV (images, audio) affects identity.
func messageBlock(msg fwkrh.Message) []byte {
	b := frameString(nil, msg.Role)
	if msg.Content.Raw != "" {
		b = frameString(b, "text")
		b = frameString(b, msg.Content.Raw)
		return b
	}
	for _, part := range msg.Content.Structured {
		if part.Type == "text" {
			b = frameString(b, "text")
			b = frameString(b, part.Text)
			continue
		}
		if raw, err := json.Marshal(part); err == nil {
			b = frameString(b, part.Type)
			b = frame(b, raw)
		}
	}
	return b
}

// anthropicSystemBlock frames the /v1/messages system prompt, or nil when
// absent. The system field accepts only text.
func anthropicSystemBlock(system fwkrh.AnthropicContent) []byte {
	b := frameString(nil, "system")
	parts := 0
	if system.Raw != "" {
		b = frameString(b, "text")
		b = frameString(b, system.Raw)
		parts++
	} else {
		for _, part := range system.Structured {
			if part.Type == "text" {
				b = frameString(b, "text")
				b = frameString(b, part.Text)
				parts++
			}
		}
	}
	if parts == 0 {
		return nil
	}
	return b
}

// anthropicMessageBlock frames one /v1/messages message the same way
// messageBlock frames a chat-completions message.
func anthropicMessageBlock(msg fwkrh.AnthropicMessage) []byte {
	b := frameString(nil, msg.Role)
	if msg.Content.Raw != "" {
		b = frameString(b, "text")
		b = frameString(b, msg.Content.Raw)
		return b
	}
	for _, part := range msg.Content.Structured {
		if part.Type == "text" {
			b = frameString(b, "text")
			b = frameString(b, part.Text)
			continue
		}
		if raw, err := json.Marshal(part); err == nil {
			b = frameString(b, part.Type)
			b = frame(b, raw)
		}
	}
	return b
}

// frame appends part to dst prefixed with its uvarint length.
func frame(dst, part []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(part)))
	return append(dst, part...)
}

func frameString(dst []byte, s string) []byte {
	return frame(dst, []byte(s))
}
