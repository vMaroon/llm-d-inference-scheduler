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

package decodeprogress

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	logutil "github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrconcurrency "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/concurrency"
)

const PluginType = "decode-progress-scorer"

type Config struct {
	InFlightLoadProducerName string `json:"inFlightLoadProducerName,omitempty"`
}

type Scorer struct {
	typedName           fwkplugin.TypedName
	inFlightLoadDataKey fwkplugin.DataKey
	now                 func() time.Time
}

type endpointPressure struct {
	valid                 bool
	requests              int64
	awaitingFirstResponse int64
	unobservableRequests  int64
	totalSilenceMillis    int64
	maxSilenceMillis      int64
}

type scoredPressure struct {
	endpoint fwksched.Endpoint
	pressure endpointPressure
}

var _ fwksched.Scorer = (*Scorer)(nil)
var _ fwkplugin.ConsumerPlugin = (*Scorer)(nil)

func Factory(name string, decoder *json.Decoder, _ fwkplugin.Handle) (fwkplugin.Plugin, error) {
	cfg := Config{}
	if decoder != nil {
		if err := decoder.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("failed to decode %s parameters: %w", PluginType, err)
		}
	}
	return NewWithProducerName(cfg.InFlightLoadProducerName).WithName(name), nil
}

func New() *Scorer {
	return NewWithProducerName("")
}

func NewWithProducerName(producerName string) *Scorer {
	return &Scorer{
		typedName:           fwkplugin.TypedName{Type: PluginType, Name: PluginType},
		inFlightLoadDataKey: attrconcurrency.InFlightLoadDataKey.WithNonEmptyProducerName(producerName),
		now:                 time.Now,
	}
}

func (s *Scorer) TypedName() fwkplugin.TypedName {
	return s.typedName
}

func (s *Scorer) WithName(name string) *Scorer {
	s.typedName.Name = name
	return s
}

func (s *Scorer) Category() fwksched.ScorerCategory {
	return fwksched.Distribution
}

func (s *Scorer) Consumes() fwkplugin.DataDependencies {
	return fwkplugin.DataDependencies{
		Required: map[fwkplugin.DataKey]any{
			s.inFlightLoadDataKey: attrconcurrency.InFlightLoad{},
		},
	}
}

func (s *Scorer) Score(ctx context.Context, _ *fwksched.InferenceRequest, endpoints []fwksched.Endpoint) map[fwksched.Endpoint]float64 {
	pressures := make([]scoredPressure, 0, len(endpoints))
	nowUnixMilli := s.currentTime().UnixMilli()
	for _, endpoint := range endpoints {
		pressures = append(pressures, scoredPressure{
			endpoint: endpoint,
			pressure: s.pressure(ctx, endpoint, nowUnixMilli),
		})
	}

	sort.SliceStable(pressures, func(i, j int) bool {
		return pressures[i].pressure.less(pressures[j].pressure)
	})

	groupCount := 0
	for i := range pressures {
		if i == 0 || pressures[i].pressure != pressures[i-1].pressure {
			groupCount++
		}
	}

	scores := make(map[fwksched.Endpoint]float64, len(endpoints))
	groupIndex := -1
	for i := range pressures {
		if i == 0 || pressures[i].pressure != pressures[i-1].pressure {
			groupIndex++
		}
		score := 1.0
		if groupCount > 1 {
			score = 1.0 - float64(groupIndex)/float64(groupCount-1)
		}
		scores[pressures[i].endpoint] = score
		log.FromContext(ctx).V(logutil.DEBUG).Info("Decode progress scorer scoring",
			"endpoint", pressures[i].endpoint.GetMetadata().ID.String(),
			"requests", pressures[i].pressure.requests,
			"awaitingFirstResponse", pressures[i].pressure.awaitingFirstResponse,
			"unobservableRequests", pressures[i].pressure.unobservableRequests,
			"totalSilenceMillis", pressures[i].pressure.totalSilenceMillis,
			"maxSilenceMillis", pressures[i].pressure.maxSilenceMillis,
			"score", score)
	}
	return scores
}

func (s *Scorer) pressure(ctx context.Context, endpoint fwksched.Endpoint, nowUnixMilli int64) endpointPressure {
	value, ok := endpoint.Get(s.inFlightLoadDataKey)
	if !ok {
		return endpointPressure{}
	}
	load, ok := value.(*attrconcurrency.InFlightLoad)
	if !ok || load == nil {
		log.FromContext(ctx).V(logutil.TRACE).Info("Ignoring invalid in-flight load attribute",
			"endpoint", endpoint.GetMetadata().ID.String(), "attributeType", fmt.Sprintf("%T", value))
		return endpointPressure{}
	}

	requests := nonNegative(load.Requests)
	observable := min(nonNegative(load.ObservableRequests), requests)
	awaiting := min(nonNegative(load.AwaitingFirstResponse), observable)
	totalSilence := observable*nowUnixMilli - load.ProgressTimestampSumUnixMilli
	if totalSilence < 0 {
		totalSilence = 0
	}
	maxSilence := int64(0)
	if load.OldestProgressUnixMilli > 0 && load.OldestProgressUnixMilli < nowUnixMilli {
		maxSilence = nowUnixMilli - load.OldestProgressUnixMilli
	}

	return endpointPressure{
		valid:                 true,
		requests:              requests,
		awaitingFirstResponse: awaiting,
		unobservableRequests:  requests - observable,
		totalSilenceMillis:    totalSilence,
		maxSilenceMillis:      maxSilence,
	}
}

func (s *Scorer) currentTime() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (p endpointPressure) less(other endpointPressure) bool {
	switch {
	case p.valid != other.valid:
		return p.valid
	case p.requests != other.requests:
		return p.requests < other.requests
	case p.awaitingFirstResponse != other.awaitingFirstResponse:
		return p.awaitingFirstResponse < other.awaitingFirstResponse
	case p.unobservableRequests != other.unobservableRequests:
		return p.unobservableRequests < other.unobservableRequests
	case p.totalSilenceMillis != other.totalSilenceMillis:
		return p.totalSilenceMillis < other.totalSilenceMillis
	default:
		return p.maxSilenceMillis < other.maxSilenceMillis
	}
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
