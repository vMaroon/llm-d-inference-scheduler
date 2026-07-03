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
// Score returns, per endpoint, the fraction of the incoming request's
// estimated prompt already covered on that endpoint: min(coverage, X) / X.
// Load-aware scorers in the same scheduling profile supply the distribution
// term; this scorer is purely an affinity signal and is inert for requests
// that carry no session id.
//
// A session is a linear append chain: successive requests extend a shared
// prefix, so per-endpoint coverage is a single high-water mark. When a
// request's estimated prompt shrinks well below the session's best-known
// coverage the prefix was rewritten (e.g. history compaction); the entry is
// reset and the session re-places fresh ("rollover" detection).
//
// Cross-session KV sharing (reuse is a forest, not per-session silos):
//
//   - share_as: a request may declare its prompt as a shared root. The root
//     gets its own index entry; its coverage is fed from responses.
//   - root reference: a request may declare that its prompt extends a shared
//     root. Scoring takes the best of the session's own coverage and the
//     root's coverage, and the serving endpoint's root coverage is raised
//     from the response — every endpoint that ever served a child holds the
//     root prefix, so later children spread across warm endpoints.
//   - fork: a new session may declare it forks from a parent session; it
//     adopts the parent's coverage on first sight and diverges from there.
//
// Children referencing a root never bump the root's coverage at scheduling
// time: during a burst that would mark endpoints warm before the KV exists
// and herd the whole burst onto the first pick. Only the declaring (share_as)
// request records its placement optimistically; all other root warmth comes
// from responses, and load scorers spread the cold remainder.
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

	defaultHeaderName        = "x-session-id"
	defaultRootHeaderName    = "x-root-id"
	defaultShareAsHeaderName = "x-share-as"
	defaultForkHeaderName    = "x-fork-from"
	defaultCharsPerToken     = 4.0
	defaultSessionTTL        = 30 * time.Minute
	defaultMaxSessions       = 100_000
	defaultRolloverRatio     = 0.5
	defaultQueueWeight       = 1.0
	defaultDecodeAllowance   = 750.0

	// inFlightTTL bounds how long an in-flight accounting entry may live
	// without its response arriving (crashed streams, dropped clients).
	inFlightTTL = 15 * time.Minute

	// rootKeyPrefix namespaces shared-root entries within the session index.
	rootKeyPrefix = "root:"

	// sweepInterval is how often expired sessions are removed in the background.
	sweepInterval = time.Minute
	// capEvictBatch bounds the number of arbitrary entries dropped when the
	// index is at capacity and none are expired (overload safety valve).
	capEvictBatch = 128
)

// parameters configures the SessionCoverage scorer.
type parameters struct {
	// HeaderName is the request header carrying the session id. Defaults to
	// x-session-id. When the session-id-producer is configured its attribute
	// takes precedence over the header.
	HeaderName string `json:"headerName"`
	// RootHeaderName is the request header declaring that this request's
	// prompt extends a shared root. Defaults to x-root-id.
	RootHeaderName string `json:"rootHeaderName"`
	// ShareAsHeaderName is the request header declaring this request's prompt
	// as a shared root under the given id. Defaults to x-share-as.
	ShareAsHeaderName string `json:"shareAsHeaderName"`
	// ForkHeaderName is the request header declaring that this session forks
	// from the given parent session, adopting its coverage on first sight.
	// Defaults to x-fork-from.
	ForkHeaderName string `json:"forkHeaderName"`
	// CharsPerToken is the characters-per-token ratio used to estimate prompt
	// tokens when no tokenized prompt is available. Defaults to 4.0.
	CharsPerToken float64 `json:"charsPerToken"`
	// SessionTTLSeconds is the idle time after which a session entry is
	// dropped from the index. Defaults to 1800 (30 minutes).
	SessionTTLSeconds int `json:"sessionTTLSeconds"`
	// MaxSessions caps the number of tracked sessions. Defaults to 100000.
	MaxSessions int `json:"maxSessions"`
	// RolloverRatio triggers an entry reset when the incoming prompt estimate
	// falls below this fraction of the session's best-known coverage.
	// Defaults to 0.5 when unset; a negative value disables rollover detection.
	RolloverRatio float64 `json:"rolloverRatio"`
	// QueueWeight blends endpoint load into the placement cost:
	// cost_e = gap_e + QueueWeight * inflight_e, making placement a continuum
	// between longest-prefix-match (0) and least-loaded (large). In-flight
	// work is tracked inside the plugin from PreRequest/ResponseBody, so it
	// is fresh under bursts where scraped metrics lag. Defaults to 1.0; a
	// negative value disables the load term and scores pure coverage
	// fraction.
	QueueWeight float64 `json:"queueWeight"`
	// DecodeAllowanceTokens prices a resident request's decode phase, in
	// estimated-token units added to its remaining prefill gap when
	// accounting in-flight work. Decode tokens are far slower than prefill
	// tokens (measured ~17x on H200/32B), so 100 output tokens occupy about
	// 700-800 estimate units. Defaults to 750.
	DecodeAllowanceTokens float64 `json:"decodeAllowanceTokens"`
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
	rootHeader := strings.ToLower(strings.TrimSpace(params.RootHeaderName))
	if rootHeader == "" {
		rootHeader = defaultRootHeaderName
	}
	shareAsHeader := strings.ToLower(strings.TrimSpace(params.ShareAsHeaderName))
	if shareAsHeader == "" {
		shareAsHeader = defaultShareAsHeaderName
	}
	forkHeader := strings.ToLower(strings.TrimSpace(params.ForkHeaderName))
	if forkHeader == "" {
		forkHeader = defaultForkHeaderName
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
	rolloverRatio := params.RolloverRatio
	if rolloverRatio == 0 {
		rolloverRatio = defaultRolloverRatio
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
		typedName:       plugin.TypedName{Type: SessionCoverageScorerType, Name: name},
		headerName:      headerName,
		rootHeader:      rootHeader,
		shareAsHeader:   shareAsHeader,
		forkHeader:      forkHeader,
		charsPerToken:   charsPerToken,
		sessionTTL:      sessionTTL,
		maxSessions:     maxSessions,
		rolloverRatio:   rolloverRatio,
		queueWeight:     queueWeight,
		decodeAllowance: decodeAllowance,
		now:             time.Now,
		sessions:        map[string]*sessionEntry{},
		inFlight:        map[string]*inFlightEntry{},
		podLoad:         map[string]int64{},
	}
	if ctx != nil {
		go s.sweep(ctx)
	}
	return s
}

// SessionCoverage scores endpoints by the fraction of the incoming request's
// prompt already resident on each endpoint, according to the session index.
type SessionCoverage struct {
	typedName     plugin.TypedName
	headerName    string
	rootHeader    string
	shareAsHeader string
	forkHeader    string
	charsPerToken float64
	sessionTTL    time.Duration
	maxSessions   int
	rolloverRatio float64

	queueWeight     float64
	decodeAllowance float64

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
	// maxPromptEst is the highest router-side prompt estimate seen for this
	// session. Rollover detection compares estimates against estimates: usage
	// units and estimate units can differ by an arbitrary tokenizer-dependent
	// scale, so comparing an incoming estimate against usage-fed coverage
	// would misfire whenever the estimator runs low.
	maxPromptEst int64
	lastSeen     time.Time
}

// TypedName returns the typed name of the plugin.
func (s *SessionCoverage) TypedName() plugin.TypedName {
	return s.typedName
}

// Category returns the preference the scorer applies when scoring candidate endpoints.
func (s *SessionCoverage) Category() fwksched.ScorerCategory {
	return fwksched.Affinity
}

// Score assigns each endpoint the covered fraction of the request's estimated
// prompt, min(coverage, X)/X, and zero when the request carries no session id
// or the session is unknown.
func (s *SessionCoverage) Score(ctx context.Context, request *fwksched.InferenceRequest, endpoints []fwksched.Endpoint) map[fwksched.Endpoint]float64 {
	scores := make(map[fwksched.Endpoint]float64, len(endpoints))
	for _, endpoint := range endpoints {
		scores[endpoint] = 0.0
	}

	sid := s.sessionID(request)
	rootID := s.headerValue(request, s.rootHeader)
	if sid == "" && rootID == "" {
		return scores
	}
	x := s.estimatePromptTokens(request)
	if x <= 0 {
		return scores
	}

	coverage := s.effectiveCoverage(ctx, sid, rootID, s.forkParent(request), x, attrsession.HasStructuralIdentity(request))
	coverage = mergeResidency(request, endpoints, coverage)

	if s.queueWeight <= 0 {
		// Pure affinity: covered fraction of the prompt, load left to other
		// scorers in the profile.
		if coverage == nil {
			return scores
		}
		for _, endpoint := range endpoints {
			c := coverage[endpoint.GetMetadata().NamespacedName.String()]
			if c <= 0 {
				continue
			}
			if c > x {
				c = x
			}
			scores[endpoint] = float64(c) / float64(x)
		}
		return scores
	}

	// Placement cost (spec continuum between longest-prefix-match and
	// least-loaded): cost_e = gap_e + queueWeight * inflight_e, all in
	// estimated-token units. A warm endpoint loses its edge once the work
	// queued on it outweighs the prefill it saves; with no coverage anywhere
	// this degrades to least-estimated-load placement.
	load := s.podLoadSnapshot()
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

// effectiveCoverage returns a merged copy of the coverage visible to this
// request: the session's own coverage (after fork adoption and rollover
// handling) overlaid with the referenced shared root's coverage, taking the
// per-endpoint maximum. It returns nil when neither source is known.
// structural disables heuristic rollover: chain-derived ids re-key rewritten
// histories by construction, and a delta continuation's small prompt must
// not be mistaken for a rewrite.
func (s *SessionCoverage) effectiveCoverage(ctx context.Context, sid, rootID, forkFrom string, promptTokens int64, structural bool) map[string]int64 {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	var coverage map[string]int64

	if sid != "" {
		entry := s.ownEntryLocked(sid, forkFrom)
		if entry != nil && s.rolloverRatio > 0 && !structural {
			if best := entry.maxPromptEst; best > 0 && float64(promptTokens) < s.rolloverRatio*float64(best) {
				// The prompt shrank well below the session's known prefix: the
				// history was rewritten (e.g. compaction). The old KV no longer
				// prefix-matches, so drop the entry and let the session re-place.
				delete(s.sessions, sid)
				log.FromContext(ctx).V(logutil.DEFAULT).Info("session rollover detected, index entry reset",
					"scorer", s.typedName.String(), "session", sid, "promptEstimate", promptTokens, "bestPromptEstimate", best)
				entry = nil
			}
		}
		if entry != nil {
			entry.lastSeen = now
			coverage = make(map[string]int64, len(entry.coverage))
			for pod, c := range entry.coverage {
				coverage[pod] = c
			}
		}
	}

	if rootID != "" {
		if root, ok := s.sessions[rootKeyPrefix+rootID]; ok {
			root.lastSeen = now
			if coverage == nil {
				coverage = make(map[string]int64, len(root.coverage))
			}
			for pod, c := range root.coverage {
				if c > coverage[pod] {
					coverage[pod] = c
				}
			}
		}
	}

	return coverage
}

// ownEntryLocked returns the session's entry, creating it by fork adoption
// when the session is unknown but names a known parent. Callers must hold
// s.mu.
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
	// The child adopts the parent's coverage but NOT its prompt-estimate
	// high-water: rollover is a per-lineage signal, and a sub-agent's first
	// prompt (shared root + task) is routinely far below half the parent's
	// full history. Inheriting maxPromptEst would misfire rollover on every
	// such fork and delete the adopted coverage on first scoring.
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
// request of the session before the response completes.
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
	shareAs := s.headerValue(request, s.shareAsHeader)
	// Load accounting covers every scheduled request, session-tagged or not,
	// so the cost term sees the endpoint's full in-flight picture. A request
	// occupies its admission-time prefill gap plus the decode allowance —
	// covered prefixes queue no prefill work.
	if s.queueWeight > 0 && request.RequestID != "" {
		gap := x
		coverage := s.effectiveCoverage(ctx, sid, s.headerValue(request, s.rootHeader), s.forkParent(request), x, attrsession.HasStructuralIdentity(request))
		coverage = mergeResidency(request, primaryTargetEndpoints(schedulingResult), coverage)
		if c := coverage[pod]; c > 0 {
			if c > x {
				c = x
			}
			gap = x - c
		}
		s.trackInFlight(request.RequestID, pod, gap+int64(s.decodeAllowance))
	}
	if sid == "" && shareAs == "" {
		return
	}
	if sid != "" {
		s.bump(ctx, sid, pod, x, x, s.forkParent(request))
	}
	if shareAs != "" {
		// The declaring request's placement is where the root will live; an
		// immediately-following fan-out must see it. The parent's response
		// corrects the value to real usage.
		s.bump(ctx, rootKeyPrefix+shareAs, pod, x, 0, "")
	}
}

// ResponseBody raises the serving endpoint's coverage to the token usage
// reported by the model server. Streaming chunks are ignored until the final
// one; responses without usage (e.g. streams without include_usage) leave the
// PreRequest estimate in place.
func (s *SessionCoverage) ResponseBody(ctx context.Context, request *fwksched.InferenceRequest, response *requestcontrol.Response, targetEndpoint *datalayer.EndpointMetadata) {
	if response == nil || !response.EndOfStream {
		return
	}
	if request != nil && request.RequestID != "" {
		s.releaseInFlight(request.RequestID)
	}
	sid := s.sessionID(request)
	s.accountResponse(ctx, request, response, targetEndpoint, sid)

	// A declared final turn (Dynamo dialect action "close") releases the
	// session's index entry once its response has been accounted. The
	// declaration ends the lineage, not the accounting: release must run even
	// when the response carries no usage (streams without include_usage) or
	// no endpoint.
	if sid != "" {
		if _, action := dynamoSessionControl(request); action == "close" {
			s.release(sid)
		}
	}
}

// accountResponse raises coverage from the response's reported usage. It is a
// no-op for responses without usage or a known serving endpoint.
func (s *SessionCoverage) accountResponse(ctx context.Context, request *fwksched.InferenceRequest, response *requestcontrol.Response, targetEndpoint *datalayer.EndpointMetadata, sid string) {
	if targetEndpoint == nil {
		return
	}
	shareAs := s.headerValue(request, s.shareAsHeader)
	rootID := s.headerValue(request, s.rootHeader)
	if sid == "" && shareAs == "" && rootID == "" {
		return
	}
	total := int64(response.Usage.PromptTokens) + int64(response.Usage.CompletionTokens)
	if total <= 0 {
		return
	}
	pod := targetEndpoint.NamespacedName.String()
	if sid != "" {
		s.bump(ctx, sid, pod, total, 0, s.forkParent(request))
	}
	// Roots grow by served prompts only: completions belong to the requester,
	// not the shared prefix. A child's prompt extends past the root by its
	// own suffix; the overshoot is bounded and clamped at scoring time.
	if prompt := int64(response.Usage.PromptTokens); prompt > 0 {
		if shareAs != "" {
			s.bump(ctx, rootKeyPrefix+shareAs, pod, prompt, 0, "")
		}
		if rootID != "" {
			s.bump(ctx, rootKeyPrefix+rootID, pod, prompt, 0, "")
		}
	}

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

// bump raises the session's coverage high-water mark on the given endpoint
// and, when promptEst > 0, the session's prompt-estimate high-water mark.
// Both are monotone; stale smaller values never overwrite. A yet-unknown
// session naming a known fork parent adopts the parent's coverage first.
func (s *SessionCoverage) bump(ctx context.Context, sid, pod string, tokens, promptEst int64, forkFrom string) {
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
	if promptEst > entry.maxPromptEst {
		entry.maxPromptEst = promptEst
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

// forkParent resolves the request's fork-parent id: the chain-identity
// producer's discovered parent when present, then the configured header.
func (s *SessionCoverage) forkParent(request *fwksched.InferenceRequest) string {
	if id, ok := attrsession.ReadForkParent(request); ok && id != "" {
		return string(id)
	}
	return s.headerValue(request, s.forkHeader)
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
// request header, then the Dynamo nvext.session_control dialect carried in
// the request body.
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
	if request.Headers != nil {
		if id := strings.TrimSpace(request.Headers[s.headerName]); id != "" {
			return id
		}
	}
	id, _ := dynamoSessionControl(request)
	return id
}

// dynamoSessionControl extracts the Dynamo session dialect from the request
// body: {"nvext": {"session_control": {"session_id": "...", "action":
// "bind"|"open"|"close"}}}. Trace replayers (e.g. AIPerf conversation-aware
// routing) and Dynamo clients emit it on every turn of a conversation;
// adopting it as an identity carrier needs no new client-visible fields.
// Model servers tolerate the extra field, so it is purely advisory.
func dynamoSessionControl(request *fwksched.InferenceRequest) (id, action string) {
	if request == nil || request.Body == nil || request.Body.Payload == nil {
		return "", ""
	}
	payload, ok := request.Body.Payload.AsMap()
	if !ok {
		return "", ""
	}
	nvext, ok := payload["nvext"].(map[string]any)
	if !ok {
		return "", ""
	}
	sessionControl, ok := nvext["session_control"].(map[string]any)
	if !ok {
		return "", ""
	}
	id, _ = sessionControl["session_id"].(string)
	action, _ = sessionControl["action"].(string)
	return strings.TrimSpace(id), action
}

// release drops a session's index entry: the client declared the session
// finished, so its affinity should not linger until the TTL sweep.
func (s *SessionCoverage) release(sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sid)
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

// mergeResidency overlays event-fed session residency (eviction-aware,
// engine units, published by the session-residency producer) onto the
// response-fed coverage map, taking per-endpoint maxima. Residency pods are
// keyed "addr:port" as on KV events and mapped to endpoint keys via endpoint
// metadata. Maxima keep the composition safe while tagging is partial; once
// all traffic is tagged, residency alone can carry the coverage.
func mergeResidency(request *fwksched.InferenceRequest, endpoints []fwksched.Endpoint, coverage map[string]int64) map[string]int64 {
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
