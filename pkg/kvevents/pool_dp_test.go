// Copyright 2026 The llm-d Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kvevents

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
)

// newDPTestPool mirrors newTestPool with rank-granular identity enabled:
// index entries key on <host>:(servingPortBase + rank % dataParallelSizeLocal).
func newDPTestPool(t *testing.T, blockSize, dataParallelSizeLocal, servingPortBase int) (
	*Pool, kvblock.Index, kvblock.TokenProcessor,
) {
	t.Helper()

	idx, err := kvblock.NewInMemoryIndex(kvblock.DefaultInMemoryIndexConfig())
	require.NoError(t, err)

	tp, err := kvblock.NewChunkedTokenDatabase(&kvblock.TokenProcessorConfig{
		BlockSizeTokens: blockSize,
		HashSeed:        "test",
	})
	require.NoError(t, err)

	cfg := DefaultConfig()
	cfg.DataParallelSizeLocal = dataParallelSizeLocal
	cfg.ServingPortBase = servingPortBase
	pool := NewPool(cfg, idx, tp, nil)
	return pool, idx, tp
}

func rankPtr(rank int) *int { return &rank }

// storedDPBatch wraps a single BlockStoredEvent in a batch annotated with the
// given global data-parallel rank.
func storedDPBatch(rank int, tokens []uint32, engineKeys []uint64) *EventBatch {
	return &EventBatch{
		Events: []GenericEvent{
			&BlockStoredEvent{BlockHashes: engineKeys, Tokens: tokens, ParentHash: 0},
		},
		DataParallelRank: rankPtr(rank),
	}
}

// removedDPBatch wraps a single BlockRemovedEvent in a batch annotated with
// the given global data-parallel rank.
func removedDPBatch(rank int, engineKeys []uint64) *EventBatch {
	return &EventBatch{
		Events:           []GenericEvent{&BlockRemovedEvent{BlockHashes: engineKeys}},
		DataParallelRank: rankPtr(rank),
	}
}

// TestPool_DataParallelIdentity_RankGranularIdentifiers verifies that with
// servingPortBase set, batches from four global ranks across two pods index
// under four distinct serving-endpoint identifiers, and that GlobalDPRank
// resolves each identifier back to its global rank.
func TestPool_DataParallelIdentity_RankGranularIdentifiers(t *testing.T) {
	ctx := logging.NewTestLoggerIntoContext(context.Background())
	pool, idx, tp := newDPTestPool(t, 16, 2, 18000)

	tokens := makeTokens(16)
	engineKeys := makeEngineKeys(1, 700)

	// Global ranks 0-1 live on pod 10.1.0.1, ranks 2-3 on pod 10.1.0.2; the
	// topic-derived identifier carries each pod's serving host:port.
	batches := []struct {
		rank  int
		topic string
	}{
		{rank: 0, topic: "10.1.0.1:8000"},
		{rank: 1, topic: "10.1.0.1:8000"},
		{rank: 2, topic: "10.1.0.2:8000"},
		{rank: 3, topic: "10.1.0.2:8000"},
	}
	for _, b := range batches {
		pool.processEventBatch(ctx, storedDPBatch(b.rank, tokens, engineKeys), b.topic, "test-model")
	}

	requestKeys, err := tp.TokensToKVBlockKeys(kvblock.EmptyBlockHash, tokens, "test-model", nil)
	require.NoError(t, err)
	require.Len(t, requestKeys, 1)

	result, err := idx.Lookup(ctx, requestKeys, nil)
	require.NoError(t, err)
	entries := result[requestKeys[0]]
	identifiers := make([]string, 0, len(entries))
	for _, entry := range entries {
		identifiers = append(identifiers, entry.PodIdentifier)
	}
	assert.ElementsMatch(t, []string{
		"10.1.0.1:18000", "10.1.0.1:18001", "10.1.0.2:18000", "10.1.0.2:18001",
	}, identifiers, "each rank must index under its own serving endpoint")

	wantRanks := map[string]int{
		"10.1.0.1:18000": 0,
		"10.1.0.1:18001": 1,
		"10.1.0.2:18000": 2,
		"10.1.0.2:18001": 3,
	}
	for identifier, want := range wantRanks {
		rank, ok := pool.GlobalDPRank(identifier)
		require.True(t, ok, "rank lookup must resolve %s", identifier)
		assert.Equal(t, want, rank, "identifier %s", identifier)
	}
	_, ok := pool.GlobalDPRank("10.1.0.9:18000")
	assert.False(t, ok, "unseen identifier must not resolve")
}

// TestPool_DataParallelDedup_IndependentAcrossRanks verifies that two ranks
// storing identical block hashes do not dedup-suppress each other: one rank's
// remove evicts that rank's entry immediately while the other rank's entry
// survives.
func TestPool_DataParallelDedup_IndependentAcrossRanks(t *testing.T) {
	ctx := logging.NewTestLoggerIntoContext(context.Background())
	pool, idx, tp := newDPTestPool(t, 16, 2, 18000)

	tokens := makeTokens(16)
	engineKeys := makeEngineKeys(1, 710)

	// Both ranks of the same pod announce the same hashes.
	pool.processEventBatch(ctx, storedDPBatch(0, tokens, engineKeys), "10.1.0.1:8000", "test-model")
	pool.processEventBatch(ctx, storedDPBatch(1, tokens, engineKeys), "10.1.0.1:8000", "test-model")

	requestKeys, err := tp.TokensToKVBlockKeys(kvblock.EmptyBlockHash, tokens, "test-model", nil)
	require.NoError(t, err)
	require.Len(t, requestKeys, 1)

	// Rank 0's remove must be forwarded (not suppressed by rank 1's store):
	// only rank 1's entry remains.
	pool.processEventBatch(ctx, removedDPBatch(0, engineKeys), "10.1.0.1:8000", "test-model")

	result, err := idx.Lookup(ctx, requestKeys, nil)
	require.NoError(t, err)
	entries := result[requestKeys[0]]
	require.Len(t, entries, 1, "rank 0's remove must evict only rank 0's entry")
	assert.Equal(t, "10.1.0.1:18001", entries[0].PodIdentifier)
}

// TestPool_DataParallel_ServingPortBaseUnset_KeepsPodIdentity pins the
// default-config behavior with rank-annotated batches: identifiers pass
// through untouched, GlobalDPRank resolves nothing, and reference counts
// aggregate across ranks (a block stays resident on the pod until every rank
// has removed it).
func TestPool_DataParallel_ServingPortBaseUnset_KeepsPodIdentity(t *testing.T) {
	ctx := logging.NewTestLoggerIntoContext(context.Background())
	pool, idx, tp := newTestPool(t, 16)

	tokens := makeTokens(16)
	engineKeys := makeEngineKeys(1, 720)

	pool.processEventBatch(ctx, storedDPBatch(0, tokens, engineKeys), "pod-dp-legacy", "test-model")
	pool.processEventBatch(ctx, storedDPBatch(1, tokens, engineKeys), "pod-dp-legacy", "test-model")

	requestKeys, err := tp.TokensToKVBlockKeys(kvblock.EmptyBlockHash, tokens, "test-model", nil)
	require.NoError(t, err)
	require.Len(t, requestKeys, 1)

	result, err := idx.Lookup(ctx, requestKeys, nil)
	require.NoError(t, err)
	entries := result[requestKeys[0]]
	require.Len(t, entries, 1)
	assert.Equal(t, "pod-dp-legacy", entries[0].PodIdentifier,
		"without servingPortBase the topic-derived identifier passes through")

	_, ok := pool.GlobalDPRank("pod-dp-legacy")
	assert.False(t, ok, "pod-granular identity records no rank")

	// First remove is suppressed: under pod-granular identity the second
	// rank's store still references the hash.
	pool.processEventBatch(ctx, removedDPBatch(0, engineKeys), "pod-dp-legacy", "test-model")
	result, err = idx.Lookup(ctx, requestKeys, nil)
	require.NoError(t, err)
	require.Len(t, result[requestKeys[0]], 1, "first remove must be suppressed while another rank holds the block")

	// Second remove releases the last reference and evicts for real.
	pool.processEventBatch(ctx, removedDPBatch(1, engineKeys), "pod-dp-legacy", "test-model")
	result, err = idx.Lookup(ctx, requestKeys, nil)
	require.NoError(t, err)
	assert.Empty(t, result[requestKeys[0]], "second remove must evict the pod-level entry")
}
