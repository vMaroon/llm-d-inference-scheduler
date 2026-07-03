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

package chainidentity_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	k8stypes "k8s.io/apimachinery/pkg/types"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrsession "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/session"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/chainidentity"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/scheduling/scorer/sessioncoverage"
)

func newProducer(salt string) *chainidentity.Producer {
	//nolint:staticcheck // nil context intentionally skips the background sweeper in tests.
	return chainidentity.New(nil, "test", chainidentity.Parameters{TenantSalt: salt})
}

// chatReq builds a chat-completions request from (role, content) pairs.
func chatReq(model string, msgs ...[2]string) *fwksched.InferenceRequest {
	m := make([]fwkrh.Message, 0, len(msgs))
	for _, pair := range msgs {
		m = append(m, fwkrh.Message{Role: pair[0], Content: fwkrh.Content{Raw: pair[1]}})
	}
	return &fwksched.InferenceRequest{
		TargetModel: model,
		Headers:     map[string]string{},
		Body: &fwkrh.InferenceRequestBody{
			ChatCompletions: &fwkrh.ChatCompletionsRequest{Messages: m},
		},
	}
}

func produce(t *testing.T, p *chainidentity.Producer, r *fwksched.InferenceRequest) {
	t.Helper()
	if err := p.Produce(context.Background(), r, nil); err != nil {
		t.Fatalf("Produce returned error: %v", err)
	}
}

func derivedID(t *testing.T, r *fwksched.InferenceRequest) string {
	t.Helper()
	id, ok := attrsession.ReadDerivedSessionID(r)
	if !ok || id == "" {
		t.Fatalf("no derived session id published")
	}
	return string(id)
}

func TestDeterministicAcrossInstances(t *testing.T) {
	// Identity is recoverable by recomputation: two routers with the same
	// configuration derive the same id for the same content.
	r1 := chatReq("m", [2]string{"system", "sys"}, [2]string{"user", "hello"})
	r2 := chatReq("m", [2]string{"system", "sys"}, [2]string{"user", "hello"})
	produce(t, newProducer("tenant-a"), r1)
	produce(t, newProducer("tenant-a"), r2)
	if derivedID(t, r1) != derivedID(t, r2) {
		t.Errorf("same salt + content must derive the same id: %s vs %s", derivedID(t, r1), derivedID(t, r2))
	}

	// A different tenant salt derives a disjoint identity space.
	r3 := chatReq("m", [2]string{"system", "sys"}, [2]string{"user", "hello"})
	produce(t, newProducer("tenant-b"), r3)
	if derivedID(t, r1) == derivedID(t, r3) {
		t.Error("different tenant salts must not share ids")
	}

	// A different model derives a different identity: KV is model-specific.
	r4 := chatReq("other-model", [2]string{"system", "sys"}, [2]string{"user", "hello"})
	produce(t, newProducer("tenant-a"), r4)
	if derivedID(t, r1) == derivedID(t, r4) {
		t.Error("different models must not share ids")
	}
}

func TestContinuationKeepsLineage(t *testing.T) {
	p := newProducer("")
	t1 := chatReq("m", [2]string{"system", "sys"}, [2]string{"user", "u1"})
	produce(t, p, t1)

	// Turn 2 resends the history and appends: same lineage, head advances.
	t2 := chatReq("m", [2]string{"system", "sys"}, [2]string{"user", "u1"},
		[2]string{"assistant", "a1"}, [2]string{"user", "u2"})
	produce(t, p, t2)
	if derivedID(t, t1) != derivedID(t, t2) {
		t.Errorf("linear extension must keep the lineage id: %s vs %s", derivedID(t, t1), derivedID(t, t2))
	}
	if _, ok := attrsession.ReadForkParent(t2); ok {
		t.Error("a linear extension is not a fork")
	}

	// Replaying the full known prefix stays in the lineage too.
	t3 := chatReq("m", [2]string{"system", "sys"}, [2]string{"user", "u1"},
		[2]string{"assistant", "a1"}, [2]string{"user", "u2"})
	produce(t, p, t3)
	if derivedID(t, t3) != derivedID(t, t1) {
		t.Error("replay of a known prefix must resolve to the same lineage")
	}
}

func TestForkDiscoveredFromMidChain(t *testing.T) {
	p := newProducer("")
	t1 := chatReq("m", [2]string{"system", "sys"}, [2]string{"user", "u1"},
		[2]string{"assistant", "a1"}, [2]string{"user", "u2"})
	produce(t, p, t1)
	parentID := derivedID(t, t1)

	// A sub-agent resends the prefix through a1 and diverges — no
	// declaration anywhere.
	child := chatReq("m", [2]string{"system", "sys"}, [2]string{"user", "u1"},
		[2]string{"assistant", "a1"}, [2]string{"user", "subtask"})
	produce(t, p, child)

	if derivedID(t, child) == parentID {
		t.Fatal("a diverging chain must become its own lineage")
	}
	forkFrom, ok := attrsession.ReadForkParent(child)
	if !ok || string(forkFrom) != parentID {
		t.Errorf("fork parent = %q, want %q", forkFrom, parentID)
	}
}

func TestTemplateSharingForksAtSharedRoot(t *testing.T) {
	p := newProducer("")
	a := chatReq("m", [2]string{"system", "shared-template"}, [2]string{"user", "alice"})
	produce(t, p, a)
	b := chatReq("m", [2]string{"system", "shared-template"}, [2]string{"user", "bob"})
	produce(t, p, b)

	if derivedID(t, a) == derivedID(t, b) {
		t.Fatal("sessions sharing a template must not share identity")
	}
	forkFrom, ok := attrsession.ReadForkParent(b)
	if !ok || string(forkFrom) != derivedID(t, a) {
		t.Errorf("the second session forks at the shared root: parent = %q, want %q", forkFrom, derivedID(t, a))
	}
}

func TestRewriteIsNewLineage(t *testing.T) {
	// Structural rollover: a rewritten history shares no known append, so it
	// becomes a new lineage even under the same declared client id.
	p := newProducer("")
	t1 := chatReq("m", [2]string{"system", "old-sys"}, [2]string{"user", "u1"})
	t1.Headers["x-session-id"] = "conv-1"
	produce(t, p, t1)

	rewrite := chatReq("m", [2]string{"system", "new-sys"}, [2]string{"user", "summary"},
		[2]string{"user", "u2"})
	rewrite.Headers["x-session-id"] = "conv-1"
	produce(t, p, rewrite)

	if derivedID(t, rewrite) == derivedID(t, t1) {
		t.Error("a rewritten history must be a new lineage")
	}
	if _, ok := attrsession.ReadForkParent(rewrite); ok {
		t.Error("a rewrite that shares nothing is not a fork")
	}
}

func TestDeltaContinuationViaAlias(t *testing.T) {
	// Delta continuation carries no prefix to rehash; the declared id
	// bridges the handle.
	p := newProducer("")
	t1 := chatReq("m", [2]string{"system", "sys"}, [2]string{"user", "u1"},
		[2]string{"assistant", "a1"}, [2]string{"user", "u2"})
	t1.Headers["x-session-id"] = "conv-1"
	produce(t, p, t1)

	delta := chatReq("m", [2]string{"user", "u3"})
	delta.Headers["x-session-id"] = "conv-1"
	produce(t, p, delta)

	if derivedID(t, delta) != derivedID(t, t1) {
		t.Errorf("delta continuation must resolve through the alias: %s vs %s", derivedID(t, delta), derivedID(t, t1))
	}
}

func TestFramingUnambiguous(t *testing.T) {
	// Different message splits of the same concatenated bytes must derive
	// different identities — the invariant the estimate backend's
	// delimiter-free flattening cannot provide.
	a := chatReq("m", [2]string{"user", "ab"}, [2]string{"user", "c"})
	b := chatReq("m", [2]string{"user", "a"}, [2]string{"user", "bc"})
	produce(t, newProducer(""), a)
	produce(t, newProducer(""), b)
	if derivedID(t, a) == derivedID(t, b) {
		t.Error("distinct message splits must not collide")
	}
}

func TestPublishesStructuralMarker(t *testing.T) {
	r := chatReq("m", [2]string{"user", "hello"})
	produce(t, newProducer(""), r)
	if !attrsession.HasStructuralIdentity(r) {
		t.Error("derived identity must carry the structural marker")
	}
	if _, ok := attrsession.ReadForkParent(r); ok {
		t.Error("a fresh lineage has no fork parent")
	}
}

// --- lineage integration with the session-coverage scorer ---

func newScorer(t *testing.T) *sessioncoverage.SessionCoverage {
	t.Helper()
	pl, err := sessioncoverage.Factory("test", json.NewDecoder(strings.NewReader(`{"queueWeight": -1}`)), nil)
	if err != nil {
		t.Fatalf("scorer factory: %v", err)
	}
	return pl.(*sessioncoverage.SessionCoverage)
}

func newEndpoint(name string) fwksched.Endpoint {
	return fwksched.NewEndpoint(
		&fwkdl.EndpointMetadata{NamespacedName: k8stypes.NamespacedName{Namespace: "ns", Name: name}},
		&fwkdl.Metrics{},
		nil,
	)
}

func scheduledTo(endpoint fwksched.Endpoint) *fwksched.SchedulingResult {
	return &fwksched.SchedulingResult{
		PrimaryProfileName: "default",
		ProfileResults: map[string]*fwksched.ProfileRunResult{
			"default": {TargetEndpoints: []fwksched.Endpoint{endpoint}},
		},
	}
}

func eos(prompt, completion int) *requestcontrol.Response {
	return &requestcontrol.Response{
		EndOfStream: true,
		Usage:       fwkrh.Usage{PromptTokens: prompt, CompletionTokens: completion},
	}
}

func TestUndeclaredForkRoutesToParentEndpoint(t *testing.T) {
	// The full Phase-3 loop: no client declarations anywhere. The producer
	// derives identity, discovers the sub-agent fork from the chain, and the
	// scorer adopts the parent's coverage — the undeclared-fork gap closed.
	ctx := context.Background()
	p := newProducer("")
	s := newScorer(t)
	podA, podB := newEndpoint("pod-a"), newEndpoint("pod-b")

	sys := strings.Repeat("s", 8000)
	t1 := chatReq("m", [2]string{"system", sys}, [2]string{"user", "please do the task"})
	produce(t, p, t1)
	s.PreRequest(ctx, t1, scheduledTo(podA))
	s.ResponseBody(ctx, t1, eos(4800, 200), podA.GetMetadata())

	t2 := chatReq("m", [2]string{"system", sys}, [2]string{"user", "please do the task"},
		[2]string{"assistant", "working on it"}, [2]string{"user", "continue"})
	produce(t, p, t2)
	s.PreRequest(ctx, t2, scheduledTo(podA))
	s.ResponseBody(ctx, t2, eos(5200, 300), podA.GetMetadata())

	// The sub-agent resends the shared prefix and diverges.
	child := chatReq("m", [2]string{"system", sys}, [2]string{"user", "please do the task"},
		[2]string{"assistant", "working on it"}, [2]string{"user", "subtask: check the logs"})
	produce(t, p, child)
	if derivedID(t, child) == derivedID(t, t1) {
		t.Fatal("child must be its own lineage")
	}

	scores := s.Score(ctx, child, []fwksched.Endpoint{podA, podB})
	if scores[podA] <= 0 {
		t.Fatalf("undeclared fork must inherit the parent's warm endpoint: %v", scores)
	}
	if scores[podB] != 0 {
		t.Fatalf("cold endpoint must score zero: %v", scores)
	}
}

func TestDeltaContinuationDoesNotRollover(t *testing.T) {
	// A delta turn's prompt estimate is far below the lineage's high-water.
	// The structural marker must keep the scorer's shrink heuristic from
	// deleting the lineage entry.
	ctx := context.Background()
	p := newProducer("")
	s := newScorer(t)
	podA := newEndpoint("pod-a")

	t1 := chatReq("m", [2]string{"system", strings.Repeat("s", 20000)}, [2]string{"user", "u1"})
	t1.Headers["x-session-id"] = "conv-1"
	produce(t, p, t1)
	s.PreRequest(ctx, t1, scheduledTo(podA))
	s.ResponseBody(ctx, t1, eos(12000, 100), podA.GetMetadata())

	delta := chatReq("m", [2]string{"user", "tiny follow-up"})
	delta.Headers["x-session-id"] = "conv-1"
	produce(t, p, delta)
	if derivedID(t, delta) != derivedID(t, t1) {
		t.Fatal("delta must resolve to the same lineage")
	}

	scores := s.Score(ctx, delta, []fwksched.Endpoint{podA})
	if scores[podA] != 1.0 {
		t.Fatalf("delta turn must stay on the covered endpoint without rollover: %v", scores)
	}
}
