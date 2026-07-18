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

package kvevents_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	zmq4 "github.com/go-zeromq/zmq4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
	"github.com/llm-d/llm-d-router/pkg/kvevents"
	"github.com/llm-d/llm-d-router/pkg/kvevents/engineadapter"
)

// buildDPBlockStoredPayload constructs a msgpack EventBatch payload carrying
// one BlockStored event, annotated with the given global data-parallel rank
// ([ts, events, data_parallel_rank], matching vLLM's array encoding).
func buildDPBlockStoredPayload(t *testing.T, rank int, tokens []uint32, engineKey uint64) []byte {
	t.Helper()

	tokenIDs := make([]any, len(tokens))
	for i, token := range tokens {
		tokenIDs[i] = int(token)
	}
	stored := []any{
		string(kvevents.EventTypeBlockStored),
		[]any{engineKey}, // block_hashes
		nil,              // parent_block_hash
		tokenIDs,         // token_ids
		len(tokens),      // block_size
	}
	rawEvent, err := msgpack.Marshal(stored)
	require.NoError(t, err)

	batch := []any{
		1234567890.0,                   // TS
		[]msgpack.RawMessage{rawEvent}, // Events
		rank,                           // DataParallelRank
	}

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.UseArrayEncodedStructs(true)
	require.NoError(t, enc.Encode(batch))
	return buf.Bytes()
}

// TestZMQSubscriber_DataParallelRankedBatches runs the full DP chain over
// real ZMQ: two pods x two ranks publish on socketPort+{0,1,2,3} with global
// rank annotations, and the pool indexes each batch under the emitting
// rank's serving endpoint <podIP>:(servingPortBase + rank % local) while
// GlobalDPRank resolves every identifier back to its global rank.
func TestZMQSubscriber_DataParallelRankedBatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	index, err := kvblock.NewIndex(ctx, kvblock.DefaultIndexConfig())
	require.NoError(t, err)
	tokenProcessor, err := kvblock.NewChunkedTokenDatabase(kvblock.DefaultTokenProcessorConfig())
	require.NoError(t, err)

	cfg := kvevents.DefaultConfig()
	cfg.DataParallelSizeLocal = 2
	cfg.ServingPortBase = 18000
	pool := kvevents.NewPool(cfg, index, tokenProcessor, engineadapter.NewVLLMAdapter())
	pool.Start(ctx)

	// One subscriber per ZMQ port offset, mirroring the rank-range
	// subscription the pod reconciler installs (remote=false binds so the
	// in-test publishers can dial).
	const socketPortBase = 15620
	subManager := kvevents.NewSubscriberManager(pool)
	for i := 0; i < 4; i++ {
		endpoint := fmt.Sprintf("tcp://127.0.0.1:%d", socketPortBase+i)
		require.NoError(t, subManager.EnsureSubscriber(ctx, fmt.Sprintf("dp-sub-%d", i), endpoint, "kv@", false))
	}
	defer subManager.Shutdown(ctx)
	time.Sleep(100 * time.Millisecond)

	// Global ranks 0-1 belong to pod 10.1.0.1, ranks 2-3 to pod 10.1.0.2;
	// each rank's topic carries its pod's serving host:port.
	podHosts := map[int]string{0: "10.1.0.1", 1: "10.1.0.1", 2: "10.1.0.2", 3: "10.1.0.2"}
	pubs := make([]zmq4.Socket, 4)
	for rank := 0; rank < 4; rank++ {
		pub := zmq4.NewPub(ctx)
		defer pub.Close()
		require.NoError(t, pub.Dial(fmt.Sprintf("tcp://127.0.0.1:%d", socketPortBase+rank)))
		pubs[rank] = pub
	}
	time.Sleep(100 * time.Millisecond)

	tokens := make([]uint32, 16)
	for i := range tokens {
		tokens[i] = uint32(i + 1) // #nosec G115 -- test data
	}
	requestKeys, err := tokenProcessor.TokensToKVBlockKeys(kvblock.EmptyBlockHash, tokens, "TestModel", nil)
	require.NoError(t, err)
	require.Len(t, requestKeys, 1)

	wantRanks := map[string]int{
		"10.1.0.1:18000": 0,
		"10.1.0.1:18001": 1,
		"10.1.0.2:18000": 2,
		"10.1.0.2:18001": 3,
	}

	publishAll := func() {
		for rank, pub := range pubs {
			topic := fmt.Sprintf("kv@%s:8000@TestModel", podHosts[rank])
			seqBytes := make([]byte, 8)
			binary.BigEndian.PutUint64(seqBytes, 1)
			payload := buildDPBlockStoredPayload(t, rank, tokens, uint64(900+rank)) // #nosec G115 -- test data
			require.NoError(t, pub.Send(zmq4.NewMsgFrom([]byte(topic), seqBytes, payload)))
		}
	}

	// Re-publish each poll round to absorb ZMQ slow-joiner drops; duplicate
	// stores only inflate dedup reference counts and do not affect lookups.
	require.Eventually(t, func() bool {
		publishAll()
		result, err := index.Lookup(ctx, requestKeys, nil)
		if err != nil {
			return false
		}
		entries := result[requestKeys[0]]
		seen := map[string]bool{}
		for _, entry := range entries {
			seen[entry.PodIdentifier] = true
		}
		for identifier := range wantRanks {
			if !seen[identifier] {
				return false
			}
		}
		return true
	}, 10*time.Second, 100*time.Millisecond,
		"all four ranks must index under their serving endpoints")

	for identifier, want := range wantRanks {
		rank, ok := pool.GlobalDPRank(identifier)
		require.True(t, ok, "rank lookup must resolve %s", identifier)
		assert.Equal(t, want, rank, "identifier %s", identifier)
	}
}
