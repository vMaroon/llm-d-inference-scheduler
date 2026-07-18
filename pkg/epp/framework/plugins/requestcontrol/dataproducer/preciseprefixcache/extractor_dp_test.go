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
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8stypes "k8s.io/apimachinery/pkg/types"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
)

// dpRankEndpoint builds one per-rank endpoint of pod "pod-a" (the shape the
// datastore emits for multi-port pools: NamespacedName carries the rank
// suffix, PodName the shared pod).
func dpRankEndpoint(rank int) fwkdl.Endpoint {
	return fwkdl.NewEndpoint(&fwkdl.EndpointMetadata{
		NamespacedName: k8stypes.NamespacedName{Namespace: "ns", Name: fmt.Sprintf("pod-a-rank-%d", rank)},
		PodName:        "pod-a",
		Address:        "10.0.0.1",
		Port:           strconv.Itoa(8000 + rank),
		RankIndex:      rank,
	}, nil)
}

// DataParallelSizeTotal > 1 subscribes the endpoint's pod across the full
// global rank range: publishers bind SocketPort + GLOBAL rank, so every pod
// dials SocketPort+i for i in [0, total). A pod's per-rank endpoints collapse
// onto one pod-scoped subscriber per port offset, and a delete tears the
// whole range down.
func TestProducer_ExtractEndpoint_DataParallelRangeSubscribesAllRanks(t *testing.T) {
	ctx := discardCtx(t)
	p := newExtractorProducer(true)
	p.kvEventsConfig.PodDiscoveryConfig.DataParallelSizeTotal = 4
	defer p.subscribersManager.Shutdown(ctx)

	for rank := 0; rank < 2; rank++ {
		require.NoError(t, p.Extract(ctx, fwkdl.EndpointEvent{
			Type:     fwkdl.EventAddOrUpdate,
			Endpoint: dpRankEndpoint(rank),
		}))
	}

	ids, endpoints := p.subscribersManager.GetActiveSubscribers()
	assert.ElementsMatch(t, []string{
		"ns/pod-a#dp-0", "ns/pod-a#dp-1", "ns/pod-a#dp-2", "ns/pod-a#dp-3",
	}, ids, "the pod's endpoints must collapse onto one subscriber per port offset")
	assert.ElementsMatch(t, []string{
		"tcp://10.0.0.1:5557", "tcp://10.0.0.1:5558", "tcp://10.0.0.1:5559", "tcp://10.0.0.1:5560",
	}, endpoints, "every port in [SocketPort, SocketPort+total) must be dialed")

	require.NoError(t, p.Extract(ctx, fwkdl.EndpointEvent{
		Type:     fwkdl.EventDelete,
		Endpoint: dpRankEndpoint(0),
	}))
	ids, _ = p.subscribersManager.GetActiveSubscribers()
	assert.Empty(t, ids, "deleting a pod endpoint must tear down the pod's subscriber range")
}

// DataParallelSizeTotal at its default (1) keeps the per-endpoint
// SocketPort + RankIndex behavior byte-identical.
func TestProducer_ExtractEndpoint_DataParallelSizeTotalDefaultKeepsRankOffset(t *testing.T) {
	ctx := discardCtx(t)
	p := newExtractorProducer(true)
	defer p.subscribersManager.Shutdown(ctx)

	require.NoError(t, p.Extract(ctx, fwkdl.EndpointEvent{
		Type:     fwkdl.EventAddOrUpdate,
		Endpoint: dpRankEndpoint(1),
	}))

	ids, endpoints := p.subscribersManager.GetActiveSubscribers()
	assert.Equal(t, []string{"ns/pod-a-rank-1"}, ids)
	assert.Equal(t, []string{"tcp://10.0.0.1:5558"}, endpoints)
}
