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

package approximateprefix

import (
	"context"
	"encoding/binary"
	"iter"
	"unsafe"

	"github.com/cespare/xxhash/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"

	logutil "github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
)

// HashBlock wraps a block of token IDs used for calculating prefix hashes.
type HashBlock struct {
	// Tokens are the token IDs covered by this block.
	Tokens []uint32
}

// Hash computes a stable unique identifier for the HashBlock content.
func (b HashBlock) Hash() uint64 {
	if len(b.Tokens) > 0 {
		byteSlice := unsafe.Slice((*byte)(unsafe.Pointer(&b.Tokens[0])), len(b.Tokens)*4)
		return xxhash.Sum64(byteSlice)
	}

	return 0
}

// getBlockHashes divides the tokenized prompt into blocks and calculates a
// prefix cache hash for each block. Each prompt in PerPromptTokens is hashed
// independently so cross-prompt block adjacency is avoided. The first block
// hash of every prompt includes the model name and cache salt (if provided).
// For subsequent blocks, the hash is calculated as: hash(block i content, hash(i-1)).
// It requires request.Body.TokenizedPrompt to be populated by a token-producer backend.
func getBlockHashes(ctx context.Context, request *scheduling.InferenceRequest, blockSizeTokens int, maxPrefixBlocks int) [][]blockHash {
	loggerDebug := log.FromContext(ctx).V(logutil.DEBUG)
	if request == nil || request.Body == nil {
		loggerDebug.Info("Request or request data is nil, skipping hashing")
		return nil
	}

	tp := request.Body.TokenizedPrompt
	if tp == nil || tp.TokenCount() == 0 {
		loggerDebug.Info("TokenizedPrompt is empty, skipping hashing")
		return nil
	}

	var result [][]blockHash
	for _, tokens := range tp.PerPromptTokens {
		seq := getKVCacheBlocksFromTokens(tokens, blockSizeTokens)
		hashes := computeBlockHashes(seq, request, maxPrefixBlocks)
		if len(hashes) > 0 {
			result = append(result, hashes)
		}
	}
	if len(result) == 0 {
		loggerDebug.Info("No kv cache block found")
		return nil
	}
	return result
}

// ChainSeed initializes a hash chain from the given parts (e.g. model name
// and cache salt, or an identity version tag and per-tenant salt). Chains
// seeded differently never collide on equal content. Empty parts contribute
// nothing, matching a caller that skips them.
func ChainSeed(parts ...string) uint64 {
	h := xxhash.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
	}
	return h.Sum64()
}

// ChainHash extends a hash chain by one block: the block's content hash and
// the previous chain value, hashed together. This is the chain algebra shared
// by approximate prefix-cache block hashing and identity derivation (the
// chain-identity producer), so both agree on what "same prefix" means.
func ChainHash(prev uint64, block HashBlock) uint64 {
	h := xxhash.New()
	_, _ = h.Write(toBytes(block.Hash()))
	_, _ = h.Write(toBytes(prev))
	return h.Sum64()
}

// computeBlockHashes calculates the hash for content blocks.
func computeBlockHashes(seq iter.Seq[HashBlock], request *scheduling.InferenceRequest, maxPrefixBlocks int) []blockHash {
	var blockHashes []blockHash

	// Different models should have different hashes even with the same body.
	prev := ChainSeed(request.TargetModel, request.Body.TokenizedPrompt.CacheSalt)

	count := 0
	for block := range seq {
		if count >= maxPrefixBlocks {
			break
		}
		prev = ChainHash(prev, block)
		blockHashes = append(blockHashes, blockHash(prev))
		count++
	}

	return blockHashes
}

func toBytes(i uint64) []byte {
	bytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(bytes, i)
	return bytes
}

func getKVCacheBlocksFromTokens(ids []uint32, blockSizeTokens int) iter.Seq[HashBlock] {
	return func(yield func(HashBlock) bool) {
		if len(ids) == 0 || blockSizeTokens <= 0 {
			return
		}
		for i := 0; i < len(ids); i += blockSizeTokens {
			end := i + blockSizeTokens
			if end > len(ids) {
				end = len(ids)
			}
			if !yield(HashBlock{Tokens: ids[i:end]}) {
				return
			}
		}
	}
}
