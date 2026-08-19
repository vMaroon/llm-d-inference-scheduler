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

// Package latencycost produces the per-endpoint LatencyCostInfo attribute:
// the predicted time-to-first-token of the request being scheduled on each
// candidate endpoint, in milliseconds, decomposed into queue-wait, KV-fetch,
// and prefill-compute terms.
//
// The model:
//
//	QueueMs   = inFlightTokens / R
//	FetchMs   = cpuTierTokens * cpuOnloadMsPerToken
//	PrefillMs = (uncachedTokens / R) * (1 + meanContext/N_attn) * (1 + kappa*running)
//
// where R is the endpoint's prefill rate in tokens per millisecond, N_attn is
// the context length at which quadratic attention cost equals the linear
// per-token cost, and kappa is the prefill slowdown per running request.
// Every coefficient is a measurable quantity intended to come from a
// calibration run, not manual tuning.
package latencycost

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrconcurrency "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/concurrency"
	attrlatencycost "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/latencycost"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
	latencycostconstants "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/latencycost/constants"
	tokenproducer "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/tokenizer"
)

const (
	// PluginType is the registered type name of the latency-cost-producer.
	PluginType = latencycostconstants.LatencyCostProducerType

	// defaultPrefillTokensPerSecond matches the prefix-cache-affinity filter's
	// calibrated peak prefill throughput default.
	defaultPrefillTokensPerSecond = 15928

	// gpuTierKey is the CachedBlocksByTier key for GPU-resident blocks, as
	// normalized by the KV-events pool.
	gpuTierKey = "gpu"
)

// Config configures the latency-cost-producer. All coefficients are intended
// to be produced by a calibration run against the serving pods.
type Config struct {
	// PrefillTokensPerSecond is the pool's peak prefill throughput R, in
	// tokens per second. Must be positive.
	PrefillTokensPerSecond float64 `json:"prefillTokensPerSecond"`

	// AttentionHalfLengthTokens is the context length N_attn at which the
	// quadratic attention cost equals the linear per-token cost. Zero disables
	// the attention term (pure linear compute model).
	AttentionHalfLengthTokens int `json:"attentionHalfLengthTokens"`

	// CPUOnloadMsPerToken prices moving one CPU-tier cached token to the GPU.
	// Zero treats CPU-tier hits as free.
	CPUOnloadMsPerToken float64 `json:"cpuOnloadMsPerToken"`

	// DecodeInterferencePerRequest is kappa: the fractional prefill slowdown
	// contributed by each running request. Zero disables the term.
	DecodeInterferencePerRequest float64 `json:"decodeInterferencePerRequest"`

	// PrefixMatchInfoProducerName selects the PrefixCacheMatchInfo producer.
	// Empty selects the default producer.
	PrefixMatchInfoProducerName string `json:"prefixMatchInfoProducerName,omitempty"`

	// InFlightLoadProducerName selects the InFlightLoad producer. Empty
	// selects the default producer.
	InFlightLoadProducerName string `json:"inFlightLoadProducerName,omitempty"`
}

func (c *Config) validate() error {
	if c.PrefillTokensPerSecond <= 0 {
		return fmt.Errorf("prefillTokensPerSecond must be > 0, got %v", c.PrefillTokensPerSecond)
	}
	if c.AttentionHalfLengthTokens < 0 {
		return fmt.Errorf("attentionHalfLengthTokens must be >= 0, got %d", c.AttentionHalfLengthTokens)
	}
	if c.CPUOnloadMsPerToken < 0 {
		return fmt.Errorf("cpuOnloadMsPerToken must be >= 0, got %v", c.CPUOnloadMsPerToken)
	}
	if c.DecodeInterferencePerRequest < 0 {
		return fmt.Errorf("decodeInterferencePerRequest must be >= 0, got %v", c.DecodeInterferencePerRequest)
	}
	return nil
}

// compile-time type assertion
var _ requestcontrol.DataProducer = &Producer{}

// Producer computes LatencyCostInfo for every candidate endpoint on each
// scheduling cycle.
type Producer struct {
	typedName           plugin.TypedName
	config              Config
	dataKey             plugin.DataKey
	prefixMatchDataKey  plugin.DataKey
	inFlightLoadDataKey plugin.DataKey
}

// PluginFactory parses the raw plugin configuration and returns a configured
// Producer.
func PluginFactory(name string, rawParameters *json.Decoder, _ plugin.Handle) (plugin.Plugin, error) {
	cfg := Config{PrefillTokensPerSecond: defaultPrefillTokensPerSecond}
	if rawParameters != nil {
		if err := rawParameters.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("failed to parse %s plugin config: %w", PluginType, err)
		}
	}
	return New(name, cfg)
}

// New constructs a latency-cost-producer. An invalid configuration returns an
// error.
func New(name string, cfg Config) (*Producer, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", PluginType, err)
	}
	return &Producer{
		typedName:           plugin.TypedName{Type: PluginType, Name: name},
		config:              cfg,
		dataKey:             attrlatencycost.LatencyCostInfoDataKey.WithNonEmptyProducerName(name),
		prefixMatchDataKey:  attrprefix.PrefixCacheMatchInfoDataKey.WithNonEmptyProducerName(cfg.PrefixMatchInfoProducerName),
		inFlightLoadDataKey: attrconcurrency.InFlightLoadDataKey.WithNonEmptyProducerName(cfg.InFlightLoadProducerName),
	}, nil
}

// TypedName returns the plugin's registered type and name.
func (p *Producer) TypedName() plugin.TypedName { return p.typedName }

// Produces declares the LatencyCostInfo data key.
func (p *Producer) Produces() map[plugin.DataKey]any {
	return map[plugin.DataKey]any{p.dataKey: attrlatencycost.LatencyCostInfo{}}
}

// Consumes declares the tokenized prompt, prefix-cache, and in-flight-load
// dependencies so the data-layer DAG orders their producers before this one,
// auto-creating defaults when absent.
func (p *Producer) Consumes() plugin.DataDependencies {
	return plugin.DataDependencies{
		Required: map[plugin.DataKey]any{
			tokenproducer.TokenizedPromptDataKey: scheduling.TokenizedPrompt{},
			p.prefixMatchDataKey:                 attrprefix.PrefixCacheMatchInfo{},
			p.inFlightLoadDataKey:                attrconcurrency.InFlightLoad{},
		},
	}
}

// Produce computes and attaches LatencyCostInfo to every candidate endpoint.
func (p *Producer) Produce(ctx context.Context, request *scheduling.InferenceRequest, endpoints []scheduling.Endpoint) error {
	logger := log.FromContext(ctx).WithName(p.typedName.String())
	inputTokens := promptTokenCount(request)

	for _, ep := range endpoints {
		info := p.cost(ep, inputTokens)
		ep.Put(p.dataKey.String(), info)
		if v := logger.V(logging.DEBUG); v.Enabled() {
			v.Info("computed latency cost",
				"endpoint", ep.GetMetadata().NamespacedName.String(),
				"inputTokens", inputTokens,
				"queueMs", info.QueueMs, "fetchMs", info.FetchMs, "prefillMs", info.PrefillMs,
				"totalMs", info.TotalMs())
		}
	}
	return nil
}

// cost computes the LatencyCostInfo of the request on one endpoint.
func (p *Producer) cost(ep scheduling.Endpoint, inputTokens int) *attrlatencycost.LatencyCostInfo {
	msPerToken := 1000.0 / p.config.PrefillTokensPerSecond

	inFlightTokens, running := p.load(ep)
	queueMs := float64(inFlightTokens) * msPerToken

	cachedTokens, gpuTokens := p.cachedTokens(ep, inputTokens)
	onloadTokens := cachedTokens - gpuTokens
	fetchMs := float64(onloadTokens) * p.config.CPUOnloadMsPerToken

	uncachedTokens := inputTokens - cachedTokens
	attention := 1.0
	if p.config.AttentionHalfLengthTokens > 0 {
		meanContext := float64(cachedTokens) + float64(uncachedTokens)/2
		attention += meanContext / float64(p.config.AttentionHalfLengthTokens)
	}
	interference := 1.0 + p.config.DecodeInterferencePerRequest*float64(running)
	prefillMs := float64(uncachedTokens) * msPerToken * attention * interference

	return &attrlatencycost.LatencyCostInfo{
		QueueMs:   queueMs,
		FetchMs:   fetchMs,
		PrefillMs: prefillMs,
	}
}

// load returns the endpoint's committed in-flight prefill tokens and its
// running request count. Tokens come from the EPP-tracked InFlightLoad
// attribute (0 when absent); the running count prefers the same attribute and
// falls back to the scraped RunningRequestsSize.
func (p *Producer) load(ep scheduling.Endpoint) (tokens int64, running int64) {
	if raw, ok := ep.Get(p.inFlightLoadDataKey.String()); ok {
		if load, ok := raw.(*attrconcurrency.InFlightLoad); ok && load != nil {
			return load.Tokens, load.Requests
		}
	}
	if metrics := ep.GetMetrics(); metrics != nil {
		return 0, int64(metrics.RunningRequestsSize)
	}
	return 0, 0
}

// cachedTokens returns the endpoint's contiguous cached prefix tokens for the
// request, clamped to the prompt length, and the portion of them resident on
// the GPU tier. Without tier data every cached token counts as GPU-resident.
func (p *Producer) cachedTokens(ep scheduling.Endpoint, inputTokens int) (cached int, gpu int) {
	raw, ok := ep.Get(p.prefixMatchDataKey.String())
	if !ok {
		return 0, 0
	}
	info, ok := raw.(*attrprefix.PrefixCacheMatchInfo)
	if !ok || info.BlockSizeTokens() <= 0 {
		return 0, 0
	}
	blockSize := info.BlockSizeTokens()
	cached = info.CachedBlockCount() * blockSize
	if cached > inputTokens {
		cached = inputTokens
	}
	gpu = cached
	if tiers := info.CachedBlocksByTier(); tiers != nil {
		gpu = tiers[gpuTierKey] * blockSize
		if gpu > cached {
			gpu = cached
		}
	}
	return cached, gpu
}

// promptTokenCount returns the tokenized prompt length, or 0 when the request
// carries no tokenized prompt.
func promptTokenCount(request *scheduling.InferenceRequest) int {
	if request == nil || request.Body == nil || request.Body.TokenizedPrompt == nil {
		return 0
	}
	return request.Body.TokenizedPrompt.TokenCount()
}
