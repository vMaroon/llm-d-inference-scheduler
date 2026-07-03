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

package chainidentity

import (
	"fmt"
	"sync"
	"time"
)

// lineage is one session lineage: a linear chain of append ids. The id is
// derived from the chain head at creation, so a lost index rebuilds the same
// id for the same first-seen prefix — the index is a cache over content, not
// a source of truth.
type lineage struct {
	id string
	// head is the deepest append id of the lineage's main line.
	head uint64
	// chainLen is the block length of the request that last extended (or
	// created) the lineage; used to tell delta continuations (shorter than
	// the known history) from full rewrites.
	chainLen int
	// appendIDs are the append ids owned by this lineage (a fork owns only
	// its divergent suffix; the shared prefix stays with the parent).
	appendIDs []uint64
	lastSeen  time.Time
}

// resolution is the identity outcome for one request.
type resolution struct {
	// LineageID is the stable session id the request belongs to.
	LineageID string
	// ForkParent is the parent lineage id when this request was discovered
	// to fork from an existing lineage; empty otherwise.
	ForkParent string
}

// lineageIndex resolves content chains to lineages. All state is
// reconstructible from resent prefixes: eviction costs at most a fork
// re-discovery or a fresh lineage id, never correctness.
type lineageIndex struct {
	mu       sync.Mutex
	byAppend map[uint64]*lineage
	lineages map[string]*lineage
	// aliases maps declared client ids (headers, in-band dialects) to their
	// lineage, bridging delta continuations that carry no prefix to rehash.
	aliases map[string]*lineage

	ttl         time.Duration
	maxLineages int
	now         func() time.Time
}

func newLineageIndex(ttl time.Duration, maxLineages int) *lineageIndex {
	return &lineageIndex{
		byAppend:    map[uint64]*lineage{},
		lineages:    map[string]*lineage{},
		aliases:     map[string]*lineage{},
		ttl:         ttl,
		maxLineages: maxLineages,
		now:         time.Now,
	}
}

// resolve maps a request's content chain (and optional declared client id) to
// a lineage:
//
//   - deepest known append == chain head: continuation (or replay) of that
//     lineage.
//   - deepest known append == its lineage's head, with new blocks after it:
//     linear extension; the lineage's head advances.
//   - deepest known append mid-chain of its lineage: the request diverges
//     from a point the lineage has moved past — a fork, discovered without
//     any declaration. The fork owns only its divergent suffix.
//   - nothing known, declared id aliased, and the chain is shorter than the
//     lineage's known history: delta continuation — the alias bridges the
//     handle, and the delta's own hashes are NOT registered (they are keys
//     of a different chain).
//   - nothing known otherwise: a new lineage. Under a known declared id this
//     IS structural rollover — the rewritten chain is a new lineage.
func (x *lineageIndex) resolve(chain []uint64, declared string) resolution {
	if len(chain) == 0 {
		return resolution{}
	}
	now := x.now()

	x.mu.Lock()
	defer x.mu.Unlock()

	idx := -1
	var owner *lineage
	for i := len(chain) - 1; i >= 0; i-- {
		if l, ok := x.byAppend[chain[i]]; ok {
			idx, owner = i, l
			break
		}
	}

	switch {
	case owner == nil:
		if declared != "" {
			if l, ok := x.aliases[declared]; ok && len(chain) < l.chainLen {
				l.lastSeen = now
				return resolution{LineageID: l.id}
			}
		}
		l := x.newLineageLocked(chain, 0, now)
		if declared != "" {
			x.aliases[declared] = l
		}
		return resolution{LineageID: l.id}

	case idx == len(chain)-1:
		// Full prefix known: continuation or replay.
		owner.lastSeen = now
		if declared != "" {
			x.aliases[declared] = owner
		}
		return resolution{LineageID: owner.id}

	case chain[idx] == owner.head:
		// Linear extension of the owner's main line.
		for _, id := range chain[idx+1:] {
			x.byAppend[id] = owner
			owner.appendIDs = append(owner.appendIDs, id)
		}
		owner.head = chain[len(chain)-1]
		owner.chainLen = len(chain)
		owner.lastSeen = now
		if declared != "" {
			x.aliases[declared] = owner
		}
		return resolution{LineageID: owner.id}

	default:
		// Divergence from a mid-chain append: a fork, discovered structurally.
		owner.lastSeen = now
		f := x.newLineageLocked(chain, idx+1, now)
		if declared != "" {
			x.aliases[declared] = f
		}
		return resolution{LineageID: f.id, ForkParent: owner.id}
	}
}

// newLineageLocked creates a lineage owning chain[from:]. Callers must hold
// x.mu. The id derives from the chain head at creation, so recomputation
// after index loss yields the same id for the same first-seen prefix.
func (x *lineageIndex) newLineageLocked(chain []uint64, from int, now time.Time) *lineage {
	if len(x.lineages) >= x.maxLineages {
		x.evictLocked(now)
	}
	l := &lineage{
		id:       fmt.Sprintf("ch-%016x", chain[len(chain)-1]),
		head:     chain[len(chain)-1],
		chainLen: len(chain),
		lastSeen: now,
	}
	for _, id := range chain[from:] {
		x.byAppend[id] = l
		l.appendIDs = append(l.appendIDs, id)
	}
	x.lineages[l.id] = l
	return l
}

// evictLocked drops expired lineages and, if still at capacity, up to
// capEvictBatch arbitrary ones. Callers must hold x.mu.
func (x *lineageIndex) evictLocked(now time.Time) {
	cutoff := now.Add(-x.ttl)
	for id, l := range x.lineages {
		if l.lastSeen.Before(cutoff) {
			x.dropLocked(id, l)
		}
	}
	if len(x.lineages) < x.maxLineages {
		return
	}
	dropped := 0
	for id, l := range x.lineages {
		x.dropLocked(id, l)
		dropped++
		if dropped >= capEvictBatch {
			break
		}
	}
}

// dropLocked removes a lineage, its append ids, and aliases pointing at it.
// Callers must hold x.mu.
func (x *lineageIndex) dropLocked(id string, l *lineage) {
	delete(x.lineages, id)
	for _, a := range l.appendIDs {
		if x.byAppend[a] == l {
			delete(x.byAppend, a)
		}
	}
	for declared, target := range x.aliases {
		if target == l {
			delete(x.aliases, declared)
		}
	}
}

// sweep periodically evicts expired lineages until ctx is done.
func (x *lineageIndex) removeExpired() {
	now := x.now()
	cutoff := now.Add(-x.ttl)
	x.mu.Lock()
	defer x.mu.Unlock()
	for id, l := range x.lineages {
		if l.lastSeen.Before(cutoff) {
			x.dropLocked(id, l)
		}
	}
}
