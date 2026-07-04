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

// Package sessioncoverage provides a scorer that routes requests belonging to
// a session toward the endpoints that already hold that session's KV state.
//
// The scorer maintains an in-memory session index: for each session id it
// records, per endpoint, the high-water token count ("coverage") known to be
// resident on that endpoint. The index is fed from two sources:
//
//   - PreRequest: when a session request is scheduled, the destination's
//     coverage is optimistically raised to the request's estimated prompt
//     token count, so concurrent and immediately-following requests of the
//     same session observe the placement before the response completes.
//   - ResponseBody (EndOfStream): the destination's coverage is raised to
//     usage.prompt_tokens + usage.completion_tokens reported by the model
//     server, replacing the estimate with ground truth. No tokenization
//     happens in the router.
//
// Response-fed coverage is the fallback for engines that publish no KV
// events; event-fed session residency (session-residency producer) overlays
// it as per-endpoint maxima when present, or replaces it outright in
// residencyAuthoritative mode.
//
// Session identity comes from producer attributes: the chain-identity
// producer's derived lineage id when present (canonical — declared ids alias
// it inside the producer), then the session-id producer's attribute, then the
// configured request header. Chain identity also publishes fork discovery
// (ForkParentDataKey): a yet-unknown session naming a known parent adopts the
// parent's coverage on first sight and diverges from there, and a rewritten
// history becomes a new lineage by construction — cross-session KV sharing
// and history-rewrite handling need no scorer-side declarations or
// heuristics.
//
// Score prices each endpoint on a continuum between longest-prefix-match and
// least-loaded: cost_e = gap_e + queueWeight * inflight_e, in estimated-token
// units, where gap_e is the request's prompt estimate minus the endpoint's
// clamped coverage. In-flight work is tracked inside the plugin from
// PreRequest/ResponseBody, so it is fresh under bursts where scraped metrics
// lag. Per request, costs are normalized to [0, 1] scores (lowest cost scores
// highest). With no coverage anywhere placement degrades to
// least-estimated-load, optionally biased away from pods holding protected
// session KV (sacrificialWeight).
package sessioncoverage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	logutil "github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrsession "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/session"
)

const (
	// SessionCoverageScorerType is the type of the SessionCoverage scorer.
	SessionCoverageScorerType = "session-coverage-scorer"

	defaultHeaderName      = "x-session-id"
	defaultCharsPerToken   = 4.0
	defaultSessionTTL      = 30 * time.Minute
	defaultMaxSessions     = 100_000
	defaultQueueWeight     = 1.0
	defaultDecodeAllowance = 750.0

	// inFlightTTL bounds how long an in-flight accounting entry may live
	// without its response arriving (crashed streams, dropped clients).
	inFlightTTL = 15 * time.Minute

	// sweepInterval is how often expired sessions are removed in the background.
	sweepInterval = time.Minute
	// capEvictBatch bounds the number of arbitrary entries dropped when the
	// index is at capacity and none are expired (overload safety valve).
	capEvictBatch = 128
)

// parameters configures the SessionCoverage scorer.
type parameters struct {
	// HeaderName is the request header carrying the session id. Defaults to
	// x-session-id. Producer attributes (chain-identity, session-id-producer)
	// take precedence over the header.
	HeaderName string `json:"headerName"`
	// CharsPerToken is the characters-per-token ratio used to estimate prompt
	// tokens when no tokenized prompt is available. Defaults to 4.0.
	CharsPerToken float64 `json:"charsPerToken"`
	// SessionTTLSeconds is the idle time after which a session entry is
	// dropped from the index. Defaults to 1800 (30 minutes).
	SessionTTLSeconds int `json:"sessionTTLSeconds"`
	// MaxSessions caps the number of tracked sessions. Defaults to 100000.
	MaxSessions int `json:"maxSessions"`
	// QueueWeight blends endpoint load into the placement cost:
	// cost_e = gap_e + QueueWeight * inflight_e, making placement a continuum
	// between longest-prefix-match (0) and least-loaded (large). In-flight
	// work is tracked inside the plugin from PreRequest/ResponseBody, so it
	// is fresh under bursts where scraped metrics lag. Defaults to 1.0;
	// negative values clamp to 0.
	QueueWeight float64 `json:"queueWeight"`
	// DecodeAllowanceTokens prices a resident request's decode phase, in
	// estimated-token units added to its remaining prefill gap when
	// accounting in-flight work. Decode tokens are far slower than prefill
	// tokens (measured ~17x on H200/32B), so 100 output tokens occupy about
	// 700-800 estimate units. Defaults to 750.
	DecodeAllowanceTokens float64 `json:"decodeAllowanceTokens"`
	// ResidencyAuthoritative treats event-fed session residency as engine
	// truth: when a request carries a SessionResidency attribute, residency
	// REPLACES response-fed coverage (pods absent from residency are cold —
	// their KV was evicted). Sound when all traffic is identity-tagged.
	// Default false: residency merges as per-endpoint maxima, which is safe
	// under partial tagging but keeps stale belief alive across evictions.
	ResidencyAuthoritative bool `json:"residencyAuthoritative"`
	// SacrificialWeight prices each endpoint's protected session mass into
	// the cost of AFFINITY-LESS requests (no coverage anywhere): fresh and
	// one-shot traffic is steered toward pods with the least session KV to
	// lose, quarantining cache pressure deliberately instead of by placement
	// accident. Requests with any affinity are unaffected. Uses the
	// PodProtectedMass attribute (session-residency producer). Default 0
	// (off).
	SacrificialWeight float64 `json:"sacrificialWeight"`
}

// compile-time type assertions
var (
	_ fwksched.Scorer                      = &SessionCoverage{}
	_ requestcontrol.PreRequest            = &SessionCoverage{}
	_ requestcontrol.ResponseBodyProcessor = &SessionCoverage{}
)

// Factory defines the factory function for the SessionCoverage scorer.
func Factory(name string, rawParameters *json.Decoder, handle plugin.Handle) (plugin.Plugin, error) {
	params := parameters{}
	if rawParameters != nil {
		if err := rawParameters.Decode(&params); err != nil {
			return nil, fmt.Errorf("failed to parse the parameters of the '%s' scorer - %w", SessionCoverageScorerType, err)
		}
	}

	ctx := context.Background()
	if handle != nil {
		ctx = handle.Context()
	}
	return New(ctx, name, params), nil
}

// New returns a SessionCoverage scorer. Zero-valued parameters fall back to
// their defaults. The provided context bounds the background sweeper that
// evicts idle sessions.
func New(ctx context.Context, name string, params parameters) *SessionCoverage {
	headerName := strings.ToLower(strings.TrimSpace(params.HeaderName))
	if headerName == "" {
		headerName = defaultHeaderName
	}
	charsPerToken := params.CharsPerToken
	if charsPerToken <= 0 {
		charsPerToken = defaultCharsPerToken
	}
	sessionTTL := defaultSessionTTL
	if params.SessionTTLSeconds > 0 {
		sessionTTL = time.Duration(params.SessionTTLSeconds) * time.Second
	}
	maxSessions := params.MaxSessions
	if maxSessions <= 0 {
		maxSessions = defaultMaxSessions
	}
	queueWeight := params.QueueWeight
	if queueWeight == 0 {
		queueWeight = defaultQueueWeight
	}
	if queueWeight < 0 {
		queueWeight = 0
	}
	decodeAllowance := params.DecodeAllowanceTokens
	if decodeAllowance == 0 {
		decodeAllowance = defaultDecodeAllowance
	}
	if decodeAllowance < 0 {
		decodeAllowance = 0
	}

	s := &SessionCoverage{
		typedName:              plugin.TypedName{Type: SessionCoverageScorerType, Name: name},
		headerName:             headerName,
		charsPerToken:          charsPerToken,
		sessionTTL:             sessionTTL,
		maxSessions:            maxSessions,
		queueWeight:            queueWeight,
		decodeAllowance:        decodeAllowance,
		residencyAuthoritative: params.ResidencyAuthoritative,
		sacrificialWeight:      math.Max(0, params.SacrificialWeight),
		now:                    time.Now,
		sessions:               map[string]*sessionEntry{},
		inFlight:               map[string]*inFlightEntry{},
		podLoad:                map[string]int64{},
	}
	if ctx != nil {
		go s.sweep(ctx)
	}
	return s
}

// SessionCoverage scores endpoints by the placement cost of the incoming
// request, according to the session index and in-flight load accounting.
type SessionCoverage struct {
	typedName     plugin.TypedName
	headerName    string
	charsPerToken float64
	sessionTTL    time.Duration
	maxSessions   int

	queueWeight            float64
	decodeAllowance        float64
	residencyAuthoritative bool
	sacrificialWeight      float64

	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*sessionEntry
	// inFlight tracks scheduled-but-unfinished requests by request id;
	// podLoad aggregates their estimated tokens per endpoint. Maintained from
	// PreRequest/ResponseBody so it is fresh under bursts, unlike scraped
	// metrics.
	inFlight map[string]*inFlightEntry
	podLoad  map[string]int64
}

// inFlightEntry accounts one scheduled request's estimated work: the prefill
// gap it was admitted with plus the decode allowance.
type inFlightEntry struct {
	pod     string
	work    int64
	started time.Time
}

// sessionEntry tracks one session's per-endpoint coverage high-water marks.
type sessionEntry struct {
	// coverage maps endpoint (namespaced pod name) to the highest token count
	// known resident there for this session, in response-usage units.
	coverage map[string]int64
	lastSeen time.Time
}

// TypedName returns the typed name of the plugin.
func (s *SessionCoverage) TypedName() plugin.TypedName {
	return s.typedName
}

// Category returns the preference the scorer applies when scoring candidate endpoints.
func (s *SessionCoverage) Category() fwksched.ScorerCategory {
	return fwksched.Affinity
}

// Score prices each endpoint on the placement-cost continuum and normalizes
// the costs to [0, 1] scores (lowest cost scores highest). Requests without
// session identity or a usable prompt estimate score zero everywhere.
func (s *SessionCoverage) Score(ctx context.Context, request *fwksched.InferenceRequest, endpoints []fwksched.Endpoint) map[fwksched.Endpoint]float64 {
	scores := make(map[fwksched.Endpoint]float64, len(endpoints))
	for _, endpoint := range endpoints {
		scores[endpoint] = 0.0
	}

	sid := s.sessionID(request)
	if sid == "" {
		return scores
	}
	x := s.estimatePromptTokens(request)
	if x <= 0 {
		return scores
	}

	coverage := s.effectiveCoverage(sid, s.forkParent(request))
	coverage = s.mergeResidency(request, endpoints, coverage)

	// Placement cost (continuum between longest-prefix-match and
	// least-loaded): cost_e = gap_e + queueWeight * inflight_e, all in
	// estimated-token units. A warm endpoint loses its edge once the work
	// queued on it outweighs the prefill it saves; with no coverage anywhere
	// this degrades to least-estimated-load placement — optionally biased
	// away from pods holding protected session KV (sacrificial placement):
	// pressure from unaffiliated traffic is quarantined deliberately.
	load := s.podLoadSnapshot()
	sacrificial := s.sacrificialMass(request, endpoints, coverage)
	costs := make(map[fwksched.Endpoint]float64, len(endpoints))
	minCost, maxCost := math.Inf(1), math.Inf(-1)
	for _, endpoint := range endpoints {
		key := endpoint.GetMetadata().NamespacedName.String()
		var c int64
		if coverage != nil {
			c = coverage[key]
			if c > x {
				c = x
			}
		}
		cost := float64(x-c) + s.queueWeight*float64(load[key])
		if sacrificial != nil {
			cost += s.sacrificialWeight * float64(sacrificial[key])
		}
		costs[endpoint] = cost
		minCost = math.Min(minCost, cost)
		maxCost = math.Max(maxCost, cost)
	}
	for endpoint, cost := range costs {
		if maxCost > minCost {
			scores[endpoint] = (maxCost - cost) / (maxCost - minCost)
		} else {
			scores[endpoint] = 0.5
		}
	}
	return scores
}

// podLoadSnapshot returns a copy of the per-endpoint in-flight token counts.
func (s *SessionCoverage) podLoadSnapshot() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	load := make(map[string]int64, len(s.podLoad))
	for pod, tokens := range s.podLoad {
		load[pod] = tokens
	}
	return load
}

// trackInFlight records a scheduled request's estimated work on its endpoint.
// A re-track of the same request id (retry, reschedule) moves the work.
func (s *SessionCoverage) trackInFlight(requestID, pod string, work int64) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.inFlight[requestID]; ok {
		s.decrementLoadLocked(prev)
	}
	s.inFlight[requestID] = &inFlightEntry{pod: pod, work: work, started: now}
	s.podLoad[pod] += work
}

// releaseInFlight settles a request's in-flight accounting.
func (s *SessionCoverage) releaseInFlight(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.inFlight[requestID]
	if !ok {
		return
	}
	delete(s.inFlight, requestID)
	s.decrementLoadLocked(entry)
}

// decrementLoadLocked removes an entry's tokens from its endpoint's load.
// Callers must hold s.mu.
func (s *SessionCoverage) decrementLoadLocked(entry *inFlightEntry) {
	if remaining := s.podLoad[entry.pod] - entry.work; remaining > 0 {
		s.podLoad[entry.pod] = remaining
	} else {
		delete(s.podLoad, entry.pod)
	}
}

// effectiveCoverage returns a copy of the session's per-endpoint coverage
// (after fork adoption), or nil when the session is unknown.
func (s *SessionCoverage) effectiveCoverage(sid, forkFrom string) map[string]int64 {
	if sid == "" {
		return nil
	}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.ownEntryLocked(sid, forkFrom)
	if entry == nil {
		return nil
	}
	entry.lastSeen = now
	coverage := make(map[string]int64, len(entry.coverage))
	for pod, c := range entry.coverage {
		coverage[pod] = c
	}
	return coverage
}

// ownEntryLocked returns the session's entry, creating it by fork adoption
// when the session is unknown but names a known parent: the child copies the
// parent's coverage on first sight and diverges from there. Callers must
// hold s.mu.
func (s *SessionCoverage) ownEntryLocked(sid, forkFrom string) *sessionEntry {
	if entry, ok := s.sessions[sid]; ok {
		return entry
	}
	if forkFrom == "" {
		return nil
	}
	parent, ok := s.sessions[forkFrom]
	if !ok {
		return nil
	}
	entry := &sessionEntry{
		coverage: make(map[string]int64, len(parent.coverage)),
	}
	for pod, c := range parent.coverage {
		entry.coverage[pod] = c
	}
	s.sessions[sid] = entry
	return entry
}

// PreRequest optimistically raises the scheduled endpoint's coverage to the
// request's estimated prompt tokens, so the placement is visible to the next
// request of the session before the response completes, and accounts the
// request's estimated in-flight work on its endpoint.
func (s *SessionCoverage) PreRequest(ctx context.Context, request *fwksched.InferenceRequest, schedulingResult *fwksched.SchedulingResult) {
	pod := primaryTargetPod(schedulingResult)
	if pod == "" {
		return
	}
	x := s.estimatePromptTokens(request)
	if x <= 0 {
		return
	}
	sid := s.sessionID(request)
	// Load accounting covers every scheduled request, session-tagged or not,
	// so the cost term sees the endpoint's full in-flight picture. A request
	// occupies its admission-time prefill gap plus the decode allowance —
	// covered prefixes queue no prefill work.
	if s.queueWeight > 0 && request.RequestID != "" {
		gap := x
		coverage := s.effectiveCoverage(sid, s.forkParent(request))
		coverage = s.mergeResidency(request, primaryTargetEndpoints(schedulingResult), coverage)
		if c := coverage[pod]; c > 0 {
			if c > x {
				c = x
			}
			gap = x - c
		}
		s.trackInFlight(request.RequestID, pod, gap+int64(s.decodeAllowance))
	}
	if sid == "" {
		return
	}
	s.bump(ctx, sid, pod, x, s.forkParent(request))
}

// ResponseBody raises the serving endpoint's coverage to the token usage
// reported by the model server. Streaming chunks are ignored until the final
// one; responses without usage (e.g. streams without include_usage) leave the
// PreRequest estimate in place.
//
// TODO: an explicit end-of-session declaration (e.g. Dynamo's
// nvext.session_control "close") should release the session's index entry
// here instead of leaving it to the TTL sweep. Identity dialects are parsed
// in producers (chain-identity reads nvext as an alias source); if
// close-release matters it belongs there, published as an attribute — not as
// scorer-side body parsing.
func (s *SessionCoverage) ResponseBody(ctx context.Context, request *fwksched.InferenceRequest, response *requestcontrol.Response, targetEndpoint *datalayer.EndpointMetadata) {
	if response == nil || !response.EndOfStream {
		return
	}
	if request != nil && request.RequestID != "" {
		s.releaseInFlight(request.RequestID)
	}
	if targetEndpoint == nil {
		return
	}
	sid := s.sessionID(request)
	if sid == "" {
		return
	}
	total := int64(response.Usage.PromptTokens) + int64(response.Usage.CompletionTokens)
	if total <= 0 {
		return
	}
	pod := targetEndpoint.NamespacedName.String()
	s.bump(ctx, sid, pod, total, s.forkParent(request))

	if logger := log.FromContext(ctx); logger.V(logutil.TRACE).Enabled() {
		cached := 0
		if response.Usage.PromptTokenDetails != nil {
			cached = response.Usage.PromptTokenDetails.CachedTokens
		}
		logger.V(logutil.TRACE).Info("session coverage updated from response",
			"scorer", s.typedName.String(), "session", sid, "endpoint", pod,
			"promptTokens", response.Usage.PromptTokens, "completionTokens", response.Usage.CompletionTokens,
			"cachedTokens", cached)
	}
}

// bump raises the session's coverage high-water mark on the given endpoint.
// The mark is monotone; stale smaller values never overwrite. A yet-unknown
// session naming a known fork parent adopts the parent's coverage first.
func (s *SessionCoverage) bump(ctx context.Context, sid, pod string, tokens int64, forkFrom string) {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.ownEntryLocked(sid, forkFrom)
	if entry == nil {
		if len(s.sessions) >= s.maxSessions {
			s.evictLocked(ctx, now)
		}
		entry = &sessionEntry{coverage: map[string]int64{}}
		s.sessions[sid] = entry
	}
	if tokens > entry.coverage[pod] {
		entry.coverage[pod] = tokens
	}
	entry.lastSeen = now
}

// evictLocked drops expired sessions and, if the index is still at capacity,
// up to capEvictBatch arbitrary entries. Callers must hold s.mu.
func (s *SessionCoverage) evictLocked(ctx context.Context, now time.Time) {
	cutoff := now.Add(-s.sessionTTL)
	for sid, entry := range s.sessions {
		if entry.lastSeen.Before(cutoff) {
			delete(s.sessions, sid)
		}
	}
	if len(s.sessions) < s.maxSessions {
		return
	}
	dropped := 0
	for sid := range s.sessions {
		delete(s.sessions, sid)
		dropped++
		if dropped >= capEvictBatch {
			break
		}
	}
	log.FromContext(ctx).V(logutil.DEFAULT).Info("session index at capacity, dropped entries",
		"scorer", s.typedName.String(), "dropped", dropped, "maxSessions", s.maxSessions)
}

// sweep periodically removes idle sessions until ctx is cancelled.
func (s *SessionCoverage) sweep(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.removeExpired()
		}
	}
}

func (s *SessionCoverage) removeExpired() {
	now := s.now()
	cutoff := now.Add(-s.sessionTTL)
	inFlightCutoff := now.Add(-inFlightTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	for sid, entry := range s.sessions {
		if entry.lastSeen.Before(cutoff) {
			delete(s.sessions, sid)
		}
	}
	// Requests whose response never arrived (crashed streams, dropped
	// clients) must not pin phantom load on an endpoint forever.
	for requestID, entry := range s.inFlight {
		if entry.started.Before(inFlightCutoff) {
			delete(s.inFlight, requestID)
			s.decrementLoadLocked(entry)
		}
	}
}

// forkParent returns the fork-parent lineage id published by the
// chain-identity producer, or "".
func (s *SessionCoverage) forkParent(request *fwksched.InferenceRequest) string {
	if id, ok := attrsession.ReadForkParent(request); ok {
		return string(id)
	}
	return ""
}

// headerValue returns the trimmed value of the named request header.
func (s *SessionCoverage) headerValue(request *fwksched.InferenceRequest, name string) string {
	if request == nil || request.Headers == nil || name == "" {
		return ""
	}
	return strings.TrimSpace(request.Headers[name])
}

// sessionID resolves the request's session id: the chain-identity producer's
// derived lineage id when present (canonical — declared ids alias it inside
// the producer), then the session-id-producer attribute, then the configured
// request header.
func (s *SessionCoverage) sessionID(request *fwksched.InferenceRequest) string {
	if request == nil {
		return ""
	}
	if id, ok := attrsession.ReadDerivedSessionID(request); ok && id != "" {
		return string(id)
	}
	if id, ok := attrsession.ReadSessionID(request); ok && id != "" {
		return string(id)
	}
	return s.headerValue(request, s.headerName)
}

// estimatePromptTokens estimates the request's prompt token count without
// tokenizing in the router: the parser-provided token count when available,
// otherwise flattened text length divided by charsPerToken, otherwise the raw
// body size. Only relative consistency across endpoints matters for scoring.
func (s *SessionCoverage) estimatePromptTokens(request *fwksched.InferenceRequest) int64 {
	if request == nil {
		return 0
	}
	if body := request.Body; body != nil {
		if n := body.TokenizedPrompt.TokenCount(); n > 0 {
			return int64(n)
		}
		chars := 0
		switch {
		case body.ChatCompletions != nil:
			for _, msg := range body.ChatCompletions.Messages {
				chars += len(msg.Content.PlainText())
			}
		case body.Completions != nil:
			chars = len(body.Completions.Prompt.PlainText())
		}
		if chars > 0 {
			return int64(float64(chars)/s.charsPerToken) + 1
		}
	}
	if request.RequestSizeBytes > 0 {
		return int64(float64(request.RequestSizeBytes) / s.charsPerToken)
	}
	return 0
}

// primaryTargetPod returns the namespaced name of the primary profile's first
// target endpoint, or "" when unavailable.
func primaryTargetPod(result *fwksched.SchedulingResult) string {
	endpoints := primaryTargetEndpoints(result)
	if len(endpoints) == 0 {
		return ""
	}
	metadata := endpoints[0].GetMetadata()
	if metadata == nil {
		return ""
	}
	return metadata.NamespacedName.String()
}

// primaryTargetEndpoints returns the primary profile's target endpoints.
func primaryTargetEndpoints(result *fwksched.SchedulingResult) []fwksched.Endpoint {
	if result == nil {
		return nil
	}
	profileResult, ok := result.ProfileResults[result.PrimaryProfileName]
	if !ok || profileResult == nil {
		return nil
	}
	return profileResult.TargetEndpoints
}

// sacrificialMass returns per-endpoint protected session mass (engine
// units, keyed by endpoint name) for AFFINITY-LESS requests when sacrificial
// placement is enabled, nil otherwise. A request with any coverage anywhere
// is session traffic following its own KV; only unaffiliated traffic is
// steered away from session-heavy pods.
func (s *SessionCoverage) sacrificialMass(request *fwksched.InferenceRequest, endpoints []fwksched.Endpoint, coverage map[string]int64) map[string]int64 {
	if s.sacrificialWeight <= 0 {
		return nil
	}
	for _, c := range coverage {
		if c > 0 {
			return nil
		}
	}
	mass, ok := attrsession.ReadPodProtectedMass(request)
	if !ok || len(mass) == 0 {
		return nil
	}
	out := make(map[string]int64, len(endpoints))
	for _, endpoint := range endpoints {
		meta := endpoint.GetMetadata()
		if meta == nil {
			continue
		}
		key := meta.NamespacedName.String()
		if meta.Address != "" {
			out[key] = int64(mass[meta.Address+":"+meta.Port])
		}
	}
	return out
}

// mergeResidency folds event-fed session residency (eviction-aware, engine
// units, published by the session-residency producer) into the response-fed
// coverage map. Residency pods are keyed "addr:port" as on KV events and
// mapped to endpoint keys via endpoint metadata.
//
// Default mode overlays per-endpoint maxima — safe under partial tagging but
// stale belief survives evictions. In residencyAuthoritative mode a present
// residency attribute REPLACES response-fed coverage: pods absent from
// residency are cold, their KV was evicted. Requests without the attribute
// fall back to response-fed coverage in both modes.
func (s *SessionCoverage) mergeResidency(request *fwksched.InferenceRequest, endpoints []fwksched.Endpoint, coverage map[string]int64) map[string]int64 {
	residency, ok := attrsession.ReadSessionResidency(request)
	if !ok || len(residency) == 0 {
		return coverage
	}
	byAddr := make(map[string]string, 2*len(endpoints))
	for _, endpoint := range endpoints {
		meta := endpoint.GetMetadata()
		if meta == nil {
			continue
		}
		key := meta.NamespacedName.String()
		if meta.Address != "" {
			byAddr[meta.Address+":"+meta.Port] = key
		}
		byAddr[key] = key
	}
	if s.residencyAuthoritative {
		coverage = map[string]int64{}
	}
	for _, r := range residency {
		key, ok := byAddr[r.Pod]
		if !ok || r.Tokens <= 0 {
			continue
		}
		if coverage == nil {
			coverage = map[string]int64{}
		}
		if int64(r.Tokens) > coverage[key] {
			coverage[key] = int64(r.Tokens)
		}
	}
	return coverage
}
