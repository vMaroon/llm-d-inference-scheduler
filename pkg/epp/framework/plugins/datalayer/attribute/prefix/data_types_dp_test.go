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

package prefix

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrefixCacheMatchInfo_GlobalDPRankDefaultsToUnknown(t *testing.T) {
	info := NewPrefixCacheMatchInfo(5, 10, 16)
	_, ok := info.GlobalDPRank()
	// Unknown distinguishes producers without rank data from a resolved rank 0.
	assert.False(t, ok)
}

func TestPrefixCacheMatchInfo_WithGlobalDPRank(t *testing.T) {
	info := NewPrefixCacheMatchInfo(5, 10, 16).WithGlobalDPRank(0)
	rank, ok := info.GlobalDPRank()
	require.True(t, ok, "rank 0 must be distinguishable from unknown")
	assert.Equal(t, 0, rank)

	rank, ok = NewPrefixCacheMatchInfo(5, 10, 16).WithGlobalDPRank(11).GlobalDPRank()
	require.True(t, ok)
	assert.Equal(t, 11, rank)
}

func TestPrefixCacheMatchInfo_CloneCopiesGlobalDPRank(t *testing.T) {
	orig := NewPrefixCacheMatchInfo(5, 10, 16).WithGlobalDPRank(3)
	clone, ok := orig.Clone().(*PrefixCacheMatchInfo)
	require.True(t, ok)
	rank, hasRank := clone.GlobalDPRank()
	require.True(t, hasRank)
	assert.Equal(t, 3, rank)

	unknownClone, ok := NewPrefixCacheMatchInfo(5, 10, 16).Clone().(*PrefixCacheMatchInfo)
	require.True(t, ok)
	_, hasRank = unknownClone.GlobalDPRank()
	assert.False(t, hasRank)
}
