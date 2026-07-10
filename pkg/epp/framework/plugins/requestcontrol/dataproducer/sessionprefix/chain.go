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

package sessionprefix

import (
	"encoding/binary"
	"encoding/json"

	"github.com/cespare/xxhash/v2"

	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
)

const (
	// chainVersion seeds every chain; bumping it is a deliberate fleet-wide
	// identity rollover (a normalization change re-keys everything).
	chainVersion = "llm-d/session-prefix/v1"

	// textChunkBytes is the chunk size for text runs: ~256 estimated tokens
	// at 4 bytes/token. Message boundaries alone are too coarse — clients
	// that grow a conversation inside a single message (raw completions,
	// single-message chat) would re-key the whole prompt every turn.
	textChunkBytes = 1024

	// maxBlocksPerChain bounds per-request hashing and per-session append
	// registration. Blocks past the cap are excluded from identity the same
	// way a trailing partial raw chunk is: coverage clamps at the cap.
	maxBlocksPerChain = 2048

	// textPartType is the content-part type framed with its text verbatim;
	// other part types are framed as their JSON serialization.
	textPartType = "text"
)

// contentBlock is one framed unit of identity chaining, carrying the
// cumulative content chars through this block so a chain-prefix match maps
// directly to a char extent of the incoming prompt.
type contentBlock struct {
	framed   []byte
	cumChars int
}

// blockBuilder accumulates framed content blocks and the running content
// char count. Blocks past maxBlocksPerChain are excluded from identity but
// still counted, the same way excluded partial tails are.
type blockBuilder struct {
	blocks []contentBlock
	cum    int
}

// add emits one opaque framed block covering chars content chars.
func (bb *blockBuilder) add(framed []byte, chars int) {
	bb.cum += chars
	if len(bb.blocks) < maxBlocksPerChain {
		bb.blocks = append(bb.blocks, contentBlock{framed: framed, cumChars: bb.cum})
	}
}

// addTextRun chunks one text run into framed blocks of textChunkBytes:
// frame(role) || frame(type) || chunk index || frame(chunk). Full chunks are
// always emitted. A trailing partial chunk is emitted only when the run fits
// in a single chunk — message-like content, immutable once sent, so a later
// turn re-presents it byte-identically. A run spanning full chunks is
// raw-text-like: its partial tail is the growing frontier and is EXCLUDED,
// keeping turn N's chain a strict prefix of turn N+1's when the run grows in
// place (raw completions, single-message conversations). The chunk index in
// the frame keeps chunk-aligned message splits distinct.
func (bb *blockBuilder) addTextRun(role, text string) {
	data := []byte(text)
	if len(data) < textChunkBytes {
		if len(data) > 0 {
			bb.add(textRunChunk(role, 0, data), len(data))
		}
		return
	}
	idx := 0
	for len(data) >= textChunkBytes {
		bb.add(textRunChunk(role, idx, data[:textChunkBytes]), textChunkBytes)
		data = data[textChunkBytes:]
		idx++
	}
	// The partial tail of a multi-chunk run is excluded from identity but
	// still counted toward content chars.
	bb.cum += len(data)
}

func textRunChunk(role string, idx int, chunk []byte) []byte {
	b := frameString(nil, role)
	b = frameString(b, textPartType)
	b = binary.AppendUvarint(b, uint64(idx))
	return frame(b, chunk)
}

// contentBlocks flattens a request body into framed content blocks — the
// unit of identity chaining — and returns the total content chars of the
// prompt (which may exceed the last block's cumChars when a partial tail or
// over-cap blocks are excluded from identity). Every part is length-prefixed
// (uvarint), so distinct message splits always yield distinct byte streams;
// content is carried verbatim (content that affects the KV changes identity,
// which is exactly as durable as the KV it names). Bodies without message
// structure or raw text (Responses, Conversations, Embeddings, Generate)
// derive no identity.
func contentBlocks(body *fwkrh.InferenceRequestBody) ([]contentBlock, int) {
	if body == nil {
		return nil, 0
	}
	bb := &blockBuilder{}

	switch {
	case body.ChatCompletions != nil:
		chat := body.ChatCompletions
		bb.addTools(chat.Tools)
		for _, msg := range chat.Messages {
			bb.addMessage(msg.Role, msg.Content.Raw, msg.Content.Structured, nil)
		}

	case body.Messages != nil:
		req := body.Messages
		bb.addTools(req.Tools)
		bb.addMessage("system", req.System.Raw, nil, req.System.Structured)
		for _, msg := range req.Messages {
			bb.addMessage(msg.Role, msg.Content.Raw, nil, msg.Content.Structured)
		}

	case body.Completions != nil:
		bb.addTextRun("prompt", body.Completions.Prompt.PlainText())

	default:
		return nil, 0
	}
	return bb.blocks, bb.cum
}

// addTools frames the request's tool catalog as one leading block. Char
// count is the serialized length: tool schemas occupy prompt context like
// any other content.
func (bb *blockBuilder) addTools(tools []any) {
	if len(tools) == 0 {
		return
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return
	}
	bb.add(frame(frameString(nil, "tools"), raw), len(raw))
}

// addMessage frames one message: text content as chunked text runs, non-text
// parts as their JSON serialization so content that affects the KV (images,
// audio) affects identity. Exactly one of raw, parts, or anthropicParts is
// expected to be populated.
func (bb *blockBuilder) addMessage(role, raw string, parts []fwkrh.ContentBlock, anthropicParts []fwkrh.AnthropicContentBlock) {
	if raw != "" {
		bb.addTextRun(role, raw)
		return
	}
	for _, part := range parts {
		if part.Type == textPartType {
			bb.addTextRun(role, part.Text)
			continue
		}
		if encoded, err := json.Marshal(part); err == nil {
			bb.add(frame(frameString(frameString(nil, role), part.Type), encoded), len(encoded))
		}
	}
	for _, part := range anthropicParts {
		if part.Type == textPartType {
			bb.addTextRun(role, part.Text)
			continue
		}
		if encoded, err := json.Marshal(part); err == nil {
			bb.add(frame(frameString(frameString(nil, role), part.Type), encoded), len(encoded))
		}
	}
}

// chainFrom folds the framed content blocks into append ids, seeded by
// identity version, model (KV is model-specific, so identical content on
// different models must not share identity), and the request's cache salt.
// Hash-prefix equality holds exactly when content prefixes are
// byte-identical — the condition for KV reuse.
func chainFrom(model, salt string, blocks []contentBlock) []uint64 {
	seed := frameString(nil, chainVersion)
	seed = frameString(seed, model)
	seed = frameString(seed, salt)
	prev := xxhash.Sum64(seed)

	ids := make([]uint64, len(blocks))
	var buf [16]byte
	for i, b := range blocks {
		binary.LittleEndian.PutUint64(buf[:8], prev)
		binary.LittleEndian.PutUint64(buf[8:], xxhash.Sum64(b.framed))
		prev = xxhash.Sum64(buf[:])
		ids[i] = prev
	}
	return ids
}

// frame appends part to dst prefixed with its uvarint length.
func frame(dst, part []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(part)))
	return append(dst, part...)
}

func frameString(dst []byte, s string) []byte {
	return frame(dst, []byte(s))
}
