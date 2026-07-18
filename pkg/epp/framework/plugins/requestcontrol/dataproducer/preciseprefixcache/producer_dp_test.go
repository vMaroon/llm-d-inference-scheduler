/*
Copyright 2026 The llm-d Authors.

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

package preciseprefixcache

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"

	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
	"github.com/llm-d/llm-d-router/test/utils"
)

// fakeDPRankSource maps endpoint addresses to global data-parallel ranks,
// standing in for the KV-events pool's GlobalDPRank lookup.
type fakeDPRankSource struct {
	ranks map[string]int
}

func (f *fakeDPRankSource) GlobalDPRank(podIdentifier string) (int, bool) {
	rank, ok := f.ranks[podIdentifier]
	return rank, ok
}

// Produce annotates each endpoint's PrefixCacheMatchInfo with the global
// data-parallel rank the KV-events pool learned for its address; endpoints
// with no learned rank stay unannotated.
func TestProduce_AnnotatesGlobalDPRank(t *testing.T) {
	ctx := utils.NewTestContext(t)

	wantKey := kvblock.BlockHash(0xCAFE)
	idx := &fakeKVCacheIndexer{
		computeFromTokens: func(_ context.Context, _ []uint32, _ string, _ []*kvblock.BlockExtraFeatures) ([]kvblock.BlockHash, error) {
			return []kvblock.BlockHash{wantKey}, nil
		},
		index: &fakeKVBlockIndex{
			lookup: func(_ context.Context, _ []kvblock.BlockHash, _ sets.Set[string]) (map[kvblock.BlockHash][]kvblock.PodEntry, error) {
				return map[kvblock.BlockHash][]kvblock.PodEntry{
					wantKey: {{PodIdentifier: "10.0.0.1:8080"}},
				}, nil
			},
		},
	}

	p := newProducerWithIndexer(ctx, idx, &fakeKVBlockScorer{})
	p.dpRanks = &fakeDPRankSource{ranks: map[string]int{"10.0.0.1:8080": 5}}

	req := &scheduling.InferenceRequest{
		RequestID:   "req-dp-rank",
		TargetModel: "test-model",
		Body: &fwkrh.InferenceRequestBody{
			TokenizedPrompt: &fwkrh.TokenizedPrompt{PerPromptTokens: [][]uint32{{10, 20, 30}}},
		},
	}

	endpoints := freshEndpoints()
	require.NoError(t, p.Produce(ctx, req, endpoints))

	raw, ok := endpoints[0].Get(p.dk.String())
	require.True(t, ok)
	rank, ok := raw.(*attrprefix.PrefixCacheMatchInfo).GlobalDPRank()
	require.True(t, ok, "endpoint with a learned rank must be annotated")
	assert.Equal(t, 5, rank)

	raw, ok = endpoints[1].Get(p.dk.String())
	require.True(t, ok)
	_, ok = raw.(*attrprefix.PrefixCacheMatchInfo).GlobalDPRank()
	assert.False(t, ok, "endpoint without a learned rank must stay unannotated")
}
