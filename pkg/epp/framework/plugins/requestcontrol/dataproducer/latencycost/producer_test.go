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
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrconcurrency "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/concurrency"
	attrlatencycost "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/latencycost"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
)

func newEndpoint(metrics *fwkdl.Metrics) fwksched.Endpoint {
	return fwksched.NewEndpoint(
		&fwkdl.EndpointMetadata{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pod1"}},
		metrics, nil)
}

func requestWithTokens(n int) *fwksched.InferenceRequest {
	tokens := make([]uint32, n)
	return &fwksched.InferenceRequest{
		Body: &fwkrh.InferenceRequestBody{
			TokenizedPrompt: &fwkrh.TokenizedPrompt{PerPromptTokens: [][]uint32{tokens}},
		},
	}
}

func newProducer(t *testing.T, cfg Config) *Producer {
	t.Helper()
	p, err := New("test", cfg)
	require.NoError(t, err)
	return p
}

func producedInfo(t *testing.T, p *Producer, ep fwksched.Endpoint) *attrlatencycost.LatencyCostInfo {
	t.Helper()
	raw, ok := ep.Get(p.dataKey.String())
	require.True(t, ok, "LatencyCostInfo must be attached")
	info, ok := raw.(*attrlatencycost.LatencyCostInfo)
	require.True(t, ok)
	return info
}

func TestProduceQueueTerm(t *testing.T) {
	p := newProducer(t, Config{PrefillTokensPerSecond: 15928})
	ep := newEndpoint(&fwkdl.Metrics{})
	ep.Put(p.inFlightLoadDataKey.String(), &attrconcurrency.InFlightLoad{Tokens: 90000})

	require.NoError(t, p.Produce(context.Background(), requestWithTokens(0), []fwksched.Endpoint{ep}))

	info := producedInfo(t, p, ep)
	// 90000 tokens / 15.928 tokens per ms
	assert.InDelta(t, 5650.426921, info.QueueMs, 0.001)
	assert.Zero(t, info.FetchMs)
	assert.Zero(t, info.PrefillMs)
}

func TestProduceColdComputeWithAttentionAndInterference(t *testing.T) {
	p := newProducer(t, Config{
		PrefillTokensPerSecond:       15928,
		AttentionHalfLengthTokens:    25000,
		DecodeInterferencePerRequest: 0.03,
	})
	ep := newEndpoint(&fwkdl.Metrics{})
	ep.Put(p.inFlightLoadDataKey.String(), &attrconcurrency.InFlightLoad{Tokens: 0, Requests: 2})

	require.NoError(t, p.Produce(context.Background(), requestWithTokens(32000), []fwksched.Endpoint{ep}))

	info := producedInfo(t, p, ep)
	assert.Zero(t, info.QueueMs)
	assert.Zero(t, info.FetchMs)
	// (32000/15.928) * (1 + 16000/25000) * (1 + 0.03*2)
	assert.InDelta(t, 3492.516323, info.PrefillMs, 0.001)
	assert.InDelta(t, 3492.516323, info.TotalMs(), 0.001)
}

func TestProduceTierSplit(t *testing.T) {
	p := newProducer(t, Config{
		PrefillTokensPerSecond:    15928,
		AttentionHalfLengthTokens: 25000,
		CPUOnloadMsPerToken:       0.005,
	})
	ep := newEndpoint(&fwkdl.Metrics{})
	// 15 contiguous cached blocks of 16 tokens: the first 10 on GPU, 5 more
	// only on the CPU tier. Prompt is 400 tokens.
	ep.Put(p.prefixMatchDataKey.String(),
		attrprefix.NewPrefixCacheMatchInfo(15, 25, 16).
			WithCachedBlockCount(15).
			WithCachedBlocksByTier(map[string]int{"gpu": 10, "cpu": 15}))

	require.NoError(t, p.Produce(context.Background(), requestWithTokens(400), []fwksched.Endpoint{ep}))

	info := producedInfo(t, p, ep)
	assert.Zero(t, info.QueueMs)
	// 80 CPU-only tokens * 0.005 ms/token
	assert.InDelta(t, 0.4, info.FetchMs, 1e-9)
	// 160 uncached tokens * (1000/15928) * (1 + (240+80)/25000)
	assert.InDelta(t, 10.173782, info.PrefillMs, 0.001)
}

func TestProduceFullLocalHitIsFree(t *testing.T) {
	p := newProducer(t, Config{
		PrefillTokensPerSecond:    15928,
		AttentionHalfLengthTokens: 25000,
	})
	ep := newEndpoint(&fwkdl.Metrics{})
	// Cached blocks cover more than the prompt; cost must clamp to zero work.
	ep.Put(p.prefixMatchDataKey.String(),
		attrprefix.NewPrefixCacheMatchInfo(30, 30, 16).WithCachedBlockCount(30))

	require.NoError(t, p.Produce(context.Background(), requestWithTokens(400), []fwksched.Endpoint{ep}))

	info := producedInfo(t, p, ep)
	assert.Zero(t, info.QueueMs)
	assert.Zero(t, info.FetchMs)
	assert.Zero(t, info.PrefillMs)
}

func TestProduceMissingAttributesFallsBackToColdModel(t *testing.T) {
	p := newProducer(t, Config{PrefillTokensPerSecond: 16000})
	ep := newEndpoint(&fwkdl.Metrics{RunningRequestsSize: 7})

	require.NoError(t, p.Produce(context.Background(), requestWithTokens(1600), []fwksched.Endpoint{ep}))

	info := producedInfo(t, p, ep)
	assert.Zero(t, info.QueueMs)
	assert.Zero(t, info.FetchMs)
	// 1600 tokens at 16 tokens/ms, linear model (attention and interference off).
	assert.InDelta(t, 100.0, info.PrefillMs, 1e-9)
}

func TestFactoryValidation(t *testing.T) {
	cases := []struct {
		name   string
		params string
		valid  bool
	}{
		{"defaults", `{}`, true},
		{"explicit valid", `{"prefillTokensPerSecond": 1000, "attentionHalfLengthTokens": 25000}`, true},
		{"zero rate", `{"prefillTokensPerSecond": 0}`, false},
		{"negative rate", `{"prefillTokensPerSecond": -5}`, false},
		{"negative attention", `{"attentionHalfLengthTokens": -1}`, false},
		{"negative onload", `{"cpuOnloadMsPerToken": -0.1}`, false},
		{"negative interference", `{"decodeInterferencePerRequest": -0.1}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PluginFactory("test", json.NewDecoder(bytes.NewBufferString(tc.params)), nil)
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}
