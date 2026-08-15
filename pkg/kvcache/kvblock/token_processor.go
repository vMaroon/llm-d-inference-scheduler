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
	"hash/fnv"
	"sync"

	"github.com/fxamacker/cbor/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// defaultBlockSize is the default number of tokens per block.
// 16 is the default value used by vLLM.
const defaultBlockSize = 16

// TokenProcessorConfig holds the configuration for the token processor.
type TokenProcessorConfig struct {
	// BlockSize is deprecated. Use BlockSizeTokens instead.
	//
	// Deprecated: Use BlockSizeTokens instead.
	BlockSize int `json:"blockSize,omitempty"`
	// BlockSizeTokens is the number of tokens per block.
	// A value of zero is treated as "not set" and resolved to the default (16) by NewChunkedTokenDatabase.
	BlockSizeTokens int `json:"blockSizeTokens"`
	// HashSeed is used to prefix initial hash chunks, similarly to vLLM's NONE_HASH.
	// This should be aligned with vLLM's `PYTHONHASHSEED` environment variable.
	// The system's deployer is responsible for aligning the vLLM deployments
	// with the same seed value.
	HashSeed string `json:"hashSeed"`
	initHash uint64 // cache once
}

// DefaultTokenProcessorConfig returns the default configuration for the token processor.
func DefaultTokenProcessorConfig() *TokenProcessorConfig {
	return &TokenProcessorConfig{
		BlockSizeTokens: defaultBlockSize,
		HashSeed:        "",
	}
}

// TokenProcessor defines the interface for converting tokens to
// KVBlockKeys.
type TokenProcessor interface {
	// TokensToKVBlockKeys converts tokens into kv_block.Keys.
	// It accepts an optional parentKey to continue a hash chain.
	// extraFeatures provides per-block multimodal data that taints the hash;
	// nil means text-only (no taint). When non-nil, its length must match the
	// number of token chunks.
	// It returns a slice of generated Keys.
	TokensToKVBlockKeys(
		parentKey BlockHash, tokens []uint32, modelName string,
		extraFeatures []*BlockExtraFeatures,
	) ([]BlockHash, error)

	// BlockSize returns the number of tokens per block used by this processor.
	BlockSize() int
}

// chunkedTokenDatabase is a concrete implementation of TokenDatabase.
// It mimics the chunkedTokenDatabase in the Python code.
type chunkedTokenDatabase struct {
	TokenProcessorConfig
	encoder    cbor.EncMode // cached CBOR encoder for interoperable encoding
	initHashes sync.Map     // model name -> canonical initial hash
}

var _ TokenProcessor = &chunkedTokenDatabase{}

// NewChunkedTokenDatabase creates a new instance with the given config and metadata.
func NewChunkedTokenDatabase(config *TokenProcessorConfig) (TokenProcessor, error) {
	var cfg TokenProcessorConfig
	if config == nil {
		cfg = *DefaultTokenProcessorConfig()
	} else {
		cfg = *config // local copy — caller's struct is never mutated
	}

	// Apply defaults for omitted fields so partial configs (e.g. only hashSeed set) work correctly.
	if cfg.BlockSizeTokens == 0 && cfg.BlockSize == 0 {
		cfg.BlockSizeTokens = defaultBlockSize
	}

	// Handle backward compatibility: if only deprecated BlockSize is set, promote it.
	if cfg.BlockSizeTokens == 0 && cfg.BlockSize > 0 {
		cfg.BlockSizeTokens = cfg.BlockSize
	}

	if cfg.BlockSizeTokens <= 0 {
		// Report the actual invalid value the caller set, not the zero from the other field.
		invalidBlockSize := cfg.BlockSizeTokens
		if cfg.BlockSizeTokens == 0 && cfg.BlockSize != 0 {
			invalidBlockSize = cfg.BlockSize
		}
		return nil, fmt.Errorf("blockSizeTokens must be greater than 0, got %d", invalidBlockSize)
	}

	if cfg.initHash == 0 {
		h := fnv.New64a()
		_, _ = h.Write([]byte(cfg.HashSeed))
		cfg.initHash = h.Sum64()
	}

	encoder, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		return nil, fmt.Errorf("failed to create CBOR encoder: %w", err)
	}

	return &chunkedTokenDatabase{
		TokenProcessorConfig: cfg,
		encoder:              encoder,
	}, nil
}

// getInitHash returns the initial hash for the given model name.
func (db *chunkedTokenDatabase) getInitHash(modelName string) uint64 {
	if cached, ok := db.initHashes.Load(modelName); ok {
		return cached.(uint64)
	}
	computed := db.hash(db.initHash, nil, modelName)
	actual, _ := db.initHashes.LoadOrStore(modelName, computed)
	return actual.(uint64)
}

// hash computes the uint64 FNV-64a hash of the given parent, tokens,
// and extra keys.
//
// The hash is computed using FNV-64a over the CBOR canonical encoding of
// [parent, tokens, extra], ensuring deterministic results across runs and
// compatibility with vLLM's prefix caching algorithm.
//
// The extra parameter enables cache differentiation for LoRA adapters and
// multi-modal content. Supported types: nil, int, string, map[string]interface{}.
// Must be CBOR-serializable.
func (db *chunkedTokenDatabase) hash(parent uint64, tokens []uint32, extra interface{}) uint64 {
	// Text-only blocks are the overwhelmingly common path. Encode the same
	// canonical CBOR bytes directly into FNV-64a instead of constructing an
	// interface slice and allocating a CBOR buffer for every block.
	if extra == nil {
		return hashTextBlock(parent, tokens)
	}

	payload := []interface{}{parent, tokens, extra}

	b, err := db.encoder.Marshal(payload)
	if err != nil {
		log.FromContext(context.Background()).Error(err, "failed to marshal payload to CBOR")
		return 0
	}

	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}

const (
	fnv64Offset = uint64(14695981039346656037)
	fnv64Prime  = uint64(1099511628211)
)

func hashByte(sum uint64, value byte) uint64 {
	return (sum ^ uint64(value)) * fnv64Prime
}

// hashCBORMajor writes one canonical CBOR unsigned value with the supplied
// major-type prefix directly into the running FNV sum.
func hashCBORMajor(sum uint64, major byte, value uint64) uint64 {
	switch {
	case value < 24:
		return hashByte(sum, major|byte(value))
	case value <= 0xff:
		sum = hashByte(sum, major|24)
		return hashByte(sum, byte(value))
	case value <= 0xffff:
		sum = hashByte(sum, major|25)
		sum = hashByte(sum, byte(value>>8))
		return hashByte(sum, byte(value))
	case value <= 0xffffffff:
		sum = hashByte(sum, major|26)
		for shift := 24; shift >= 0; shift -= 8 {
			sum = hashByte(sum, byte(value>>shift))
		}
		return sum
	default:
		sum = hashByte(sum, major|27)
		for shift := 56; shift >= 0; shift -= 8 {
			sum = hashByte(sum, byte(value>>shift))
		}
		return sum
	}
}

// hashTextBlock is byte-for-byte equivalent to canonical CBOR encoding of
// [parent, tokens, nil] followed by FNV-64a. It changes only the implementation,
// not the vLLM-compatible hash value.
func hashTextBlock(parent uint64, tokens []uint32) uint64 {
	sum := hashByte(fnv64Offset, 0x83) // fixed-size array of three values
	sum = hashCBORMajor(sum, 0x00, parent)
	sum = hashCBORMajor(sum, 0x80, uint64(len(tokens)))
	for _, token := range tokens {
		sum = hashCBORMajor(sum, 0x00, uint64(token))
	}
	return hashByte(sum, 0xf6) // nil extra features
}

// BlockSize returns the number of tokens per block.
func (db *chunkedTokenDatabase) BlockSize() int {
	return db.BlockSizeTokens
}

// TokensToKVBlockKeys converts tokens into kv_block.Keys.
func (db *chunkedTokenDatabase) TokensToKVBlockKeys(
	parentKey BlockHash, tokens []uint32, modelName string,
	extraFeatures []*BlockExtraFeatures,
) ([]BlockHash, error) {
	var currentParentHash uint64
	if parentKey != EmptyBlockHash {
		currentParentHash = uint64(parentKey)
	} else {
		currentParentHash = db.getInitHash(modelName)
	}

	blockCount := len(tokens) / db.BlockSizeTokens
	if blockCount == 0 {
		return nil, nil
	}

	if extraFeatures != nil && len(extraFeatures) != blockCount {
		return nil, fmt.Errorf("extraFeatures length %d does not match token chunk count %d (blockSizeTokens=%d, tokens=%d)",
			len(extraFeatures), blockCount, db.BlockSizeTokens, len(tokens))
	}

	keys := make([]BlockHash, blockCount)
	for i := range blockCount {
		start := i * db.BlockSizeTokens
		end := start + db.BlockSizeTokens
		var extra any
		if extraFeatures != nil && extraFeatures[i] != nil {
			extra = extraFeatures[i].MMHashes
		}
		currentParentHash = db.hash(currentParentHash, tokens[start:end], extra)
		keys[i] = BlockHash(currentParentHash)
	}
	return keys, nil
}
