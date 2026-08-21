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

package preciseprefixcache

import (
	"fmt"
	"sync"

	"github.com/llm-d/llm-d-router/pkg/kvcache/metrics"
	"github.com/llm-d/llm-d-router/pkg/kvevents"
)

const (
	defaultFullReportThreshold = 0.80
	defaultMinMissingBlocks    = 32
)

// FullReportRepairConfig enables bounded per-request full KV-cache reports
// for endpoints whose event-derived index may be incomplete.
type FullReportRepairConfig struct {
	FullReportThreshold float64 `json:"fullReportThreshold,omitempty"`
	MinMissingBlocks    int     `json:"minMissingBlocks,omitempty"`
	// PrefillProfileName identifies the disaggregation profile whose selected
	// endpoint owns the prefix cache being repaired.
	PrefillProfileName string `json:"prefillProfileName,omitempty"`
}

func normalizeFullReportRepairConfig(config FullReportRepairConfig) (FullReportRepairConfig, error) {
	if config.FullReportThreshold == 0 {
		config.FullReportThreshold = defaultFullReportThreshold
	}
	if config.MinMissingBlocks == 0 {
		config.MinMissingBlocks = defaultMinMissingBlocks
	}
	if config.PrefillProfileName == "" {
		config.PrefillProfileName = experimentalPrefillProfile
	}
	if config.FullReportThreshold <= 0 || config.FullReportThreshold > 1 {
		return FullReportRepairConfig{}, fmt.Errorf("fullReportThreshold must be in (0, 1], got %g", config.FullReportThreshold)
	}
	if config.MinMissingBlocks < 1 {
		return FullReportRepairConfig{}, fmt.Errorf("minMissingBlocks must be positive, got %d", config.MinMissingBlocks)
	}
	return config, nil
}

func validateFullReportRepairPrerequisites(config *kvevents.Config) error {
	if config == nil || !config.DiscoverPods || config.PodDiscoveryConfig == nil {
		return fmt.Errorf("fullReportRepair requires kvEventsConfig.discoverPods with podDiscoveryConfig")
	}
	if config.ZMQEndpoint != "" {
		return fmt.Errorf("fullReportRepair does not support kvEventsConfig.zmqEndpoint global-socket mode")
	}
	return nil
}

type endpointRepairState struct {
	eligible bool
	force    bool
}

type fullReportRepair struct {
	mu                 sync.Mutex
	endpoints          map[string]endpointRepairState
	threshold          float64
	minMissing         int
	prefillProfileName string
}

func newFullReportRepair(config FullReportRepairConfig) *fullReportRepair {
	if config.PrefillProfileName == "" {
		config.PrefillProfileName = experimentalPrefillProfile
	}
	return &fullReportRepair{
		endpoints:          make(map[string]endpointRepairState),
		threshold:          config.FullReportThreshold,
		minMissing:         config.MinMissingBlocks,
		prefillProfileName: config.PrefillProfileName,
	}
}

func (r *fullReportRepair) observe(endpoint string, event kvevents.StreamEvent) {
	if r == nil || endpoint == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	metrics.FullReportRepairSignals.WithLabelValues(string(event)).Inc()
	switch event {
	case kvevents.StreamEventAttached:
		state := r.endpoints[endpoint]
		state.eligible = true
		r.endpoints[endpoint] = state
	case kvevents.StreamEventMissingParent,
		kvevents.StreamEventSequenceDiscontinuity,
		kvevents.StreamEventProcessingFailure:
		r.endpoints[endpoint] = endpointRepairState{eligible: true, force: true}
	case kvevents.StreamEventKnownEmpty,
		kvevents.StreamEventAuthoritativeSnapshot,
		kvevents.StreamEventDetached:
		delete(r.endpoints, endpoint)
	}
	metrics.FullReportRepairEligibleEndpoints.Set(float64(len(r.endpoints)))
}

func (r *fullReportRepair) shouldRequest(endpoint string, match repairMatch) (bool, string) {
	if r == nil {
		return false, ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.endpoints[endpoint]
	if !state.eligible {
		return false, ""
	}
	missing := match.total - match.confirmed
	if missing < r.minMissing || match.total <= 0 {
		return false, ""
	}
	if state.force {
		state.force = false
		r.endpoints[endpoint] = state
		return true, "integrity"
	}
	if float64(match.confirmed)/float64(match.total) < r.threshold {
		return true, "threshold"
	}
	return false, ""
}
