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

// matchedBlockCount returns the number of contiguous cached prefix blocks held
// by podID, counting from the first block until the first block the pod does
// not hold. This is the unweighted counterpart of the device-tier-weighted
// kvblock scorer: every cached block counts as one regardless of device tier,
// so a pod present at keys[0..n-1] yields n.
func matchedBlockCount(keys []kvblock.BlockHash, keyToPods map[kvblock.BlockHash][]kvblock.PodEntry, podID string) int {
	count, _ := matchedBlockCounts(keys, keyToPods, podID)
	return count
}

// matchedBlockCountByTier returns, per device tier, the number of contiguous
// cached prefix blocks podID holds in that tier, counting from the first
// block until the first block the pod does not hold in that tier. A block
// held in several tiers counts once per tier, so each tier's count is at most
// matchedBlockCount for the same pod. Tiers are recorded as found in the
// index, except speculative entries, which count under
// attrprefix.SpeculativeTierKey: PreRequest inserts them before vLLM has
// reported placement, so they carry no device tier.
// Returns a non-nil (possibly empty) map.
func matchedBlockCountByTier(keys []kvblock.BlockHash, keyToPods map[kvblock.BlockHash][]kvblock.PodEntry, podID string) map[string]int {
	_, counts := matchedBlockCounts(keys, keyToPods, podID)
	return counts
}

// matchedBlockCounts computes the unweighted and per-tier contiguous prefix
// lengths in one pass. The producer needs both results; keeping them together
// avoids scanning every block and PodEntry twice for every endpoint.
func matchedBlockCounts(
	keys []kvblock.BlockHash, keyToPods map[kvblock.BlockHash][]kvblock.PodEntry, podID string,
) (int, map[string]int) {
	counts := map[string]int{}
	alive := map[string]struct{}{}
	tiersAtKey := map[string]struct{}{}
	matched := 0
	for _, key := range keys {
		clear(tiersAtKey)
		found := false
		for _, e := range keyToPods[key] {
			if e.PodIdentifier == podID {
				found = true
				if e.Speculative {
					tiersAtKey[attrprefix.SpeculativeTierKey] = struct{}{}
				} else {
					tiersAtKey[e.DeviceTier] = struct{}{}
				}
			}
		}
		if !found {
			break
		}
		matched++
		if matched == 1 {
			for tier := range tiersAtKey {
				alive[tier] = struct{}{}
				counts[tier] = 1
			}
		} else {
			for tier := range alive {
				if _, ok := tiersAtKey[tier]; ok {
					counts[tier]++
				} else {
					delete(alive, tier)
				}
			}
		}
	}
	return matched, counts
}

type podTier struct {
	pod  string
	tier string
}

// matchedBlockCountsByPod computes every pod's unweighted and per-tier
// contiguous prefix lengths in one traversal of the block entries. PodEntry
// can contain one record per rank and device tier, so the per-key scratch sets
// deduplicate those records before advancing each live prefix chain.
func matchedBlockCountsByPod(
	keys []kvblock.BlockHash, keyToPods map[kvblock.BlockHash][]kvblock.PodEntry,
) (map[string]int, map[string]map[string]int) {
	counts := map[string]int{}
	countsByTier := map[string]map[string]int{}
	if len(keys) == 0 {
		return counts, countsByTier
	}

	alivePods := map[string]struct{}{}
	aliveTiers := map[podTier]struct{}{}
	podsAtKey := map[string]struct{}{}
	tiersAtKey := map[podTier]struct{}{}

	for keyIndex, key := range keys {
		clear(podsAtKey)
		clear(tiersAtKey)
		for _, entry := range keyToPods[key] {
			podsAtKey[entry.PodIdentifier] = struct{}{}
			tier := entry.DeviceTier
			if entry.Speculative {
				tier = attrprefix.SpeculativeTierKey
			}
			tiersAtKey[podTier{pod: entry.PodIdentifier, tier: tier}] = struct{}{}
		}

		if keyIndex == 0 {
			for podID := range podsAtKey {
				alivePods[podID] = struct{}{}
				counts[podID] = 1
			}
			for podTier := range tiersAtKey {
				aliveTiers[podTier] = struct{}{}
				if countsByTier[podTier.pod] == nil {
					countsByTier[podTier.pod] = map[string]int{}
				}
				countsByTier[podTier.pod][podTier.tier] = 1
			}
			continue
		}

		for podID := range alivePods {
			if _, ok := podsAtKey[podID]; ok {
				counts[podID]++
			} else {
				delete(alivePods, podID)
			}
		}
		for podTier := range aliveTiers {
			if _, ok := tiersAtKey[podTier]; ok {
				countsByTier[podTier.pod][podTier.tier]++
			} else {
				delete(aliveTiers, podTier)
			}
		}
		if len(alivePods) == 0 {
			break
		}
	}

	return counts, countsByTier
}
