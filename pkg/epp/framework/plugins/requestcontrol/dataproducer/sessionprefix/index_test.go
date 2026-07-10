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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testIndex(ttl time.Duration, maxSessions int) (*sessionIndex, *time.Time) {
	x := newSessionIndex(ttl, maxSessions)
	now := time.Unix(1000, 0)
	x.now = func() time.Time { return now }
	return x, &now
}

func TestIndexContinuationAdvancesHead(t *testing.T) {
	x, _ := testIndex(time.Hour, 100)

	turn1 := []uint64{1, 2}
	turn2 := []uint64{1, 2, 3, 4}

	id1 := x.Commit(turn1, nil, []string{"pod-a"}, 10)
	require.NotEmpty(t, id1)
	id2 := x.Commit(turn2, nil, []string{"pod-a"}, 20)
	assert.Equal(t, id1, id2, "a linear extension keeps the session id")

	res := x.Match(turn2, nil)
	require.True(t, res.found)
	assert.Equal(t, id1, res.sessionID)
	assert.False(t, res.divergent)
	assert.Equal(t, int64(20), res.engines["pod-a"])
}

func TestIndexReplayOfShorterHistory(t *testing.T) {
	x, _ := testIndex(time.Hour, 100)
	x.Commit([]uint64{1, 2, 3, 4}, nil, []string{"pod-a"}, 40)

	res := x.Match([]uint64{1, 2}, nil)
	require.True(t, res.found)
	assert.False(t, res.divergent, "a fully-known prefix is a replay, not a divergence")
}

func TestIndexForkCreatesNewSession(t *testing.T) {
	x, _ := testIndex(time.Hour, 100)
	parent := x.Commit([]uint64{1, 2, 3, 4}, nil, []string{"pod-a"}, 40)

	res := x.Match([]uint64{1, 2, 9}, nil)
	require.True(t, res.found)
	assert.Equal(t, parent, res.sessionID, "before commit, a divergent chain matches the parent")
	assert.True(t, res.divergent)
	assert.Equal(t, 1, res.matchedIdx, "match clamps at the last shared append")

	fork := x.Commit([]uint64{1, 2, 9}, nil, []string{"pod-b"}, 30)
	require.NotEmpty(t, fork)
	assert.NotEqual(t, parent, fork, "a divergent chain becomes a new session")

	forkRes := x.Match([]uint64{1, 2, 9}, nil)
	require.True(t, forkRes.found)
	assert.Equal(t, fork, forkRes.sessionID)
	assert.False(t, forkRes.divergent)
	assert.Equal(t, int64(30), forkRes.engines["pod-b"])

	parentRes := x.Match([]uint64{1, 2, 3, 4}, nil)
	require.True(t, parentRes.found)
	assert.Equal(t, parent, parentRes.sessionID, "the parent keeps the shared prefix")
}

func TestIndexAliasDeltaContinuation(t *testing.T) {
	x, _ := testIndex(time.Hour, 100)
	id := x.Commit([]uint64{1, 2, 3, 4}, []string{"hdr:s1"}, []string{"pod-a"}, 40)

	// A delta request carries no known prefix, only the declared id, and is
	// shorter than the session's known history.
	res := x.Match([]uint64{7}, []string{"hdr:s1"})
	require.True(t, res.found, "the alias bridges a delta continuation")
	assert.Equal(t, id, res.sessionID)
	assert.False(t, res.divergent)
	assert.Equal(t, int64(40), res.engines["pod-a"])

	assert.Equal(t, id, x.Commit([]uint64{7}, []string{"hdr:s1"}, []string{"pod-a"}, 5))
	after := x.Match([]uint64{7, 8}, []string{"hdr:s1"})
	require.True(t, after.found)
	assert.Equal(t, int64(40), after.engines["pod-a"], "a small delta must not shrink the extent")
}

func TestIndexAliasRewriteGuard(t *testing.T) {
	x, _ := testIndex(time.Hour, 100)
	id := x.Commit([]uint64{1, 2, 3, 4}, []string{"hdr:s1"}, []string{"pod-a"}, 40)

	// A full-length rewrite under a stable client id shares no appends and is
	// not shorter than the known history: a new lineage.
	rewrite := x.Commit([]uint64{5, 6, 7, 8}, []string{"hdr:s1"}, []string{"pod-b"}, 40)
	assert.NotEqual(t, id, rewrite)

	// Content wins: the alias now points at the rewrite.
	res := x.Match([]uint64{9}, []string{"hdr:s1"})
	require.True(t, res.found)
	assert.Equal(t, rewrite, res.sessionID)
}

func TestIndexTTLExpiry(t *testing.T) {
	x, now := testIndex(time.Minute, 100)
	x.Commit([]uint64{1, 2}, []string{"hdr:s1"}, []string{"pod-a"}, 10)

	*now = now.Add(2 * time.Minute)
	x.removeExpired()

	assert.False(t, x.Match([]uint64{1, 2}, nil).found, "expired sessions drop their appends")
	assert.False(t, x.Match([]uint64{9}, []string{"hdr:s1"}).found, "expired sessions drop their aliases")
}

func TestIndexCapEviction(t *testing.T) {
	x, now := testIndex(time.Hour, 2)
	x.Commit([]uint64{1}, nil, nil, 1)
	*now = now.Add(time.Minute)
	id2 := x.Commit([]uint64{2}, nil, nil, 1)
	*now = now.Add(time.Minute)
	id3 := x.Commit([]uint64{3}, nil, nil, 1)

	x.mu.Lock()
	size := len(x.sessions)
	x.mu.Unlock()
	assert.LessOrEqual(t, size, 2, "the cap bounds the session count")
	assert.False(t, x.Match([]uint64{1}, nil).found, "cap eviction drops the longest-idle session")
	assert.Equal(t, id2, x.Match([]uint64{2}, nil).sessionID, "recently active sessions survive cap eviction")
	assert.Equal(t, id3, x.Match([]uint64{3}, nil).sessionID)
}

func TestIndexRecordUsage(t *testing.T) {
	x, _ := testIndex(time.Hour, 100)
	id := x.Commit([]uint64{1, 2}, nil, []string{"pod-a"}, 10)

	x.RecordUsage(id, "pod-a", 50)
	assert.Equal(t, int64(50), x.Match([]uint64{1, 2}, nil).engines["pod-a"])

	x.RecordUsage(id, "pod-a", 20)
	assert.Equal(t, int64(50), x.Match([]uint64{1, 2}, nil).engines["pod-a"], "extents are monotone-max")

	x.RecordUsage("unknown", "pod-a", 99)
	x.RecordUsage(id, "", 99)
	x.RecordUsage(id, "pod-b", 0)
	assert.NotContains(t, x.Match([]uint64{1, 2}, nil).engines, "pod-b")
}

func TestIndexRemoveEnginesNotIn(t *testing.T) {
	x, _ := testIndex(time.Hour, 100)
	x.Commit([]uint64{1, 2}, nil, []string{"pod-a", "pod-b"}, 10)

	x.removeEnginesNotIn(map[string]struct{}{"pod-b": {}})
	res := x.Match([]uint64{1, 2}, nil)
	require.True(t, res.found)
	assert.NotContains(t, res.engines, "pod-a")
	assert.Equal(t, int64(10), res.engines["pod-b"])
}

func TestIndexMatchIsReadOnly(t *testing.T) {
	x, _ := testIndex(time.Hour, 100)
	x.Commit([]uint64{1, 2}, nil, []string{"pod-a"}, 10)

	res := x.Match([]uint64{1, 2, 3}, []string{"hdr:new"})
	require.True(t, res.found)

	x.mu.Lock()
	_, appendRegistered := x.byAppend[3]
	_, aliasRegistered := x.aliases["hdr:new"]
	x.mu.Unlock()
	assert.False(t, appendRegistered, "Match must not register appends")
	assert.False(t, aliasRegistered, "Match must not record aliases")
}

func TestIndexEmptyChainCommit(t *testing.T) {
	x, _ := testIndex(time.Hour, 100)
	assert.Empty(t, x.Commit(nil, []string{"hdr:s1"}, []string{"pod-a"}, 10))
}
