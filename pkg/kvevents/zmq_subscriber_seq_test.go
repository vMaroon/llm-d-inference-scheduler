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

package kvevents //nolint:testpackage // tests use unexported checkSequence and pool internals

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
	"github.com/llm-d/llm-d-router/pkg/kvcache/metrics"
)

// newSeqTestSubscriber returns a subscriber whose pool is not started, so
// tasks enqueued by checkSequence stay observable in the queues. The empty
// source endpoint exercises the topic-derived identity fallback; the
// endpoint-identity path is covered by TestCheckSequence_SourceEndpointResync.
func newSeqTestSubscriber(t *testing.T) (*zmqSubscriber, *Pool) {
	t.Helper()
	pool, _, _ := newTestPool(t, 16)
	pool.adapter = fakeTopicAdapter{}
	return newZMQSubscriber(pool, "seq-test-pod", "", "tcp://127.0.0.1:5557", "kv@", false), pool
}

// drainTasks pops every queued task without running workers.
func drainTasks(p *Pool) []*RawMessage {
	var tasks []*RawMessage
	for _, q := range p.queues {
		for q.Len() > 0 {
			task, _ := q.Get()
			q.Done(task)
			tasks = append(tasks, task)
		}
	}
	return tasks
}

// TestCheckSequence_FirstMessageBaselinesSilently verifies that the first
// message on a topic, at any sequence value, records a baseline with no
// metric increment and no reset task, independently per topic.
func TestCheckSequence_FirstMessageBaselinesSilently(t *testing.T) {
	sub, pool := newSeqTestSubscriber(t)
	before := counterValue(t, metrics.SequenceGaps)

	sub.checkSequence(logr.Discard(), "kv@pod-a@model", 7)
	sub.checkSequence(logr.Discard(), "kv@pod-b@model", 3)

	assert.Zero(t, counterValue(t, metrics.SequenceGaps)-before,
		"first message per topic must not count as a gap")
	assert.Empty(t, drainTasks(pool), "first message per topic must not enqueue a reset")
}

// TestCheckSequence_ConsecutiveNoAction verifies that strictly consecutive
// sequences produce no metric increment and no reset task.
func TestCheckSequence_ConsecutiveNoAction(t *testing.T) {
	sub, pool := newSeqTestSubscriber(t)
	before := counterValue(t, metrics.SequenceGaps)

	topic := "kv@pod-a@model"
	for seq := uint64(1); seq <= 5; seq++ {
		sub.checkSequence(logr.Discard(), topic, seq)
	}

	assert.Zero(t, counterValue(t, metrics.SequenceGaps)-before,
		"consecutive sequences must not count as gaps")
	assert.Empty(t, drainTasks(pool), "consecutive sequences must not enqueue resets")
}

// TestCheckSequence_GapIncrementsMetricAndEnqueuesReset verifies that a
// forward gap counts once and enqueues one reset task carrying the topic, and
// that continuity resumes from the gapped value.
func TestCheckSequence_GapIncrementsMetricAndEnqueuesReset(t *testing.T) {
	sub, pool := newSeqTestSubscriber(t)
	before := counterValue(t, metrics.SequenceGaps)

	topic := "kv@pod-a@model"
	sub.checkSequence(logr.Discard(), topic, 1)
	sub.checkSequence(logr.Discard(), topic, 3) // gap: expected 2

	assert.Equal(t, 1.0, counterValue(t, metrics.SequenceGaps)-before)
	tasks := drainTasks(pool)
	require.Len(t, tasks, 1, "a gap must enqueue exactly one reset task")
	assert.True(t, tasks[0].Reset)
	assert.Equal(t, topic, tasks[0].Topic, "reset must shard to the gapped topic's pod")

	// The gapped value is the new baseline: the next consecutive message is normal.
	sub.checkSequence(logr.Discard(), topic, 4)
	assert.Equal(t, 1.0, counterValue(t, metrics.SequenceGaps)-before)
	assert.Empty(t, drainTasks(pool))
}

// TestCheckSequence_RegressionResyncs verifies that a sequence regression
// (publisher restart) counts as a discontinuity and enqueues a reset.
func TestCheckSequence_RegressionResyncs(t *testing.T) {
	sub, pool := newSeqTestSubscriber(t)
	before := counterValue(t, metrics.SequenceGaps)

	topic := "kv@pod-a@model"
	sub.checkSequence(logr.Discard(), topic, 10)
	sub.checkSequence(logr.Discard(), topic, 4) // regression: seq < last

	assert.Equal(t, 1.0, counterValue(t, metrics.SequenceGaps)-before)
	tasks := drainTasks(pool)
	require.Len(t, tasks, 1)
	assert.True(t, tasks[0].Reset)

	// An equal sequence (seq == last) is also a discontinuity.
	sub.checkSequence(logr.Discard(), topic, 4)
	assert.Equal(t, 2.0, counterValue(t, metrics.SequenceGaps)-before)
	tasks = drainTasks(pool)
	require.Len(t, tasks, 1)
	assert.True(t, tasks[0].Reset)
}

// TestCheckSequence_ResyncDisabledStillCounts verifies that
// resyncOnSeqGap=false suppresses the reset task but not the metric.
func TestCheckSequence_ResyncDisabledStillCounts(t *testing.T) {
	sub, pool := newSeqTestSubscriber(t)
	pool.resyncOnSeqGap = false
	before := counterValue(t, metrics.SequenceGaps)

	topic := "kv@pod-a@model"
	sub.checkSequence(logr.Discard(), topic, 1)
	sub.checkSequence(logr.Discard(), topic, 5) // gap: expected 2

	assert.Equal(t, 1.0, counterValue(t, metrics.SequenceGaps)-before,
		"detection and the metric are unconditional")
	assert.Empty(t, drainTasks(pool), "disabled resync must not enqueue a reset")
}

// TestConfig_ResyncOnSeqGapResolution pins the nil-resolves-true contract.
func TestConfig_ResyncOnSeqGapResolution(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *bool
		want bool
	}{
		{"nil defaults to true", nil, true},
		{"explicit true", ptrBool(true), true},
		{"explicit false", ptrBool(false), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.ResyncOnSeqGap = tc.cfg
			pool := NewPool(cfg, nil, nil, fakeTopicAdapter{})
			assert.Equal(t, tc.want, pool.resyncOnSeqGap)
		})
	}
}

func ptrBool(b bool) *bool { return &b }

// TestCheckSequence_SourceEndpointResync pins the reset task's identity to the
// one regular events are indexed and sharded under: with a subscriber-serving
// endpoint set, the reset carries it, lands on the same worker queue as the
// subscriber's regular messages, and clears exactly that identity.
func TestCheckSequence_SourceEndpointResync(t *testing.T) {
	const sourceEndpoint = "10.9.9.9:8000"
	topic := "kv@pod-a@model"

	pool, idx, _ := newTestPool(t, 16)
	pool.adapter = fakeTopicAdapter{}
	sub := newZMQSubscriber(pool, "seq-test-pod", sourceEndpoint, "tcp://127.0.0.1:5557", "kv@", false)

	ctx := t.Context()
	key := []kvblock.BlockHash{kvblock.BlockHash(1234)}
	require.NoError(t, idx.Add(ctx, nil, key,
		[]kvblock.PodEntry{{PodIdentifier: sourceEndpoint, DeviceTier: "gpu"}}))
	require.NoError(t, idx.Add(ctx, nil, key,
		[]kvblock.PodEntry{{PodIdentifier: "pod-a", DeviceTier: "gpu"}}))

	sub.checkSequence(logr.Discard(), topic, 1)
	sub.checkSequence(logr.Discard(), topic, 5) // gap: expected 2

	// The reset and a regular message from the same subscriber must land on
	// the same worker queue: the endpoint identity is the sharding key.
	sub.pool.AddTask(&RawMessage{Topic: topic, SourceEndpoint: sourceEndpoint, Payload: []byte("x")})
	occupied := 0
	var tasks []*RawMessage
	for _, q := range pool.queues {
		if q.Len() > 0 {
			occupied++
			for q.Len() > 0 {
				task, _ := q.Get()
				q.Done(task)
				tasks = append(tasks, task)
			}
		}
	}
	assert.Equal(t, 1, occupied, "reset and regular message must share one ordered queue")
	require.Len(t, tasks, 2)
	require.True(t, tasks[0].Reset, "reset must precede the gapped message's successors")
	assert.Equal(t, sourceEndpoint, tasks[0].SourceEndpoint)

	// Processing the reset clears the endpoint identity and nothing else.
	pool.processRawMessage(ctx, tasks[0])
	pods, err := idx.Lookup(ctx, key, nil)
	require.NoError(t, err)
	assert.NotContains(t, pods[key[0]],
		kvblock.PodEntry{PodIdentifier: sourceEndpoint, DeviceTier: "gpu"},
		"reset must clear the source-endpoint identity regular events are indexed under")
	assert.Contains(t, pods[key[0]],
		kvblock.PodEntry{PodIdentifier: "pod-a", DeviceTier: "gpu"},
		"other identities must survive the reset")
}
