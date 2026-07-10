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

// Package sessionprefix provides a DataProducer for session-granularity
// prefix-cache affinity. It derives session identity from request content as
// chained hashes over framed message blocks — hash-prefix equality holds
// exactly when content prefixes are byte-identical, the condition for KV
// reuse — and maintains an estimate-seeded session index mapping each
// session to the engines holding its prefix (the prefix is assumed to live
// where the turn was served; engine-reported usage refines the extent).
// Declared client ids (session headers, prompt_cache_key,
// previous_response_id) alias the derived chain, bridging delta
// continuations.
//
// Per request it publishes the covered fraction of the incoming prompt per
// endpoint as the standard PrefixCacheMatchInfo attribute under this
// producer's name; the generic prefix-cache-scorer binds to it via its
// prefixMatchInfoProducerName parameter. Unlike the approximate producer it
// needs no tokenizer and keeps no per-block state, and unlike the precise
// producer it needs no engine-side KV events — it works against stock
// engines, at the price of estimated (not observed) extents.
package sessionprefix

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	logutil "github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
	tokenproducer "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/tokenizer"
)

// ProducerType is the plugin type registered with the framework.
const ProducerType = "session-prefix-cache-producer"

var (
	_ requestcontrol.DataProducer          = &Producer{}
	_ requestcontrol.PreRequest            = &Producer{}
	_ requestcontrol.ResponseBodyProcessor = &Producer{}
)

// Parameters configures the session-prefix producer.
type Parameters struct {
	// SessionTTLSeconds is the idle time after which a session is dropped
	// from the index. Defaults to 1800 (30 minutes).
	SessionTTLSeconds int `json:"sessionTTLSeconds"`
	// MaxSessions caps the number of tracked sessions. Defaults to 100000.
	MaxSessions int `json:"maxSessions"`
	// CharsPerToken calibrates the char-based token estimate used for
	// extents and covered fractions. Defaults to 4.0.
	CharsPerToken float64 `json:"charsPerToken"`
	// SessionHeaders are the request headers read for declared client
	// session ids, in precedence order. Defaults to x-session-id plus the
	// provider headers recognized by the agent-identity preadmitter.
	SessionHeaders []string `json:"sessionHeaders"`
}

// Producer derives session identity per request, maintains the session
// index, and publishes per-endpoint prefix-cache match info.
type Producer struct {
	typedName      plugin.TypedName
	dk             plugin.DataKey
	charsPerToken  float64
	sessionHeaders []string
	index          *sessionIndex
	pluginState    *plugin.PluginState
}

// Factory builds a Producer from raw plugin parameters.
func Factory(name string, rawParameters *json.Decoder, handle plugin.Handle) (plugin.Plugin, error) {
	params := Parameters{}
	if rawParameters != nil {
		if err := rawParameters.Decode(&params); err != nil {
			return nil, fmt.Errorf("failed to parse the parameters of the '%s' producer: %w", ProducerType, err)
		}
	}
	if params.SessionTTLSeconds < 0 {
		return nil, fmt.Errorf("invalid configuration: sessionTTLSeconds must be >= 0 (current value: %d)", params.SessionTTLSeconds)
	}
	if params.MaxSessions < 0 {
		return nil, fmt.Errorf("invalid configuration: maxSessions must be >= 0 (current value: %d)", params.MaxSessions)
	}
	if params.CharsPerToken < 0 {
		return nil, fmt.Errorf("invalid configuration: charsPerToken must be >= 0 (current value: %f)", params.CharsPerToken)
	}

	ctx := context.Background()
	if handle != nil {
		ctx = handle.Context()
	}
	return New(ctx, name, params, handle), nil
}

// New returns a session-prefix producer. Zero-valued parameters fall back to
// their defaults. The provided context bounds the background sweepers; a nil
// handle disables inactive-pod cleanup.
func New(ctx context.Context, name string, params Parameters, handle plugin.Handle) *Producer {
	ttl := defaultSessionTTL
	if params.SessionTTLSeconds > 0 {
		ttl = time.Duration(params.SessionTTLSeconds) * time.Second
	}
	maxSessions := params.MaxSessions
	if maxSessions <= 0 {
		maxSessions = defaultMaxSessions
	}
	charsPerToken := params.CharsPerToken
	if charsPerToken <= 0 {
		charsPerToken = defaultCharsPerToken
	}
	sessionHeaders := defaultSessionHeaders
	if len(params.SessionHeaders) > 0 {
		sessionHeaders = make([]string, 0, len(params.SessionHeaders))
		for _, h := range params.SessionHeaders {
			// Request headers are lowercased at ingestion.
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				sessionHeaders = append(sessionHeaders, h)
			}
		}
	}

	p := &Producer{
		typedName:      plugin.TypedName{Type: ProducerType, Name: name},
		dk:             attrprefix.PrefixCacheMatchInfoDataKey.WithNonEmptyProducerName(name),
		charsPerToken:  charsPerToken,
		sessionHeaders: sessionHeaders,
		index:          newSessionIndex(ttl, maxSessions),
		pluginState:    plugin.NewPluginState(ctx),
	}
	go p.sweep(ctx)
	if handle != nil {
		go p.cleanUpInactivePods(ctx, handle)
	}
	return p
}

// TypedName returns the type and name of the plugin.
func (p *Producer) TypedName() plugin.TypedName {
	return p.typedName
}

// Produces returns the data produced by the plugin.
func (p *Producer) Produces() map[plugin.DataKey]any {
	return map[plugin.DataKey]any{p.dk: attrprefix.PrefixCacheMatchInfo{}}
}

// Consumes returns the data consumed by the plugin.
func (p *Producer) Consumes() plugin.DataDependencies {
	return plugin.DataDependencies{}
}

// Produce derives the request's session chain, matches it against the index
// (read-only), and publishes the covered fraction of the incoming prompt on
// every endpoint. Requests whose bodies derive no identity publish zero
// coverage so the downstream scorer always finds the attribute.
func (p *Producer) Produce(ctx context.Context, request *fwksched.InferenceRequest, pods []fwksched.Endpoint) error {
	if request == nil {
		return nil
	}
	blocks, totalChars := contentBlocks(request.Body)
	if len(blocks) == 0 {
		// A one-token denominator with zero coverage scores 0 without
		// tripping the scorer's zero-total error branch.
		p.publish(pods, nil, 0, 1)
		return nil
	}

	chain := chainFrom(request.TargetModel, tokenproducer.CacheSaltFromBody(request.Body), blocks)
	declared := p.declaredIDs(request)
	res := p.index.Match(chain, declared)

	x := p.estTokens(totalChars)
	coveredCap := x
	if res.divergent {
		// Only the shared prefix up to the fork point is reusable; the
		// parent's extent past it covers content this request rewrote.
		coveredCap = p.estTokens(blocks[res.matchedIdx].cumChars)
	}
	p.publish(pods, res.engines, coveredCap, x)

	log.FromContext(ctx).V(logutil.DEBUG).Info("session-prefix identity resolved",
		"requestID", request.RequestID, "matched", res.found, "session", res.sessionID,
		"divergent", res.divergent, "estPromptTokens", x)

	p.pluginState.Write(request.RequestID, plugin.StateKey(p.typedName.Name), &produceState{
		chain:    chain,
		declared: declared,
		xEst:     int64(x),
	})
	return nil
}

// PreRequest commits the chain to the index once placement is known: the
// resolved session's extent on the serving engines is raised to the
// estimated prompt tokens — the optimistic seed that makes coverage visible
// to the session's immediately-following turns.
func (p *Producer) PreRequest(ctx context.Context, request *fwksched.InferenceRequest, schedulingResult *fwksched.SchedulingResult) {
	stateKey := plugin.StateKey(p.typedName.Name)
	state, err := plugin.ReadPluginStateKey[*produceState](p.pluginState, request.RequestID, stateKey)
	if err != nil {
		// No identity was derived for this request.
		return
	}

	engines := targetEngines(schedulingResult)
	if len(engines) == 0 {
		p.pluginState.Delete(request.RequestID)
		return
	}

	state.sessionID = p.index.Commit(state.chain, state.declared, engines, state.xEst)
	p.pluginState.Write(request.RequestID, stateKey, state)

	log.FromContext(ctx).V(logutil.DEBUG).Info("session-prefix index seeded",
		"requestID", request.RequestID, "session", state.sessionID, "engines", engines, "estTokens", state.xEst)
}

// ResponseBody, on the final chunk, raises the serving engine's extent to
// the engine-reported prompt plus completion tokens — the only place the
// response's KV contribution is observable — and releases the per-request
// state.
func (p *Producer) ResponseBody(ctx context.Context, request *fwksched.InferenceRequest, response *requestcontrol.Response, targetEndpoint *datalayer.EndpointMetadata) {
	if response == nil || !response.EndOfStream {
		return
	}
	state, err := plugin.ReadPluginStateKey[*produceState](p.pluginState, request.RequestID, plugin.StateKey(p.typedName.Name))
	if err != nil {
		return
	}
	defer p.pluginState.Delete(request.RequestID)
	if state.sessionID == "" || targetEndpoint == nil {
		return
	}
	tokens := int64(response.Usage.PromptTokens + response.Usage.CompletionTokens)
	p.index.RecordUsage(state.sessionID, targetEndpoint.NamespacedName.String(), tokens)
	if v := log.FromContext(ctx).V(logutil.TRACE); v.Enabled() {
		v.Info("session-prefix extent recorded",
			"requestID", request.RequestID, "session", state.sessionID,
			"engine", targetEndpoint.NamespacedName.String(), "tokens", tokens)
	}
}

// publish writes PrefixCacheMatchInfo on every endpoint: covered estimated
// tokens over total estimated tokens, one "block" per token. Endpoints
// absent from extents get zero coverage rather than a missing attribute.
func (p *Producer) publish(pods []fwksched.Endpoint, extents map[string]int64, coveredCap, total int) {
	for _, pod := range pods {
		covered := int64(0)
		if extents != nil {
			covered = extents[pod.GetMetadata().NamespacedName.String()]
		}
		covered = min(covered, int64(coveredCap))
		pod.Put(p.dk.String(), attrprefix.NewPrefixCacheMatchInfo(int(covered), total, 1))
	}
}

// estTokens converts content chars to estimated tokens, rounding up so any
// non-empty prompt has a non-zero denominator.
func (p *Producer) estTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return int(math.Ceil(float64(chars) / p.charsPerToken))
}

// declaredIDs collects the client-declared session ids, namespaced per
// source so ids from different sources never collide: configured headers,
// then prompt_cache_key, then previous_response_id from the request payload.
func (p *Producer) declaredIDs(request *fwksched.InferenceRequest) []string {
	var ids []string
	for _, h := range p.sessionHeaders {
		if v := strings.TrimSpace(request.Headers[h]); v != "" {
			ids = append(ids, "hdr:"+v)
		}
	}
	if request.Body == nil || request.Body.Payload == nil {
		return ids
	}
	payload, ok := request.Body.Payload.AsMap()
	if !ok {
		return ids
	}
	if v, _ := payload["prompt_cache_key"].(string); strings.TrimSpace(v) != "" {
		ids = append(ids, "pck:"+strings.TrimSpace(v))
	}
	if v, _ := payload["previous_response_id"].(string); strings.TrimSpace(v) != "" {
		ids = append(ids, "prid:"+strings.TrimSpace(v))
	}
	return ids
}

// targetEngines returns the engine keys the request was scheduled to: the
// primary profile's endpoint, plus the prefill endpoint in P/D mode.
func targetEngines(schedulingResult *fwksched.SchedulingResult) []string {
	if schedulingResult == nil {
		return nil
	}
	var engines []string
	primary := schedulingResult.ProfileResults[schedulingResult.PrimaryProfileName]
	if primary != nil && len(primary.TargetEndpoints) > 0 {
		engines = append(engines, primary.TargetEndpoints[0].GetMetadata().NamespacedName.String())
	}
	if pr, exists := schedulingResult.ProfileResults[experimentalDefaultPrefillProfile]; exists && len(pr.TargetEndpoints) > 0 {
		engines = append(engines, pr.TargetEndpoints[0].GetMetadata().NamespacedName.String())
	}
	return engines
}

// sweep periodically evicts expired sessions until ctx is done.
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

// cleanUpInactivePods periodically drops extents of pods that left the
// active set.
func (p *Producer) cleanUpInactivePods(ctx context.Context, handle plugin.Handle) {
	ticker := time.NewTicker(podActiveCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			active := make(map[string]struct{})
			for _, nsn := range handle.PodList() {
				active[nsn.String()] = struct{}{}
			}
			p.index.removeEnginesNotIn(active)
		}
	}
}
