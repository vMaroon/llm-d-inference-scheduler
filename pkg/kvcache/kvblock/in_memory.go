/*
Copyright 2025 The llm-d Authors.

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

package kvblock

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	lru "github.com/hashicorp/golang-lru/v2"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-router/pkg/common/collections"
	"github.com/llm-d/llm-d-router/pkg/common/observability/logging"
)

const (
	defaultInMemoryIndexSize = 1e8 // TODO: change to memory-size based configuration
	defaultPodsPerKey        = 10  // number of pods per key
)

// InMemoryIndexConfig holds the configuration for the InMemoryIndex.
type InMemoryIndexConfig struct {
	// Size is the maximum number of keys that can be stored in the index.
	Size int `json:"size"`
	// PodCacheSize is the maximum number of pod entries per key.
	PodCacheSize int `json:"podCacheSize"`
}

// DefaultInMemoryIndexConfig returns a default configuration for the InMemoryIndex.
func DefaultInMemoryIndexConfig() *InMemoryIndexConfig {
	return &InMemoryIndexConfig{
		Size:         defaultInMemoryIndexSize,
		PodCacheSize: defaultPodsPerKey,
	}
}

// NewInMemoryIndex creates a new InMemoryIndex instance.
func NewInMemoryIndex(cfg *InMemoryIndexConfig) (*InMemoryIndex, error) {
	if cfg == nil {
		cfg = DefaultInMemoryIndexConfig()
	}

	index := &InMemoryIndex{podCacheSize: cfg.PodCacheSize}
	cache, err := lru.NewWithEvict(cfg.Size, func(key BlockHash, evicted *PodCache) {
		if current, ok := index.routingData.Load(key); ok && current == evicted {
			index.routingData.Delete(key)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize in-memory index: %w", err)
	}

	engineToRequestKeys, err := lru.New[BlockHash, []BlockHash](cfg.Size)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize in-memory engine key map: %w", err)
	}

	index.data = cache
	index.engineToRequestKeys = engineToRequestKeys
	return index, nil
}

// InMemoryIndex is an in-memory implementation of the Index interface.
type InMemoryIndex struct {
	// mu protects engine-key-level check-and-act operations (Evict's allEmpty
	// check + mapping removal vs Add's pod entry insertion) to prevent TOCTOU races.
	mu sync.Mutex
	// data holds the mapping of requestKeys to sets of pod identifiers.
	data *lru.Cache[BlockHash, *PodCache]
	// routingData mirrors only requestKey -> PodCache pointers. Exact lookup is
	// read-only and should not serialize every block through the global LRU
	// mutex or promote cache recency. The LRU eviction callback removes mirrors.
	routingData sync.Map
	// engineToRequestKeys holds the mapping of engineKeys to requestKeys.
	engineToRequestKeys *lru.Cache[BlockHash, []BlockHash]
	// podCacheSize is the maximum number of pod entries per key.
	podCacheSize int
}

var _ Index = &InMemoryIndex{}
var _ PrefixMatchLookup = &InMemoryIndex{}

// PodCache represents a cache for pod entries.
type PodCache struct {
	cache *podLRU
	// mu protects the cache from concurrent access during check-and-set operations.
	mu sync.Mutex
	// routingSnapshot is an immutable pod/tier view built lazily for exact
	// routing. Add and Remove invalidate it without retaining a second copy of
	// every rank/group PodEntry in the full index.
	routingSnapshot atomic.Pointer[podRoutingSnapshot]
}

type podRoute struct {
	pod         string
	deviceTier  string
	speculative bool
}

type podRoutingSnapshot struct {
	routes []podRoute
}

// podLRU is the per-block LRU. Most blocks have only a few entries, so a compact
// oldest-to-newest slice uses substantially less live memory than a map plus
// linked-list node per entry. Mutations are linear in this small local set;
// request-time routing uses the immutable collapsed snapshot above it.
type podLRU struct {
	size    int
	entries []PodEntry
}

func newPodLRU(size int) (*podLRU, error) {
	if size <= 0 {
		return nil, fmt.Errorf("must provide a positive size")
	}
	return &podLRU{size: size}, nil
}

func (c *podLRU) Add(entry PodEntry) bool {
	for i := range c.entries {
		if c.entries[i] != entry {
			continue
		}
		// Entries are oldest to newest. Refresh recency without allocating.
		copy(c.entries[i:], c.entries[i+1:])
		c.entries[len(c.entries)-1] = entry
		return false
	}
	if len(c.entries) < c.size {
		c.entries = append(c.entries, entry)
		return true
	}
	copy(c.entries, c.entries[1:])
	c.entries[len(c.entries)-1] = entry
	return true
}

func (c *podLRU) Remove(entry PodEntry) bool {
	for i := range c.entries {
		if c.entries[i] != entry {
			continue
		}
		copy(c.entries[i:], c.entries[i+1:])
		c.entries[len(c.entries)-1] = PodEntry{}
		c.entries = c.entries[:len(c.entries)-1]
		return true
	}
	return false
}

func (c *podLRU) Len() int { return len(c.entries) }

func (c *podLRU) Keys() []PodEntry {
	entries := make([]PodEntry, len(c.entries))
	copy(entries, c.entries)
	return entries
}

func (c *podLRU) Range(yield func(PodEntry) bool) {
	for _, entry := range c.entries {
		if !yield(entry) {
			return
		}
	}
}

// routes returns an immutable, rank/group-collapsed routing view. The full
// PodEntry set remains authoritative for exact eviction and LRU capacity.
func (c *PodCache) routes() []podRoute {
	if snapshot := c.routingSnapshot.Load(); snapshot != nil {
		return snapshot.routes
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if snapshot := c.routingSnapshot.Load(); snapshot != nil {
		return snapshot.routes
	}

	deduplicated := make(map[podRoute]struct{}, c.cache.Len())
	routes := make([]podRoute, 0, c.cache.Len())
	c.cache.Range(func(entry PodEntry) bool {
		route := podRoute{
			pod:         entry.PodIdentifier,
			deviceTier:  entry.DeviceTier,
			speculative: entry.Speculative,
		}
		if _, exists := deduplicated[route]; exists {
			return true
		}
		deduplicated[route] = struct{}{}
		routes = append(routes, route)
		return true
	})
	snapshot := &podRoutingSnapshot{routes: routes}
	c.routingSnapshot.Store(snapshot)
	return snapshot.routes
}

type prefixPodTier struct {
	pod  string
	tier string
}

// LookupPrefixMatches computes all candidate pods' weighted and unweighted
// contiguous-prefix matches in one index traversal.
func (m *InMemoryIndex) LookupPrefixMatches(
	ctx context.Context,
	requestKeys []BlockHash,
	podIdentifierSet sets.Set[string],
	mediumWeights map[string]float64,
	speculativeTier string,
) (map[string]PrefixMatch, error) {
	if len(requestKeys) == 0 {
		return nil, fmt.Errorf("no requestKeys provided for lookup")
	}

	matches := map[string]PrefixMatch{}
	alivePods := map[string]struct{}{}
	aliveTiers := map[prefixPodTier]struct{}{}
	weightsAtKey := map[string]float64{}
	tiersAtKey := map[prefixPodTier]struct{}{}

	for keyIndex, requestKey := range requestKeys {
		if keyIndex&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}

		clear(weightsAtKey)
		clear(tiersAtKey)
		if value, found := m.routingData.Load(requestKey); found {
			pods := value.(*PodCache)
			for _, route := range pods.routes() {
				if podIdentifierSet.Len() > 0 && !podIdentifierSet.Has(route.pod) {
					continue
				}
				weight := 1.0
				if configured, ok := mediumWeights[route.deviceTier]; ok {
					weight = configured
				}
				if current, ok := weightsAtKey[route.pod]; !ok || weight > current {
					weightsAtKey[route.pod] = weight
				}
				tier := route.deviceTier
				if route.speculative {
					tier = speculativeTier
				}
				tiersAtKey[prefixPodTier{pod: route.pod, tier: tier}] = struct{}{}
			}
		}

		if keyIndex == 0 {
			for podID, weight := range weightsAtKey {
				alivePods[podID] = struct{}{}
				matches[podID] = PrefixMatch{
					Score:              weight,
					CachedBlocks:       1,
					CachedBlocksByTier: map[string]int{},
				}
			}
			for podTier := range tiersAtKey {
				aliveTiers[podTier] = struct{}{}
				match := matches[podTier.pod]
				match.CachedBlocksByTier[podTier.tier] = 1
				matches[podTier.pod] = match
			}
			if len(alivePods) == 0 {
				break
			}
			continue
		}

		for podID := range alivePods {
			if weight, ok := weightsAtKey[podID]; ok {
				match := matches[podID]
				match.Score += weight
				match.CachedBlocks++
				matches[podID] = match
			} else {
				delete(alivePods, podID)
			}
		}
		for podTier := range aliveTiers {
			if _, ok := tiersAtKey[podTier]; ok {
				matches[podTier.pod].CachedBlocksByTier[podTier.tier]++
			} else {
				delete(aliveTiers, podTier)
			}
		}
		if len(alivePods) == 0 {
			break
		}
	}

	return matches, nil
}

// Lookup receives a list of requestKeys and a set of pod identifiers,
// and retrieves the filtered pods associated with those keys.
// The filtering is done based on the pod identifiers provided.
// If the podIdentifierSet is empty, all pods are returned.
//
// It returns:
// 1. A map where the keys are those in (1) and the values are pod-identifiers.
// 2. An error if any occurred during the operation.
func (m *InMemoryIndex) Lookup(ctx context.Context, requestKeys []BlockHash,
	podIdentifierSet sets.Set[string],
) (map[BlockHash][]PodEntry, error) {
	if len(requestKeys) == 0 {
		return nil, fmt.Errorf("no requestKeys provided for lookup")
	}

	traceLogger := log.FromContext(ctx).V(logging.TRACE).WithName("kvblock.InMemoryIndex.Lookup")

	podsPerKey := make(map[BlockHash][]PodEntry, len(requestKeys))
	highestHitIdx := 0

	for idx, requestKey := range requestKeys {
		if idx&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if pods, found := m.data.Get(requestKey); found { //nolint:nestif // TODO: can this be optimized?
			if pods == nil {
				traceLogger.Info("no pods found for key, cutting search", "key", requestKey)
				return podsPerKey, nil // early stop since prefix-chain breaks here
			}

			pods.mu.Lock()
			if pods.cache.Len() == 0 {
				pods.mu.Unlock()
				traceLogger.Info("no pods found for key, cutting search", "key", requestKey)
				return podsPerKey, nil // early stop since prefix-chain breaks here
			}

			highestHitIdx = idx

			entries := pods.cache.Keys()
			pods.mu.Unlock()
			if podIdentifierSet.Len() == 0 {
				// If no pod identifiers are provided, return all pods
				podsPerKey[requestKey] = entries
			} else {
				// Filter pods based on the provided pod identifiers
				filtered := make([]PodEntry, 0, min(len(entries), podIdentifierSet.Len()))
				for _, pod := range entries {
					if podIdentifierSet.Has(pod.PodIdentifier) {
						filtered = append(filtered, pod)
					}
				}
				if len(filtered) > 0 {
					podsPerKey[requestKey] = filtered
				}
			}
		} else {
			traceLogger.Info("key not found in index", "key", requestKey)
		}
	}

	if traceLogger.Enabled() {
		traceLogger.Info("lookup completed", "highest-hit-index", highestHitIdx,
			"pods-per-key", podsPerKeyPrintHelper(podsPerKey))
	}

	return podsPerKey, nil
}

// Add adds a set of engineKeys/requestKeys and their associated pod entries to the index backend.
// If engineKeys is nil, only requestKey -> PodEntry mappings are created (no engineKey -> requestKey mapping).
// This is used for speculative entries where engine keys are not yet known.
// When engineKeys is non-nil, the mapping type is inferred from the ratio of array lengths.
func (m *InMemoryIndex) Add(ctx context.Context, engineKeys, requestKeys []BlockHash, entries []PodEntry) error {
	if len(requestKeys) == 0 || len(entries) == 0 {
		return fmt.Errorf("no keys or entries provided for adding to index")
	}

	traceLogger := log.FromContext(ctx).V(logging.TRACE).WithName("kvblock.InMemoryIndex.Add")

	// Build engine->request mappings when engine keys are provided.
	// The ratio of array lengths determines the mapping type:
	//   equal  (4 eng, 4 req) -> 1:1   E0->R0, E1->R1, ...
	//   many:1 (4 eng, 1 req) -> E0->R0, E1->R0, E2->R0, E3->R0
	//   1:many (1 eng, 4 req) -> E0->[R0, R1, R2, R3]
	if engineKeys != nil {
		mappings := engineToRequestMapping(engineKeys, requestKeys)
		for ek, rks := range mappings {
			m.engineToRequestKeys.Add(ek, rks)
		}
	}

	// Store requestKey -> PodCache mappings for all request keys.
	// Hold m.mu to prevent Evict from checking emptiness and removing the
	// engine→request mapping while we are inserting pod entries.
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, requestKey := range requestKeys {
		var podCache *PodCache
		var found bool

		// Try to get existing cache first
		podCache, found = m.data.Get(requestKey)
		//nolint:nestif // double-checked locking pattern
		if !found {
			// Create new cache
			cache, err := newPodLRU(m.podCacheSize)
			if err != nil {
				return fmt.Errorf("failed to create pod cache for key %s: %w", requestKey.String(), err)
			}
			newPodCache := &PodCache{cache: cache}

			// Try to add, but use existing if another thread added it first
			// This is a bounded retry (1) - not perfectly safe but for practical use-cases and scenarios
			// this should be sufficient
			contains, _ := m.data.ContainsOrAdd(requestKey, newPodCache)
			if contains {
				podCache, found = m.data.Get(requestKey)
				if !found { // Extremely irregular workload pattern - key evicted
					m.data.Add(requestKey, newPodCache)
					podCache = newPodCache
				}
			} else {
				// We successfully added our cache
				podCache = newPodCache
			}
		}

		podCache.mu.Lock()
		changed := false
		for _, entry := range entries {
			changed = podCache.cache.Add(entry) || changed
		}
		if changed {
			podCache.routingSnapshot.Store(nil)
		}
		podCache.mu.Unlock()
		m.routingData.Store(requestKey, podCache)

		traceLogger.Info("added pods to key", "requestKey", requestKey, "pods", entries)
	}

	return nil
}

// Evict removes a key and its associated pod entries from the index backend.
// keyType indicates whether the key is an EngineKey (requires engine→request lookup)
// or a RequestKey (used directly for speculative entries without engineKey mapping).
func (m *InMemoryIndex) Evict(ctx context.Context, key BlockHash, keyType KeyType, entries []PodEntry) error {
	if len(entries) == 0 {
		return fmt.Errorf("no entries provided for eviction from index")
	}

	traceLogger := log.FromContext(ctx).V(logging.TRACE).WithName("kvblock.InMemoryIndex.Evict")

	switch keyType {
	case EngineKey:
		rks, found := m.engineToRequestKeys.Get(key)
		if !found {
			traceLogger.Info("engineKey not found in mapping, nothing to evict", "engineKey", key)
			return nil
		}

		for _, rk := range rks {
			m.evictPodsFromRequestKey(rk, key, entries, traceLogger)
		}

		m.mu.Lock()
		allEmpty := true
		for _, rk := range rks {
			if pc, found := m.data.Get(rk); found && pc != nil {
				pc.mu.Lock()
				nonEmpty := pc.cache.Len() > 0
				pc.mu.Unlock()
				if nonEmpty {
					allEmpty = false
					break
				}
			}
		}
		if allEmpty {
			m.engineToRequestKeys.Remove(key)
		}
		m.mu.Unlock()
		return nil
	case RequestKey:
		m.evictPodsFromRequestKey(key, EmptyBlockHash, entries, traceLogger)
		return nil
	default:
		return fmt.Errorf("unknown key type: %d", keyType)
	}
}

// evictPodsFromRequestKey removes the given pod entries from a single request key's cache.
// If the cache becomes empty, the request key is removed from the index.
func (m *InMemoryIndex) evictPodsFromRequestKey(requestKey, engineKey BlockHash, entries []PodEntry, traceLogger logr.Logger) {
	podCache, found := m.data.Get(requestKey)
	if !found || podCache == nil {
		traceLogger.Info("requestKey not found in index, nothing to evict", "requestKey", requestKey, "engineKey", engineKey)
		return
	}

	podCache.mu.Lock()
	changed := false
	for _, entry := range entries {
		changed = podCache.cache.Remove(entry) || changed
	}
	if changed {
		podCache.routingSnapshot.Store(nil)
	}

	isEmpty := podCache.cache.Len() == 0
	podCache.mu.Unlock()

	traceLogger.Info("evicted pods from key", "requestKey", requestKey, "engineKey", engineKey, "pods", entries)

	if !isEmpty {
		return
	}

	// Remove key from main cache if empty.
	// Re-fetch and hold the lock through removal to prevent racing with Add.
	currentCache, stillExists := m.data.Get(requestKey)
	if !stillExists || currentCache == nil {
		return
	}

	currentCache.mu.Lock()
	if currentCache.cache.Len() == 0 {
		m.data.Remove(requestKey)
		traceLogger.Info("removed requestKey from index as no pods remain", "requestKey", requestKey)
	}
	currentCache.mu.Unlock()
}

// Clear removes every entry for the pod from the index, across all device tiers.
// O(N) over the index, but Clear is rare and off the Lookup/Add hot path. Reuses
// evictPodsFromRequestKey for race-safe removal, and holds no global lock — only
// each PodCache's mu, briefly — so it does not stall Lookup.
//
// The engineKey->requestKey mapping (engineToRequestKeys) is intentionally left
// untouched: it is LRU-bounded, self-heals when the pod re-Adds the same prefixes,
// and any stale mapping resolves to an emptied request key that correctly breaks
// the prefix chain in Lookup.
func (m *InMemoryIndex) Clear(ctx context.Context, podIdentifier string) error {
	traceLogger := log.FromContext(ctx).V(logging.TRACE).WithName("kvblock.InMemoryIndex.Clear")

	for _, requestKey := range m.data.Keys() {
		// Peek so a clear does not promote LRU recency on keys it scans.
		podCache, found := m.data.Peek(requestKey)
		if !found || podCache == nil {
			continue
		}

		podCache.mu.Lock()
		var matched []PodEntry
		for _, entry := range podCache.cache.Keys() {
			if entry.PodIdentifier == podIdentifier {
				matched = append(matched, entry)
			}
		}
		podCache.mu.Unlock()

		if len(matched) > 0 {
			m.evictPodsFromRequestKey(requestKey, EmptyBlockHash, matched, traceLogger)
		}
	}

	traceLogger.Info("cleared pod from index", "pod", podIdentifier)
	return nil
}

// GetRequestKey returns the last request key (highest index in the chain) associated with the given engineKey.
// This is what Pool uses for parent hash resolution.
// Returns an error if the engineKey mapping is missing (e.g., already evicted).
func (m *InMemoryIndex) GetRequestKey(ctx context.Context, engineKey BlockHash) (BlockHash, error) {
	rks, found := m.engineToRequestKeys.Get(engineKey)
	if !found || len(rks) == 0 {
		return EmptyBlockHash, fmt.Errorf("engine key not found: %s", engineKey.String())
	}
	return rks[len(rks)-1], nil
}

// podsPerKeyPrintHelper formats a map of keys to pod names for printing.
func podsPerKeyPrintHelper(ks map[BlockHash][]PodEntry) string {
	var b strings.Builder
	for k, v := range ks {
		fmt.Fprintf(&b, "%s: %v\n", k.String(), collections.SliceMap(v, func(pod PodEntry) string {
			return pod.String()
		}))
	}
	return b.String()
}
