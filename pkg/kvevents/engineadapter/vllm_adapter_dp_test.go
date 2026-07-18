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

package engineadapter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/llm-d/llm-d-router/pkg/kvevents"
)

// dpBatchPayload encodes an EventBatch carrying a single AllBlocksCleared
// event with the given trailing batch fields, mirroring vLLM's
// msgspec(array_like=True, omit_defaults=True) layout
// [ts, events, data_parallel_rank].
func dpBatchPayload(t *testing.T, batchTail ...any) []byte {
	t.Helper()
	event := []any{string(kvevents.EventTypeAllBlocksCleared)}
	batch := append([]any{1234567890.0, []any{event}}, batchTail...)
	payload, err := msgpack.Marshal(batch)
	require.NoError(t, err)
	return payload
}

// TestVLLMParseMessage_DataParallelRank verifies the batch-level
// data_parallel_rank annotation ([ts, events, data_parallel_rank]) surfaces
// on the parsed EventBatch: a present rank round-trips and an explicit nil
// parses as nil. vLLM's publisher always stamps an int rank (0 for non-DP)
// before encoding, so the wire batch always carries the third element.
func TestVLLMParseMessage_DataParallelRank(t *testing.T) {
	adapter := NewVLLMAdapter()
	msg := func(payload []byte) *kvevents.RawMessage {
		return &kvevents.RawMessage{Topic: "kv@pod-1@llama-2-7b", Payload: payload}
	}

	_, _, batch, err := adapter.ParseMessage(msg(dpBatchPayload(t, 3)))
	require.NoError(t, err)
	require.NotNil(t, batch.DataParallelRank)
	assert.Equal(t, 3, *batch.DataParallelRank)

	_, _, batch, err = adapter.ParseMessage(msg(dpBatchPayload(t, nil)))
	require.NoError(t, err)
	assert.Nil(t, batch.DataParallelRank)
}

// TestSGLangParseMessage_DataParallelRank mirrors the vLLM case for the
// SGLang adapter, which shares the batch wire layout.
func TestSGLangParseMessage_DataParallelRank(t *testing.T) {
	adapter := NewSGLangAdapter()
	msg := func(payload []byte) *kvevents.RawMessage {
		return &kvevents.RawMessage{Topic: "kv@pod-1@llama-2-7b", Payload: payload}
	}

	_, _, batch, err := adapter.ParseMessage(msg(dpBatchPayload(t, 7)))
	require.NoError(t, err)
	require.NotNil(t, batch.DataParallelRank)
	assert.Equal(t, 7, *batch.DataParallelRank)

	_, _, batch, err = adapter.ParseMessage(msg(dpBatchPayload(t, nil)))
	require.NoError(t, err)
	assert.Nil(t, batch.DataParallelRank)
}
