package scorer

import (
	"testing"

	"github.com/llm-d/llm-d-kv-cache/pkg/kvevents"
	"github.com/llm-d/llm-d-kv-cache/pkg/tokenization/types"
	k8stypes "k8s.io/apimachinery/pkg/types"
	fwkdl "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/datalayer"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/scheduling"

	"github.com/llm-d/llm-d-inference-scheduler/test/utils"
	"golang.org/x/net/context"
)

func makeEndpoints(n int) []scheduling.Endpoint {
	endpoints := make([]scheduling.Endpoint, n)
	for i := range n {
		endpoints[i] = scheduling.NewEndpoint(
			&fwkdl.EndpointMetadata{
				NamespacedName: k8stypes.NamespacedName{Name: "pod", Namespace: "default"},
				Address:        "10.0.0.1:8080",
			},
			nil, nil,
		)
	}
	return endpoints
}

func makeScores(endpoints []scheduling.Endpoint, spread bool) map[string]float64 {
	scores := make(map[string]float64, len(endpoints))
	for i, ep := range endpoints {
		addr := ep.GetMetadata().Address
		if spread {
			scores[addr] = float64(i)
		} else {
			scores[addr] = 0
		}
	}
	return scores
}

// BenchmarkScoreTokenized benchmarks the fast path: pre-tokenized data.
func BenchmarkScoreTokenized(b *testing.B) {
	ctx := utils.NewTestContext(b)
	endpoints := makeEndpoints(50)
	tokenIDs := make([]uint32, 2048)
	for i := range tokenIDs {
		tokenIDs[i] = uint32(i)
	}

	scorer := &PrecisePrefixCacheScorer{
		typedName:      plugin.TypedName{Type: PrecisePrefixCachePluginType, Name: "bench"},
		kvEventsConfig: &kvevents.Config{},
		kvCacheIndexer: &mockKVCacheIndexer{
			scoreTokensFunc: func(_ context.Context, _ []uint32, _ string, _ []string) (map[string]float64, error) {
				return makeScores(endpoints, true), nil
			},
		},
	}

	request := &scheduling.LLMRequest{
		TargetModel: "bench-model",
		TokenizedPrompt: &scheduling.TokenizedPrompt{
			TokenIDs: tokenIDs,
		},
	}

	b.ResetTimer()
	for range b.N {
		scorer.Score(ctx, scheduling.NewCycleState(), request, endpoints)
	}
}

// BenchmarkScoreFallback benchmarks the fallback path: internal tokenization.
func BenchmarkScoreFallback(b *testing.B) {
	ctx := utils.NewTestContext(b)
	endpoints := makeEndpoints(50)

	scorer := &PrecisePrefixCacheScorer{
		typedName:      plugin.TypedName{Type: PrecisePrefixCachePluginType, Name: "bench"},
		kvEventsConfig: &kvevents.Config{},
		kvCacheIndexer: &mockKVCacheIndexer{
			getPodScoresFunc: func(_ context.Context, _ *types.RenderChatRequest, _ string, _ string, _ []string) (map[string]float64, error) {
				return makeScores(endpoints, true), nil
			},
		},
	}

	request := &scheduling.LLMRequest{
		TargetModel: "bench-model",
		Body: &scheduling.LLMRequestBody{
			Completions: &scheduling.CompletionsRequest{
				Prompt: "Benchmark prompt for internal tokenization fallback path.",
			},
		},
	}

	b.ResetTimer()
	for range b.N {
		scorer.Score(ctx, scheduling.NewCycleState(), request, endpoints)
	}
}

// BenchmarkNormalization benchmarks the score normalization in isolation.
func BenchmarkNormalization(b *testing.B) {
	endpoints := makeEndpoints(100)
	scores := makeScores(endpoints, true)

	endpointToKey := func(ep scheduling.Endpoint) (string, bool) {
		m := ep.GetMetadata()
		if m == nil {
			return "", false
		}
		return m.Address, true
	}

	b.ResetTimer()
	for range b.N {
		indexedScoresToNormalizedScoredPods(endpoints, endpointToKey, scores)
	}
}

// BenchmarkNoHitLRUCold benchmarks NoHitLRU scoring for cold requests.
func BenchmarkNoHitLRUCold(b *testing.B) {
	ctx := utils.NewTestContext(b)
	lruScorer := NewNoHitLRU(ctx, nil)
	endpoints := makeEndpoints(50)

	cycleState := scheduling.NewCycleState()
	cycleState.Write(plugin.StateKey(plugin.TypedName{
		Type: PrecisePrefixCachePluginType,
		Name: PrecisePrefixCachePluginType,
	}.String()), &PrefixCacheHitState{HasCacheHit: false})

	request := &scheduling.LLMRequest{RequestId: "bench-cold"}

	b.ResetTimer()
	for range b.N {
		lruScorer.Score(ctx, cycleState, request, endpoints)
	}
}

// BenchmarkNoHitLRUWarm benchmarks NoHitLRU scoring for warm requests (neutral).
func BenchmarkNoHitLRUWarm(b *testing.B) {
	ctx := utils.NewTestContext(b)
	lruScorer := NewNoHitLRU(ctx, nil)
	endpoints := makeEndpoints(50)

	cycleState := scheduling.NewCycleState()
	cycleState.Write(plugin.StateKey(plugin.TypedName{
		Type: PrecisePrefixCachePluginType,
		Name: PrecisePrefixCachePluginType,
	}.String()), &PrefixCacheHitState{HasCacheHit: true, MaxRawScore: 5.0})

	request := &scheduling.LLMRequest{RequestId: "bench-warm"}

	b.ResetTimer()
	for range b.N {
		lruScorer.Score(ctx, cycleState, request, endpoints)
	}
}
