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

	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
)

// extractEndpointSet builds the "address:port" identifier set used to filter
// kvblock.Index lookups to candidate endpoints. Endpoints without metadata
// are skipped.
func extractEndpointSet(endpoints []scheduling.Endpoint) sets.Set[string] {
	endpointSet := sets.New[string]()
	for _, ep := range endpoints {
		if m := ep.GetMetadata(); m != nil {
			endpointSet.Insert(fmt.Sprintf("%s:%s", m.Address, m.Port))
		}
	}
	return endpointSet
}

// endpointStats holds the prefix-match measurements for one pod, all derived
// from the same contiguous-from-key-0 walk over a prompt's block keys.
type endpointStats struct {
	// weightedScore is the device-tier-weighted longest-prefix score
	// (kvcache.LongestPrefixScorer semantics): per key, the pod's maximum
	// entry weight across tiers, summed while the pod holds every key from
	// the first.
	weightedScore float64
	// cachedBlocks is the unweighted length of the same contiguous run:
	// every held block counts as one regardless of device tier.
	cachedBlocks int
	// cachedBlocksByTier counts, per device tier, the contiguous run the pod
	// holds in that tier; a block held in several tiers counts once per tier,
	// so each tier's count is at most cachedBlocks. Speculative entries count
	// under attrprefix.SpeculativeTierKey: PreRequest inserts them before
	// vLLM has reported placement, so they carry no device tier. A tier's run
	// can end before the pod's run does. Non-nil, possibly empty.
	cachedBlocksByTier map[string]int
}

// podScan is the per-pod walk state for endpointPrefixStats.
type podScan struct {
	stats *endpointStats
	// aliveTiers maps tier -> index of the last key at which the tier's
	// contiguous run was intact. A tier only advances when its recorded index
	// matches the preceding key, so a gap retires it without allocating
	// per-key tier sets.
	aliveTiers map[string]int
}

// endpointPrefixStats walks keys once and returns per-pod endpointStats for
// every pod holding the first key. Entry weight is mediumWeights by
// DeviceTier with a 1.0 fallback; a pod leaves the run at the first key it
// holds no entry for. Scratch maps are reused across keys so the walk
// allocates per pod, not per key.
func endpointPrefixStats(keys []kvblock.BlockHash, keyToPods map[kvblock.BlockHash][]kvblock.PodEntry,
	mediumWeights map[string]float64,
) map[string]*endpointStats {
	stats := make(map[string]*endpointStats)
	if len(keys) == 0 {
		return stats
	}

	active := make(map[string]*podScan)
	// Per-key max weight per pod; doubles as the presence marker for the
	// contiguous-run check.
	curWeights := make(map[string]float64)

	for i, key := range keys {
		if i > 0 && len(active) == 0 {
			break
		}
		clear(curWeights)
		for _, e := range keyToPods[key] {
			ps := active[e.PodIdentifier]
			if ps == nil {
				if i > 0 {
					continue // pod's contiguous run ended (or never started)
				}
				st := &endpointStats{cachedBlocksByTier: map[string]int{}}
				stats[e.PodIdentifier] = st
				ps = &podScan{stats: st, aliveTiers: map[string]int{}}
				active[e.PodIdentifier] = ps
			}
			weight := 1.0
			if w, ok := mediumWeights[e.DeviceTier]; ok {
				weight = w
			}
			if cur, ok := curWeights[e.PodIdentifier]; !ok || weight > cur {
				curWeights[e.PodIdentifier] = weight
			}
			tier := e.DeviceTier
			if e.Speculative {
				tier = attrprefix.SpeculativeTierKey
			}
			if i == 0 {
				if _, ok := ps.aliveTiers[tier]; !ok {
					ps.aliveTiers[tier] = 0
					ps.stats.cachedBlocksByTier[tier] = 1
				}
			} else if last, ok := ps.aliveTiers[tier]; ok && last == i-1 {
				ps.aliveTiers[tier] = i
				ps.stats.cachedBlocksByTier[tier]++
			}
		}
		for pod, ps := range active {
			if w, ok := curWeights[pod]; ok {
				ps.stats.weightedScore += w
				ps.stats.cachedBlocks++
			} else {
				delete(active, pod)
			}
		}
	}
	return stats
}
