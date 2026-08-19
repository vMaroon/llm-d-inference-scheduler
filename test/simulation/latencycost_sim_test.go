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

// Package simulation verifies latency-cost routing against a physics-based
// fleet simulator before any cluster deployment. Two gates:
//
//   - Scenario gate: routing decisions match the analytic model on hand-built
//     states, including the exact queue depth where cache affinity must break.
//   - Workload gate: on a shared-prefix workload, latency-cost routing must
//     not lose to the default weighted profile (queue + prefix) on mean
//     simulated TTFT, and its cost predictions must track simulated TTFT.
package simulation

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrconcurrency "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/concurrency"
	attrlatencycost "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/latencycost"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
	latencycostproducer "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/latencycost"
	latencycostscorer "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/scheduling/scorer/latencycost"
	prefixscorer "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/scheduling/scorer/prefix"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/scheduling/scorer/queuedepth"
	testutils "github.com/llm-d/llm-d-router/test/utils"
)

const (
	blockSizeTokens = 64

	// Physics coefficients shared by the router model and the simulator.
	prefillTokensPerSecond    = 16000.0
	attentionHalfLengthTokens = 25000.0
	msPerToken                = 1000.0 / prefillTokensPerSecond
)

func requestWithTokens(n int) *fwksched.InferenceRequest {
	return &fwksched.InferenceRequest{
		Body: &fwkrh.InferenceRequestBody{
			TokenizedPrompt: &fwkrh.TokenizedPrompt{PerPromptTokens: [][]uint32{make([]uint32, n)}},
		},
	}
}

func newRouterEndpoint(name string, waiting int, cachedBlocks, totalBlocks int, inflightTokens, inflightReqs int64) fwksched.Endpoint {
	ep := fwksched.NewEndpoint(
		&fwkdl.EndpointMetadata{NamespacedName: types.NamespacedName{Namespace: "sim", Name: name}},
		&fwkdl.Metrics{WaitingQueueSize: waiting, RunningRequestsSize: waiting},
		nil)
	ep.Put(attrprefix.PrefixCacheMatchInfoDataKey.String(),
		attrprefix.NewPrefixCacheMatchInfo(cachedBlocks, totalBlocks, blockSizeTokens))
	ep.Put(attrconcurrency.InFlightLoadDataKey.String(),
		&attrconcurrency.InFlightLoad{Tokens: inflightTokens, Requests: inflightReqs})
	return ep
}

func argmax(endpoints []fwksched.Endpoint, scores map[fwksched.Endpoint]float64) int {
	best, bestScore := 0, math.Inf(-1)
	for i, ep := range endpoints {
		if s := scores[ep]; s > bestScore {
			best, bestScore = i, s
		}
	}
	return best
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// latencyCostRouter routes with the latency-cost producer + scorer.
type latencyCostRouter struct {
	producer *latencycostproducer.Producer
	scorer   *latencycostscorer.Scorer
	dataKey  fwkplugin.DataKey
}

func newLatencyCostRouter(t *testing.T) *latencyCostRouter {
	t.Helper()
	producer, err := latencycostproducer.New(latencycostproducer.PluginType, latencycostproducer.Config{
		PrefillTokensPerSecond:    prefillTokensPerSecond,
		AttentionHalfLengthTokens: int(attentionHalfLengthTokens),
	})
	require.NoError(t, err)
	return &latencyCostRouter{
		producer: producer,
		scorer:   latencycostscorer.New("latency-cost-scorer", latencycostscorer.Config{}),
		dataKey:  attrlatencycost.LatencyCostInfoDataKey,
	}
}

// route returns the chosen endpoint index and its predicted TTFT in ms.
func (r *latencyCostRouter) route(t *testing.T, req *fwksched.InferenceRequest, endpoints []fwksched.Endpoint) (int, float64) {
	t.Helper()
	require.NoError(t, r.producer.Produce(context.Background(), req, endpoints))
	choice := argmax(endpoints, r.scorer.Score(context.Background(), req, endpoints))
	raw, ok := endpoints[choice].Get(r.dataKey.String())
	require.True(t, ok)
	info, ok := raw.(*attrlatencycost.LatencyCostInfo)
	require.True(t, ok)
	return choice, info.TotalMs()
}

// baselineRouter routes with the default weighted profile's load and prefix
// scorers (queue-scorer weight 2, prefix-cache-scorer weight 3).
type baselineRouter struct {
	queue  fwksched.Scorer
	prefix fwksched.Scorer
}

func newBaselineRouter(t *testing.T) *baselineRouter {
	t.Helper()
	handle := testutils.NewTestHandle(context.Background())
	qp, err := queuedepth.QueueScorerFactory("queue-scorer", nil, handle)
	require.NoError(t, err)
	pp, err := prefixscorer.PrefixCachePluginFactory("prefix-cache-scorer", nil, handle)
	require.NoError(t, err)
	queue, ok := qp.(fwksched.Scorer)
	require.True(t, ok)
	prefix, ok := pp.(fwksched.Scorer)
	require.True(t, ok)
	return &baselineRouter{queue: queue, prefix: prefix}
}

func (r *baselineRouter) route(req *fwksched.InferenceRequest, endpoints []fwksched.Endpoint) int {
	queueScores := r.queue.Score(context.Background(), req, endpoints)
	prefixScores := r.prefix.Score(context.Background(), req, endpoints)
	combined := make(map[fwksched.Endpoint]float64, len(endpoints))
	for _, ep := range endpoints {
		combined[ep] = 2.0*clamp01(queueScores[ep]) + 3.0*clamp01(prefixScores[ep])
	}
	return argmax(endpoints, combined)
}

// --- Scenario gate ---------------------------------------------------------

// TestScenarioAffinityBreakPoint pins the analytic flip point: with a fully
// cached pod A and a cold idle pod B, disregarding queue the model must stick
// to A; once A's queued work costs more than B's cold recompute, it must
// break to B. With prompt P and attention half-length N the flip sits at
// exactly P*(1+P/(2N)) queued tokens, independent of the prefill rate.
func TestScenarioAffinityBreakPoint(t *testing.T) {
	router := newLatencyCostRouter(t)
	const promptTokens = 32000
	const promptBlocks = promptTokens / blockSizeTokens
	flipTokens := int64(promptTokens * (1 + promptTokens/(2*attentionHalfLengthTokens))) // 52480

	cases := []struct {
		name       string
		queuedOnA  int64
		wantChoice int
	}{
		{"idle cache holder wins", 0, 0},
		{"just below flip sticks", flipTokens - 500, 0},
		{"just above flip breaks", flipTokens + 500, 1},
		{"deep queue breaks decisively", 3 * flipTokens, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoints := []fwksched.Endpoint{
				newRouterEndpoint("pod-a", int(tc.queuedOnA/promptTokens), promptBlocks, promptBlocks, tc.queuedOnA, tc.queuedOnA/promptTokens),
				newRouterEndpoint("pod-b", 0, 0, promptBlocks, 0, 0),
			}
			choice, predicted := router.route(t, requestWithTokens(promptTokens), endpoints)
			assert.Equal(t, tc.wantChoice, choice)
			// A fully cached idle pod costs exactly 0 ms; every other state
			// costs something.
			assert.GreaterOrEqual(t, predicted, 0.0)
		})
	}
}

// --- Workload gate ---------------------------------------------------------

// lcg is a deterministic linear congruential generator so the workload and
// noise are reproducible.
type lcg struct{ state uint64 }

func (r *lcg) next() float64 {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return float64(r.state>>11) / float64(1<<53)
}

type simJob struct {
	finishAt float64
	tokens   int64
}

type simPod struct {
	name       string
	lastFinish float64
	jobs       []simJob
	inflight   int64
	cached     map[int]bool
}

func (p *simPod) drain(now float64) {
	kept := p.jobs[:0]
	for _, j := range p.jobs {
		if j.finishAt > now {
			kept = append(kept, j)
		} else {
			p.inflight -= j.tokens
		}
	}
	p.jobs = kept
}

// serve dispatches a request to the pod and returns its simulated TTFT.
func (p *simPod) serve(now float64, group, sharedTokens, uniqueTokens int, noise float64) float64 {
	cached := 0
	if p.cached[group] {
		cached = sharedTokens
	}
	uncached := sharedTokens + uniqueTokens - cached
	meanContext := float64(cached) + float64(uncached)/2
	service := float64(uncached) * msPerToken * (1 + meanContext/attentionHalfLengthTokens) * noise

	wait := math.Max(0, p.lastFinish-now)
	ttft := wait + service
	p.lastFinish = now + ttft
	p.jobs = append(p.jobs, simJob{finishAt: now + ttft, tokens: int64(uncached)})
	p.inflight += int64(uncached)
	p.cached[group] = true
	return ttft
}

func newFleet(n int) []*simPod {
	fleet := make([]*simPod, n)
	for i := range fleet {
		fleet[i] = &simPod{name: fmt.Sprintf("pod-%d", i), cached: map[int]bool{}}
	}
	return fleet
}

func (p *simPod) routerView(group, sharedTokens, uniqueTokens int) (cachedBlocks, totalBlocks int) {
	totalBlocks = (sharedTokens + uniqueTokens) / blockSizeTokens
	if p.cached[group] {
		cachedBlocks = sharedTokens / blockSizeTokens
	}
	return cachedBlocks, totalBlocks
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// TestWorkloadLatencyCostVsBaseline replays the same arrival sequence, with
// identical per-request noise, through two fleets: one routed by latency
// cost, one by the default weighted queue+prefix profile. Half the traffic
// shares one hot prefix, so its cache holder overloads: the weighted profile
// pins the hot group to that pod regardless of queue depth (prefix weight 3
// times a 0.75 hit ratio always beats queue weight 2), while the cost model
// breaks affinity once queued work exceeds a cold recompute and replicates
// the hot prefix onto a second pod. Gates: latency-cost routing must beat
// the baseline on mean simulated TTFT, and its predicted TTFT must track the
// simulated TTFT within 20% mean absolute error.
func TestWorkloadLatencyCostVsBaseline(t *testing.T) {
	const (
		pods         = 3
		groups       = 6
		requests     = 300
		interarrival = 60.0 // ms
		sharedTokens = 6144
		uniqueTokens = 2048
		noiseAmp     = 0.08
		maeThreshold = 0.20
		promptTokens = sharedTokens + uniqueTokens
	)

	lcRouter := newLatencyCostRouter(t)
	blRouter := newBaselineRouter(t)
	lcFleet := newFleet(pods)
	blFleet := newFleet(pods)

	rng := &lcg{state: 42}
	noises := make([]float64, requests)
	groupOf := make([]int, requests)
	for i := range noises {
		noises[i] = 1 + noiseAmp*(2*rng.next()-1)
		if rng.next() < 0.5 {
			groupOf[i] = 0 // hot shared prefix
		} else {
			groupOf[i] = 1 + i%(groups-1)
		}
	}

	var lcTTFTs, blTTFTs []float64
	var absErrSum, ttftSum float64

	for i := 0; i < requests; i++ {
		now := float64(i) * interarrival
		group := groupOf[i]
		req := requestWithTokens(promptTokens)

		// Latency-cost fleet.
		endpoints := make([]fwksched.Endpoint, pods)
		for j, pod := range lcFleet {
			pod.drain(now)
			cachedBlocks, totalBlocks := pod.routerView(group, sharedTokens, uniqueTokens)
			endpoints[j] = newRouterEndpoint(pod.name, len(pod.jobs), cachedBlocks, totalBlocks, pod.inflight, int64(len(pod.jobs)))
		}
		choice, predicted := lcRouter.route(t, req, endpoints)
		ttft := lcFleet[choice].serve(now, group, sharedTokens, uniqueTokens, noises[i])
		lcTTFTs = append(lcTTFTs, ttft)
		absErrSum += math.Abs(predicted - ttft)
		ttftSum += ttft

		// Baseline fleet, same arrivals and noise.
		for j, pod := range blFleet {
			pod.drain(now)
			cachedBlocks, totalBlocks := pod.routerView(group, sharedTokens, uniqueTokens)
			endpoints[j] = newRouterEndpoint(pod.name, len(pod.jobs), cachedBlocks, totalBlocks, pod.inflight, int64(len(pod.jobs)))
		}
		blChoice := blRouter.route(req, endpoints)
		blTTFTs = append(blTTFTs, blFleet[blChoice].serve(now, group, sharedTokens, uniqueTokens, noises[i]))
	}

	mean := func(v []float64) float64 {
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s / float64(len(v))
	}
	lcMean, blMean := mean(lcTTFTs), mean(blTTFTs)
	sort.Float64s(lcTTFTs)
	sort.Float64s(blTTFTs)
	mae := absErrSum / float64(requests) / (ttftSum / float64(requests))

	t.Logf("latency-cost: mean %.1f ms, p50 %.1f ms, p90 %.1f ms", lcMean, percentile(lcTTFTs, 0.5), percentile(lcTTFTs, 0.9))
	t.Logf("baseline:     mean %.1f ms, p50 %.1f ms, p90 %.1f ms", blMean, percentile(blTTFTs, 0.5), percentile(blTTFTs, 0.9))
	t.Logf("prediction MAE / mean TTFT: %.3f", mae)

	assert.Less(t, lcMean, blMean,
		"latency-cost routing must beat the baseline weighted profile on mean TTFT under a hot prefix")
	assert.LessOrEqual(t, mae, maeThreshold,
		"predicted TTFT must track simulated TTFT")
}
