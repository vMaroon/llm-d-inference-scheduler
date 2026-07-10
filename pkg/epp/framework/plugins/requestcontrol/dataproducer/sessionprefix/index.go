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

package sessionprefix

import (
	"fmt"
	"slices"
	"sync"
	"time"
)

// session is one lineage of appends with per-engine extents. The id derives
// from the chain head at creation, so a lost index rebuilds the same id for
// the same first-seen prefix — the index is a cache over content, never a
// source of truth.
type session struct {
	id string
	// head is the deepest append id of the session's main line.
	head uint64
	// chainLen is the block length of the request that last extended (or
	// created) the session; used to tell delta continuations (shorter than
	// the known history) from full rewrites.
	chainLen int
	// appendIDs are the append ids owned by this session (a fork owns only
	// its divergent suffix; the shared prefix stays with the parent).
	appendIDs []uint64
	// aliasKeys are the declared client ids recorded against this session,
	// kept so eviction drops aliases without scanning the alias map.
	aliasKeys []string
	// engines maps an engine (pod NamespacedName string) to the estimated
	// token extent of this session's prefix resident there. Extents are
	// raised monotonically by seeding and usage; staleness after engine-side
	// eviction is bounded by the session TTL.
	engines  map[string]int64
	lastSeen time.Time
}

// matchResult is the read-only outcome of resolving an incoming chain.
type matchResult struct {
	found     bool
	sessionID string
	// divergent reports a mid-chain match on a session that has moved past
	// the matched append: the incoming request rewrote history after it, so
	// coverage clamps at the matched block instead of the full prompt.
	divergent bool
	// matchedIdx is the index into the incoming chain of the deepest known
	// append; meaningful when divergent.
	matchedIdx int
	// engines is a snapshot of the matched session's per-engine extents.
	engines map[string]int64
}

// sessionIndex maps content chains to sessions and sessions to per-engine
// extents: the cluster's memory map at session granularity, estimate-seeded.
// All state is reconstructible from resent prefixes: eviction costs at most
// a fork re-discovery or a fresh session id, never correctness.
type sessionIndex struct {
	mu       sync.Mutex
	byAppend map[uint64]*session
	sessions map[string]*session
	// aliases maps declared client ids to their session, bridging delta
	// continuations that carry no prefix to rehash.
	aliases map[string]*session

	ttl         time.Duration
	maxSessions int
	now         func() time.Time
}

func newSessionIndex(ttl time.Duration, maxSessions int) *sessionIndex {
	return &sessionIndex{
		byAppend:    map[uint64]*session{},
		sessions:    map[string]*session{},
		aliases:     map[string]*session{},
		ttl:         ttl,
		maxSessions: maxSessions,
		now:         time.Now,
	}
}

// Match resolves an incoming chain (and optional declared aliases) to a
// session without mutating the index; scoring must not create state, only
// scheduled requests do (Commit).
func (x *sessionIndex) Match(chain []uint64, declared []string) matchResult {
	x.mu.Lock()
	defer x.mu.Unlock()

	idx, owner := x.deepestKnownLocked(chain)
	if owner != nil {
		return matchResult{
			found:      true,
			sessionID:  owner.id,
			divergent:  idx < len(chain)-1 && chain[idx] != owner.head,
			matchedIdx: idx,
			engines:    copyExtents(owner.engines),
		}
	}
	if s := x.aliasTargetLocked(chain, declared); s != nil {
		return matchResult{found: true, sessionID: s.id, matchedIdx: -1, engines: copyExtents(s.engines)}
	}
	return matchResult{matchedIdx: -1}
}

// Commit maps a scheduled request's chain to a session — advancing the head
// on linear extension, creating a fork session on mid-chain divergence, a
// fresh session when nothing matches — then records the declared aliases
// against the resolved session and raises its extent on the serving engines
// to xEst (the prefix is assumed to live where the turn was served).
// Returns the resolved session id, or "" for an empty chain.
func (x *sessionIndex) Commit(chain []uint64, declared []string, engines []string, xEst int64) string {
	if len(chain) == 0 {
		return ""
	}
	now := x.now()

	x.mu.Lock()
	defer x.mu.Unlock()

	idx, owner := x.deepestKnownLocked(chain)
	var resolved *session
	switch {
	case owner == nil:
		if s := x.aliasTargetLocked(chain, declared); s != nil {
			// Delta continuation: the alias bridges the handle; the delta's
			// own hashes are NOT registered (they are keys of a different
			// chain).
			resolved = s
		} else {
			resolved = x.newSessionLocked(chain, 0, now)
		}

	case idx == len(chain)-1:
		// Full prefix known: continuation or replay.
		resolved = owner

	case chain[idx] == owner.head:
		// Linear extension of the owner's main line.
		for _, id := range chain[idx+1:] {
			x.byAppend[id] = owner
			owner.appendIDs = append(owner.appendIDs, id)
		}
		owner.head = chain[len(chain)-1]
		owner.chainLen = len(chain)
		resolved = owner

	default:
		// Divergence from a mid-chain append: a fork, discovered
		// structurally. The fork owns only its divergent suffix.
		owner.lastSeen = now
		resolved = x.newSessionLocked(chain, idx+1, now)
	}

	resolved.lastSeen = now
	for _, key := range declared {
		if x.aliases[key] != resolved {
			x.aliases[key] = resolved
			resolved.aliasKeys = append(resolved.aliasKeys, key)
		}
	}
	for _, e := range engines {
		if resolved.engines[e] < xEst {
			resolved.engines[e] = xEst
		}
	}
	return resolved.id
}

// RecordUsage raises the session's extent on the serving engine to the
// engine-reported token count (prompt plus generated output — the response's
// KV contribution, unobservable anywhere else). Extents are monotone-max, so
// absent or partial usage never shrinks a seeded estimate.
func (x *sessionIndex) RecordUsage(sessionID, engine string, tokens int64) {
	if sessionID == "" || engine == "" || tokens <= 0 {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	s, ok := x.sessions[sessionID]
	if !ok {
		return
	}
	if s.engines[engine] < tokens {
		s.engines[engine] = tokens
	}
	s.lastSeen = x.now()
}

// removeEnginesNotIn drops the extents of every engine absent from the
// active set, e.g. pods that were removed from the pool.
func (x *sessionIndex) removeEnginesNotIn(active map[string]struct{}) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, s := range x.sessions {
		for e := range s.engines {
			if _, ok := active[e]; !ok {
				delete(s.engines, e)
			}
		}
	}
}

// deepestKnownLocked returns the deepest incoming append known to the index
// and its owning session, or (-1, nil). Callers must hold x.mu.
func (x *sessionIndex) deepestKnownLocked(chain []uint64) (int, *session) {
	for i := len(chain) - 1; i >= 0; i-- {
		if s, ok := x.byAppend[chain[i]]; ok {
			return i, s
		}
	}
	return -1, nil
}

// aliasTargetLocked returns the session aliased by the first matching
// declared id, provided the incoming chain is shorter than the session's
// known history — a delta continuation. A rewrite at full length under a
// stable client id stays a new lineage. Callers must hold x.mu.
func (x *sessionIndex) aliasTargetLocked(chain []uint64, declared []string) *session {
	for _, key := range declared {
		if s, ok := x.aliases[key]; ok && len(chain) < s.chainLen {
			return s
		}
	}
	return nil
}

// newSessionLocked creates a session owning chain[from:]. Callers must hold
// x.mu. The id derives from the chain head at creation, so recomputation
// after index loss yields the same id for the same first-seen prefix.
func (x *sessionIndex) newSessionLocked(chain []uint64, from int, now time.Time) *session {
	if len(x.sessions) >= x.maxSessions {
		x.evictLocked(now)
	}
	s := &session{
		id:       fmt.Sprintf("sp-%016x", chain[len(chain)-1]),
		head:     chain[len(chain)-1],
		chainLen: len(chain),
		engines:  map[string]int64{},
		lastSeen: now,
	}
	for _, id := range chain[from:] {
		x.byAppend[id] = s
		s.appendIDs = append(s.appendIDs, id)
	}
	x.sessions[s.id] = s
	return s
}

// evictLocked drops expired sessions and, if still at capacity, the
// longest-idle ones — active lineages must not be sacrificed while idle ones
// remain, or state that downstream policies address (retention, prefetch
// targets) silently disappears. Callers must hold x.mu.
func (x *sessionIndex) evictLocked(now time.Time) {
	cutoff := now.Add(-x.ttl)
	for id, s := range x.sessions {
		if s.lastSeen.Before(cutoff) {
			x.dropLocked(id, s)
		}
	}
	if len(x.sessions) < x.maxSessions {
		return
	}
	byIdle := make([]*session, 0, len(x.sessions))
	for _, s := range x.sessions {
		byIdle = append(byIdle, s)
	}
	slices.SortFunc(byIdle, func(a, b *session) int { return a.lastSeen.Compare(b.lastSeen) })
	// Over-evict by 0.1% of capacity so sustained at-cap traffic pays the
	// sort once per margin, not per insert.
	drop := min(len(byIdle), 1+len(x.sessions)-x.maxSessions+x.maxSessions/1000)
	for _, s := range byIdle[:drop] {
		x.dropLocked(s.id, s)
	}
}

// dropLocked removes a session, its append ids, and aliases still pointing
// at it. Callers must hold x.mu.
func (x *sessionIndex) dropLocked(id string, s *session) {
	delete(x.sessions, id)
	for _, a := range s.appendIDs {
		if x.byAppend[a] == s {
			delete(x.byAppend, a)
		}
	}
	for _, key := range s.aliasKeys {
		if x.aliases[key] == s {
			delete(x.aliases, key)
		}
	}
}

// removeExpired evicts sessions idle past the TTL; called by the sweeper.
func (x *sessionIndex) removeExpired() {
	now := x.now()
	cutoff := now.Add(-x.ttl)
	x.mu.Lock()
	defer x.mu.Unlock()
	for id, s := range x.sessions {
		if s.lastSeen.Before(cutoff) {
			x.dropLocked(id, s)
		}
	}
}

func copyExtents(extents map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(extents))
	for k, v := range extents {
		out[k] = v
	}
	return out
}
