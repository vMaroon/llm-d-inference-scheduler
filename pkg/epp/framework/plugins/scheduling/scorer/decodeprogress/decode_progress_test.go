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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrconcurrency "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/concurrency"
)

type stubEndpoint struct {
	metadata *datalayer.EndpointMetadata
	attrs    datalayer.AttributeMap
}

func newEndpoint(name string, load *attrconcurrency.InFlightLoad) scheduling.Endpoint {
	ep := &stubEndpoint{
		metadata: &datalayer.EndpointMetadata{ID: k8stypes.NamespacedName{Namespace: "default", Name: name}},
		attrs:    datalayer.NewAttributes(),
	}
	if load != nil {
		ep.Put(attrconcurrency.InFlightLoadDataKey, load)
	}
	return ep
}

func (e *stubEndpoint) GetMetadata() *datalayer.EndpointMetadata   { return e.metadata }
func (e *stubEndpoint) UpdateMetadata(*datalayer.EndpointMetadata) {}
func (e *stubEndpoint) GetMetrics() *datalayer.Metrics             { return nil }
func (e *stubEndpoint) UpdateMetrics(*datalayer.Metrics)           {}
func (e *stubEndpoint) String() string                             { return e.metadata.ID.String() }
func (e *stubEndpoint) Put(key plugin.DataKey, value datalayer.Cloneable) {
	e.attrs.Put(key, value)
}
func (e *stubEndpoint) Get(key plugin.DataKey) (datalayer.Cloneable, bool) {
	return e.attrs.Get(key)
}
func (e *stubEndpoint) Keys() []plugin.DataKey        { return e.attrs.Keys() }
func (e *stubEndpoint) Clone() datalayer.AttributeMap { return e.attrs.Clone() }

func TestScoreOrdersExactRequestProgress(t *testing.T) {
	t.Parallel()

	scorer := New()
	scorer.now = func() time.Time { return time.UnixMilli(2_000) }

	tests := []struct {
		name  string
		loads []*attrconcurrency.InFlightLoad
		want  []float64
	}{
		{
			name: "active count remains primary",
			loads: []*attrconcurrency.InFlightLoad{
				{Requests: 1, ObservableRequests: 1, AwaitingFirstResponse: 1, ProgressTimestampSumUnixMilli: 1_000, OldestProgressUnixMilli: 1_000},
				{Requests: 2, ObservableRequests: 2, ProgressTimestampSumUnixMilli: 3_900, OldestProgressUnixMilli: 1_900},
			},
			want: []float64{1, 0},
		},
		{
			name: "first response breaks equal-count tie",
			loads: []*attrconcurrency.InFlightLoad{
				{Requests: 1, ObservableRequests: 1, AwaitingFirstResponse: 1, ProgressTimestampSumUnixMilli: 1_900, OldestProgressUnixMilli: 1_900},
				{Requests: 1, ObservableRequests: 1, ProgressTimestampSumUnixMilli: 1_000, OldestProgressUnixMilli: 1_000},
			},
			want: []float64{0, 1},
		},
		{
			name: "observable progress breaks equal-count tie",
			loads: []*attrconcurrency.InFlightLoad{
				{Requests: 1},
				{Requests: 1, ObservableRequests: 1, ProgressTimestampSumUnixMilli: 1_000, OldestProgressUnixMilli: 1_000},
			},
			want: []float64{0, 1},
		},
		{
			name: "recent progress breaks otherwise equal tie",
			loads: []*attrconcurrency.InFlightLoad{
				{Requests: 1, ObservableRequests: 1, ProgressTimestampSumUnixMilli: 1_000, OldestProgressUnixMilli: 1_000},
				{Requests: 1, ObservableRequests: 1, ProgressTimestampSumUnixMilli: 1_900, OldestProgressUnixMilli: 1_900},
			},
			want: []float64{0, 1},
		},
		{
			name: "identical load receives identical score",
			loads: []*attrconcurrency.InFlightLoad{
				{Requests: 1},
				{Requests: 1},
			},
			want: []float64{1, 1},
		},
		{
			name:  "missing data is neutral",
			loads: []*attrconcurrency.InFlightLoad{nil, nil},
			want:  []float64{1, 1},
		},
		{
			name: "missing data does not look idle",
			loads: []*attrconcurrency.InFlightLoad{
				nil,
				{},
			},
			want: []float64{0, 1},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoints := make([]scheduling.Endpoint, 0, len(test.loads))
			for i, load := range test.loads {
				endpoints = append(endpoints, newEndpoint(string(rune('a'+i)), load))
			}

			scores := scorer.Score(t.Context(), nil, endpoints)
			for i, endpoint := range endpoints {
				require.Equal(t, test.want[i], scores[endpoint])
			}
		})
	}
}

func TestConsumesInFlightLoad(t *testing.T) {
	t.Parallel()

	deps := New().Consumes()
	require.Equal(t, map[plugin.DataKey]any{
		attrconcurrency.InFlightLoadDataKey: attrconcurrency.InFlightLoad{},
	}, deps.Required)
}
