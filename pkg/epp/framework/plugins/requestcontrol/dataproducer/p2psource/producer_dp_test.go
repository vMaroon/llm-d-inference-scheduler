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

package p2psource

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-router/pkg/common/routing"
	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
	"github.com/llm-d/llm-d-router/test/utils"
)

// dpEndpoint builds a candidate whose PrefixCacheMatchInfo carries both a
// cached-block count and a global data-parallel rank annotation.
func dpEndpoint(p *Producer, name, address string, cachedBlocks, globalDPRank int) scheduling.Endpoint {
	e := scheduling.NewEndpoint(&fwkdl.EndpointMetadata{
		NamespacedName: k8stypes.NamespacedName{Name: name},
		Address:        address,
		Port:           "8080",
	}, nil, nil)
	e.Put(p.prefixMatchDataKey.String(),
		attrprefix.NewPrefixCacheMatchInfo(cachedBlocks, 4, testBlockSize).
			WithCachedBlockCount(cachedBlocks).
			WithGlobalDPRank(globalDPRank))
	return e
}

// Factory wires p2pPortBase; default is 0 (header off).
func TestPluginFactory_P2PPortBase(t *testing.T) {
	def, err := PluginFactory("d", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, def.(*Producer).p2pPortBase)

	dec := json.NewDecoder(strings.NewReader(`{"p2pPortBase": 7777}`))
	custom, err := PluginFactory("c", dec, nil)
	require.NoError(t, err)
	assert.Equal(t, 7777, custom.(*Producer).p2pPortBase)

	dec = json.NewDecoder(strings.NewReader(`{"p2pPortBase": -1}`))
	_, err = PluginFactory("neg", dec, nil)
	require.Error(t, err)
}

// Produce captures the best peer's global data-parallel rank alongside its
// host:port.
func TestProduce_CapturesGlobalDPRank(t *testing.T) {
	ctx := utils.NewTestContext(t)
	p := New("test", Config{MinCachedTokenDelta: 1, P2PPortBase: 7777})

	req := &scheduling.InferenceRequest{RequestID: "req-dp-stash"}
	eps := []scheduling.Endpoint{
		endpoint(p, "pod-a", "10.0.0.1", 1), // no rank annotation
		dpEndpoint(p, "pod-b", "10.0.0.2", 3, 11),
	}
	require.NoError(t, p.Produce(ctx, req, eps))

	best, ok := scheduling.ReadRequestAttribute[*bestMatchPeer](req, p.attrKey())
	require.True(t, ok)
	assert.Equal(t, "10.0.0.2:8080", best.hostPort)
	require.True(t, best.hasGlobalDPRank)
	assert.Equal(t, 11, best.globalDPRank)
}

// Best peer with global rank 11 and p2pPortBase 7777: both headers are set,
// the p2p port being base + rank.
func TestPreRequest_SetsP2PPortHeader(t *testing.T) {
	ctx := utils.NewTestContext(t)
	p := New("test", Config{MinCachedTokenDelta: 1, P2PPortBase: 7777})

	req := &scheduling.InferenceRequest{RequestID: "req-dp-hdr", Headers: map[string]string{}}
	require.NoError(t, p.Produce(ctx, req, []scheduling.Endpoint{
		endpoint(p, "pod-a", "10.0.0.1", 1),
		dpEndpoint(p, "pod-b", "10.0.0.2", 3, 11),
	}))

	p.PreRequest(ctx, req, decodeOnly(endpoint(p, "pod-a", "10.0.0.1", 1)))

	assert.Equal(t, "10.0.0.2:8080", req.Headers[routing.KVCacheSourceHeader])
	assert.Equal(t, "7788", req.Headers[routing.KVCacheSourceP2PPortHeader])
}

// p2pPortBase unset (default 0): only the legacy host:port header is set.
func TestPreRequest_P2PPortBaseUnset_OnlyLegacyHeader(t *testing.T) {
	ctx := utils.NewTestContext(t)
	p := New("test", Config{MinCachedTokenDelta: 1})

	req := &scheduling.InferenceRequest{RequestID: "req-dp-off", Headers: map[string]string{}}
	require.NoError(t, p.Produce(ctx, req, []scheduling.Endpoint{
		dpEndpoint(p, "pod-b", "10.0.0.2", 3, 11),
	}))

	p.PreRequest(ctx, req, decodeOnly(endpoint(p, "pod-a", "10.0.0.1", 1)))

	assert.Equal(t, "10.0.0.2:8080", req.Headers[routing.KVCacheSourceHeader])
	assert.NotContains(t, req.Headers, routing.KVCacheSourceP2PPortHeader)
}

// Best peer without a resolved rank: the p2p port would be a guess, so only
// the legacy header is set and the sidecar falls back to its flag.
func TestPreRequest_RankUnknown_OnlyLegacyHeader(t *testing.T) {
	ctx := utils.NewTestContext(t)
	p := New("test", Config{MinCachedTokenDelta: 1, P2PPortBase: 7777})

	req := &scheduling.InferenceRequest{RequestID: "req-dp-norank", Headers: map[string]string{}}
	require.NoError(t, p.Produce(ctx, req, []scheduling.Endpoint{
		endpoint(p, "pod-b", "10.0.0.2", 3),
	}))

	p.PreRequest(ctx, req, decodeOnly(endpoint(p, "pod-a", "10.0.0.1", 1)))

	assert.Equal(t, "10.0.0.2:8080", req.Headers[routing.KVCacheSourceHeader])
	assert.NotContains(t, req.Headers, routing.KVCacheSourceP2PPortHeader)
}

// Self-match suppression covers both headers: pulling from the pod computing
// the prefix stays a no-op with p2pPortBase configured.
func TestPreRequest_SelfMatch_NoP2PPortHeader(t *testing.T) {
	ctx := utils.NewTestContext(t)
	p := New("test", Config{MinCachedTokenDelta: 1, P2PPortBase: 7777})

	req := &scheduling.InferenceRequest{RequestID: "req-dp-self", Headers: map[string]string{}}
	require.NoError(t, p.Produce(ctx, req, []scheduling.Endpoint{
		dpEndpoint(p, "pod-a", "10.0.0.1", 2, 11),
	}))

	p.PreRequest(ctx, req, decodeOnly(dpEndpoint(p, "pod-a", "10.0.0.1", 2, 11)))

	assert.NotContains(t, req.Headers, routing.KVCacheSourceHeader)
	assert.NotContains(t, req.Headers, routing.KVCacheSourceP2PPortHeader)
}

// Inbound (spoofed) p2p-port header is removed even when no best-match
// attribute was stashed.
func TestPreRequest_DeletesInboundP2PPortHeader(t *testing.T) {
	ctx := utils.NewTestContext(t)
	p := New("test", Config{MinCachedTokenDelta: 1})

	req := &scheduling.InferenceRequest{
		RequestID: "req-dp-spoof",
		Headers:   map[string]string{routing.KVCacheSourceP2PPortHeader: "9999"},
	}

	p.PreRequest(ctx, req, decodeOnly(endpoint(p, "pod-a", "10.0.0.1", 0)))

	assert.NotContains(t, req.Headers, routing.KVCacheSourceP2PPortHeader)
}
