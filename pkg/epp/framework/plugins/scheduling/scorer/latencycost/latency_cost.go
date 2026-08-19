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

// Package latencycost scores endpoints by their predicted time-to-first-token
// from the LatencyCostInfo attribute: the cheapest endpoint in milliseconds
// scores 1.0, the most expensive 0.0, linearly in between. Endpoints without
// the attribute score 0.
package latencycost

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrlatencycost "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/latencycost"
)

const (
	// LatencyCostScorerType is the registered type name of the scorer.
	LatencyCostScorerType = "latency-cost-scorer"
)

// Config configures the latency-cost-scorer.
type Config struct {
	// LatencyCostInfoProducerName selects the LatencyCostInfo producer. Empty
	// selects the default producer.
	LatencyCostInfoProducerName string `json:"latencyCostInfoProducerName,omitempty"`
}

// compile-time type assertion
var _ fwksched.Scorer = &Scorer{}

// Scorer ranks endpoints by ascending predicted time-to-first-token.
type Scorer struct {
	typedName fwkplugin.TypedName
	dataKey   fwkplugin.DataKey
}

// Factory parses the raw plugin configuration and returns a configured
// Scorer.
func Factory(name string, rawParameters *json.Decoder, _ fwkplugin.Handle) (fwkplugin.Plugin, error) {
	cfg := Config{}
	if rawParameters != nil {
		if err := rawParameters.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("failed to parse %s plugin config: %w", LatencyCostScorerType, err)
		}
	}
	return New(name, cfg), nil
}

// New constructs a latency-cost-scorer bound to the configured
// LatencyCostInfo producer name.
func New(name string, cfg Config) *Scorer {
	return &Scorer{
		typedName: fwkplugin.TypedName{Type: LatencyCostScorerType, Name: name},
		dataKey:   attrlatencycost.LatencyCostInfoDataKey.WithNonEmptyProducerName(cfg.LatencyCostInfoProducerName),
	}
}

// TypedName returns the plugin's registered type and name.
func (s *Scorer) TypedName() fwkplugin.TypedName { return s.typedName }

// Category returns the scorer category.
func (s *Scorer) Category() fwksched.ScorerCategory { return fwksched.Balance }

// Consumes declares the LatencyCostInfo dependency so the data-layer DAG
// orders the producing plugin before scheduling, auto-creating the default
// producer when absent.
func (s *Scorer) Consumes() fwkplugin.DataDependencies {
	return fwkplugin.DataDependencies{
		Required: map[fwkplugin.DataKey]any{s.dataKey: attrlatencycost.LatencyCostInfo{}},
	}
}

// Score maps each endpoint's predicted time-to-first-token to [0, 1] by
// min-max normalization across the candidate set: the cheapest endpoint gets
// 1.0, the most expensive 0.0. When all costs are equal every endpoint with
// cost data gets 1.0. Endpoints without cost data get 0.
func (s *Scorer) Score(ctx context.Context, _ *fwksched.InferenceRequest, endpoints []fwksched.Endpoint) map[fwksched.Endpoint]float64 {
	scores := make(map[fwksched.Endpoint]float64, len(endpoints))
	costs := make(map[fwksched.Endpoint]float64, len(endpoints))

	minCost, maxCost := 0.0, 0.0
	first := true
	for _, ep := range endpoints {
		scores[ep] = 0.0
		raw, ok := ep.Get(s.dataKey.String())
		if !ok {
			continue
		}
		info, ok := raw.(*attrlatencycost.LatencyCostInfo)
		if !ok || info == nil {
			continue
		}
		cost := info.TotalMs()
		costs[ep] = cost
		if first || cost < minCost {
			minCost = cost
		}
		if first || cost > maxCost {
			maxCost = cost
		}
		first = false
	}

	if len(costs) == 0 {
		log.FromContext(ctx).WithName(s.typedName.String()).Info(
			"no endpoint carries latency cost data; all scores are 0")
		return scores
	}

	spread := maxCost - minCost
	for ep, cost := range costs {
		if spread <= 0 {
			scores[ep] = 1.0
			continue
		}
		scores[ep] = 1.0 - (cost-minCost)/spread
	}
	return scores
}
