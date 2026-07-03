/*
Copyright 2026 The Kubernetes Authors.

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

// Package sessionresidency provides a DataProducer that publishes event-fed,
// eviction-aware session residency: which engines hold the request's session
// KV, up to which continuation, in engine token units.
//
// It reuses the llm-d-kv-cache KV-events machinery wholesale — the ZMQ
// subscribers, the pod-sharded worker pool, and the block index all run
// unchanged; a SessionView projection is fed in the same worker pass from
// the (session_tag, continuation_id) labels that session-tagged engines echo
// on BlockStored events. Evictions fold through the projection, so residency
// is always "the deepest continuation whose blocks all survive".
//
// The producer publishes the residency for the request's derived session id
// (the chain-identity producer's lineage id — the same value stamped
// outbound as session_tag, so the keyspace is closed), falling back to the
// declared session attribute. The session-coverage scorer consumes it as
// engine-truth reuse.
package sessionresidency

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-kv-cache/pkg/kvcache/kvblock"
	"github.com/llm-d/llm-d-kv-cache/pkg/kvevents"
	"github.com/llm-d/llm-d-kv-cache/pkg/kvevents/engineadapter"

	"github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrsession "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/session"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/sessionresidency/constants"
)

// SessionResidencyProducerType is the plugin type registered with the framework.
const SessionResidencyProducerType = constants.SessionResidencyProducerType

const sweepInterval = time.Minute

// PluginConfig configures the session-residency producer.
type PluginConfig struct {
	// KVEventsConfig configures the KV-events pool and pod discovery,
	// identical to the precise prefix-cache producer's configuration.
	KVEventsConfig *kvevents.Config `json:"kvEventsConfig"`
	// SessionTTLSeconds is the idle time after which a session's view state
	// is dropped. Defaults to 1800.
	SessionTTLSeconds int `json:"sessionTTLSeconds"`
	// MaxSessions caps tracked sessions in the view. Defaults to 100000.
	MaxSessions int `json:"maxSessions"`
}

var (
	_ requestcontrol.DataProducer = &Producer{}
	_ fwkdl.EndpointExtractor     = &Producer{}
)

// Producer feeds a session view from KV events and publishes per-request
// session residency.
type Producer struct {
	typedName          fwkplugin.TypedName
	dk                 fwkplugin.DataKey
	view               *kvevents.InMemorySessionView
	blockIndex         kvblock.Index
	subscribersManager *kvevents.SubscriberManager
	kvEventsConfig     *kvevents.Config
	subscriberCtx      context.Context
}

// Factory builds a Producer from raw plugin parameters.
func Factory(name string, rawParameters *json.Decoder, handle fwkplugin.Handle) (fwkplugin.Plugin, error) {
	config := PluginConfig{KVEventsConfig: kvevents.DefaultConfig()}
	if rawParameters != nil {
		if err := rawParameters.Decode(&config); err != nil {
			return nil, fmt.Errorf("failed to parse %s plugin config: %w", SessionResidencyProducerType, err)
		}
	}
	if config.KVEventsConfig == nil {
		config.KVEventsConfig = kvevents.DefaultConfig()
	}

	p, err := New(handle.Context(), name, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create %s plugin: %w", SessionResidencyProducerType, err)
	}
	return p, nil
}

// New constructs a session-residency producer. The KV-events pool, block
// index, and session view start in background goroutines bound to ctx.
func New(ctx context.Context, name string, config PluginConfig) (*Producer, error) {
	blockIndex, err := kvblock.NewIndex(ctx, kvblock.DefaultIndexConfig())
	if err != nil {
		return nil, fmt.Errorf("failed to create kv-block index: %w", err)
	}
	tokenProcessor, err := kvblock.NewChunkedTokenDatabase(kvblock.DefaultTokenProcessorConfig())
	if err != nil {
		return nil, fmt.Errorf("failed to create token processor: %w", err)
	}

	view := kvevents.NewInMemorySessionView(
		time.Duration(config.SessionTTLSeconds)*time.Second, config.MaxSessions)

	pool := kvevents.NewPool(config.KVEventsConfig, blockIndex, tokenProcessor,
		engineadapter.NewVLLMAdapter(), kvevents.WithSessionView(view))
	pool.Start(ctx)

	subscribersManager := kvevents.NewSubscriberManager(pool)
	if config.KVEventsConfig.ZMQEndpoint != "" {
		if err := subscribersManager.EnsureSubscriber(ctx, "local-subscriber",
			config.KVEventsConfig.ZMQEndpoint, config.KVEventsConfig.TopicFilter, false); err != nil {
			return nil, fmt.Errorf("failed to create local subscriber: %w", err)
		}
	}

	p := &Producer{
		typedName:          fwkplugin.TypedName{Type: SessionResidencyProducerType, Name: name},
		dk:                 attrsession.SessionResidencyDataKey.WithNonEmptyProducerName(SessionResidencyProducerType),
		view:               view,
		blockIndex:         blockIndex,
		subscribersManager: subscribersManager,
		kvEventsConfig:     config.KVEventsConfig,
		subscriberCtx:      ctx,
	}
	if ctx != nil {
		go p.sweep(ctx)
	}
	return p, nil
}

// TypedName returns the plugin's registered type and name.
func (p *Producer) TypedName() fwkplugin.TypedName {
	return p.typedName
}

// Produces declares the SessionResidency attribute written by this producer.
func (p *Producer) Produces() map[fwkplugin.DataKey]any {
	return map[fwkplugin.DataKey]any{p.dk: []attrsession.PodResidency{}}
}

// Produce publishes the event-fed residency for the request's session. The
// derived (chain) id is preferred — it is the value stamped outbound as
// session_tag, so event labels and lookups share one keyspace; the declared
// session attribute is the fallback for deployments without derivation.
func (p *Producer) Produce(_ context.Context, request *fwksched.InferenceRequest, _ []fwksched.Endpoint) error {
	if request == nil {
		return nil
	}
	sid := ""
	if id, ok := attrsession.ReadDerivedSessionID(request); ok && id != "" {
		sid = string(id)
	} else if id, ok := attrsession.ReadSessionID(request); ok && id != "" {
		sid = string(id)
	}
	if sid == "" {
		return nil
	}
	residency := p.view.Residency(sid)
	if len(residency) == 0 {
		return nil
	}
	out := make([]attrsession.PodResidency, 0, len(residency))
	for _, r := range residency {
		out = append(out, attrsession.PodResidency{Pod: r.Pod, Tier: r.Tier, UpTo: r.UpTo, Tokens: r.Tokens})
	}
	request.PutAttribute(p.dk.String(), out)
	return nil
}

// Extract processes endpoint lifecycle events: add/update installs a per-pod
// ZMQ KV-events subscriber, delete tears one down and clears the pod's state
// from both projections. Mirrors the precise prefix-cache producer.
func (p *Producer) Extract(ctx context.Context, event fwkdl.EndpointEvent) error {
	if !p.kvEventsConfig.DiscoverPods || p.kvEventsConfig.PodDiscoveryConfig == nil {
		return nil
	}
	meta := event.Endpoint.GetMetadata()
	if meta == nil || meta.NamespacedName.Name == "" {
		return nil
	}

	logger := log.FromContext(ctx).WithName(p.typedName.String())
	endpointKey := meta.NamespacedName.String()

	switch event.Type {
	case fwkdl.EventAddOrUpdate:
		if meta.Address == "" {
			return nil
		}
		port := p.kvEventsConfig.PodDiscoveryConfig.SocketPort + meta.GetRankIndex()
		zmqEndpoint := fmt.Sprintf("tcp://%s:%d", meta.Address, port)
		if err := p.subscribersManager.EnsureSubscriber(p.subscriberCtx, endpointKey,
			zmqEndpoint, p.kvEventsConfig.TopicFilter, true); err != nil {
			logger.Error(err, "Failed to ensure KV-events subscriber", "endpoint", endpointKey)
			return fmt.Errorf("ensure subscriber for %s: %w", endpointKey, err)
		}
		logger.V(logging.DEBUG).Info("Ensured KV-events subscriber", "endpoint", endpointKey, "zmq", zmqEndpoint)
	case fwkdl.EventDelete:
		p.subscribersManager.RemoveSubscriber(ctx, endpointKey)
		if meta.Address != "" {
			pod := fmt.Sprintf("%s:%s", meta.Address, meta.Port)
			p.view.ClearPod(pod)
			if err := p.blockIndex.Clear(ctx, pod); err != nil {
				logger.Error(err, "Failed to clear block index for removed endpoint", "endpoint", endpointKey)
			}
		}
		logger.V(logging.DEBUG).Info("Removed KV-events subscriber", "endpoint", endpointKey)
	}
	return nil
}

func (p *Producer) sweep(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.view.RemoveExpired()
		}
	}
}
