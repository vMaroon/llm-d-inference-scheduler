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

package kvevents

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
	"github.com/llm-d/llm-d-router/pkg/kvcache/metrics"
)

const (
	defaultEventSourceDeviceTier = "gpu"
	defaultPodSelector           = "llm-d.ai/inference-serving=true"
)

// normalizeDeviceTier lowercases an event's device tier and defaults an empty
// value to the GPU source tier. Store and remove events that mean the same tier
// must normalize identically so they build equal PodEntries and dedup scopes;
// keeping this in one place prevents the two call sites from drifting apart.
func normalizeDeviceTier(deviceTier string) string {
	if deviceTier == "" {
		return defaultEventSourceDeviceTier
	}
	return strings.ToLower(deviceTier)
}

func isPrefixIndexableSpecKind(kind KVCacheSpecKind) bool {
	switch kind {
	case KVCacheSpecKindFullAttention, KVCacheSpecKindMlaAttention, KVCacheSpecKindSinkFull:
		return true
	default:
		return false
	}
}

func cacheKindLabel(kind KVCacheSpecKind) string {
	if kind == "" {
		return string(KVCacheSpecKindUnknown)
	}
	return string(kind)
}

func blockStoredEventDigestible(ev *BlockStoredEvent) (bool, string) {
	if ev.GroupIdx == nil {
		return true, ""
	}
	if !isPrefixIndexableSpecKind(ev.KVCacheSpecKind) {
		return false, "unsupported_cache_kind"
	}
	if len(ev.Tokens) == 0 {
		return true, ""
	}
	if ev.BlockSize <= 0 {
		return false, "invalid_block_size"
	}
	if len(ev.Tokens)%ev.BlockSize != 0 || len(ev.Tokens)/ev.BlockSize != len(ev.BlockHashes) {
		return false, "non_dense_block_span"
	}
	return true, ""
}

// Config holds the configuration for the event processing pool.
type Config struct {
	// ZMQEndpoint is the ZMQ address to connect to (e.g., "tcp://indexer:5557").
	ZMQEndpoint string `json:"zmqEndpoint,omitempty"`
	// TopicFilter is the ZMQ subscription filter (e.g., "kv@").
	TopicFilter string `json:"topicFilter"`
	// Concurrency is the number of parallel workers to run.
	Concurrency int `json:"concurrency"`
	// EngineType selects the inference engine adapter ("vllm" or "sglang").
	// Default: "vllm".
	EngineType string `json:"engineType,omitempty"`
	// DiscoverPods enables the Kubernetes pod reconciler for automatic
	// per-pod subscriber management. When enabled, the reconciler watches
	// Kubernetes pods and creates/removes ZMQ subscribers dynamically.
	DiscoverPods bool `json:"discoverPods"`
	// PodDiscoveryConfig holds the configuration for pod discovery.
	// Only used when DiscoverPods is true.
	PodDiscoveryConfig *PodDiscoveryConfig `json:"podDiscoveryConfig,omitempty"`
}

// PodDiscoveryConfig holds configuration for the Kubernetes pod reconciler.
type PodDiscoveryConfig struct {
	// PodLabelSelector is a label selector string for filtering which pods to watch.
	// Example: "app=vllm" or "app=vllm,tier=gpu"
	PodLabelSelector string `json:"podLabelSelector"`
	// PodNamespace limits the reconciler to watch pods in a specific namespace.
	// If empty, watches all namespaces (requires appropriate RBAC).
	PodNamespace string `json:"podNamespace,omitempty"`
	// SocketPort is the port number where vLLM pods expose their ZMQ socket.
	// The reconciler will connect to tcp://<PodIP>:<SocketPort>
	// Default: 5557
	SocketPort int `json:"socketPort"`
	// ReplaySocketPort is the port where vLLM pods expose their ZMQ ROUTER
	// socket for replay requests. Disabled when not set (0 or negative).
	ReplaySocketPort int `json:"replaySocketPort,omitempty"`
}

// EffectiveReplayPort returns the replay socket port.
// Returns -1 (disabled) when not explicitly configured.
func (c *PodDiscoveryConfig) EffectiveReplayPort() int {
	if c.ReplaySocketPort <= 0 {
		return -1
	}
	return c.ReplaySocketPort
}

// DefaultPodReconcilerConfig returns a default configuration for the pod reconciler.
func DefaultPodReconcilerConfig() *PodDiscoveryConfig {
	return &PodDiscoveryConfig{
		PodLabelSelector: defaultPodSelector,
		SocketPort:       5557,
	}
}

// DefaultConfig returns a default configuration for the event processing pool.
func DefaultConfig() *Config {
	return &Config{
		TopicFilter:        "kv@",
		Concurrency:        4,
		DiscoverPods:       true,
		PodDiscoveryConfig: DefaultPodReconcilerConfig(),
	}
}

// Pool is a sharded worker pool that processes events from ZMQ subscribers.
// It ensures that events for the same PodIdentifier are processed in order.
// Pool keeps transient event-stream state while durable key mappings are
// delegated to the Index.
type Pool struct {
	queues         []workqueue.TypedRateLimitingInterface[*RawMessage]
	concurrency    int // can replace use with len(queues)
	index          kvblock.Index
	tokenProcessor kvblock.TokenProcessor
	adapter        EngineAdapter
	groupCatalog   *kvblock.GroupCatalog
	// dedup lives in the Pool, not as an Index decorator, because its scope is
	// built from event fields absent from the Index.Evict signature (device
	// tier, KV-cache group, DP rank) and a store must be counted only after
	// Index.Add succeeds — both of which only the Pool observes.
	dedup *eventDedupFilter
	wg    sync.WaitGroup
	// queueDepth mirrors the number of tasks queued across all shards. It is
	// tracked incrementally rather than by summing queue.Len() so that the
	// depth gauge stays O(1) on the enqueue/dequeue hot path.
	queueDepth         atomic.Int64
	observerMu         sync.RWMutex
	observer           StreamObserver
	snapshotMu         sync.Mutex
	snapshots          map[string]snapshotState
	snapshotGeneration atomic.Uint64
}

type snapshotState struct {
	generation uint64
	failed     bool
	active     bool
}

// NewPool creates a Pool with a sharded worker setup.
// Subscribers are managed by SubscriberManager which is controlled by the pod
// reconciler.
//
// Side effect: it registers the kvcache metrics with the controller-runtime
// registry so that the kvevents metrics are scraped wherever a pool runs.
// Registration is idempotent (guarded by a sync.Once).
func NewPool(cfg *Config, index kvblock.Index, tokenProcessor kvblock.TokenProcessor,
	adapter EngineAdapter,
) *Pool {
	if cfg == nil {
		cfg = DefaultConfig()
	}

	p := &Pool{
		queues:         make([]workqueue.TypedRateLimitingInterface[*RawMessage], cfg.Concurrency),
		concurrency:    cfg.Concurrency,
		index:          index,
		tokenProcessor: tokenProcessor,
		adapter:        adapter,
		groupCatalog:   kvblock.NewGroupCatalog(),
		dedup:          newEventDedupFilter(),
		snapshots:      make(map[string]snapshotState),
	}

	for i := 0; i < p.concurrency; i++ {
		p.queues[i] = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[*RawMessage]())
	}

	metrics.Register()

	return p
}

// SetStreamObserver installs the observer for endpoint stream state. It is
// normally called before Start.
func (p *Pool) SetStreamObserver(observer StreamObserver) {
	p.observerMu.Lock()
	defer p.observerMu.Unlock()
	p.observer = observer
}

// NotifyStreamEvent reports a stream transition using the scheduler's serving
// endpoint identity.
func (p *Pool) NotifyStreamEvent(sourceEndpoint string, event StreamEvent) {
	p.notifyStreamEvent(sourceEndpoint, event, 0)
}

func (p *Pool) notifyStreamEvent(sourceEndpoint string, event StreamEvent, snapshotGeneration uint64) {
	if sourceEndpoint == "" || event == "" {
		return
	}
	if event == StreamEventDetached {
		// Retire every generation allocated before this detach. The old
		// subscriber may still have queued replay tasks while its cancellation
		// completes; the high-water mark keeps those tasks stale.
		p.snapshotMu.Lock()
		p.snapshots[sourceEndpoint] = snapshotState{generation: p.snapshotGeneration.Add(1)}
		p.snapshotMu.Unlock()
	} else if event == StreamEventMissingParent || event == StreamEventSequenceDiscontinuity ||
		event == StreamEventProcessingFailure {
		p.snapshotMu.Lock()
		if state, exists := p.snapshots[sourceEndpoint]; exists && state.active &&
			(snapshotGeneration == 0 || snapshotGeneration == state.generation) {
			state.failed = true
			p.snapshots[sourceEndpoint] = state
		}
		p.snapshotMu.Unlock()
	}
	p.observerMu.RLock()
	observer := p.observer
	p.observerMu.RUnlock()
	if observer != nil {
		observer(sourceEndpoint, event)
	}
}

// addQueueDepth adjusts the tracked queue depth by delta and publishes the new
// total to the depth gauge.
func (p *Pool) addQueueDepth(delta int64) {
	metrics.PoolQueueDepth.Set(float64(p.queueDepth.Add(delta)))
}

// GroupCatalog returns the KV cache group metadata learned from events.
func (p *Pool) GroupCatalog() *kvblock.GroupCatalog {
	return p.groupCatalog
}

// Start begins the worker pool.
// It is non-blocking.
func (p *Pool) Start(ctx context.Context) {
	logger := log.FromContext(ctx)
	logger.Info("Starting sharded event processing pool", "workers", p.concurrency)

	metrics.PoolCapacity.Set(float64(p.concurrency))

	p.wg.Add(p.concurrency)
	for i := 0; i < p.concurrency; i++ {
		// Each worker is given its own dedicated queue shard.
		go p.worker(ctx, i)
	}
}

// Shutdown gracefully stops the pool and its global subscriber if present.
func (p *Pool) Shutdown(ctx context.Context) {
	logger := log.FromContext(ctx)
	logger.Info("Shutting down event processing pool...")

	for _, queue := range p.queues {
		queue.ShutDown()
	}

	p.wg.Wait()

	// Tasks still queued at shutdown are dropped with the queues, so reset the
	// depth rather than leaving the gauge pinned at the undrained count.
	p.queueDepth.Store(0)
	metrics.PoolQueueDepth.Set(0)

	logger.Info("event processing pool shut down.")
}

// AddTask is called by the subscriber to add a message to the processing queue.
// It hashes the sharding key to select a queue, ensuring messages for the
// same source endpoint always go to the same worker (ordered queue).
func (p *Pool) AddTask(task *RawMessage) {
	key := task.SourceEndpoint
	if key == "" {
		key = p.adapter.ShardingKey(task)
	}
	// Use an FNV-1a hash to deterministically select a queue.
	h := fnv.New32a()
	_, err := h.Write([]byte(key))
	if err != nil {
		return
	}

	//nolint:gosec // if concurrency overflows then the world is in trouble anyway
	queueIndex := h.Sum32() % uint32(p.concurrency)
	p.queues[queueIndex].Add(task)
	p.addQueueDepth(1)
}

// resetForSource queues a pod reset on the same shard as its event stream.
func (p *Pool) resetForSource(topic, sourceEndpoint string, snapshotGeneration uint64) {
	p.AddTask(&RawMessage{
		Topic: topic, SourceEndpoint: sourceEndpoint, reset: true,
		snapshotGeneration: snapshotGeneration,
	})
}

// signalAfterEvents queues an integrity transition behind all prior events
// from the same source endpoint.
func (p *Pool) beginSnapshot(sourceEndpoint string) uint64 {
	generation := p.snapshotGeneration.Add(1)
	p.AddTask(&RawMessage{
		SourceEndpoint: sourceEndpoint, snapshotStart: true,
		snapshotGeneration: generation,
	})
	return generation
}

func (p *Pool) finishSnapshot(sourceEndpoint string, generation uint64) {
	p.AddTask(&RawMessage{
		SourceEndpoint: sourceEndpoint, snapshotEnd: true,
		snapshotGeneration: generation,
	})
}

func (p *Pool) abortSnapshot(sourceEndpoint string, generation uint64) {
	p.AddTask(&RawMessage{
		SourceEndpoint: sourceEndpoint, snapshotAbort: true,
		snapshotGeneration: generation,
	})
}

// worker is the main processing loop for a single worker goroutine.
// It processes messages from its dedicated queue using the workqueue pattern.
func (p *Pool) worker(ctx context.Context, workerIndex int) {
	defer p.wg.Done()
	queue := p.queues[workerIndex]
	for {
		task, shutdown := queue.Get()
		if shutdown {
			return
		}

		// Use a nested func to ensure Done is always called.
		func(task *RawMessage) {
			defer queue.Done(task)
			p.processRawMessage(ctx, task)
			// Task succeeded, remove it from the queue.
			queue.Forget(task)
		}(task)
		p.addQueueDepth(-1)

		// Check if context was cancelled after processing a task.
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// processRawMessage decodes the raw message payload using the adapter and processes the resulting event batch.
func (p *Pool) processRawMessage(ctx context.Context, msg *RawMessage) {
	logger := log.FromContext(ctx)
	if msg.snapshotStart {
		p.snapshotMu.Lock()
		state, active := p.snapshots[msg.SourceEndpoint]
		if !active || msg.snapshotGeneration >= state.generation {
			p.snapshots[msg.SourceEndpoint] = snapshotState{
				generation: msg.snapshotGeneration,
				active:     true,
			}
		}
		p.snapshotMu.Unlock()
		return
	}
	if msg.snapshotEnd || msg.snapshotAbort {
		p.snapshotMu.Lock()
		state, active := p.snapshots[msg.SourceEndpoint]
		current := active && state.active && state.generation == msg.snapshotGeneration
		if current {
			state.active = false
			p.snapshots[msg.SourceEndpoint] = state
		}
		p.snapshotMu.Unlock()
		if msg.snapshotEnd && current && !state.failed {
			p.NotifyStreamEvent(msg.SourceEndpoint, StreamEventAuthoritativeSnapshot)
		}
		return
	}
	if msg.snapshotGeneration != 0 && !p.isCurrentSnapshot(msg.SourceEndpoint, msg.snapshotGeneration) {
		return
	}
	if msg.streamEvent != "" {
		p.notifyStreamEvent(msg.SourceEndpoint, msg.streamEvent, msg.snapshotGeneration)
		return
	}
	if msg.reset {
		podID := msg.SourceEndpoint
		if podID == "" {
			podID = p.adapter.ShardingKey(msg)
		}
		if !p.clearPod(ctx, podID) {
			p.notifyStreamEvent(podID, StreamEventProcessingFailure, msg.snapshotGeneration)
		}
		return
	}

	podID, modelName, batch, err := p.adapter.ParseMessage(msg)
	if err != nil {
		logger.Error(err, "Failed to parse message")
		p.notifyStreamEvent(msg.SourceEndpoint, StreamEventProcessingFailure, msg.snapshotGeneration)
		return
	}
	if msg.SourceEndpoint != "" {
		podID = msg.SourceEndpoint
	}

	p.processEventBatchWithGeneration(ctx, &batch, podID, modelName, msg.snapshotGeneration)
}

func (p *Pool) isCurrentSnapshot(sourceEndpoint string, generation uint64) bool {
	p.snapshotMu.Lock()
	defer p.snapshotMu.Unlock()
	state, exists := p.snapshots[sourceEndpoint]
	return exists && state.active && state.generation == generation
}

func (p *Pool) clearPod(ctx context.Context, podIdentifier string) bool {
	debugLogger := log.FromContext(ctx).V(logging.DEBUG)
	if err := p.index.Clear(ctx, podIdentifier); err != nil {
		debugLogger.Error(err, "Failed to clear pod from index",
			"podIdentifier", podIdentifier)
		return false
	}
	p.dedup.clear(podIdentifier)
	return true
}

// realignExtraFeatures converts per-engine-block extra features to per-canonical-block
// granularity so that len(result) matches the canonical chunk count expected by
// TokensToKVBlockKeys.
//
// For 1:many (engine BS > canonical BS): each engine block's features are replicated
// to all its constituent canonical sub-blocks.
// For many:1 (engine BS < canonical BS): features from multiple engine blocks are
// merged (union of MMHashes) into each canonical block.
//
// When all entries are nil (text-only prompts), this simply produces a nil-filled
// slice of the correct length.
func realignExtraFeatures(engineFeatures []*kvblock.BlockExtraFeatures, canonicalBlockCount int) []*kvblock.BlockExtraFeatures {
	engineBlockCount := len(engineFeatures)
	if canonicalBlockCount == 0 {
		return nil
	}
	if engineBlockCount == 0 || engineBlockCount == canonicalBlockCount {
		return engineFeatures
	}

	canonical := make([]*kvblock.BlockExtraFeatures, canonicalBlockCount)

	if engineBlockCount < canonicalBlockCount {
		// 1:many -> replicate each engine feature to its canonical sub-blocks
		for i := range canonicalBlockCount {
			engineIdx := i * engineBlockCount / canonicalBlockCount
			canonical[i] = engineFeatures[engineIdx]
		}
	} else {
		// many:1 -> merge constituent engine features into each canonical block
		for i, ef := range engineFeatures {
			canonicalIdx := i * canonicalBlockCount / engineBlockCount
			if ef == nil {
				continue
			}
			if canonical[canonicalIdx] == nil {
				canonical[canonicalIdx] = &kvblock.BlockExtraFeatures{}
			}
			canonical[canonicalIdx].MMHashes = append(
				canonical[canonicalIdx].MMHashes, ef.MMHashes...)
		}
	}

	return canonical
}

// handleDeviceTierUpdate handles offloading/location-only events (e.g., DeviceTier=CPU
// with no tokens). It resolves existing request keys from the engine→request mapping and
// adds the new PodEntry so the EPP tracks which device tiers hold each block.
//
// It returns true only when at least one engine key resolved and the resulting
// PodEntry was added to the index, so the caller knows the store took effect
// and can reference-count it.
func (p *Pool) handleDeviceTierUpdate(
	ctx context.Context, tokens []uint32, engineKeys []kvblock.BlockHash,
	podEntries []kvblock.PodEntry, podIdentifier, deviceTier string, snapshotGeneration uint64,
) bool {
	debugLogger := log.FromContext(ctx).V(logging.DEBUG)

	// Only attempt resolution when tokens are truly absent; partial-block
	// events (tokens < blockSize) should just be skipped.
	if len(tokens) != 0 || len(engineKeys) == 0 {
		return false
	}

	seen := make(map[kvblock.BlockHash]struct{})
	var resolvedKeys []kvblock.BlockHash
	for _, ek := range engineKeys {
		rk, err := p.index.GetRequestKey(ctx, ek)
		if err != nil {
			continue
		}
		if _, ok := seen[rk]; !ok {
			seen[rk] = struct{}{}
			resolvedKeys = append(resolvedKeys, rk)
		}
	}

	if len(resolvedKeys) == 0 {
		debugLogger.Info("no indexed engine keys found for device-tier update, skipping",
			"podIdentifier", podIdentifier, "engineKeyCount", len(engineKeys))
		return false
	}

	if err := p.index.Add(ctx, nil, resolvedKeys, podEntries); err != nil {
		debugLogger.Error(err, "Failed to add device-tier update to index",
			"podIdentifier", podIdentifier, "deviceTier", deviceTier)
		p.notifyStreamEvent(podIdentifier, StreamEventProcessingFailure, snapshotGeneration)
		return false
	}
	return true
}

// processEventBatch processes a batch of events using type switches.
func (p *Pool) processEventBatch(ctx context.Context, batch *EventBatch, podIdentifier, modelName string) {
	p.processEventBatchWithGeneration(ctx, batch, podIdentifier, modelName, 0)
}

func (p *Pool) processEventBatchWithGeneration(
	ctx context.Context, batch *EventBatch, podIdentifier, modelName string, snapshotGeneration uint64,
) {
	debugLogger := log.FromContext(ctx).V(logging.DEBUG)
	debugLogger.V(logging.TRACE).Info("Processing event batch",
		"podID", podIdentifier,
		"modelName", modelName,
		"eventCount", len(batch.Events))

	// Process each event in the batch
	for _, genericEvent := range batch.Events {
		switch ev := genericEvent.(type) {
		case *BlockStoredEvent:
			deviceTier := normalizeDeviceTier(ev.DeviceTier)

			// Scope for reference-counting this store against duplicate removes.
			// Mirrors the index eviction identity (pod, tier, group); DP rank is
			// the sentinel until PR #370 makes the index DP-aware.
			storeScope := blockScope{
				podIdentifier:    podIdentifier,
				deviceTier:       deviceTier,
				groupIdx:         groupIdxOrNoGroup(ev.GroupIdx),
				dataParallelRank: noDataParallelRank,
			}

			// Use LoRA name as model identifier if available, otherwise fall back to base model name.
			effectiveModelName := modelName
			if ev.LoraName != nil && *ev.LoraName != "" {
				effectiveModelName = *ev.LoraName
			}

			// Create PodEntry for this specific event's device tier.
			podEntries := []kvblock.PodEntry{{PodIdentifier: podIdentifier, DeviceTier: deviceTier}}
			if ev.GroupIdx != nil {
				g := kvblock.GroupID(*ev.GroupIdx)
				if ev.KVCacheSpecKind == "" {
					if meta, found := p.groupCatalog.Get(podIdentifier, g); found {
						ev.KVCacheSpecKind = KVCacheSpecKind(meta.Kind)
					}
				} else {
					p.groupCatalog.Learn(podIdentifier, g, kvblock.GroupMetadata{
						Kind:              string(ev.KVCacheSpecKind),
						BlockSize:         ev.BlockSize,
						SlidingWindowSize: ev.KVCacheSpecSlidingWindowSize,
					})
				}
				podEntries[0].HasGroup = true
				podEntries[0].GroupIdx = g
			}

			if digestible, reason := blockStoredEventDigestible(ev); !digestible {
				metrics.KVEventStoresSkipped.WithLabelValues(cacheKindLabel(ev.KVCacheSpecKind), reason).Inc()
				log.FromContext(ctx).V(logging.TRACE).Info("Skipping KV cache store event",
					"podIdentifier", podIdentifier,
					"groupIdx", ev.GroupIdx,
					"cacheKind", ev.KVCacheSpecKind,
					"reason", reason,
					"numTokens", len(ev.Tokens),
					"numBlockHashes", len(ev.BlockHashes),
					"blockSize", ev.BlockSize)
				if reason != "unsupported_cache_kind" {
					p.notifyStreamEvent(podIdentifier, StreamEventProcessingFailure, snapshotGeneration)
				}
				continue
			}

			engineKeys := make([]kvblock.BlockHash, len(ev.BlockHashes))
			for i, hash := range ev.BlockHashes {
				engineKeys[i] = kvblock.BlockHash(hash)
			}

			parentRequestKey := kvblock.EmptyBlockHash
			if ev.ParentHash != 0 {
				parentEngineKey := kvblock.BlockHash(ev.ParentHash)
				key, err := p.index.GetRequestKey(ctx, parentEngineKey)
				if err != nil {
					debugLogger.Error(err, "Failed to get request key for parent block",
						"parentEngineKey", parentEngineKey,
						"effectiveModelName", effectiveModelName,
						"groupIdx", ev.GroupIdx,
						"cacheKind", ev.KVCacheSpecKind,
						"numTokens", len(ev.Tokens),
						"numBlockHashes", len(ev.BlockHashes),
						"blockSize", ev.BlockSize)
					p.notifyStreamEvent(podIdentifier, StreamEventMissingParent, snapshotGeneration)
					continue
				}
				parentRequestKey = key
			}

			var extraFeatures []*kvblock.BlockExtraFeatures
			if ev.ExtraKeys != nil {
				var err error
				extraFeatures, err = kvblock.ParseRawExtraKeys(ev.ExtraKeys)
				if err != nil {
					debugLogger.Error(err, "Failed to parse extra keys",
						"podIdentifier", podIdentifier)
					p.notifyStreamEvent(podIdentifier, StreamEventProcessingFailure, snapshotGeneration)
					continue
				}
			}

			// Realign extraFeatures from engine-block granularity to canonical-block
			// granularity. ParseRawExtraKeys returns one entry per engine block, but
			// TokensToKVBlockKeys expects one entry per canonical block.
			if extraFeatures != nil {
				canonicalBlockCount := len(ev.Tokens) / p.tokenProcessor.BlockSize()
				if canonicalBlockCount == 0 {
					// Tokens don't fill a complete canonical block; no realignment needed
					// since TokensToKVBlockKeys will produce zero keys anyway.
					extraFeatures = nil
				} else if len(extraFeatures) != canonicalBlockCount {
					extraFeatures = realignExtraFeatures(extraFeatures, canonicalBlockCount)
				}
			}

			traceLogger := log.FromContext(ctx).V(logging.TRACE)
			if traceLogger.Enabled() {
				nonNil := 0
				for _, ef := range extraFeatures {
					if ef != nil {
						nonNil++
					}
				}
				traceLogger.Info("BlockStored extra_features",
					"podIdentifier", podIdentifier,
					"hasExtraKeys", ev.ExtraKeys != nil,
					"parsedBlockCount", len(extraFeatures),
					"nonNilBlocks", nonNil,
					"numTokens", len(ev.Tokens),
					"numEngineKeys", len(ev.BlockHashes))
				for bIdx, ef := range extraFeatures {
					if ef != nil {
						traceLogger.Info("BlockStored block extra",
							"podIdentifier", podIdentifier,
							"blockIdx", bIdx,
							"mmHashes", fmt.Sprintf("%+v", ef.MMHashes))
					}
				}
			}

			// Compute request keys at canonical block size (= BlockSize)
			requestKeys, err := p.tokenProcessor.TokensToKVBlockKeys(
				parentRequestKey, ev.Tokens, effectiveModelName, extraFeatures)
			if err != nil {
				debugLogger.Error(err, "Failed to generate request keys",
					"podIdentifier", podIdentifier, "effectiveModelName", effectiveModelName)
				p.notifyStreamEvent(podIdentifier, StreamEventProcessingFailure, snapshotGeneration)
				continue
			}

			if len(requestKeys) == 0 {
				if p.handleDeviceTierUpdate(
					ctx, ev.Tokens, engineKeys, podEntries, podIdentifier, deviceTier, snapshotGeneration,
				) {
					p.dedup.trackStore(storeScope, ev.BlockHashes)
				}
				continue
			}

			// Index.Add infers the engine->request mapping from the ratio of
			// len(engineKeys) to len(requestKeys) (1:1, many:1, or 1:many).
			if err := p.index.Add(ctx, engineKeys, requestKeys, podEntries); err != nil {
				debugLogger.Error(err, "Failed to add event to index",
					"podIdentifier", podIdentifier, "event", ev)
				p.notifyStreamEvent(podIdentifier, StreamEventProcessingFailure, snapshotGeneration)
				continue
			}
			p.dedup.trackStore(storeScope, ev.BlockHashes)

		case *BlockRemovedEvent:
			deviceTier := normalizeDeviceTier(ev.DeviceTier)
			if ev.GroupIdx != nil {
				groupIdx := kvblock.GroupID(*ev.GroupIdx)
				meta, found := p.groupCatalog.Get(podIdentifier, groupIdx)
				if !found || !isPrefixIndexableSpecKind(KVCacheSpecKind(meta.Kind)) {
					reason := "unsupported_cache_kind"
					if !found {
						reason = "unknown_group"
					}
					metrics.KVEventRemovalsSkipped.WithLabelValues(
						cacheKindLabel(KVCacheSpecKind(meta.Kind)), reason).Inc()
					log.FromContext(ctx).V(logging.TRACE).Info("Skipping KV cache remove event",
						"podIdentifier", podIdentifier,
						"groupIdx", groupIdx,
						"cacheKind", meta.Kind,
						"groupKnown", found)
					if !found {
						p.notifyStreamEvent(podIdentifier, StreamEventProcessingFailure, snapshotGeneration)
					}
					continue
				}
			}

			// Create PodEntry for this specific event's device tier.
			podEntries := []kvblock.PodEntry{{PodIdentifier: podIdentifier, DeviceTier: deviceTier}}
			if ev.GroupIdx != nil {
				podEntries[0].HasGroup = true
				podEntries[0].GroupIdx = kvblock.GroupID(*ev.GroupIdx)
			}

			// Reference-count duplicate removes: vLLM chunk-mode offloading can
			// re-announce a shared constituent hash across overlapping chunks, so
			// only forward a hash to the index once no outstanding store still
			// references it. Unknown hashes pass through (Evict is a no-op).
			removeScope := blockScope{
				podIdentifier:    podIdentifier,
				deviceTier:       deviceTier,
				groupIdx:         groupIdxOrNoGroup(ev.GroupIdx),
				dataParallelRank: noDataParallelRank,
			}
			hashesToEvict := p.dedup.filterRemove(removeScope, ev.BlockHashes)

			// Observe how many constituent block hashes were forwarded vs.
			// suppressed (these count block hashes, not BlockRemoved events).
			if forwarded := len(hashesToEvict); forwarded > 0 {
				metrics.DedupRemovedHashesForwarded.Add(float64(forwarded))
			}
			if suppressed := len(ev.BlockHashes) - len(hashesToEvict); suppressed > 0 {
				metrics.DedupRemovedHashesSuppressed.Add(float64(suppressed))
				log.FromContext(ctx).V(logging.TRACE).Info("Suppressed duplicate block removals",
					"podIdentifier", podIdentifier, "deviceTier", deviceTier,
					"received", len(ev.BlockHashes), "forwarded", len(hashesToEvict), "suppressed", suppressed)
			}

			// Iterate over the surviving hashes and evict each key.
			// The Index handles engine->request key resolution internally for both
			// 1:1 (legacy) and 1:many (canonical) mappings.
			for _, hash := range hashesToEvict {
				engineKey := kvblock.BlockHash(hash)
				if err := p.index.Evict(ctx, engineKey, kvblock.EngineKey, podEntries); err != nil {
					debugLogger.Error(err, "Failed to evict engine key from index",
						"podIdentifier", podIdentifier, "engineKey", engineKey)
					p.notifyStreamEvent(podIdentifier, StreamEventProcessingFailure, snapshotGeneration)
					continue
				}
			}

		case *AllBlocksClearedEvent:
			debugLogger.Info("All blocks cleared event received",
				"podIdentifier", podIdentifier,
				"deviceTier", ev.DeviceTier,
				"modelName", modelName)

			// AllBlocksCleared is pod-wide: vLLM reset its entire prefix cache
			// (e.g. after an RLHF weight update), so drop every entry for this pod
			// across all tiers. vLLM and SGLang both emit it with no tier annotation.
			// Index.Clear cannot scope by tier, so if an engine ever starts setting
			// DeviceTier (a tier-scoped reset), this would over-wipe the other tiers.
			// Surface that here so the regression does not pass silently.
			if ev.DeviceTier != "" {
				debugLogger.Info("AllBlocksCleared carried a device tier; clearing all tiers "+
					"anyway (tier-scoped clear is not supported)",
					"podIdentifier", podIdentifier, "deviceTier", ev.DeviceTier)
			}
			if p.clearPod(ctx, podIdentifier) {
				// A historical clear inside a full replay is not the endpoint's
				// final state: later replay events may repopulate the cache or fail.
				// Only a live clear is authoritative by itself; a clean replay is
				// declared authoritative by its ordered snapshot end marker.
				if snapshotGeneration == 0 {
					p.notifyStreamEvent(podIdentifier, StreamEventKnownEmpty, 0)
				}
			} else {
				p.notifyStreamEvent(podIdentifier, StreamEventProcessingFailure, snapshotGeneration)
			}

		default:
			debugLogger.Info("Unknown event", "podIdentifier", podIdentifier, "event", genericEvent)
		}
	}
}
