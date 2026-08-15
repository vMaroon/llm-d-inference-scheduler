/*
Copyright 2025 The llm-d Authors.

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

package kvblock_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/llm-d/llm-d-router/pkg/kvcache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	. "github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
)

var (
	benchmarkPodsPerKey map[BlockHash][]PodEntry
	benchmarkMatches    map[string]PrefixMatch
	benchmarkScores     map[string]float64
)

// createInMemoryIndexForTesting creates a new InMemoryIndex for testing.
func createInMemoryIndexForTesting(t *testing.T) Index {
	t.Helper()
	cfg := DefaultInMemoryIndexConfig()
	// Set PodCacheSize to 500 to accommodate testConcurrentOperations
	// (100 goroutines * 4 pods each = 400 max concurrent pods)
	cfg.PodCacheSize = 500
	index, err := NewInMemoryIndex(cfg)
	require.NoError(t, err)
	return index
}

func TestInMemoryIndexBehavior(t *testing.T) {
	testCommonIndexBehavior(t, createInMemoryIndexForTesting)
}

func TestInMemoryIndexLookupHonorsCancelledContext(t *testing.T) {
	index := createInMemoryIndexForTesting(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := index.Lookup(ctx, []BlockHash{1}, nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestInMemoryIndexLookupPrefixMatches(t *testing.T) {
	ctx := logging.NewTestLoggerIntoContext(t.Context())
	index := createInMemoryIndexForTesting(t)
	lookup, ok := index.(PrefixMatchLookup)
	require.True(t, ok)

	const (
		podA = "10.0.0.1:8000"
		podB = "10.0.0.2:8000"
	)
	keys := []BlockHash{1, 2, 3}
	require.NoError(t, index.Add(ctx, nil, []BlockHash{keys[0]}, []PodEntry{
		{PodIdentifier: podA, DeviceTier: "gpu"},
		{PodIdentifier: podA, DeviceTier: "cpu"},
		{PodIdentifier: podB, DeviceTier: "gpu"},
	}))
	require.NoError(t, index.Add(ctx, nil, []BlockHash{keys[1]}, []PodEntry{
		{PodIdentifier: podA, DeviceTier: "gpu"},
		{PodIdentifier: podB, DeviceTier: "cpu"},
	}))
	require.NoError(t, index.Add(ctx, nil, []BlockHash{keys[2]}, []PodEntry{
		{PodIdentifier: podB, DeviceTier: "cpu"},
	}))

	matches, err := lookup.LookupPrefixMatches(ctx, keys, nil, map[string]float64{"gpu": 1, "cpu": 0.5}, "speculative")
	require.NoError(t, err)
	assert.Equal(t, map[string]PrefixMatch{
		podA: {Score: 2, CachedBlocks: 2, CachedBlocksByTier: map[string]int{"gpu": 2, "cpu": 1}},
		podB: {Score: 2, CachedBlocks: 3, CachedBlocksByTier: map[string]int{"gpu": 1}},
	}, matches)

	matches, err = lookup.LookupPrefixMatches(ctx, keys, sets.New(podA), map[string]float64{"gpu": 1, "cpu": 0.5}, "speculative")
	require.NoError(t, err)
	assert.Equal(t, map[string]PrefixMatch{
		podA: {Score: 2, CachedBlocks: 2, CachedBlocksByTier: map[string]int{"gpu": 2, "cpu": 1}},
	}, matches)
}

func TestInMemoryIndexLookupPrefixMatchesRefreshesCollapsedRoutes(t *testing.T) {
	ctx := logging.NewTestLoggerIntoContext(t.Context())
	cfg := DefaultInMemoryIndexConfig()
	cfg.Size = 4
	cfg.PodCacheSize = 16
	index, err := NewInMemoryIndex(cfg)
	require.NoError(t, err)
	lookup := any(index).(PrefixMatchLookup)

	const pod = "10.0.0.1:8000"
	key := BlockHash(1)
	gpu0 := PodEntry{PodIdentifier: pod, DeviceTier: "gpu", HasGroup: true, GroupIdx: 0}
	gpu1 := PodEntry{PodIdentifier: pod, DeviceTier: "gpu", HasGroup: true, GroupIdx: 1}
	cpu0 := PodEntry{PodIdentifier: pod, DeviceTier: "cpu", HasGroup: true, GroupIdx: 0}
	cpu1 := PodEntry{PodIdentifier: pod, DeviceTier: "cpu", HasGroup: true, GroupIdx: 1}

	require.NoError(t, index.Add(ctx, []BlockHash{10}, []BlockHash{key}, []PodEntry{gpu0, gpu1}))
	matches, err := lookup.LookupPrefixMatches(ctx, []BlockHash{key}, nil,
		map[string]float64{"gpu": 1, "cpu": 0.5}, "speculative")
	require.NoError(t, err)
	assert.Equal(t, PrefixMatch{Score: 1, CachedBlocks: 1, CachedBlocksByTier: map[string]int{"gpu": 1}}, matches[pod])

	// Adding another tier invalidates the immutable routing snapshot. Multiple
	// group entries collapse into one pod/tier route for request-time lookup.
	require.NoError(t, index.Add(ctx, []BlockHash{11}, []BlockHash{key}, []PodEntry{cpu0, cpu1}))
	matches, err = lookup.LookupPrefixMatches(ctx, []BlockHash{key}, nil,
		map[string]float64{"gpu": 1, "cpu": 0.5}, "speculative")
	require.NoError(t, err)
	assert.Equal(t, PrefixMatch{
		Score: 1, CachedBlocks: 1, CachedBlocksByTier: map[string]int{"gpu": 1, "cpu": 1},
	}, matches[pod])

	// Removing one group keeps the collapsed tier route alive.
	require.NoError(t, index.Evict(ctx, 11, EngineKey, []PodEntry{cpu0}))
	matches, err = lookup.LookupPrefixMatches(ctx, []BlockHash{key}, nil,
		map[string]float64{"gpu": 1, "cpu": 0.5}, "speculative")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"gpu": 1, "cpu": 1}, matches[pod].CachedBlocksByTier)

	// Removing the final group removes the tier from the next snapshot.
	require.NoError(t, index.Evict(ctx, 11, EngineKey, []PodEntry{cpu1}))
	matches, err = lookup.LookupPrefixMatches(ctx, []BlockHash{key}, nil,
		map[string]float64{"gpu": 1, "cpu": 0.5}, "speculative")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"gpu": 1}, matches[pod].CachedBlocksByTier)
}

func TestInMemoryIndexPrefixLookupDoesNotRetainLRUEviction(t *testing.T) {
	ctx := logging.NewTestLoggerIntoContext(t.Context())
	index, err := NewInMemoryIndex(&InMemoryIndexConfig{Size: 1, PodCacheSize: 4})
	require.NoError(t, err)
	lookup := any(index).(PrefixMatchLookup)
	pod := PodEntry{PodIdentifier: "10.0.0.1:8000", DeviceTier: "gpu"}

	require.NoError(t, index.Add(ctx, nil, []BlockHash{1}, []PodEntry{pod}))
	_, err = lookup.LookupPrefixMatches(ctx, []BlockHash{1}, nil, map[string]float64{"gpu": 1}, "speculative")
	require.NoError(t, err)
	require.NoError(t, index.Add(ctx, nil, []BlockHash{2}, []PodEntry{pod}))

	evicted, err := lookup.LookupPrefixMatches(ctx, []BlockHash{1}, nil, map[string]float64{"gpu": 1}, "speculative")
	require.NoError(t, err)
	assert.Empty(t, evicted)
	present, err := lookup.LookupPrefixMatches(ctx, []BlockHash{2}, nil, map[string]float64{"gpu": 1}, "speculative")
	require.NoError(t, err)
	assert.Contains(t, present, pod.PodIdentifier)
}

func BenchmarkInMemoryIndexPrefixMatches(b *testing.B) {
	const (
		blockCount = 1034
		podCount   = 40
		rankCount  = 8
	)
	ctx := context.Background()
	cfg := DefaultInMemoryIndexConfig()
	cfg.Size = blockCount + 1
	cfg.PodCacheSize = podCount * rankCount
	index, err := NewInMemoryIndex(cfg)
	require.NoError(b, err)

	keys := make([]BlockHash, blockCount)
	entries := make([]PodEntry, 0, podCount*rankCount)
	podSet := sets.New[string]()
	for pod := range podCount {
		podID := fmt.Sprintf("10.0.0.%d:8080", pod)
		podSet.Insert(podID)
		for rank := range rankCount {
			entries = append(entries, PodEntry{
				PodIdentifier: podID,
				DeviceTier:    "gpu",
				HasGroup:      true,
				GroupIdx:      GroupID(rank),
			})
		}
	}
	for block := range blockCount {
		key := BlockHash(block + 1)
		keys[block] = key
		require.NoError(b, index.Add(ctx, nil, []BlockHash{key}, entries))
	}

	scorer := &kvcache.LongestPrefixScorer{MediumWeights: map[string]float64{"gpu": 1}}
	fastLookup := any(index).(PrefixMatchLookup)
	b.ResetTimer()

	b.Run("materialized-lookup-and-score", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkPodsPerKey, err = index.Lookup(ctx, keys, podSet)
			require.NoError(b, err)
			benchmarkScores, err = scorer.Score(ctx, keys, benchmarkPodsPerKey)
			require.NoError(b, err)
		}
	})
	b.Run("streaming-prefix-match", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkMatches, err = fastLookup.LookupPrefixMatches(
				ctx, keys, podSet, scorer.MediumWeights, "speculative",
			)
			require.NoError(b, err)
		}
	})
}

func TestInMemoryIndexSize(t *testing.T) {
	ctx := logging.NewTestLoggerIntoContext(t.Context())

	// Test with small size to verify eviction
	cfg := &InMemoryIndexConfig{
		Size:         2, // Only 2 keys max
		PodCacheSize: 1, // Pod cache size doesn't matter for this test
	}

	index, err := NewInMemoryIndex(cfg)
	require.NoError(t, err)

	// Add first key
	engineKey1 := BlockHash(72735753)
	requestKey1 := BlockHash(79215516)
	err = index.Add(ctx, []BlockHash{engineKey1}, []BlockHash{requestKey1}, []PodEntry{{PodIdentifier: "pod1", DeviceTier: "gpu"}})
	require.NoError(t, err)

	// Add second key
	engineKey2 := BlockHash(41341092)
	requestKey2 := BlockHash(12871930)
	err = index.Add(ctx, []BlockHash{engineKey2}, []BlockHash{requestKey2}, []PodEntry{{PodIdentifier: "pod2", DeviceTier: "gpu"}})
	require.NoError(t, err)

	// Add third key - should evict the first one due to LRU
	engineKey3 := BlockHash(34012886)
	requestKey3 := BlockHash(69914638)
	err = index.Add(ctx, []BlockHash{engineKey3}, []BlockHash{requestKey3}, []PodEntry{{PodIdentifier: "pod3", DeviceTier: "cpu"}})
	require.NoError(t, err)

	// Lookup should only return the last two keys
	podsPerKey, err := index.Lookup(ctx, []BlockHash{requestKey1, requestKey2, requestKey3}, nil)
	require.NoError(t, err)

	assert.Len(t, podsPerKey, 2) // Only key2 and key3 should be present
	assert.Len(t, podsPerKey[requestKey2], 1)
	assert.Len(t, podsPerKey[requestKey3], 1)
	assert.Contains(t, podsPerKey[requestKey2], PodEntry{PodIdentifier: "pod2", DeviceTier: "gpu"})
	assert.Contains(t, podsPerKey[requestKey3], PodEntry{PodIdentifier: "pod3", DeviceTier: "cpu"})
}

func TestInMemoryIndexPodCacheSize(t *testing.T) {
	ctx := logging.NewTestLoggerIntoContext(t.Context())

	// Test with small limits to verify enforcement
	cfg := &InMemoryIndexConfig{
		Size:         1, // Only 1 key max
		PodCacheSize: 2, // Only 2 pods per key
	}

	index, err := NewInMemoryIndex(cfg)
	require.NoError(t, err)

	// Test PodCacheSize limit: add more pods than the limit for one key
	engineKey := BlockHash(28409753)
	requestKey := BlockHash(51374550)
	pods := []PodEntry{
		{PodIdentifier: "pod1", DeviceTier: "gpu"},
		{PodIdentifier: "pod2", DeviceTier: "gpu"},
		{PodIdentifier: "pod3", DeviceTier: "cpu"}, // This should evict pod1 due to LRU
	}

	err = index.Add(ctx, []BlockHash{engineKey}, []BlockHash{requestKey}, pods)
	require.NoError(t, err)

	// Lookup should only return 2 pods (pod2 and pod3), pod1 should be evicted
	podsPerKey, err := index.Lookup(ctx, []BlockHash{requestKey}, nil)
	require.NoError(t, err)
	assert.Len(t, podsPerKey, 1)
	assert.Len(t, podsPerKey[requestKey], 2, "Should only have 2 pods due to PodCacheSize limit")
	assert.Contains(t, podsPerKey[requestKey], PodEntry{PodIdentifier: "pod2", DeviceTier: "gpu"})
	assert.Contains(t, podsPerKey[requestKey], PodEntry{PodIdentifier: "pod3", DeviceTier: "cpu"})
}

// TestSpeculativeAnnotation tests that speculative and confirmed PodEntries
// are treated as separate entries due to the Speculative field, and that
// evicting one does not affect the other.
func TestSpeculativeAnnotation(t *testing.T) {
	ctx := logging.NewTestLoggerIntoContext(t.Context())
	index := createInMemoryIndexForTesting(t)

	requestKey := BlockHash(22222222)

	t.Run("SpeculativeAddWithNilEngineKeys", func(t *testing.T) {
		// Add a speculative entry with nil engineKeys (no engineKey mapping)
		speculativePod := PodEntry{PodIdentifier: "10.0.0.1:8080", Speculative: true}
		err := index.Add(ctx, nil, []BlockHash{requestKey}, []PodEntry{speculativePod})
		require.NoError(t, err)

		// Lookup should return the speculative pod
		podsPerKey, err := index.Lookup(ctx, []BlockHash{requestKey}, nil)
		require.NoError(t, err)
		assert.Len(t, podsPerKey[requestKey], 1)
		assert.Contains(t, podsPerKey[requestKey], speculativePod)
	})

	t.Run("SpeculativeAndConfirmedCoexist", func(t *testing.T) {
		// Add a confirmed entry for the same pod (with engineKey mapping, same requestKey)
		confirmedEngineKey := BlockHash(33333333)
		confirmedPod := PodEntry{PodIdentifier: "10.0.0.1:8080"}
		err := index.Add(ctx, []BlockHash{confirmedEngineKey}, []BlockHash{requestKey}, []PodEntry{confirmedPod})
		require.NoError(t, err)

		// Both speculative and confirmed should coexist
		podsPerKey, err := index.Lookup(ctx, []BlockHash{requestKey}, nil)
		require.NoError(t, err)
		assert.Len(t, podsPerKey[requestKey], 2)
		assert.Contains(t, podsPerKey[requestKey],
			PodEntry{PodIdentifier: "10.0.0.1:8080", Speculative: true})
		assert.Contains(t, podsPerKey[requestKey], PodEntry{PodIdentifier: "10.0.0.1:8080"})
	})

	t.Run("SpeculativeEvictPreservesConfirmed", func(t *testing.T) {
		// Evict the speculative entry using requestKey directly (no engineKey mapping exists).
		speculativePod := PodEntry{PodIdentifier: "10.0.0.1:8080", Speculative: true}
		err := index.Evict(ctx, requestKey, RequestKey, []PodEntry{speculativePod})
		require.NoError(t, err)

		// Confirmed entry should remain
		podsPerKey, err := index.Lookup(ctx, []BlockHash{requestKey}, nil)
		require.NoError(t, err)
		assert.Len(t, podsPerKey[requestKey], 1)
		assert.Contains(t, podsPerKey[requestKey], PodEntry{PodIdentifier: "10.0.0.1:8080"})
		// Speculative should be gone
		assert.NotContains(t, podsPerKey[requestKey],
			PodEntry{PodIdentifier: "10.0.0.1:8080", Speculative: true})
	})
}

// TestSpeculativeEvictThenEmpty tests that when only a speculative entry exists
// (added with nil engineKeys) and is evicted, the key is cleaned up properly.
func TestSpeculativeEvictThenEmpty(t *testing.T) {
	ctx := logging.NewTestLoggerIntoContext(t.Context())
	index := createInMemoryIndexForTesting(t)

	requestKey := BlockHash(44444444)
	speculativePod := PodEntry{PodIdentifier: "10.0.0.2:8080", Speculative: true}

	// Add speculative entry with nil engineKeys (no engineKey mapping)
	err := index.Add(ctx, nil, []BlockHash{requestKey}, []PodEntry{speculativePod})
	require.NoError(t, err)

	// Verify it exists
	podsPerKey, err := index.Lookup(ctx, []BlockHash{requestKey}, nil)
	require.NoError(t, err)
	assert.Len(t, podsPerKey[requestKey], 1)

	// Evict speculative entry using requestKey directly
	err = index.Evict(ctx, requestKey, RequestKey, []PodEntry{speculativePod})
	require.NoError(t, err)

	// Lookup should return empty
	podsPerKey, err = index.Lookup(ctx, []BlockHash{requestKey}, nil)
	require.NoError(t, err)
	assert.Empty(t, podsPerKey[requestKey])
}

// TestAddWithNilEngineKeys tests that Add() with nil engineKeys only creates
// requestKey -> PodEntry mappings without engineKey -> requestKey mappings.
func TestAddWithNilEngineKeys(t *testing.T) {
	ctx := logging.NewTestLoggerIntoContext(t.Context())
	index := createInMemoryIndexForTesting(t)

	requestKey := BlockHash(55555555)
	pod := PodEntry{PodIdentifier: "10.0.0.3:8080", Speculative: true}

	// Add with nil engineKeys
	err := index.Add(ctx, nil, []BlockHash{requestKey}, []PodEntry{pod})
	require.NoError(t, err)

	// Lookup by requestKey should work
	podsPerKey, err := index.Lookup(ctx, []BlockHash{requestKey}, nil)
	require.NoError(t, err)
	assert.Len(t, podsPerKey[requestKey], 1)
	assert.Contains(t, podsPerKey[requestKey], pod)

	// GetRequestKey should NOT find a mapping (no engineKey was stored)
	_, err = index.GetRequestKey(ctx, requestKey)
	assert.Error(t, err, "GetRequestKey should fail since no engineKey mapping was created")
}
