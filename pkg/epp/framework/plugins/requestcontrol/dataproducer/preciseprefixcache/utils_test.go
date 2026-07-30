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
	"math/rand"
	"slices"
	"testing"

	"github.com/llm-d/llm-d-router/pkg/kvcache"
	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"

	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
)

// podStats returns the stats for podID with zero values for absent pods, the
// same defaulting produceFromBlockKeys applies per endpoint.
func podStats(stats map[string]*endpointStats, podID string) (float64, int, map[string]int) {
	if st := stats[podID]; st != nil {
		return st.weightedScore, st.cachedBlocks, st.cachedBlocksByTier
	}
	return 0, 0, map[string]int{}
}

func TestEndpointPrefixStats_CachedBlocks(t *testing.T) {
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
			_, cachedBlocks, _ := podStats(endpointPrefixStats(keys, tt.keyToPods, nil), tt.podID)
			assert.Equal(t, tt.want, cachedBlocks)
		})
	}
}

func TestEndpointPrefixStats_CachedBlocksByTier(t *testing.T) {
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
			name: "tier map dies before pod presence does",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {gpu(podA)}, 2: {cpu(podA)}, 3: {cpu(podA)},
			},
			podID: podA,
			want:  map[string]int{"gpu": 1},
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
			_, cachedBlocks, byTier := podStats(endpointPrefixStats(keys, tt.keyToPods, nil), tt.podID)
			assert.NotNil(t, byTier)
			assert.Equal(t, tt.want, byTier)
			// Each tier's contiguous count never exceeds the any-tier count.
			for tier, count := range byTier {
				assert.LessOrEqual(t, count, cachedBlocks, "tier %q", tier)
			}
		})
	}
}

func TestEndpointPrefixStats_WeightedScore(t *testing.T) {
	const podA = "10.0.0.1:8000"
	keys := []kvblock.BlockHash{1, 2, 3}
	weights := map[string]float64{"gpu": 1.0, "cpu": 0.8}

	gpu := kvblock.PodEntry{PodIdentifier: podA, DeviceTier: "gpu"}
	cpu := kvblock.PodEntry{PodIdentifier: podA, DeviceTier: "cpu"}
	disk := kvblock.PodEntry{PodIdentifier: podA, DeviceTier: "disk"}
	speculative := kvblock.PodEntry{PodIdentifier: podA, Speculative: true}

	tests := []struct {
		name      string
		keyToPods map[kvblock.BlockHash][]kvblock.PodEntry
		want      float64
	}{
		{
			name: "per-key max weight across tiers",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {cpu, gpu}, 2: {cpu}, 3: {gpu},
			},
			want: 1.0 + 0.8 + 1.0,
		},
		{
			name: "unknown tier falls back to 1.0",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {disk}, 2: {cpu},
			},
			want: 1.0 + 0.8,
		},
		{
			name: "speculative entry carries no tier, weight 1.0",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {speculative},
			},
			want: 1.0,
		},
		{
			name: "gap ends the weighted run",
			keyToPods: map[kvblock.BlockHash][]kvblock.PodEntry{
				1: {gpu}, 3: {gpu},
			},
			want: 1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			weightedScore, _, _ := podStats(endpointPrefixStats(keys, tt.keyToPods, weights), podA)
			assert.Equal(t, tt.want, weightedScore)
		})
	}
}

// referenceMatchedBlockCount is the multi-pass unweighted contiguous count the
// single-pass endpointPrefixStats replaces, kept as the differential oracle.
func referenceMatchedBlockCount(keys []kvblock.BlockHash, keyToPods map[kvblock.BlockHash][]kvblock.PodEntry, podID string) int {
	count := 0
	for _, key := range keys {
		if !slices.ContainsFunc(keyToPods[key], func(e kvblock.PodEntry) bool { return e.PodIdentifier == podID }) {
			break
		}
		count++
	}
	return count
}

// referenceMatchedBlockCountByTier is the multi-pass per-tier contiguous count
// the single-pass endpointPrefixStats replaces, kept as the differential oracle.
func referenceMatchedBlockCountByTier(keys []kvblock.BlockHash, keyToPods map[kvblock.BlockHash][]kvblock.PodEntry, podID string) map[string]int {
	counts := map[string]int{}
	var alive sets.Set[string]
	for _, key := range keys {
		tiersAtKey := sets.New[string]()
		for _, e := range keyToPods[key] {
			if e.PodIdentifier == podID {
				if e.Speculative {
					tiersAtKey.Insert(attrprefix.SpeculativeTierKey)
				} else {
					tiersAtKey.Insert(e.DeviceTier)
				}
			}
		}
		if alive == nil {
			alive = tiersAtKey
		} else {
			alive = alive.Intersection(tiersAtKey)
		}
		if alive.Len() == 0 {
			break
		}
		for tier := range alive {
			counts[tier]++
		}
	}
	return counts
}

// Randomized differential check: the single-pass walk must agree with
// kvcache.LongestPrefixScorer on the weighted score and with the reference
// per-pod counters on cachedBlocks and cachedBlocksByTier.
func TestEndpointPrefixStats_DifferentialAgainstReferences(t *testing.T) {
	pods := []string{"p0", "p1", "p2", "p3"}
	tiers := []string{"gpu", "cpu", "disk", ""}
	weightSets := []map[string]float64{
		nil,
		{"gpu": 1.0, "cpu": 0.8},
		{"gpu": 2.0, "cpu": 0.5, "disk": 0.1},
	}

	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 500; trial++ {
		numKeys := rng.Intn(9)
		keys := make([]kvblock.BlockHash, numKeys)
		keyToPods := map[kvblock.BlockHash][]kvblock.PodEntry{}
		for i := range keys {
			keys[i] = kvblock.BlockHash(1000 + i)
			var entries []kvblock.PodEntry
			for _, pod := range pods {
				for _, tier := range tiers {
					if rng.Intn(3) == 0 {
						entries = append(entries, kvblock.PodEntry{
							PodIdentifier: pod,
							DeviceTier:    tier,
							Speculative:   rng.Intn(4) == 0,
						})
					}
				}
			}
			rng.Shuffle(len(entries), func(a, b int) { entries[a], entries[b] = entries[b], entries[a] })
			keyToPods[keys[i]] = entries
		}
		mediumWeights := weightSets[rng.Intn(len(weightSets))]

		stats := endpointPrefixStats(keys, keyToPods, mediumWeights)

		wantScores, err := (&kvcache.LongestPrefixScorer{MediumWeights: mediumWeights}).Score(
			context.Background(), keys, keyToPods)
		require.NoError(t, err)

		gotScores := make(map[string]float64, len(stats))
		for pod, st := range stats {
			gotScores[pod] = st.weightedScore
		}
		assert.Equal(t, wantScores, gotScores, "trial %d", trial)

		for _, pod := range pods {
			label := fmt.Sprintf("trial %d pod %s", trial, pod)
			weightedScore, cachedBlocks, byTier := podStats(stats, pod)
			assert.Equal(t, wantScores[pod], weightedScore, label)
			assert.Equal(t, referenceMatchedBlockCount(keys, keyToPods, pod), cachedBlocks, label)
			assert.Equal(t, referenceMatchedBlockCountByTier(keys, keyToPods, pod), byTier, label)
		}
	}
}

// benchFixture builds a warm-lookup shape: every pod holds every key on the
// gpu tier, so both walks run their full length (the worst case).
func benchFixture(numKeys, numPods int) ([]kvblock.BlockHash, map[kvblock.BlockHash][]kvblock.PodEntry, []string) {
	keys := make([]kvblock.BlockHash, numKeys)
	keyToPods := make(map[kvblock.BlockHash][]kvblock.PodEntry, numKeys)
	podIDs := make([]string, numPods)
	for p := range podIDs {
		podIDs[p] = fmt.Sprintf("10.0.0.%d:8000", p)
	}
	for i := range keys {
		keys[i] = kvblock.BlockHash(1000 + i)
		entries := make([]kvblock.PodEntry, numPods)
		for p, pod := range podIDs {
			entries[p] = kvblock.PodEntry{PodIdentifier: pod, DeviceTier: "gpu"}
		}
		keyToPods[keys[i]] = entries
	}
	return keys, keyToPods, podIDs
}

func BenchmarkEndpointPrefixStatsSinglePass(b *testing.B) {
	keys, keyToPods, _ := benchFixture(625, 10)
	weights := map[string]float64{"gpu": 1.0, "cpu": 0.8}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		endpointPrefixStats(keys, keyToPods, weights)
	}
}

func BenchmarkEndpointPrefixStatsMultiPassReference(b *testing.B) {
	keys, keyToPods, podIDs := benchFixture(625, 10)
	weights := map[string]float64{"gpu": 1.0, "cpu": 0.8}
	scorer := &kvcache.LongestPrefixScorer{MediumWeights: weights}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = scorer.Score(context.Background(), keys, keyToPods)
		for _, pod := range podIDs {
			_ = referenceMatchedBlockCount(keys, keyToPods, pod)
			_ = referenceMatchedBlockCountByTier(keys, keyToPods, pod)
		}
	}
}
