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
	"fmt"
	"testing"

	"github.com/llm-d/llm-d-router/pkg/kvcache"
	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"

	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
)

var (
	benchmarkCounts       map[string]int
	benchmarkCountsByTier map[string]map[string]int
	benchmarkScores       map[string]float64
	benchmarkMatches      map[string]kvblock.PrefixMatch
)

func TestMatchedBlockCount(t *testing.T) {
	const (
		podA = "10.0.0.1:8000"
		podB = "10.0.0.2:8000"
	)
	keys := []kvblock.BlockHash{1, 2, 3, 4}

	// gpu/cpu tiers must count identically — the unweighted count ignores tier.
	gpu := func(pod string) kvblock.PodEntry { return kvblock.PodEntry{PodIdentifier: pod, DeviceTier: "gpu"} }
	cpu := func(pod string) kvblock.PodEntry { return kvblock.PodEntry{PodIdentifier: pod, DeviceTier: "cpu"} }

	tests := []struct {
		name      string
		keyToPods map[kvblock.BlockHash][]kvblock.PodEntry
		podID     string
		want      int
	}{
		{
			name: "all blocks held on RAM/cpu tier count fully (unweighted)",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {cpu(podA)}, 2: {cpu(podA)}, 3: {cpu(podA)}, 4: {cpu(podA)},
			},
			podID: podA,
			want:  4,
		},
		{
			name: "single RAM block counts as one, not zero",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {cpu(podA)},
			},
			podID: podA,
			want:  1,
		},
		{
			name: "stops at first missing block",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {gpu(podA)}, 2: {gpu(podA)}, 4: {gpu(podA)}, // block 3 missing
			},
			podID: podA,
			want:  2,
		},
		{
			name: "pod absent from first block yields zero",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {gpu(podB)}, 2: {gpu(podA)},
			},
			podID: podA,
			want:  0,
		},
		{
			name: "counts are per-pod independent",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {gpu(podA), cpu(podB)}, 2: {gpu(podA)}, 3: {cpu(podB)},
			},
			podID: podA,
			want:  2,
		},
		{
			name:      "empty index yields zero",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{},
			podID:     podA,
			want:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, matchedBlockCount(keys, tt.keyToPods, tt.podID))
		})
	}
}

func TestMatchedBlockCountByTier(t *testing.T) {
	const (
		podA = "10.0.0.1:8000"
		podB = "10.0.0.2:8000"
	)
	keys := []kvblock.BlockHash{1, 2, 3, 4}

	gpu := func(pod string) kvblock.PodEntry { return kvblock.PodEntry{PodIdentifier: pod, DeviceTier: "gpu"} }
	cpu := func(pod string) kvblock.PodEntry { return kvblock.PodEntry{PodIdentifier: pod, DeviceTier: "cpu"} }
	speculative := func(pod string) kvblock.PodEntry {
		return kvblock.PodEntry{PodIdentifier: pod, Speculative: true}
	}

	tests := []struct {
		name      string
		keyToPods map[kvblock.BlockHash][]kvblock.PodEntry
		podID     string
		want      map[string]int
	}{
		{
			name: "all blocks on one tier",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {gpu(podA)}, 2: {gpu(podA)}, 3: {gpu(podA)}, 4: {gpu(podA)},
			},
			podID: podA,
			want:  map[string]int{"gpu": 4},
		},
		{
			name: "dual-tier block counts once per tier",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {gpu(podA), cpu(podA)}, 2: {gpu(podA)},
			},
			podID: podA,
			want:  map[string]int{"gpu": 2, "cpu": 1},
		},
		{
			name: "tier-specific gap stops that tier only",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {gpu(podA), cpu(podA)}, 2: {gpu(podA), cpu(podA)}, 3: {gpu(podA)}, 4: {gpu(podA), cpu(podA)},
			},
			podID: podA,
			want:  map[string]int{"gpu": 4, "cpu": 2},
		},
		{
			name: "pod absent from first block yields empty map",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {gpu(podB)}, 2: {gpu(podA)},
			},
			podID: podA,
			want:  map[string]int{},
		},
		{
			name: "counts are per-pod independent",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {gpu(podA), cpu(podB)}, 2: {gpu(podA), cpu(podB)}, 3: {cpu(podB)},
			},
			podID: podA,
			want:  map[string]int{"gpu": 2},
		},
		{
			name: "speculative entries count under the speculative key",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {speculative(podA), gpu(podA)}, 2: {speculative(podA)},
			},
			podID: podA,
			want:  map[string]int{"gpu": 1, attrprefix.SpeculativeTierKey: 2},
		},
		{
			name:      "empty index yields empty map",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{},
			podID:     podA,
			want:      map[string]int{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchedBlockCountByTier(keys, tt.keyToPods, tt.podID)
			assert.NotNil(t, got)
			assert.Equal(t, tt.want, got)
			// Each tier's contiguous count never exceeds the any-tier count.
			anyTier := matchedBlockCount(keys, tt.keyToPods, tt.podID)
			for tier, count := range got {
				assert.LessOrEqual(t, count, anyTier, "tier %q", tier)
			}
		})
	}
}

func TestMatchedBlockCountsByPod(t *testing.T) {
	const (
		podA = "10.0.0.1:8000"
		podB = "10.0.0.2:8000"
		podC = "10.0.0.3:8000"
	)
	keys := []kvblock.BlockHash{1, 2, 3, 4}
	keyToPods := map[kvblock.BlockHash][]kvblock.PodEntry{
		1: {
			{PodIdentifier: podA, DeviceTier: "gpu"},
			{PodIdentifier: podA, DeviceTier: "cpu"},
			{PodIdentifier: podB, DeviceTier: "gpu"},
			{PodIdentifier: podC, Speculative: true},
		},
		2: {
			{PodIdentifier: podA, DeviceTier: "gpu"},
			{PodIdentifier: podB, DeviceTier: "gpu"},
			{PodIdentifier: podC, Speculative: true},
		},
		3: {
			{PodIdentifier: podB, DeviceTier: "gpu"},
			{PodIdentifier: podC, DeviceTier: "gpu"},
		},
		4: {
			{PodIdentifier: podA, DeviceTier: "gpu"},
			{PodIdentifier: podB, DeviceTier: "gpu"},
		},
	}

	counts, countsByTier := matchedBlockCountsByPod(keys, keyToPods)

	for _, podID := range []string{podA, podB, podC} {
		wantCount, wantByTier := matchedBlockCounts(keys, keyToPods, podID)
		assert.Equal(t, wantCount, counts[podID], "pod %q", podID)
		assert.Equal(t, wantByTier, countsByTier[podID], "pod %q", podID)
	}
	assert.Equal(t, map[string]int{podA: 2, podB: 4, podC: 3}, counts)
	assert.Equal(t, map[string]map[string]int{
		podA: {"gpu": 2, "cpu": 1},
		podB: {"gpu": 4},
		podC: {attrprefix.SpeculativeTierKey: 2},
	}, countsByTier)
}

func BenchmarkMatchedBlockCountsAllPods(b *testing.B) {
	const (
		blockCount = 1034
		podCount   = 40
		rankCount  = 8
	)
	keys := make([]kvblock.BlockHash, blockCount)
	keyToPods := make(map[kvblock.BlockHash][]kvblock.PodEntry, blockCount)
	podIDs := make([]string, podCount)
	for pod := range podCount {
		podIDs[pod] = fmt.Sprintf("10.0.0.%d:8080", pod)
	}
	for block := range blockCount {
		key := kvblock.BlockHash(block + 1)
		keys[block] = key
		entries := make([]kvblock.PodEntry, 0, podCount*rankCount*2)
		for _, podID := range podIDs {
			for range rankCount {
				entries = append(entries,
					kvblock.PodEntry{PodIdentifier: podID, DeviceTier: "gpu"},
					kvblock.PodEntry{PodIdentifier: podID, DeviceTier: "cpu"},
				)
			}
		}
		keyToPods[key] = entries
	}

	b.Run("per-endpoint", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			counts := make(map[string]int, podCount)
			countsByTier := make(map[string]map[string]int, podCount)
			for _, podID := range podIDs {
				counts[podID], countsByTier[podID] = matchedBlockCounts(keys, keyToPods, podID)
			}
			benchmarkCounts = counts
			benchmarkCountsByTier = countsByTier
		}
	})

	b.Run("all-pods", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkCounts, benchmarkCountsByTier = matchedBlockCountsByPod(keys, keyToPods)
		}
	})
}

func BenchmarkPrecisePrefixLookupPipeline(b *testing.B) {
	const (
		blockCount = 1034
		podCount   = 40
		rankCount  = 8
	)
	ctx := context.Background()
	cfg := kvblock.DefaultInMemoryIndexConfig()
	cfg.Size = blockCount + 1
	cfg.PodCacheSize = podCount * rankCount
	index, err := kvblock.NewInMemoryIndex(cfg)
	require.NoError(b, err)

	keys := make([]kvblock.BlockHash, blockCount)
	entries := make([]kvblock.PodEntry, 0, podCount*rankCount)
	podIDs := make([]string, podCount)
	podSet := sets.New[string]()
	for pod := range podCount {
		podID := fmt.Sprintf("10.0.0.%d:8080", pod)
		podIDs[pod] = podID
		podSet.Insert(podID)
		for rank := range rankCount {
			entries = append(entries, kvblock.PodEntry{
				PodIdentifier: podID,
				DeviceTier:    "gpu",
				HasGroup:      true,
				GroupIdx:      kvblock.GroupID(rank),
			})
		}
	}
	for block := range blockCount {
		key := kvblock.BlockHash(block + 1)
		keys[block] = key
		require.NoError(b, index.Add(ctx, nil, []kvblock.BlockHash{key}, entries))
	}

	scorer := &kvcache.LongestPrefixScorer{MediumWeights: map[string]float64{"gpu": 1}}
	fastLookup := any(index).(kvblock.PrefixMatchLookup)
	wrappedFastLookup := any(kvblock.NewTracedIndex(kvblock.NewInstrumentedIndex(index))).(kvblock.PrefixMatchLookup)
	b.ResetTimer()

	b.Run("materialized-lookup-score-and-endpoints", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			keyToPods, lookupErr := index.Lookup(ctx, keys, podSet)
			require.NoError(b, lookupErr)
			benchmarkScores, lookupErr = scorer.Score(ctx, keys, keyToPods)
			require.NoError(b, lookupErr)
			counts := make(map[string]int, podCount)
			countsByTier := make(map[string]map[string]int, podCount)
			for _, podID := range podIDs {
				counts[podID], countsByTier[podID] = matchedBlockCounts(keys, keyToPods, podID)
			}
			benchmarkCounts = counts
			benchmarkCountsByTier = countsByTier
		}
	})
	b.Run("streaming-prefix-match", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkMatches, err = fastLookup.LookupPrefixMatches(
				ctx, keys, podSet, scorer.MediumWeights, attrprefix.SpeculativeTierKey,
			)
			require.NoError(b, err)
		}
	})
	b.Run("wrapped-streaming-prefix-match", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkMatches, err = wrappedFastLookup.LookupPrefixMatches(
				ctx, keys, podSet, scorer.MediumWeights, attrprefix.SpeculativeTierKey,
			)
			require.NoError(b, err)
		}
	})
}
