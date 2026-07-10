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
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8stypes "k8s.io/apimachinery/pkg/types"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
	prefixscorer "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/scheduling/scorer/prefix"
)

func newTestProducer(t *testing.T) *Producer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return New(ctx, ProducerType, Parameters{}, nil)
}

func newTestEndpoint(name string) fwksched.Endpoint {
	return fwksched.NewEndpoint(
		&fwkdl.EndpointMetadata{NamespacedName: k8stypes.NamespacedName{Namespace: "ns", Name: name}},
		fwkdl.NewMetrics(), nil)
}

func newTestRequest(id string, body *fwkrh.InferenceRequestBody, headers map[string]string) *fwksched.InferenceRequest {
	if headers == nil {
		headers = map[string]string{}
	}
	return &fwksched.InferenceRequest{RequestID: id, TargetModel: "m1", Body: body, Headers: headers}
}

func scheduledTo(ep fwksched.Endpoint) *fwksched.SchedulingResult {
	return &fwksched.SchedulingResult{
		PrimaryProfileName: "default",
		ProfileResults: map[string]*fwksched.ProfileRunResult{
			"default": {TargetEndpoints: []fwksched.Endpoint{ep}},
		},
	}
}

func eosResponse(id string, promptTokens, completionTokens int) *requestcontrol.Response {
	return &requestcontrol.Response{
		RequestID:   id,
		EndOfStream: true,
		Usage:       fwkrh.Usage{PromptTokens: promptTokens, CompletionTokens: completionTokens},
	}
}

func matchInfo(t *testing.T, ep fwksched.Endpoint) *attrprefix.PrefixCacheMatchInfo {
	t.Helper()
	key := attrprefix.PrefixCacheMatchInfoDataKey.WithNonEmptyProducerName(ProducerType).String()
	raw, ok := ep.Get(key)
	require.True(t, ok, "PrefixCacheMatchInfo must be published on every endpoint")
	info, ok := raw.(*attrprefix.PrefixCacheMatchInfo)
	require.True(t, ok)
	return info
}

// runTurn drives one full request lifecycle against the producer.
func runTurn(t *testing.T, p *Producer, req *fwksched.InferenceRequest, pods []fwksched.Endpoint, target fwksched.Endpoint, promptTokens, completionTokens int) {
	t.Helper()
	require.NoError(t, p.Produce(context.Background(), req, pods))
	p.PreRequest(context.Background(), req, scheduledTo(target))
	p.ResponseBody(context.Background(), req, eosResponse(req.RequestID, promptTokens, completionTokens), target.GetMetadata())
}

var (
	turn1Messages = []fwkrh.Message{
		textMessage("system", "you are a helpful assistant for Acme Corp"),
		textMessage("user", "hello, I need help with my order"),
	}
	turn2Messages = slices.Concat(turn1Messages, []fwkrh.Message{
		textMessage("assistant", "of course, what is your order number?"),
		textMessage("user", "order 12345, it never arrived"),
	})
)

// L1 acceptance: a resent prefix recomputes the chain — two independently
// constructed producers, fed identical turn sequences through the public
// hooks only (no engine involvement), resolve identical coverage.
func TestProducerL1DeterministicAcrossInstances(t *testing.T) {
	results := make([]*attrprefix.PrefixCacheMatchInfo, 0, 2)
	for range 2 {
		p := newTestProducer(t)
		podA, podB := newTestEndpoint("pod-a"), newTestEndpoint("pod-b")
		pods := []fwksched.Endpoint{podA, podB}

		runTurn(t, p, newTestRequest("r1", chatBody(turn1Messages...), nil), pods, podA, 100, 20)

		req2 := newTestRequest("r2", chatBody(turn2Messages...), nil)
		require.NoError(t, p.Produce(context.Background(), req2, pods))

		infoA := matchInfo(t, podA)
		assert.Positive(t, infoA.MatchBlocks(), "the continuing turn must be covered on the pod that served turn 1")
		assert.Zero(t, matchInfo(t, podB).MatchBlocks(), "a pod that never served the session has no coverage")
		results = append(results, infoA)
	}
	assert.Equal(t, results[0].MatchBlocks(), results[1].MatchBlocks(), "coverage must be identical across independently built producers")
	assert.Equal(t, results[0].TotalBlocks(), results[1].TotalBlocks())
}

// L2 acceptance: the covered fraction reaches scheduling through the real
// generic prefix-cache-scorer bound to this producer's name.
func TestProducerL2GenericScorerIntegration(t *testing.T) {
	p := newTestProducer(t)
	podA, podB := newTestEndpoint("pod-a"), newTestEndpoint("pod-b")
	pods := []fwksched.Endpoint{podA, podB}

	runTurn(t, p, newTestRequest("r1", chatBody(turn1Messages...), nil), pods, podA, 100, 20)

	req2 := newTestRequest("r2", chatBody(turn2Messages...), nil)
	require.NoError(t, p.Produce(context.Background(), req2, pods))

	scorer, err := prefixscorer.New(context.Background(), "prefix-cache-scorer", ProducerType)
	require.NoError(t, err)
	scores := scorer.Score(context.Background(), req2, pods)

	assert.Greater(t, scores[podA], scores[podB], "the scorer must rank the session's pod above a cold pod")
	assert.Zero(t, scores[podB])
	assert.LessOrEqual(t, scores[podA], 1.0)
	assert.Positive(t, scores[podA])
}

// A fork (shared prefix, divergent suffix) is covered only up to the fork
// point on the parent's pod.
func TestProducerForkCoverageClampsAtDivergence(t *testing.T) {
	p := newTestProducer(t)
	podA, podB := newTestEndpoint("pod-a"), newTestEndpoint("pod-b")
	pods := []fwksched.Endpoint{podA, podB}

	runTurn(t, p, newTestRequest("r1", chatBody(turn2Messages...), nil), pods, podA, 200, 50)

	forked := slices.Concat(turn1Messages, []fwkrh.Message{
		textMessage("assistant", "a completely different reply"),
		textMessage("user", "a completely different follow-up"),
	})
	reqFork := newTestRequest("r2", chatBody(forked...), nil)
	require.NoError(t, p.Produce(context.Background(), reqFork, pods))

	infoA := matchInfo(t, podA)
	assert.Positive(t, infoA.MatchBlocks(), "the shared prefix is covered on the parent's pod")
	assert.Less(t, infoA.MatchBlocks(), infoA.TotalBlocks(), "coverage must clamp at the fork point, not claim the rewritten suffix")
	assert.Zero(t, matchInfo(t, podB).MatchBlocks())

	blocks, _ := contentBlocks(chatBody(turn1Messages...))
	wantCap := p.estTokens(blocks[len(blocks)-1].cumChars)
	assert.Equal(t, wantCap, infoA.MatchBlocks(), "the clamp is the estimated tokens of the shared prefix")
}

// A delta continuation carrying only a declared session id resolves through
// the alias and scores fully on the session's pod.
func TestProducerDeclaredHeaderBridgesDelta(t *testing.T) {
	p := newTestProducer(t)
	podA, podB := newTestEndpoint("pod-a"), newTestEndpoint("pod-b")
	pods := []fwksched.Endpoint{podA, podB}

	headers := map[string]string{"x-session-id": "sess-1"}
	runTurn(t, p, newTestRequest("r1", chatBody(turn1Messages...), headers), pods, podA, 200, 50)

	delta := newTestRequest("r2", chatBody(textMessage("user", "and one more thing")), headers)
	require.NoError(t, p.Produce(context.Background(), delta, pods))

	infoA := matchInfo(t, podA)
	assert.Equal(t, infoA.TotalBlocks(), infoA.MatchBlocks(), "a delta bridged by the alias is fully covered on the session's pod")
	assert.Positive(t, infoA.TotalBlocks())
	assert.Zero(t, matchInfo(t, podB).MatchBlocks())
}

func TestProducerUnsupportedBodyPublishesZeroInfo(t *testing.T) {
	p := newTestProducer(t)
	podA := newTestEndpoint("pod-a")

	req := newTestRequest("r1", &fwkrh.InferenceRequestBody{Embeddings: &fwkrh.EmbeddingsRequest{}}, nil)
	require.NoError(t, p.Produce(context.Background(), req, []fwksched.Endpoint{podA}))

	info := matchInfo(t, podA)
	assert.Zero(t, info.MatchBlocks())
	assert.Equal(t, 1, info.TotalBlocks(), "no identity publishes a one-token denominator so the scorer maps it to score 0 without a zero-total error")
}

// Streaming responses without usage keep the PreRequest estimate: extents
// are monotone-max, so a zero usage never erases the seed.
func TestProducerMissingUsageKeepsSeededEstimate(t *testing.T) {
	p := newTestProducer(t)
	podA, podB := newTestEndpoint("pod-a"), newTestEndpoint("pod-b")
	pods := []fwksched.Endpoint{podA, podB}

	req1 := newTestRequest("r1", chatBody(turn1Messages...), nil)
	require.NoError(t, p.Produce(context.Background(), req1, pods))
	p.PreRequest(context.Background(), req1, scheduledTo(podA))
	// Intermediate chunk, then a final chunk with no usage fields.
	p.ResponseBody(context.Background(), req1, &requestcontrol.Response{RequestID: "r1"}, podA.GetMetadata())
	p.ResponseBody(context.Background(), req1, eosResponse("r1", 0, 0), podA.GetMetadata())

	req2 := newTestRequest("r2", chatBody(turn2Messages...), nil)
	require.NoError(t, p.Produce(context.Background(), req2, pods))
	assert.Positive(t, matchInfo(t, podA).MatchBlocks(), "the optimistic seed must survive a usage-less response")
}

// The DAG validates producer/consumer data types with reflect.TypeOf; the
// published value type must match the generic scorer's consumed type exactly.
func TestProducerProducesMatchesScorerConsumes(t *testing.T) {
	p := newTestProducer(t)
	scorer, err := prefixscorer.New(context.Background(), "prefix-cache-scorer", ProducerType)
	require.NoError(t, err)

	produced := p.Produces()
	consumed := scorer.Consumes().Required
	require.Len(t, consumed, 1)
	for key, consumedVal := range consumed {
		producedVal, ok := produced[key]
		require.True(t, ok, "the scorer's consumed key must be produced under this producer's name")
		assert.Equal(t, reflect.TypeOf(consumedVal), reflect.TypeOf(producedVal))
	}
}

func TestFactory(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		wantErr bool
	}{
		{name: "no parameters", params: ""},
		{name: "valid parameters", params: `{"sessionTTLSeconds": 600, "maxSessions": 1000, "charsPerToken": 3.5, "sessionHeaders": ["X-My-Session"]}`},
		{name: "unknown field rejected", params: `{"unknownField": 1}`, wantErr: true},
		{name: "negative ttl rejected", params: `{"sessionTTLSeconds": -1}`, wantErr: true},
		{name: "negative charsPerToken rejected", params: `{"charsPerToken": -0.5}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pl, err := Factory(ProducerType, plugin.StrictDecoder(json.RawMessage(tt.params)), nil)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, ProducerType, pl.TypedName().Type)
		})
	}
}
