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

// Package latencycost defines the per-endpoint latency-cost attribute:
// the predicted time-to-first-token of the request being scheduled on the
// endpoint, decomposed into queue-wait, KV-fetch, and prefill-compute terms.
// Milliseconds are the shared unit: any producer able to price a term in
// milliseconds can populate the attribute, and consumers rank or gate on it
// without knowing which model produced the numbers.
package latencycost

import (
	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	latencycostconstants "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/latencycost/constants"
)

// LatencyCostInfoDataKey carries the per-endpoint LatencyCostInfo for the
// request being scheduled. Populated on each candidate endpoint at the start
// of every scheduling cycle.
var LatencyCostInfoDataKey = plugin.NewDataKey("LatencyCostInfoDataKey", latencycostconstants.LatencyCostProducerType)

// LatencyCostInfo is the predicted time-to-first-token of the current request
// on an endpoint, in milliseconds, decomposed by cause.
type LatencyCostInfo struct {
	// QueueMs is the wait before this request's prefill starts: uncached
	// prefill tokens already committed to the endpoint, at its prefill rate.
	QueueMs float64

	// FetchMs is the cost of moving cached KV the endpoint cannot serve from
	// GPU memory (CPU-tier onload).
	FetchMs float64

	// PrefillMs is the compute cost of the uncached suffix, including the
	// attention term for long contexts and decode interference.
	PrefillMs float64
}

// TotalMs returns the predicted time-to-first-token: the sum of all terms.
func (i *LatencyCostInfo) TotalMs() float64 {
	return i.QueueMs + i.FetchMs + i.PrefillMs
}

// Clone returns an independent copy of the LatencyCostInfo.
func (i *LatencyCostInfo) Clone() fwkdl.Cloneable {
	if i == nil {
		return nil
	}
	cp := *i
	return &cp
}
