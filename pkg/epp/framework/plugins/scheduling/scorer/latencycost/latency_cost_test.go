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

package latencycost

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrlatencycost "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/latencycost"
)

func newEndpoint(name string) fwksched.Endpoint {
	return fwksched.NewEndpoint(
		&fwkdl.EndpointMetadata{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}},
		&fwkdl.Metrics{}, nil)
}

func withCost(s *Scorer, ep fwksched.Endpoint, queueMs, fetchMs, prefillMs float64) fwksched.Endpoint {
	ep.Put(s.dataKey.String(), &attrlatencycost.LatencyCostInfo{
		QueueMs: queueMs, FetchMs: fetchMs, PrefillMs: prefillMs,
	})
	return ep
}

func TestScoreRanksByAscendingCost(t *testing.T) {
	s := New("test", Config{})
	cheap := withCost(s, newEndpoint("cheap"), 0, 0, 100)
	mid := withCost(s, newEndpoint("mid"), 100, 50, 50)
	costly := withCost(s, newEndpoint("costly"), 300, 0, 100)
	endpoints := []fwksched.Endpoint{cheap, mid, costly}

	scores := s.Score(context.Background(), &fwksched.InferenceRequest{}, endpoints)

	assert.InDelta(t, 1.0, scores[cheap], 1e-9)
	assert.InDelta(t, 1.0-(200.0-100.0)/300.0, scores[mid], 1e-9)
	assert.InDelta(t, 0.0, scores[costly], 1e-9)
}

func TestScoreEqualCostsAreNeutral(t *testing.T) {
	s := New("test", Config{})
	a := withCost(s, newEndpoint("a"), 50, 0, 50)
	b := withCost(s, newEndpoint("b"), 0, 0, 100)

	scores := s.Score(context.Background(), &fwksched.InferenceRequest{}, []fwksched.Endpoint{a, b})

	assert.InDelta(t, 1.0, scores[a], 1e-9)
	assert.InDelta(t, 1.0, scores[b], 1e-9)
}

func TestScoreMissingInfoScoresZero(t *testing.T) {
	s := New("test", Config{})
	priced := withCost(s, newEndpoint("priced"), 0, 0, 100)
	unpriced := newEndpoint("unpriced")

	scores := s.Score(context.Background(), &fwksched.InferenceRequest{}, []fwksched.Endpoint{priced, unpriced})

	assert.InDelta(t, 1.0, scores[priced], 1e-9)
	assert.InDelta(t, 0.0, scores[unpriced], 1e-9)
}

func TestScoreNoInfoAnywhere(t *testing.T) {
	s := New("test", Config{})
	a := newEndpoint("a")
	b := newEndpoint("b")

	scores := s.Score(context.Background(), &fwksched.InferenceRequest{}, []fwksched.Endpoint{a, b})

	assert.InDelta(t, 0.0, scores[a], 1e-9)
	assert.InDelta(t, 0.0, scores[b], 1e-9)
}
