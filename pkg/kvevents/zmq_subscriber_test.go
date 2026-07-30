// Copyright 2025 The llm-d Authors.
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
	"net"
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

// buildEventBatchPayload constructs a minimal valid msgpack EventBatch payload.
func buildEventBatchPayload(t *testing.T) []byte {
	t.Helper()

	// AllBlocksCleared event — simplest valid event, processed without side-effects.
	allCleared := []any{string(kvevents.EventTypeAllBlocksCleared)}
	rawEvent, err := msgpack.Marshal(allCleared)
	require.NoError(t, err)

	// EventBatch is array-encoded: [TS, Events, DataParallelRank]
	batch := []any{
		1234567890.0,                   // TS
		[]msgpack.RawMessage{rawEvent}, // Events
		nil,                            // DataParallelRank
	}

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.UseArrayEncodedStructs(true)
	require.NoError(t, enc.Encode(batch))
	return buf.Bytes()
}

func buildBlockStoredEventBatchPayload(t *testing.T, blockHashBase uint64, dataParallelRank int) []byte {
	t.Helper()

	tokens := make([]uint32, 64)
	blockHashes := make([]any, 4)
	for i := range tokens {
		tokens[i] = uint32(i + 1) // #nosec G115 -- test data
	}
	for i := range blockHashes {
		blockHashes[i] = blockHashBase + uint64(i) // #nosec G115 -- test data
	}

	blockStored := []any{
		string(kvevents.EventTypeBlockStored),
		blockHashes,
		uint64(0),
		tokens,
		16,
		nil,
		"gpu",
		nil,
		nil,
	}
	payload, err := msgpack.Marshal([]any{
		1234567890.0,
		[]any{blockStored},
		dataParallelRank,
	})
	require.NoError(t, err)
	return payload
}

func availableEndpoint(t *testing.T, ctx context.Context) string {
	t.Helper()

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	endpoint := fmt.Sprintf("tcp://%s", ln.Addr().String())
	require.NoError(t, ln.Close())
	return endpoint
}

// TestZMQPubSub verifies that the pure-Go ZMQ library correctly implements
// the PUB/SUB pattern used by the zmqSubscriber.
func TestZMQPubSub(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	endpoint := "tcp://127.0.0.1:15558"
	filter := "kv@"

	// Subscriber binds (Listen), publisher connects (Dial).
	sub := zmq4.NewSub(ctx)
	defer sub.Close()
	require.NoError(t, sub.Listen(endpoint))
	require.NoError(t, sub.SetOption(zmq4.OptionSubscribe, filter))

	// Give subscriber time to bind.
	time.Sleep(50 * time.Millisecond)

	pub := zmq4.NewPub(ctx)
	defer pub.Close()
	require.NoError(t, pub.Dial(endpoint))

	// Give the connection time to establish.
	time.Sleep(50 * time.Millisecond)

	// Build a 3-frame message: [topic, seqBytes, payload]
	topic := "kv@10.0.0.1@TestModel"
	seqBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(seqBytes, 42)
	payload := []byte("hello")

	require.NoError(t, pub.Send(zmq4.NewMsgFrom([]byte(topic), seqBytes, payload)))

	// Receive with timeout.
	recvDone := make(chan zmq4.Msg, 1)
	go func() {
		msg, err := sub.Recv()
		if err == nil {
			recvDone <- msg
		}
	}()

	select {
	case msg := <-recvDone:
		require.Len(t, msg.Frames, 3)
		assert.Equal(t, topic, string(msg.Frames[0]))
		assert.Equal(t, seqBytes, msg.Frames[1])
		assert.Equal(t, payload, msg.Frames[2])
	case <-ctx.Done():
		t.Fatal("timeout waiting for ZMQ message")
	}
}

// TestZMQSubscriber_ReceivesMessages verifies the full message path:
// publisher → zmqSubscriber → pool (end-to-end without mocks).
func TestZMQSubscriber_ReceivesMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Setup pool.
	index, err := kvblock.NewIndex(ctx, kvblock.DefaultIndexConfig())
	require.NoError(t, err)
	tokenProcessor, err := kvblock.NewChunkedTokenDatabase(kvblock.DefaultTokenProcessorConfig())
	require.NoError(t, err)
	pool := kvevents.NewPool(kvevents.DefaultConfig(), index, tokenProcessor, engineadapter.NewVLLMAdapter())
	pool.Start(ctx)

	// Start subscriber — remote=false means it binds (Listen).
	endpoint := "tcp://127.0.0.1:15559"
	subManager := kvevents.NewSubscriberManager(pool)
	err = subManager.EnsureSubscriber(ctx, "test-pod", "", endpoint, "kv@", false)
	require.NoError(t, err)

	// Give subscriber time to bind.
	time.Sleep(100 * time.Millisecond)

	// Publisher dials into the subscriber's bound address.
	pub := zmq4.NewPub(ctx)
	defer pub.Close()
	require.NoError(t, pub.Dial(endpoint))

	// Give the connection time to establish and subscription filter to propagate.
	time.Sleep(100 * time.Millisecond)

	// Send a valid 3-frame ZMQ message.
	topic := "kv@10.0.0.1@TestModel"
	seqBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(seqBytes, 1)
	payload := buildEventBatchPayload(t)

	require.NoError(t, pub.Send(zmq4.NewMsgFrom([]byte(topic), seqBytes, payload)))

	// Allow time for the message to be received and processed.
	time.Sleep(200 * time.Millisecond)

	subManager.Shutdown(ctx)
}

func TestZMQSubscribers_SameTopicUsesServingEndpointIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	index, err := kvblock.NewIndex(ctx, kvblock.DefaultIndexConfig())
	require.NoError(t, err)
	tokenProcessor, err := kvblock.NewChunkedTokenDatabase(kvblock.DefaultTokenProcessorConfig())
	require.NoError(t, err)
	pool := kvevents.NewPool(kvevents.DefaultConfig(), index, tokenProcessor, engineadapter.NewVLLMAdapter())
	pool.Start(ctx)

	subManager := kvevents.NewSubscriberManager(pool)
	defer subManager.Shutdown(ctx)

	sourceEndpoints := []string{"10.0.0.1:8000", "10.0.0.1:8003"}
	zmqEndpoints := []string{availableEndpoint(t, ctx), availableEndpoint(t, ctx)}
	for i := range sourceEndpoints {
		require.NoError(t, subManager.EnsureSubscriber(
			ctx,
			fmt.Sprintf("test-rank-%d", i),
			sourceEndpoints[i],
			zmqEndpoints[i],
			"kv@",
			false,
		))
	}
	time.Sleep(100 * time.Millisecond)

	publishers := make([]zmq4.Socket, len(zmqEndpoints))
	for i, endpoint := range zmqEndpoints {
		publishers[i] = zmq4.NewPub(ctx)
		defer publishers[i].Close()
		require.NoError(t, publishers[i].Dial(endpoint))
	}
	time.Sleep(100 * time.Millisecond)

	topic := []byte("kv@10.0.0.1:8000@TestModel")
	payloads := [][]byte{
		buildBlockStoredEventBatchPayload(t, 100, 8),
		buildBlockStoredEventBatchPayload(t, 200, 11),
	}
	tokens := make([]uint32, 64)
	for i := range tokens {
		tokens[i] = uint32(i + 1) // #nosec G115 -- test data
	}
	keys, err := tokenProcessor.TokensToKVBlockKeys(
		kvblock.EmptyBlockHash, tokens, "TestModel", nil)
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	firstKey := keys[0]

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for i, publisher := range publishers {
			seq := make([]byte, 8)
			binary.BigEndian.PutUint64(seq, uint64(i+1))
			require.NoError(t, publisher.Send(zmq4.NewMsgFrom(topic, seq, payloads[i])))
		}

		time.Sleep(50 * time.Millisecond)
		result, lookupErr := index.Lookup(ctx, keys, nil)
		require.NoError(t, lookupErr)
		if len(result[firstKey]) != 2 {
			continue
		}

		got := []string{
			result[firstKey][0].PodIdentifier,
			result[firstKey][1].PodIdentifier,
		}
		assert.ElementsMatch(t, sourceEndpoints, got)
		return
	}

	t.Fatal("timed out waiting for blocks from both serving endpoints")
}

// TestZMQSubscriber_ShortSequenceFrameSkipped verifies that a message with a
// truncated sequence frame (< 8 bytes) is skipped instead of panicking.
func TestZMQSubscriber_ShortSequenceFrameSkipped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Setup pool.
	index, err := kvblock.NewIndex(ctx, kvblock.DefaultIndexConfig())
	require.NoError(t, err)
	tokenProcessor, err := kvblock.NewChunkedTokenDatabase(kvblock.DefaultTokenProcessorConfig())
	require.NoError(t, err)
	pool := kvevents.NewPool(kvevents.DefaultConfig(), index, tokenProcessor, engineadapter.NewVLLMAdapter())
	pool.Start(ctx)

	// Pick an available ephemeral port to avoid conflicts with parallel tests or CI.
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	endpoint := fmt.Sprintf("tcp://%s", ln.Addr().String())
	ln.Close()
	subManager := kvevents.NewSubscriberManager(pool)
	err = subManager.EnsureSubscriber(ctx, "test-pod", "", endpoint, "kv@", false)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	// Publisher dials into the subscriber's bound address.
	pub := zmq4.NewPub(ctx)
	defer pub.Close()
	require.NoError(t, pub.Dial(endpoint))
	time.Sleep(100 * time.Millisecond)

	// Send malformed messages with a truncated sequence frame (3 bytes instead of 8).
	// Before the fix this would panic with index-out-of-range in binary.BigEndian.Uint64.
	// Retry sending for a short window to mitigate ZMQ "slow joiner" behavior where
	// early sends can be dropped before the subscription is fully established.
	shortSeq := []byte{0x01, 0x02, 0x03}
	sendDeadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(sendDeadline) {
		require.NoError(t, pub.Send(zmq4.NewMsgFrom([]byte("kv@10.0.0.1@TestModel"), shortSeq, []byte("bad"))))
		time.Sleep(10 * time.Millisecond)
	}

	// Allow a brief moment for any in-flight message to be processed before shutdown.
	time.Sleep(100 * time.Millisecond)

	// If we reach here without a panic, the short frame was correctly skipped.
	subManager.Shutdown(ctx)
}

// buildBlockStoredPayload constructs a msgpack EventBatch payload with a
// single BlockStored event.
func buildBlockStoredPayload(t *testing.T, hashes []uint64, tokens []uint32, blockSize int) []byte {
	t.Helper()

	stored := []any{string(kvevents.EventTypeBlockStored), hashes, nil, tokens, blockSize}
	rawEvent, err := msgpack.Marshal(stored)
	require.NoError(t, err)
	return buildBatchPayload(t, []msgpack.RawMessage{rawEvent})
}

// buildBatchPayload wraps raw events in the array-encoded EventBatch layout
// [TS, Events, DataParallelRank].
func buildBatchPayload(t *testing.T, events []msgpack.RawMessage) []byte {
	t.Helper()

	batch := []any{1234567890.0, events, nil}
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.UseArrayEncodedStructs(true)
	require.NoError(t, enc.Encode(batch))
	return buf.Bytes()
}

// TestZMQSubscriber_SeqGapResyncsPod verifies the full resync path: a
// sequence jump on a topic injects a reset task through the pod's ordered
// queue, clearing the pod's index entries.
func TestZMQSubscriber_SeqGapResyncsPod(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	index, err := kvblock.NewIndex(ctx, kvblock.DefaultIndexConfig())
	require.NoError(t, err)
	tokenProcessor, err := kvblock.NewChunkedTokenDatabase(kvblock.DefaultTokenProcessorConfig())
	require.NoError(t, err)
	pool := kvevents.NewPool(kvevents.DefaultConfig(), index, tokenProcessor, engineadapter.NewVLLMAdapter())
	pool.Start(ctx)

	// Pick an available ephemeral port to avoid conflicts with parallel tests or CI.
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	endpoint := fmt.Sprintf("tcp://%s", ln.Addr().String())
	ln.Close()
	subManager := kvevents.NewSubscriberManager(pool)
	require.NoError(t, subManager.EnsureSubscriber(ctx, "test-pod", "", endpoint, "kv@", false))
	defer subManager.Shutdown(ctx)
	time.Sleep(100 * time.Millisecond)

	pub := zmq4.NewPub(ctx)
	defer pub.Close()
	require.NoError(t, pub.Dial(endpoint))
	time.Sleep(100 * time.Millisecond)

	topic := "kv@10.0.0.1@TestModel"
	tokens := make([]uint32, 64)
	for i := range tokens {
		tokens[i] = uint32(i + 1) // #nosec G115 -- test data, i is small
	}
	hashes := []uint64{101, 102, 103, 104}

	canonicalKeys, err := tokenProcessor.TokensToKVBlockKeys(kvblock.EmptyBlockHash, tokens, "TestModel", nil)
	require.NoError(t, err)
	require.NotEmpty(t, canonicalKeys)

	seq := uint64(0)
	send := func(payload []byte) {
		seq++
		seqBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(seqBytes, seq)
		require.NoError(t, pub.Send(zmq4.NewMsgFrom([]byte(topic), seqBytes, payload)))
	}

	podPresent := func() bool {
		result, err := index.Lookup(ctx, canonicalKeys, nil)
		require.NoError(t, err)
		for _, entries := range result {
			for _, entry := range entries {
				if entry.PodIdentifier == "10.0.0.1" {
					return true
				}
			}
		}
		return false
	}

	// Phase 1: send stores with consecutive sequences until one lands (ZMQ
	// slow-joiner may drop early sends; consecutive numbering keeps the
	// retries gap-free, so no resync fires here).
	storePayload := buildBlockStoredPayload(t, hashes, tokens, 16)
	deadline := time.Now().Add(5 * time.Second)
	for !podPresent() && time.Now().Before(deadline) {
		send(storePayload)
		time.Sleep(50 * time.Millisecond)
	}
	require.True(t, podPresent(), "pod should be indexed after the stores")

	// Phase 2: jump the sequence. The first received message is a gap and
	// injects a reset that clears the pod; the empty batches store nothing
	// and stay consecutive, so the cleared state is terminal.
	seq += 100
	emptyPayload := buildBatchPayload(t, []msgpack.RawMessage{})
	deadline = time.Now().Add(5 * time.Second)
	for podPresent() && time.Now().Before(deadline) {
		send(emptyPayload)
		time.Sleep(50 * time.Millisecond)
	}
	assert.False(t, podPresent(), "sequence gap should have resynced (cleared) the pod")
}
