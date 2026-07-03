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
	if params.QueueWeight == 0 {
		// Most tests pin the pure-affinity scoring mode; load-cost tests opt
		// in with an explicit positive QueueWeight.
		params.QueueWeight = -1
	}
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

func TestScoreNoSessionID(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")

	scores := s.Score(context.Background(), chatRequest("", 4000), []fwksched.Endpoint{podA})

	expectScore(t, scores, podA, 0.0)
}

func TestScoreUnknownSession(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")

	scores := s.Score(context.Background(), chatRequest("s1", 4000), []fwksched.Endpoint{podA})

	expectScore(t, scores, podA, 0.0)
}

func TestPreRequestThenScore(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	// Turn 1: 4000 chars => 1001 estimated tokens, scheduled to pod-a.
	s.PreRequest(context.Background(), chatRequest("s1", 4000), schedulingResultFor(podA))

	// Turn 2: 4800 chars => 1201 estimated tokens.
	scores := s.Score(context.Background(), chatRequest("s1", 4800), []fwksched.Endpoint{podA, podB})

	expectScore(t, scores, podA, 1001.0/1201.0)
	expectScore(t, scores, podB, 0.0)
}

func TestResponseBodyOverridesEstimate(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")
	request := chatRequest("s1", 4000)

	s.PreRequest(context.Background(), request, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), request, endOfStreamResponse(1100, 100), podA.GetMetadata())

	// Coverage is now 1200 ground-truth tokens.
	scores := s.Score(context.Background(), chatRequest("s1", 5200), []fwksched.Endpoint{podA}) // 1301 tokens
	expectScore(t, scores, podA, 1200.0/1301.0)

	// A smaller, stale usage report must not shrink coverage.
	s.ResponseBody(context.Background(), request, endOfStreamResponse(400, 100), podA.GetMetadata())
	scores = s.Score(context.Background(), chatRequest("s1", 5200), []fwksched.Endpoint{podA})
	expectScore(t, scores, podA, 1200.0/1301.0)
}

func TestScoreClampsToFullCoverage(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")
	request := chatRequest("s1", 4000)

	s.ResponseBody(context.Background(), request, endOfStreamResponse(1800, 200), podA.GetMetadata())

	// 4000 chars => 1001 tokens; coverage 2000 > X, at the rollover boundary
	// (1001 >= 0.5*2000) so no reset, and the fraction clamps to 1.0.
	scores := s.Score(context.Background(), chatRequest("s1", 4000), []fwksched.Endpoint{podA})
	expectScore(t, scores, podA, 1.0)
}

func TestRolloverResetsSession(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")
	request := chatRequest("s1", 40000) // estimate 10001

	s.PreRequest(context.Background(), request, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), request, endOfStreamResponse(9500, 500), podA.GetMetadata())

	// Prompt estimate shrinks to 2001 < 0.5 * 10001: rollover.
	scores := s.Score(context.Background(), chatRequest("s1", 8000), []fwksched.Endpoint{podA})
	expectScore(t, scores, podA, 0.0)

	s.mu.Lock()
	_, exists := s.sessions["s1"]
	s.mu.Unlock()
	if exists {
		t.Error("session entry should be removed after rollover")
	}
}

func TestRolloverComparesEstimateUnitsNotUsageUnits(t *testing.T) {
	// Regression: usage-reported tokens can run well above the router-side
	// estimate (token-dense text). A growing conversation must not be
	// mistaken for a prefix rewrite just because estimate < usage coverage.
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")
	turn1 := chatRequest("s1", 4000) // estimate 1001

	s.PreRequest(context.Background(), turn1, schedulingResultFor(podA))
	// Real tokenizer reports ~2.4x the estimate.
	s.ResponseBody(context.Background(), turn1, endOfStreamResponse(2400, 24), podA.GetMetadata())

	// Turn 2 grows the text; estimate 1201 is still far below usage coverage
	// 2424. No rollover may fire, and the covered pod scores full.
	scores := s.Score(context.Background(), chatRequest("s1", 4800), []fwksched.Endpoint{podA})
	expectScore(t, scores, podA, 1.0)

	s.mu.Lock()
	_, exists := s.sessions["s1"]
	s.mu.Unlock()
	if !exists {
		t.Error("session entry must survive a growing conversation with estimate/usage scale mismatch")
	}
}

func TestRolloverDisabled(t *testing.T) {
	s, _ := newTestScorer(parameters{RolloverRatio: -1})
	podA := newEndpoint("pod-a")
	request := chatRequest("s1", 40000)

	s.ResponseBody(context.Background(), request, endOfStreamResponse(9500, 500), podA.GetMetadata())

	// Same shrunken prompt as the rollover test, but detection is disabled:
	// full coverage of the small prompt.
	scores := s.Score(context.Background(), chatRequest("s1", 8000), []fwksched.Endpoint{podA})
	expectScore(t, scores, podA, 1.0)
}

func TestStreamingChunksIgnoredUntilEndOfStream(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")
	request := chatRequest("s1", 4000)

	chunk := &requestcontrol.Response{EndOfStream: false, Usage: fwkrh.Usage{PromptTokens: 5000}}
	s.ResponseBody(context.Background(), request, chunk, podA.GetMetadata())

	scores := s.Score(context.Background(), chatRequest("s1", 4000), []fwksched.Endpoint{podA})
	expectScore(t, scores, podA, 0.0)
}

func TestZeroUsageKeepsEstimate(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")
	request := chatRequest("s1", 4000) // 1001 estimated tokens

	s.PreRequest(context.Background(), request, schedulingResultFor(podA))
	// Stream finished without usage (no include_usage): estimate survives.
	s.ResponseBody(context.Background(), request, endOfStreamResponse(0, 0), podA.GetMetadata())

	scores := s.Score(context.Background(), chatRequest("s1", 4800), []fwksched.Endpoint{podA}) // 1201 tokens
	expectScore(t, scores, podA, 1001.0/1201.0)
}

func TestSessionsExpire(t *testing.T) {
	s, clock := newTestScorer(parameters{SessionTTLSeconds: 60})
	podA := newEndpoint("pod-a")

	s.PreRequest(context.Background(), chatRequest("s1", 4000), schedulingResultFor(podA))

	clock.advance(2 * time.Minute)
	s.removeExpired()

	scores := s.Score(context.Background(), chatRequest("s1", 4000), []fwksched.Endpoint{podA})
	expectScore(t, scores, podA, 0.0)
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

	scores := s.Score(context.Background(), chatRequest("s1", 4000), []fwksched.Endpoint{podA})
	expectScore(t, scores, podA, 1.0)
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

func TestStructuralIdentitySkipsRollover(t *testing.T) {
	// Chain-derived ids re-key rewritten histories by construction; a delta
	// continuation's small prompt must not trip the shrink heuristic.
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")

	s.PreRequest(context.Background(), chatRequest("s1", 40000), schedulingResultFor(podA)) // est 10001

	small := chatRequest("s1", 8000) // est 2001 < 0.5 * 10001
	small.PutAttribute(attrsession.StructuralIdentityDataKey.String(), true)
	scores := s.Score(context.Background(), small, []fwksched.Endpoint{podA})
	expectScore(t, scores, podA, 1.0)

	s.mu.Lock()
	_, exists := s.sessions["s1"]
	s.mu.Unlock()
	if !exists {
		t.Error("structural identity must suppress heuristic rollover")
	}
}

func TestForkParentAttributeAdoption(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")

	parent := chatRequest("parent", 4000)
	s.PreRequest(context.Background(), parent, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), parent, endOfStreamResponse(1200, 100), podA.GetMetadata())

	child := chatRequest("child", 4400)
	child.PutAttribute(attrsession.ForkParentDataKey.String(), attrsession.SessionID("parent"))
	scores := s.Score(context.Background(), child, []fwksched.Endpoint{podA})
	if scores[podA] <= 0 {
		t.Fatalf("fork-parent attribute must adopt the parent's coverage: %v", scores)
	}
}

func TestResidencyMergesAsEngineTruth(t *testing.T) {
	// Event-fed residency (keyed addr:port as on KV events) overlays the
	// response-fed coverage: a session unknown to the scorer's own index
	// still scores warm on the pod the events say holds its KV.
	s, _ := newTestScorer(parameters{})
	podA := fwksched.NewEndpoint(
		&fwkdl.EndpointMetadata{
			NamespacedName: k8stypes.NamespacedName{Namespace: "ns", Name: "pod-a"},
			Address:        "10.0.0.1", Port: "8000",
		}, &fwkdl.Metrics{}, nil)
	podB := newEndpoint("pod-b")

	request := chatRequest("s1", 4000) // estimate 1001, session unknown
	request.PutAttribute(attrsession.SessionResidencyDataKey.String(),
		[]attrsession.PodResidency{{Pod: "10.0.0.1:8000", Tier: "gpu", UpTo: "c3", Tokens: 900}})

	scores := s.Score(context.Background(), request, []fwksched.Endpoint{podA, podB})
	expectScore(t, scores, podA, 900.0/1001.0)
	expectScore(t, scores, podB, 0.0)
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

func withHeader(r *fwksched.InferenceRequest, name, value string) *fwksched.InferenceRequest {
	r.Headers[name] = value
	return r
}

func TestRootSharingAcrossSessions(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	// Parent declares its prompt as root R and lands on pod-a.
	parent := withHeader(chatRequest("parent", 48000), defaultShareAsHeaderName, "R")
	s.PreRequest(context.Background(), parent, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), parent, endOfStreamResponse(12000, 50), podA.GetMetadata())

	// A fresh session referencing R sees pod-a warm (full coverage of its
	// prompt, clamped), pod-b cold.
	child1 := withHeader(chatRequest("child-1", 47000), defaultRootHeaderName, "R")
	scores := s.Score(context.Background(), child1, []fwksched.Endpoint{podA, podB})
	expectScore(t, scores, podA, 1.0)
	expectScore(t, scores, podB, 0.0)

	// child-1 gets served on pod-b; its response warms the root there too
	// (the root prefix is now resident on both endpoints — reuse is a forest).
	s.ResponseBody(context.Background(), child1, endOfStreamResponse(12150, 40), podB.GetMetadata())
	child2 := withHeader(chatRequest("child-2", 47000), defaultRootHeaderName, "R")
	scores = s.Score(context.Background(), child2, []fwksched.Endpoint{podA, podB})
	expectScore(t, scores, podA, 1.0)
	expectScore(t, scores, podB, 1.0)
}

func TestRootReferenceToUnknownRootScoresZero(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")

	child := withHeader(chatRequest("child", 4000), defaultRootHeaderName, "nope")
	scores := s.Score(context.Background(), child, []fwksched.Endpoint{podA})
	expectScore(t, scores, podA, 0.0)
}

func TestChildrenDoNotWarmRootAtScheduling(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA, podC := newEndpoint("pod-a"), newEndpoint("pod-c")

	parent := withHeader(chatRequest("parent", 48000), defaultShareAsHeaderName, "R")
	s.PreRequest(context.Background(), parent, schedulingResultFor(podA))

	// A child scheduled to pod-c must NOT mark the root warm there before its
	// response exists — optimistic root warmth would herd a burst.
	child := withHeader(chatRequest("child-1", 47000), defaultRootHeaderName, "R")
	s.PreRequest(context.Background(), child, schedulingResultFor(podC))

	probe := withHeader(chatRequest("child-2", 47000), defaultRootHeaderName, "R")
	scores := s.Score(context.Background(), probe, []fwksched.Endpoint{podA, podC})
	expectScore(t, scores, podA, 1.0) // parent's own optimistic placement
	expectScore(t, scores, podC, 0.0) // child scheduling left no trace
}

func TestForkAdoptsParentCoverage(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	parent := chatRequest("parent", 4000)
	s.PreRequest(context.Background(), parent, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), parent, endOfStreamResponse(1200, 100), podA.GetMetadata())

	// First sight of the fork: adopts the parent's coverage.
	fork := withHeader(chatRequest("fork-1", 4400), defaultForkHeaderName, "parent")
	scores := s.Score(context.Background(), fork, []fwksched.Endpoint{podA, podB})
	if scores[podA] <= 0 {
		t.Fatalf("fork should score the parent's endpoint, got %v", scores[podA])
	}

	// The fork then diverges on pod-b without touching the parent.
	s.ResponseBody(context.Background(), fork, endOfStreamResponse(2000, 100), podB.GetMetadata())
	parentScores := s.Score(context.Background(), chatRequest("parent", 4000), []fwksched.Endpoint{podA, podB})
	expectScore(t, parentScores, podB, 0.0)
}

func TestForkWithSmallerPromptKeepsAdoptedCoverage(t *testing.T) {
	// Regression: a sub-agent's first prompt (shared root + task) is routinely
	// far below half the parent's full history. Inheriting the parent's prompt
	// high-water made rollover treat the fork as a rewrite and delete the
	// adopted entry on first scoring, permanently.
	s, _ := newTestScorer(parameters{})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	parent := chatRequest("parent", 40000) // estimate 10001
	s.PreRequest(context.Background(), parent, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), parent, endOfStreamResponse(24000, 500), podA.GetMetadata())

	// Fork estimate 3251 < 0.5 * 10001: adoption must survive, not roll over.
	fork := withHeader(chatRequest("fork-1", 13000), defaultForkHeaderName, "parent")
	scores := s.Score(context.Background(), fork, []fwksched.Endpoint{podA, podB})
	expectScore(t, scores, podA, 1.0)
	expectScore(t, scores, podB, 0.0)

	s.mu.Lock()
	_, exists := s.sessions["fork-1"]
	s.mu.Unlock()
	if !exists {
		t.Error("fork entry must survive its first scoring")
	}
}

func TestForkRolloverUsesOwnHistory(t *testing.T) {
	// Rollover still protects the fork once its OWN prompt high-water exists.
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")

	parent := chatRequest("parent", 40000)
	s.PreRequest(context.Background(), parent, schedulingResultFor(podA))

	fork := withHeader(chatRequest("fork-1", 20000), defaultForkHeaderName, "parent") // est 5001
	s.PreRequest(context.Background(), fork, schedulingResultFor(podA))

	// The fork's history is rewritten: est 2001 < 0.5 * 5001.
	scores := s.Score(context.Background(), chatRequest("fork-1", 8000), []fwksched.Endpoint{podA})
	expectScore(t, scores, podA, 0.0)

	s.mu.Lock()
	_, exists := s.sessions["fork-1"]
	s.mu.Unlock()
	if exists {
		t.Error("fork entry should be removed after its own rollover")
	}
}

// costScenario builds a warm shared root on pod-a: the parent declared root R
// (est 12001), completed, and left no in-flight work.
func costScenario(t *testing.T) (*SessionCoverage, fwksched.Endpoint, fwksched.Endpoint) {
	t.Helper()
	s, _ := newTestScorer(parameters{QueueWeight: 1})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")
	parent := withRequestID(withHeader(chatRequest("parent", 48000), defaultShareAsHeaderName, "R"), "req-parent")
	s.PreRequest(context.Background(), parent, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), parent, endOfStreamResponse(12000, 20), podA.GetMetadata())
	return s, podA, podB
}

func TestCostBurstSpillsFromWarmEndpoint(t *testing.T) {
	s, podA, podB := costScenario(t)

	// Each stacked child occupies its admission gap (250) plus the decode
	// allowance (750). A lightly-stacked warm pod keeps winning — reuse is
	// worth more than the queue — until the stacked work outweighs a cold
	// 12k prefill (threshold: 13 children).
	for i := 0; i < 13; i++ {
		child := withRequestID(withHeader(chatRequest(fmt.Sprintf("child-%d", i), 49000), defaultRootHeaderName, "R"),
			fmt.Sprintf("req-c%d", i))
		scores := s.Score(context.Background(), child, []fwksched.Endpoint{podA, podB})
		if i < 3 && scores[podA] <= scores[podB] {
			t.Fatalf("lightly-stacked warm pod must still win at child %d: %v", i, scores)
		}
		s.PreRequest(context.Background(), child, schedulingResultFor(podA))
	}

	overflow := withRequestID(withHeader(chatRequest("child-13", 49000), defaultRootHeaderName, "R"), "req-c13")
	scores := s.Score(context.Background(), overflow, []fwksched.Endpoint{podA, podB})
	if scores[podB] <= scores[podA] {
		t.Fatalf("burst must spill once stacked work outweighs the cold prefill: %v", scores)
	}
}

func TestCostStaggeredFollowsWarmEndpoint(t *testing.T) {
	s, podA, podB := costScenario(t)

	child1 := withRequestID(withHeader(chatRequest("child-1", 49000), defaultRootHeaderName, "R"), "req-c1")
	s.PreRequest(context.Background(), child1, schedulingResultFor(podA))
	// child-1 completes before child-2 arrives (staggered arrivals).
	s.ResponseBody(context.Background(), child1, endOfStreamResponse(12250, 20), podA.GetMetadata())

	child2 := withRequestID(withHeader(chatRequest("child-2", 49000), defaultRootHeaderName, "R"), "req-c2")
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

// dynamoRequest builds a chat request carrying the Dynamo
// nvext.session_control dialect in its body payload instead of headers.
func dynamoRequest(sid, action string, chars int) *fwksched.InferenceRequest {
	r := chatRequest("", chars)
	r.Body.Payload = fwkrh.PayloadMap(map[string]any{
		"nvext": map[string]any{
			"session_control": map[string]any{"session_id": sid, "action": action},
		},
	})
	return r
}

func TestDynamoDialectCarriesSessionIdentity(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	s.PreRequest(context.Background(), dynamoRequest("conv-1", "bind", 4000), schedulingResultFor(podA))

	scores := s.Score(context.Background(), dynamoRequest("conv-1", "bind", 4800), []fwksched.Endpoint{podA, podB})
	expectScore(t, scores, podA, 1001.0/1201.0)
	expectScore(t, scores, podB, 0.0)
}

func TestDynamoCloseReleasesSession(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")

	bindTurn := dynamoRequest("conv-1", "bind", 4000)
	s.PreRequest(context.Background(), bindTurn, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), bindTurn, endOfStreamResponse(1200, 50), podA.GetMetadata())

	// The final turn still routes with affinity, then releases the entry.
	closeTurn := dynamoRequest("conv-1", "close", 4400)
	scores := s.Score(context.Background(), closeTurn, []fwksched.Endpoint{podA})
	if scores[podA] <= 0 {
		t.Fatalf("final turn must still score its session's endpoint: %v", scores)
	}
	s.ResponseBody(context.Background(), closeTurn, endOfStreamResponse(1400, 50), podA.GetMetadata())

	s.mu.Lock()
	_, exists := s.sessions["conv-1"]
	s.mu.Unlock()
	if exists {
		t.Error("close action must release the session entry")
	}
}

func TestDynamoCloseReleasesWithoutUsage(t *testing.T) {
	// Regression: a close on a stream without include_usage arrives with zero
	// usage in the final chunk. The declaration ends the lineage, not the
	// accounting — release must not sit behind the usage early-return.
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")

	bindTurn := dynamoRequest("conv-1", "bind", 4000)
	s.PreRequest(context.Background(), bindTurn, schedulingResultFor(podA))

	closeTurn := dynamoRequest("conv-1", "close", 4400)
	s.ResponseBody(context.Background(), closeTurn, endOfStreamResponse(0, 0), podA.GetMetadata())

	s.mu.Lock()
	_, exists := s.sessions["conv-1"]
	s.mu.Unlock()
	if exists {
		t.Error("close must release the session even when the response carries no usage")
	}

	// Same for a response with no resolved endpoint metadata.
	s.PreRequest(context.Background(), bindTurn, schedulingResultFor(podA))
	s.ResponseBody(context.Background(), dynamoRequest("conv-1", "close", 4400), endOfStreamResponse(1000, 50), nil)

	s.mu.Lock()
	_, exists = s.sessions["conv-1"]
	s.mu.Unlock()
	if exists {
		t.Error("close must release the session even without endpoint metadata")
	}
}

func TestHeaderPrecedesDynamoDialect(t *testing.T) {
	s, _ := newTestScorer(parameters{})
	podA := newEndpoint("pod-a")

	r := dynamoRequest("nvext-id", "bind", 4000)
	r.Headers[defaultHeaderName] = "header-id"
	s.PreRequest(context.Background(), r, schedulingResultFor(podA))

	s.mu.Lock()
	_, headerExists := s.sessions["header-id"]
	_, nvextExists := s.sessions["nvext-id"]
	s.mu.Unlock()
	if !headerExists || nvextExists {
		t.Errorf("header must take precedence: header=%v nvext=%v", headerExists, nvextExists)
	}
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
		s.queueWeight != defaultQueueWeight {
		t.Errorf("Factory defaults not applied: %+v", s)
	}
	if s.TypedName().Type != SessionCoverageScorerType {
		t.Errorf("TypedName().Type = %s, want %s", s.TypedName().Type, SessionCoverageScorerType)
	}
	if s.Category() != fwksched.Affinity {
		t.Errorf("Category() = %s, want Affinity", s.Category())
	}
}
