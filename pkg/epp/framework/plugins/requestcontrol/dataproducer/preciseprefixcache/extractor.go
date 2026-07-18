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

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
)

var _ fwkdl.EndpointExtractor = &Producer{}

// Extract processes endpoint lifecycle events emitted by the
// endpoint-notification-source: add/update installs a per-pod ZMQ KV-events
// subscriber, delete tears one down. No-op unless per-pod discovery is
// enabled.
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
		if err := p.ensureSubscriber(ctx, meta); err != nil {
			return err
		}
		logger.V(logging.DEBUG).Info("Adding subscriber", "endpoint", endpointKey)
	case fwkdl.EventDelete:
		if total := p.kvEventsConfig.PodDiscoveryConfig.DataParallelSizeTotal; total > 1 {
			for i := 0; i < total; i++ {
				p.subscribersManager.RemoveSubscriber(ctx, podRangeSubscriberKey(meta, i))
			}
		} else {
			p.subscribersManager.RemoveSubscriber(ctx, endpointKey)
		}
		if meta.Address != "" {
			if err := p.kvCacheIndexer.KVBlockIndex().Clear(ctx, fmt.Sprintf("%s:%s", meta.Address, meta.Port)); err != nil {
				logger.Error(err, "Failed to clear index entries for removed endpoint",
					"endpoint", endpointKey, "address", meta.Address, "port", meta.Port)
			}
		}
		logger.V(logging.DEBUG).Info("Removed KV-events subscriber", "endpoint", endpointKey)
	}
	return nil
}

// ensureSubscriber idempotently installs KV-events subscribers for the given
// endpoint. With DataParallelSizeTotal <= 1 it dials SocketPort + RankIndex to
// match standard inference-engine port offsetting (one ZMQ PUB socket per DP
// rank on the same pod IP). With DataParallelSizeTotal > 1 it subscribes the
// endpoint's pod across the full global rank range instead: publishers bind
// SocketPort + GLOBAL rank, and a pod's global rank range is not knowable
// from a single endpoint.
func (p *Producer) ensureSubscriber(ctx context.Context, meta *fwkdl.EndpointMetadata) error {
	if meta == nil || meta.Address == "" {
		return nil
	}
	if p.kvEventsConfig.PodDiscoveryConfig.DataParallelSizeTotal > 1 {
		return p.ensurePodRangeSubscribers(ctx, meta)
	}
	endpointKey := meta.NamespacedName.String()
	port := p.kvEventsConfig.PodDiscoveryConfig.SocketPort + meta.GetRankIndex()
	zmqEndpoint := fmt.Sprintf("tcp://%s:%d", meta.Address, port)

	logger := log.FromContext(ctx).WithName(p.typedName.String())
	// subscriberCtx is plugin-lifetime; caller ctx would tear subscribers
	// down on request completion.
	if err := p.subscribersManager.EnsureSubscriber(p.subscriberCtx, endpointKey,
		zmqEndpoint, p.kvEventsConfig.TopicFilter, true); err != nil {
		logger.Error(err, "Failed to ensure KV-events subscriber for endpoint",
			"endpoint", endpointKey, "address", meta.Address)
		return fmt.Errorf("ensure subscriber for %s: %w", endpointKey, err)
	}
	logger.V(logging.DEBUG).Info("Ensured KV-events subscriber", "endpoint", endpointKey, "zmq", zmqEndpoint)
	return nil
}

// ensurePodRangeSubscribers subscribes the endpoint's pod at every ZMQ port
// in [SocketPort, SocketPort+DataParallelSizeTotal). ZMQ connects to
// never-bound ports are lazy and idle harmlessly, so dialing the full range
// on every pod covers any global rank placement without knowing it.
func (p *Producer) ensurePodRangeSubscribers(ctx context.Context, meta *fwkdl.EndpointMetadata) error {
	logger := log.FromContext(ctx).WithName(p.typedName.String())
	discovery := p.kvEventsConfig.PodDiscoveryConfig
	for i := 0; i < discovery.DataParallelSizeTotal; i++ {
		key := podRangeSubscriberKey(meta, i)
		zmqEndpoint := fmt.Sprintf("tcp://%s:%d", meta.Address, discovery.SocketPort+i)
		// subscriberCtx is plugin-lifetime; caller ctx would tear subscribers
		// down on request completion.
		if err := p.subscribersManager.EnsureSubscriber(p.subscriberCtx, key,
			zmqEndpoint, p.kvEventsConfig.TopicFilter, true); err != nil {
			logger.Error(err, "Failed to ensure KV-events subscriber for pod rank range",
				"subscriber", key, "address", meta.Address)
			return fmt.Errorf("ensure subscriber for %s: %w", key, err)
		}
	}
	logger.V(logging.DEBUG).Info("Ensured pod rank-range KV-events subscribers",
		"pod", meta.PodName, "address", meta.Address, "count", discovery.DataParallelSizeTotal)
	return nil
}

// podRangeSubscriberKey identifies one pod-scoped subscriber slot. Keys
// derive from the pod (not the endpoint), so a pod's per-rank endpoints
// collapse onto a single subscriber per ZMQ port offset and duplicate
// endpoint events become EnsureSubscriber no-ops.
func podRangeSubscriberKey(meta *fwkdl.EndpointMetadata, offset int) string {
	podName := meta.PodName
	if podName == "" {
		podName = meta.NamespacedName.Name
	}
	return fmt.Sprintf("%s/%s#dp-%d", meta.NamespacedName.Namespace, podName, offset)
}
