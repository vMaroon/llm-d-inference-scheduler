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

// Package p2psource emits the KV cache source header: the candidate pod
// holding the most cached prefix KV blocks for the request, for the routing
// sidecar to pull from over the P2P connector instead of recomputing them.
package p2psource

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	"github.com/llm-d/llm-d-router/pkg/common/routing"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
)

const (
	// PluginType is the registered type name of the p2p-source-producer.
	PluginType = "p2p-source-producer"

	// defaultPrefillProfile is the disaggregation prefill profile name; when
	// that profile's result carries an endpoint, that endpoint computes the
	// prefix. Operators who rename it (disagg-profile-handler profiles.prefill)
	// must set prefillProfileName to match.
	defaultPrefillProfile = "prefill"

	defaultMinCachedTokenDelta = 1
)

// Config configures the p2p-source-producer.
type Config struct {
	// PrefixMatchInfoProducerName is the name of the data producer that
	// produces PrefixCacheMatchInfo. Empty selects the default producer.
	PrefixMatchInfoProducerName string `json:"prefixMatchInfoProducerName,omitempty"`
	// MinCachedTokenDelta is the minimum number of cached prompt tokens the
	// best peer must hold beyond the pod computing the prefix for the header
	// to be set. Must be >= 1; defaults to 1.
	MinCachedTokenDelta int `json:"minCachedTokenDelta,omitempty"`
	// PrefillProfileName is the disaggregation prefill profile name, matching
	// the disagg-profile-handler's profiles.prefill. Empty defaults to
	// "prefill".
	PrefillProfileName string `json:"prefillProfileName,omitempty"`
	// P2PPortBase is the base port of the engines' OffloadingConnector P2P
	// tier; each data-parallel rank binds P2PPortBase + its GLOBAL rank. When
	// > 0 and the best peer's global rank is known, PreRequest also sets
	// routing.KVCacheSourceP2PPortHeader to the source rank's P2P port so the
	// sidecar pulls from the exact rank holding the blocks. 0 disables the
	// header (the sidecar falls back to its --p2p-connector-port flag).
	P2PPortBase int `json:"p2pPortBase,omitempty"`
}

// compile-time type assertions
var (
	_ requestcontrol.DataProducer = &Producer{}
	_ requestcontrol.PreRequest   = &Producer{}
)

// Producer stashes the candidate endpoint holding the most cached prefix
// tokens during Produce, and in PreRequest sets routing.KVCacheSourceHeader
// to that peer when it out-caches the pod computing the prefix (the prefill
// endpoint under P/D disaggregation, the primary endpoint otherwise) by at
// least minCachedTokenDelta tokens.
type Producer struct {
	typedName           plugin.TypedName
	prefixMatchDataKey  plugin.DataKey
	minCachedTokenDelta int
	prefillProfile      string
	p2pPortBase         int
	attrKeyValue        string
}

// PluginFactory parses the raw plugin configuration and returns a configured
// Producer.
func PluginFactory(name string, rawParameters *json.Decoder, _ plugin.Handle) (plugin.Plugin, error) {
	cfg := Config{MinCachedTokenDelta: defaultMinCachedTokenDelta}
	if rawParameters != nil {
		if err := rawParameters.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("failed to parse %s plugin config: %w", PluginType, err)
		}
	}
	if cfg.MinCachedTokenDelta < 1 {
		return nil, fmt.Errorf("%s: minCachedTokenDelta must be >= 1, got %d", PluginType, cfg.MinCachedTokenDelta)
	}
	if cfg.P2PPortBase < 0 {
		return nil, fmt.Errorf("%s: p2pPortBase must be >= 0, got %d", PluginType, cfg.P2PPortBase)
	}
	return New(name, cfg), nil
}

// New constructs a p2p-source-producer bound to the configured
// PrefixCacheMatchInfo producer name.
func New(name string, cfg Config) *Producer {
	prefillProfile := cfg.PrefillProfileName
	if prefillProfile == "" {
		prefillProfile = defaultPrefillProfile
	}
	return &Producer{
		typedName:           plugin.TypedName{Type: PluginType, Name: name},
		prefixMatchDataKey:  attrprefix.PrefixCacheMatchInfoDataKey.WithNonEmptyProducerName(cfg.PrefixMatchInfoProducerName),
		minCachedTokenDelta: cfg.MinCachedTokenDelta,
		prefillProfile:      prefillProfile,
		p2pPortBase:         cfg.P2PPortBase,
		attrKeyValue:        fmt.Sprintf("%s/%s/best-match", PluginType, name),
	}
}

// TypedName returns the plugin's registered type and name.
func (p *Producer) TypedName() plugin.TypedName { return p.typedName }

// Produces declares no produced data keys; the best-match result is carried
// as a request attribute consumed by this plugin's own PreRequest.
func (p *Producer) Produces() map[plugin.DataKey]any { return map[plugin.DataKey]any{} }

// Consumes declares the PrefixCacheMatchInfo dependency so the data-layer
// DAG orders the producing plugin before this one.
func (p *Producer) Consumes() plugin.DataDependencies {
	return plugin.DataDependencies{
		Required: map[plugin.DataKey]any{p.prefixMatchDataKey: attrprefix.PrefixCacheMatchInfo{}},
	}
}

// bestMatchPeer is the candidate endpoint holding the most cached prompt
// tokens for the request, stashed as a request attribute so PreRequest can
// compare it against the scheduled endpoint. globalDPRank carries the peer
// engine's global data-parallel rank when its PrefixCacheMatchInfo resolves
// one; hasGlobalDPRank distinguishes rank 0 from "no rank known".
type bestMatchPeer struct {
	hostPort        string
	cachedTokens    int
	globalDPRank    int
	hasGlobalDPRank bool
}

// attrKey returns the request-attribute key carrying the best-match peer,
// name-bound to this plugin instance.
func (p *Producer) attrKey() string {
	return p.attrKeyValue
}

// Produce reads each candidate's PrefixCacheMatchInfo and stashes the
// endpoint holding the most cached prompt tokens on the request. No-op when
// no candidate holds any cached block.
func (p *Producer) Produce(ctx context.Context, request *scheduling.InferenceRequest, endpoints []scheduling.Endpoint) error {
	best := bestMatchPeer{}
	for _, ep := range endpoints {
		md := ep.GetMetadata()
		if md == nil {
			continue
		}
		if cached := p.cachedTokenCount(ep); cached > best.cachedTokens {
			best = bestMatchPeer{
				hostPort:     net.JoinHostPort(md.Address, md.Port),
				cachedTokens: cached,
			}
			if info := p.matchInfo(ep); info != nil {
				best.globalDPRank, best.hasGlobalDPRank = info.GlobalDPRank()
			}
		}
	}
	if best.cachedTokens > 0 {
		request.PutAttribute(p.attrKey(), &best)
	}
	log.FromContext(ctx).WithName(p.typedName.String()).V(logging.TRACE).Info("Produce completed",
		"requestID", request.RequestID, "endpoints", len(endpoints),
		"bestHostPort", best.hostPort, "bestCachedTokens", best.cachedTokens)
	return nil
}

// PreRequest sets routing.KVCacheSourceHeader to the best-match peer stashed
// by Produce when it out-caches the pod computing the prefix by at least
// minCachedTokenDelta tokens. With p2pPortBase configured and the peer's
// global data-parallel rank known, routing.KVCacheSourceP2PPortHeader
// additionally carries the peer rank's P2P tier port
// (p2pPortBase + GLOBAL rank). Any inbound values of both headers are removed.
func (p *Producer) PreRequest(ctx context.Context, request *scheduling.InferenceRequest, schedulingResult *scheduling.SchedulingResult) {
	logger := log.FromContext(ctx).WithName(p.typedName.String()).V(logging.TRACE)
	delete(request.Headers, routing.KVCacheSourceHeader)
	delete(request.Headers, routing.KVCacheSourceP2PPortHeader)

	best, ok := scheduling.ReadRequestAttribute[*bestMatchPeer](request, p.attrKey())
	if !ok {
		logger.Info("no best-match peer stashed", "requestID", request.RequestID)
		return
	}

	computing := schedulingResult.ProfileResults[schedulingResult.PrimaryProfileName]
	if pr, exists := schedulingResult.ProfileResults[p.prefillProfile]; exists && pr != nil && len(pr.TargetEndpoints) > 0 {
		computing = pr
	}
	if computing == nil || len(computing.TargetEndpoints) == 0 {
		return
	}
	endpoint := computing.TargetEndpoints[0]
	md := endpoint.GetMetadata()
	if md == nil {
		return
	}
	computingHostPort := net.JoinHostPort(md.Address, md.Port)
	computingCached := p.cachedTokenCount(endpoint)
	logger.Info("evaluating KV cache source",
		"requestID", request.RequestID, "best", best.hostPort, "bestCachedTokens", best.cachedTokens,
		"computing", computingHostPort, "computingCachedTokens", computingCached)
	// Never emit the header pointing at the computing pod itself. Redundant
	// with the delta check below while minCachedTokenDelta >= 1 (a self-match
	// is delta 0), but explicit against a future lower floor.
	if best.hostPort == computingHostPort {
		return
	}
	if best.cachedTokens-computingCached < p.minCachedTokenDelta {
		return
	}

	if request.Headers == nil {
		request.Headers = map[string]string{}
	}
	request.Headers[routing.KVCacheSourceHeader] = best.hostPort
	// Each rank's P2P tier binds p2pPortBase + GLOBAL rank; without a known
	// rank the header is omitted and the sidecar falls back to its
	// --p2p-connector-port flag.
	if p.p2pPortBase > 0 && best.hasGlobalDPRank {
		request.Headers[routing.KVCacheSourceP2PPortHeader] = strconv.Itoa(p.p2pPortBase + best.globalDPRank)
	}
	logger.Info("set KV cache source header", "requestID", request.RequestID, "value", best.hostPort,
		"p2pPort", request.Headers[routing.KVCacheSourceP2PPortHeader])
}

// matchInfo returns the endpoint's PrefixCacheMatchInfo from the configured
// producer, or nil when absent.
func (p *Producer) matchInfo(ep scheduling.Endpoint) *attrprefix.PrefixCacheMatchInfo {
	raw, ok := ep.Get(p.prefixMatchDataKey.String())
	if !ok {
		return nil
	}
	info, ok := raw.(*attrprefix.PrefixCacheMatchInfo)
	if !ok {
		return nil
	}
	return info
}

// cachedTokenCount returns the endpoint's cached prompt tokens (unweighted
// cached-block count times the block size) from its PrefixCacheMatchInfo,
// or 0 when absent.
func (p *Producer) cachedTokenCount(ep scheduling.Endpoint) int {
	info := p.matchInfo(ep)
	if info == nil {
		return 0
	}
	return info.CachedBlockCount() * info.BlockSizeTokens()
}
