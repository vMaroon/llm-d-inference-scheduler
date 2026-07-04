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

package sessioncoverage

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrsession "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/session"
	chainidentityconstants "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/chainidentity/constants"
	sessionidconstants "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/sessionid/constants"
)

const testEpsilon = 1e-9

type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestScorer(params parameters) (*SessionCoverage, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	//nolint:staticcheck // nil context intentionally skips the background sweeper in tests.
	s := New(nil, "test", params)
	s.now = clock.now
	return s, clock
}

func withRequestID(r *fwksched.InferenceRequest, id string) *fwksched.InferenceRequest {
	r.RequestID = id
	return r
}

func newEndpoint(name string) fwksched.Endpoint {
	return fwksched.NewEndpoint(
		&fwkdl.EndpointMetadata{NamespacedName: k8stypes.NamespacedName{Namespace: "ns", Name: name}},
		&fwkdl.Metrics{},
		nil,
	)
}

// newAddressedEndpoint builds an endpoint carrying an address, so attributes
// keyed "addr:port" as on KV events (residency, protected mass) resolve to it.
func newAddressedEndpoint(name, address string) fwksched.Endpoint {
	return fwksched.NewEndpoint(
		&fwkdl.EndpointMetadata{
			NamespacedName: k8stypes.NamespacedName{Namespace: "ns", Name: name},
			Address:        address, Port: "8000",
		}, &fwkdl.Metrics{}, nil)
}

func podKey(endpoint fwksched.Endpoint) string {
	return endpoint.GetMetadata().NamespacedName.String()
}

// chatRequest builds a chat-completions request whose flattened content is
// `chars` characters long, carrying the session id in the default header.
func chatRequest(sid string, chars int) *fwksched.InferenceRequest {
	headers := map[string]string{}
	if sid != "" {
		headers[defaultHeaderName] = sid
	}
	return &fwksched.InferenceRequest{
		Headers: headers,
		Body: &fwkrh.InferenceRequestBody{
			ChatCompletions: &fwkrh.ChatCompletionsRequest{
				Messages: []fwkrh.Message{
					{Role: "user", Content: fwkrh.Content{Raw: strings.Repeat("a", chars)}},
				},
			},
		},
	}
}

// forkChild builds a request for its own session that names a fork parent
// via the chain-identity attribute.
func forkChild(sid, parent string, chars int) *fwksched.InferenceRequest {
	r := chatRequest(sid, chars)
	r.PutAttribute(attrsession.ForkParentDataKey.String(), attrsession.SessionID(parent))
	return r
}

func schedulingResultFor(endpoint fwksched.Endpoint) *fwksched.SchedulingResult {
	return &fwksched.SchedulingResult{
		PrimaryProfileName: "default",
		ProfileResults: map[string]*fwksched.ProfileRunResult{
			"default": {TargetEndpoints: []fwksched.Endpoint{endpoint}},
		},
	}
}

func endOfStreamResponse(promptTokens, completionTokens int) *requestcontrol.Response {
	return &requestcontrol.Response{
		EndOfStream: true,
		Usage: fwkrh.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
		},
	}
}

func expectScore(t *testing.T, scores map[fwksched.Endpoint]float64, endpoint fwksched.Endpoint, want float64) {
	t.Helper()
	got, ok := scores[endpoint]
	if !ok {
		t.Fatalf("endpoint %s missing from scores", podKey(endpoint))
	}
	if math.Abs(got-want) > testEpsilon {
		t.Errorf("endpoint %s score = %v, want %v", podKey(endpoint), got, want)
	}
}

// indexedCoverage reads the session's coverage on the endpoint straight from
// the index; ok reports whether the session has an entry at all.
func indexedCoverage(s *SessionCoverage, sid string, endpoint fwksched.Endpoint) (tokens int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.sessions[sid]
	if !ok {
		return 0, false
	}
	return entry.coverage[podKey(endpoint)], true
}

func TestScoreNoSessionIDIsNeutral(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	// Identity-less traffic flows through the cost path (so the sacrificial
	// term reaches it); with no load and no mass attribute it is neutral.
	scores := s.Score(context.Background(), chatRequest("", 4000), []fwksched.Endpoint{podA, podB})

	expectScore(t, scores, podA, 0.5)
	expectScore(t, scores, podB, 0.5)
}

func TestScoreUnknownSessionIsNeutral(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	// No coverage, no load: every endpoint carries the same cost.
	scores := s.Score(context.Background(), chatRequest("s1", 4000), []fwksched.Endpoint{podA, podB})

	expectScore(t, scores, podA, 0.5)
	expectScore(t, scores, podB, 0.5)
}

func TestPreRequestThenScore(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	// Turn 1: 4000 chars => 1001 estimated tokens, scheduled to pod-a.
	s.PreRequest(context.Background(), chatRequest("s1", 4000), schedulingResultFor(podA))

	// Turn 2: the covered endpoint carries the lowest cost.
	scores := s.Score(context.Background(), chatRequest("s1", 4800), []fwksched.Endpoint{podA, podB})

	expectScore(t, scores, podA, 1.0)
	expectScore(t, scores, podB, 0.0)
}

func TestResponseBodyOverridesEstimate(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")
	request := chatRequest("s1", 4000) // estimate 1001

	s.PreRequest(context.Background(), request, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), request, endOfStreamResponse(1100, 100), podA.GetMetadata())

	if c, ok := indexedCoverage(s, "s1", podA); !ok || c != 1200 {
		t.Fatalf("coverage = %d (exists=%v), want ground-truth 1200 over the 1001 estimate", c, ok)
	}

	// A smaller, stale usage report must not shrink coverage.
	s.ResponseBody(context.Background(), request, endOfStreamResponse(400, 100), podA.GetMetadata())
	if c, _ := indexedCoverage(s, "s1", podA); c != 1200 {
		t.Errorf("coverage = %d after stale report, want 1200", c)
	}
}

func TestScoreClampsToFullCoverage(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA, podB, podC := newEndpoint("pod-a"), newEndpoint("pod-b"), newEndpoint("pod-c")

	// pod-a holds 2000 tokens, pod-b exactly the request's 1001 estimate.
	// Overshooting coverage clamps to the prompt: both cost zero gap and tie,
	// while the cold pod loses.
	s.ResponseBody(context.Background(), chatRequest("s1", 4000), endOfStreamResponse(1800, 200), podA.GetMetadata())
	s.ResponseBody(context.Background(), chatRequest("s1", 4000), endOfStreamResponse(901, 100), podB.GetMetadata())

	scores := s.Score(context.Background(), chatRequest("s1", 4000), []fwksched.Endpoint{podA, podB, podC})
	expectScore(t, scores, podA, 1.0)
	expectScore(t, scores, podB, 1.0)
	expectScore(t, scores, podC, 0.0)
}

func TestStreamingChunksIgnoredUntilEndOfStream(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")
	request := chatRequest("s1", 4000)

	chunk := &requestcontrol.Response{EndOfStream: false, Usage: fwkrh.Usage{PromptTokens: 5000}}
	s.ResponseBody(context.Background(), request, chunk, podA.GetMetadata())

	if _, ok := indexedCoverage(s, "s1", podA); ok {
		t.Error("streaming chunk must not create an index entry")
	}
}

func TestZeroUsageKeepsEstimate(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")
	request := chatRequest("s1", 4000) // 1001 estimated tokens

	s.PreRequest(context.Background(), request, schedulingResultFor(podA))
	// Stream finished without usage (no include_usage): estimate survives.
	s.ResponseBody(context.Background(), request, endOfStreamResponse(0, 0), podA.GetMetadata())

	if c, ok := indexedCoverage(s, "s1", podA); !ok || c != 1001 {
		t.Errorf("coverage = %d (exists=%v), want the 1001 estimate to survive", c, ok)
	}
}

func TestSessionsExpire(t *testing.T) {
	s, clock := newTestScorer(parameters{SessionTTLSeconds: 60})
	podA := newEndpoint("pod-a")

	s.PreRequest(context.Background(), chatRequest("s1", 4000), schedulingResultFor(podA))

	clock.advance(2 * time.Minute)
	s.removeExpired()

	if _, ok := indexedCoverage(s, "s1", podA); ok {
		t.Error("session entry must expire past the TTL")
	}
}

func TestScoreRefreshesLastSeen(t *testing.T) {
	s, clock := newTestScorer(parameters{SessionTTLSeconds: 60})
	podA := newEndpoint("pod-a")

	s.PreRequest(context.Background(), chatRequest("s1", 4000), schedulingResultFor(podA))

	// Scoring within the TTL refreshes last-seen, so the entry survives a
	// sweep that runs just past the original insertion time.
	clock.advance(45 * time.Second)
	s.Score(context.Background(), chatRequest("s1", 4000), []fwksched.Endpoint{podA})
	clock.advance(45 * time.Second)
	s.removeExpired()

	if _, ok := indexedCoverage(s, "s1", podA); !ok {
		t.Error("scoring must refresh last-seen and keep the entry alive")
	}
}

func TestCapacityEviction(t *testing.T) {
	s, _ := newTestScorer(parameters{MaxSessions: 2})
	podA := newEndpoint("pod-a")

	for _, sid := range []string{"s1", "s2", "s3"} {
		s.PreRequest(context.Background(), chatRequest(sid, 4000), schedulingResultFor(podA))
	}

	s.mu.Lock()
	size := len(s.sessions)
	s.mu.Unlock()
	if size > 2 {
		t.Errorf("session index size = %d, want <= 2", size)
	}
}

func TestSessionIDFromAttributePrecedesHeader(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")

	request := chatRequest("header-session", 4000)
	key := attrsession.SessionIDDataKey.WithNonEmptyProducerName(sessionidconstants.SessionIDProducerType).String()
	request.PutAttribute(key, attrsession.SessionID("attr-session"))

	s.PreRequest(context.Background(), request, schedulingResultFor(podA))

	s.mu.Lock()
	_, attrExists := s.sessions["attr-session"]
	_, headerExists := s.sessions["header-session"]
	s.mu.Unlock()
	if !attrExists || headerExists {
		t.Errorf("attribute session id should take precedence: attr=%v header=%v", attrExists, headerExists)
	}
}

func TestDerivedSessionIDPrecedesAll(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")

	request := chatRequest("header-session", 4000)
	request.PutAttribute(
		attrsession.SessionIDDataKey.WithNonEmptyProducerName(sessionidconstants.SessionIDProducerType).String(),
		attrsession.SessionID("attr-session"))
	request.PutAttribute(
		attrsession.SessionIDDataKey.WithNonEmptyProducerName(chainidentityconstants.ChainIdentityProducerType).String(),
		attrsession.SessionID("chain-session"))

	s.PreRequest(context.Background(), request, schedulingResultFor(podA))

	s.mu.Lock()
	_, chainExists := s.sessions["chain-session"]
	_, attrExists := s.sessions["attr-session"]
	s.mu.Unlock()
	if !chainExists || attrExists {
		t.Errorf("derived chain id must take precedence: chain=%v attr=%v", chainExists, attrExists)
	}
}

func TestForkParentAttributeAdoption(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	parent := chatRequest("parent", 4000)
	s.PreRequest(context.Background(), parent, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), parent, endOfStreamResponse(1200, 100), podA.GetMetadata())

	child := forkChild("child", "parent", 4400)
	scores := s.Score(context.Background(), child, []fwksched.Endpoint{podA, podB})
	if scores[podA] <= scores[podB] {
		t.Fatalf("fork-parent attribute must adopt the parent's coverage: %v", scores)
	}
}

func TestForkDivergesWithoutTouchingParent(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	parent := chatRequest("parent", 4000)
	s.PreRequest(context.Background(), parent, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), parent, endOfStreamResponse(1200, 100), podA.GetMetadata())

	// The fork adopts on first sight, then diverges on pod-b without
	// touching the parent's entry.
	fork := forkChild("fork-1", "parent", 4400)
	s.Score(context.Background(), fork, []fwksched.Endpoint{podA, podB})
	s.ResponseBody(context.Background(), fork, endOfStreamResponse(2000, 100), podB.GetMetadata())

	if c, _ := indexedCoverage(s, "fork-1", podB); c != 2100 {
		t.Errorf("fork coverage on pod-b = %d, want 2100", c)
	}
	if c, _ := indexedCoverage(s, "parent", podB); c != 0 {
		t.Errorf("parent coverage on pod-b = %d, want 0 (fork diverges alone)", c)
	}
}

func TestResidencyMergesAsEngineTruth(t *testing.T) {
	// Event-fed residency (keyed addr:port as on KV events) overlays the
	// response-fed coverage: a session unknown to the scorer's own index
	// still scores warm on the pod the events say holds its KV.
	s, _ := newTestScorer(parameters{})
	podA := newAddressedEndpoint("pod-a", "10.0.0.1")
	podB := newEndpoint("pod-b")

	request := chatRequest("s1", 4000) // estimate 1001, session unknown
	request.PutAttribute(attrsession.SessionResidencyDataKey.String(),
		[]attrsession.PodResidency{{Pod: "10.0.0.1:8000", Tier: "gpu", UpTo: "c3", Tokens: 900}})

	scores := s.Score(context.Background(), request, []fwksched.Endpoint{podA, podB})
	expectScore(t, scores, podA, 1.0)
	expectScore(t, scores, podB, 0.0)
}

func TestResidencyAuthoritativeReplacesBelief(t *testing.T) {
	// Authoritative mode: a present residency attribute is engine truth.
	// Response-fed belief that pod-a is warm dies when residency says the
	// session's KV survives only on pod-b (pod-a evicted it).
	s, _ := newTestScorer(parameters{ResidencyAuthoritative: true})
	podA := newAddressedEndpoint("pod-a", "10.0.0.1")
	podB := newAddressedEndpoint("pod-b", "10.0.0.2")

	// Response-fed belief: warm on pod-a.
	warm := chatRequest("s1", 4000)
	s.PreRequest(context.Background(), warm, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), warm, endOfStreamResponse(1200, 100), podA.GetMetadata())

	// Engine truth: only pod-b holds intact KV.
	resume := chatRequest("s1", 4000) // estimate 1001
	resume.PutAttribute(attrsession.SessionResidencyDataKey.String(),
		[]attrsession.PodResidency{{Pod: "10.0.0.2:8000", Tier: "gpu", UpTo: "c2", Tokens: 800}})

	scores := s.Score(context.Background(), resume, []fwksched.Endpoint{podA, podB})
	expectScore(t, scores, podA, 0.0)
	expectScore(t, scores, podB, 1.0)

	// Without the attribute, response-fed belief still applies (fallback).
	blind := chatRequest("s1", 4000)
	scores = s.Score(context.Background(), blind, []fwksched.Endpoint{podA, podB})
	expectScore(t, scores, podA, 1.0)
	expectScore(t, scores, podB, 0.0)
}

func TestSacrificialPlacementSteersFreshTraffic(t *testing.T) {
	// Affinity-less traffic pays for each pod's protected session mass:
	// equal load, pod-a holds 50k of session KV, pod-b holds 2k — fresh
	// one-shot requests go to pod-b. Session traffic is unaffected.
	s, _ := newTestScorer(parameters{QueueWeight: 1, SacrificialWeight: 0.5})
	podA := newAddressedEndpoint("pod-a", "10.0.0.1")
	podB := newAddressedEndpoint("pod-b", "10.0.0.2")
	mass := map[string]int{"10.0.0.1:8000": 50000, "10.0.0.2:8000": 2000}

	fresh := chatRequest("one-shot", 4000)
	fresh.PutAttribute(attrsession.PodProtectedMassDataKey.String(), mass)
	scores := s.Score(context.Background(), fresh, []fwksched.Endpoint{podA, podB})
	if scores[podB] <= scores[podA] {
		t.Fatalf("fresh traffic must prefer the low-mass pod: %v", scores)
	}

	// A session with coverage on the heavy pod still follows its KV.
	warm := chatRequest("agent-1", 4000)
	s.PreRequest(context.Background(), warm, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), warm, endOfStreamResponse(1200, 100), podA.GetMetadata())
	followup := chatRequest("agent-1", 4400)
	followup.PutAttribute(attrsession.PodProtectedMassDataKey.String(), mass)
	scores = s.Score(context.Background(), followup, []fwksched.Endpoint{podA, podB})
	if scores[podA] <= scores[podB] {
		t.Fatalf("session traffic must still follow its own KV: %v", scores)
	}
}

func TestPackingPrefersSessionDensePodForFirstTurn(t *testing.T) {
	// A session's first turn (identity, no coverage anywhere) is discounted
	// toward pods already holding protected session mass: equal load, pod-a
	// holds 50k of session KV, pod-b holds 2k — the new session lands on
	// pod-a, keeping pod-b free to absorb unaffiliated churn.
	s, _ := newTestScorer(parameters{QueueWeight: 1, PackingWeight: 0.5})
	podA := newAddressedEndpoint("pod-a", "10.0.0.1")
	podB := newAddressedEndpoint("pod-b", "10.0.0.2")
	mass := map[string]int{"10.0.0.1:8000": 50000, "10.0.0.2:8000": 2000}

	first := chatRequest("s-new", 4000)
	first.PutAttribute(attrsession.PodProtectedMassDataKey.String(), mass)
	scores := s.Score(context.Background(), first, []fwksched.Endpoint{podA, podB})
	if scores[podA] <= scores[podB] {
		t.Fatalf("session start must prefer the session-dense pod: %v", scores)
	}
}

func TestPackingRespectsCap(t *testing.T) {
	// A pod at or above packingCapTokens attracts no further session starts:
	// it earns no discount, tying with a mass-less pod, while the massiest
	// under-cap pod wins.
	s, _ := newTestScorer(parameters{QueueWeight: 1, PackingWeight: 0.5, PackingCapTokens: 100000})
	podA := newAddressedEndpoint("pod-a", "10.0.0.1")
	podB := newAddressedEndpoint("pod-b", "10.0.0.2")
	podC := newAddressedEndpoint("pod-c", "10.0.0.3")
	mass := map[string]int{"10.0.0.1:8000": 120000, "10.0.0.2:8000": 50000, "10.0.0.3:8000": 0}

	first := chatRequest("s-new", 4000)
	first.PutAttribute(attrsession.PodProtectedMassDataKey.String(), mass)
	scores := s.Score(context.Background(), first, []fwksched.Endpoint{podA, podB, podC})
	if scores[podB] <= scores[podA] || scores[podB] <= scores[podC] {
		t.Fatalf("the massiest under-cap pod must win: %v", scores)
	}
	if math.Abs(scores[podA]-scores[podC]) > testEpsilon {
		t.Errorf("an over-cap pod must earn no discount: pod-a=%v pod-c=%v", scores[podA], scores[podC])
	}
}

func TestPackingRequiresSessionIdentity(t *testing.T) {
	// One-shot traffic without session identity never packs: the sacrificial
	// term still steers it away from session-dense pods, even with packing
	// enabled alongside.
	s, _ := newTestScorer(parameters{QueueWeight: 1, SacrificialWeight: 0.5, PackingWeight: 0.5})
	podA := newAddressedEndpoint("pod-a", "10.0.0.1")
	podB := newAddressedEndpoint("pod-b", "10.0.0.2")
	mass := map[string]int{"10.0.0.1:8000": 50000, "10.0.0.2:8000": 2000}

	oneShot := chatRequest("", 4000)
	oneShot.PutAttribute(attrsession.PodProtectedMassDataKey.String(), mass)
	scores := s.Score(context.Background(), oneShot, []fwksched.Endpoint{podA, podB})
	if scores[podB] <= scores[podA] {
		t.Fatalf("identity-less traffic must still avoid session mass: %v", scores)
	}
}

func TestPackingLeavesWarmSessionsAlone(t *testing.T) {
	// A session with coverage follows its own KV even when another pod holds
	// far more protected mass: packing only shapes first turns.
	s, _ := newTestScorer(parameters{QueueWeight: 1, PackingWeight: 0.5})
	podA := newAddressedEndpoint("pod-a", "10.0.0.1")
	podB := newAddressedEndpoint("pod-b", "10.0.0.2")
	mass := map[string]int{"10.0.0.1:8000": 50000, "10.0.0.2:8000": 2000}

	warm := chatRequest("agent-1", 4000)
	s.PreRequest(context.Background(), warm, schedulingResultFor(podB))
	s.ResponseBody(context.Background(), warm, endOfStreamResponse(1200, 100), podB.GetMetadata())

	followup := chatRequest("agent-1", 4400)
	followup.PutAttribute(attrsession.PodProtectedMassDataKey.String(), mass)
	scores := s.Score(context.Background(), followup, []fwksched.Endpoint{podA, podB})
	if scores[podB] <= scores[podA] {
		t.Fatalf("warm session must follow its own KV, not the mass: %v", scores)
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	s, _ := newTestScorer(parameters{})

	chat := chatRequest("s1", 4000)
	if got := s.estimatePromptTokens(chat); got != 1001 {
		t.Errorf("chat estimate = %d, want 1001", got)
	}

	completions := &fwksched.InferenceRequest{
		Body: &fwkrh.InferenceRequestBody{
			Completions: &fwkrh.CompletionsRequest{Prompt: fwkrh.Prompt{Raw: strings.Repeat("b", 2000)}},
		},
	}
	if got := s.estimatePromptTokens(completions); got != 501 {
		t.Errorf("completions estimate = %d, want 501", got)
	}

	sizeOnly := &fwksched.InferenceRequest{RequestSizeBytes: 4096}
	if got := s.estimatePromptTokens(sizeOnly); got != 1024 {
		t.Errorf("size-based estimate = %d, want 1024", got)
	}

	if got := s.estimatePromptTokens(&fwksched.InferenceRequest{}); got != 0 {
		t.Errorf("empty request estimate = %d, want 0", got)
	}
}

// costScenario builds a completed 12k-token session on pod-a ("parent",
// usage-fed coverage 12020, no in-flight work). Children fork it via the
// chain-identity ForkParent attribute and adopt that coverage on first sight.
func costScenario(t *testing.T) (*SessionCoverage, fwksched.Endpoint, fwksched.Endpoint) {
	t.Helper()
	s, _ := newTestScorer(parameters{QueueWeight: 1})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")
	parent := withRequestID(chatRequest("parent", 48000), "req-parent")
	s.PreRequest(context.Background(), parent, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), parent, endOfStreamResponse(12000, 20), podA.GetMetadata())
	return s, podA, podB
}

func TestCostBurstSpillsFromWarmEndpoint(t *testing.T) {
	s, podA, podB := costScenario(t)

	// Each stacked fork occupies its admission gap (231) plus the decode
	// allowance (750). The warm pod keeps winning — reuse is worth more than
	// the queue — until the stacked work outweighs a cold 12.3k prefill
	// (threshold: 13 children).
	for i := 0; i < 13; i++ {
		child := withRequestID(forkChild(fmt.Sprintf("child-%d", i), "parent", 49000), fmt.Sprintf("req-c%d", i))
		scores := s.Score(context.Background(), child, []fwksched.Endpoint{podA, podB})
		if scores[podA] <= scores[podB] {
			t.Fatalf("stacked warm pod must still win at child %d: %v", i, scores)
		}
		s.PreRequest(context.Background(), child, schedulingResultFor(podA))
	}

	overflow := withRequestID(forkChild("child-13", "parent", 49000), "req-c13")
	scores := s.Score(context.Background(), overflow, []fwksched.Endpoint{podA, podB})
	if scores[podB] <= scores[podA] {
		t.Fatalf("burst must spill once stacked work outweighs the cold prefill: %v", scores)
	}
}

func TestCostStaggeredFollowsWarmEndpoint(t *testing.T) {
	s, podA, podB := costScenario(t)

	child1 := withRequestID(forkChild("child-1", "parent", 49000), "req-c1")
	s.Score(context.Background(), child1, []fwksched.Endpoint{podA, podB})
	s.PreRequest(context.Background(), child1, schedulingResultFor(podA))
	// child-1 completes before child-2 arrives (staggered arrivals).
	s.ResponseBody(context.Background(), child1, endOfStreamResponse(12250, 20), podA.GetMetadata())

	child2 := withRequestID(forkChild("child-2", "parent", 49000), "req-c2")
	scores := s.Score(context.Background(), child2, []fwksched.Endpoint{podA, podB})
	if scores[podA] <= scores[podB] {
		t.Fatalf("staggered arrivals must follow the warm pod: %v", scores)
	}
}

func TestCostNewSessionSpreadsByLoad(t *testing.T) {
	s, _ := newTestScorer(parameters{QueueWeight: 1})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	// Unrelated in-flight work occupies pod-a.
	other := withRequestID(chatRequest("", 4000), "req-other")
	s.PreRequest(context.Background(), other, schedulingResultFor(podA))

	// A brand-new session has no coverage anywhere: least-loaded pod wins.
	scores := s.Score(context.Background(), chatRequest("s-new", 4000), []fwksched.Endpoint{podA, podB})
	if scores[podB] <= scores[podA] {
		t.Fatalf("new session must prefer the less-loaded pod: %v", scores)
	}

	// Once the other request completes, the pods tie.
	s.ResponseBody(context.Background(), other, endOfStreamResponse(1000, 20), podA.GetMetadata())
	scores = s.Score(context.Background(), chatRequest("s-new", 4000), []fwksched.Endpoint{podA, podB})
	expectScore(t, scores, podA, 0.5)
	expectScore(t, scores, podB, 0.5)
}

func TestInFlightOrphansExpire(t *testing.T) {
	s, clock := newTestScorer(parameters{QueueWeight: 1})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	orphan := withRequestID(chatRequest("", 4000), "req-orphan")
	s.PreRequest(context.Background(), orphan, schedulingResultFor(podA))

	clock.advance(inFlightTTL + time.Minute)
	s.removeExpired()

	scores := s.Score(context.Background(), chatRequest("s-new", 4000), []fwksched.Endpoint{podA, podB})
	expectScore(t, scores, podA, 0.5)
	expectScore(t, scores, podB, 0.5)
}

func TestFactoryDefaults(t *testing.T) {
	p, err := Factory("session-coverage", nil, nil)
	if err != nil {
		t.Fatalf("Factory returned error: %v", err)
	}
	s, ok := p.(*SessionCoverage)
	if !ok {
		t.Fatalf("Factory returned %T, want *SessionCoverage", p)
	}
	if s.headerName != defaultHeaderName || s.charsPerToken != defaultCharsPerToken ||
		s.sessionTTL != defaultSessionTTL || s.maxSessions != defaultMaxSessions ||
		s.queueWeight != defaultQueueWeight ||
		s.packingWeight != 0 || s.packingCapTokens != defaultPackingCap {
		t.Errorf("Factory defaults not applied: %+v", s)
	}
	if s.TypedName().Type != SessionCoverageScorerType {
		t.Errorf("TypedName().Type = %s, want %s", s.TypedName().Type, SessionCoverageScorerType)
	}
	if s.Category() != fwksched.Affinity {
		t.Errorf("Category() = %s, want Affinity", s.Category())
	}
}
