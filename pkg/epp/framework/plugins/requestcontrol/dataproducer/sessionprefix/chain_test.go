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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	tokenproducer "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/tokenizer"
)

func chatBody(messages ...fwkrh.Message) *fwkrh.InferenceRequestBody {
	return &fwkrh.InferenceRequestBody{
		ChatCompletions: &fwkrh.ChatCompletionsRequest{Messages: messages},
	}
}

func textMessage(role, text string) fwkrh.Message {
	return fwkrh.Message{Role: role, Content: fwkrh.Content{Raw: text}}
}

func chainOf(t *testing.T, model string, body *fwkrh.InferenceRequestBody) []uint64 {
	t.Helper()
	blocks, _ := contentBlocks(body)
	return chainFrom(model, tokenproducer.CacheSaltFromBody(body), blocks)
}

func TestChainDeterminism(t *testing.T) {
	body := chatBody(
		textMessage("system", "you are helpful"),
		textMessage("user", "hello"),
	)
	first := chainOf(t, "m1", body)
	second := chainOf(t, "m1", chatBody(
		textMessage("system", "you are helpful"),
		textMessage("user", "hello"),
	))
	require.NotEmpty(t, first)
	assert.Equal(t, first, second, "identical content must recompute the identical chain")
}

func TestChainStrictPrefixAcrossTurns(t *testing.T) {
	turn1 := chatBody(
		textMessage("system", "you are helpful"),
		textMessage("user", "hello"),
	)
	turn2 := chatBody(
		textMessage("system", "you are helpful"),
		textMessage("user", "hello"),
		textMessage("assistant", "hi, how can I help?"),
		textMessage("user", "what is the weather?"),
	)
	c1 := chainOf(t, "m1", turn1)
	c2 := chainOf(t, "m1", turn2)
	require.Len(t, c1, 2)
	require.Len(t, c2, 4)
	assert.Equal(t, c1, c2[:len(c1)], "turn N chain must be a strict prefix of turn N+1")
}

func TestChainFramingDisambiguatesMessageSplits(t *testing.T) {
	split1 := chainOf(t, "m1", chatBody(textMessage("user", "ab"), textMessage("user", "c")))
	split2 := chainOf(t, "m1", chatBody(textMessage("user", "a"), textMessage("user", "bc")))
	assert.NotEqual(t, split1[len(split1)-1], split2[len(split2)-1],
		"distinct message splits of the same bytes must yield distinct chains")
}

func TestChainSeedSeparation(t *testing.T) {
	body := chatBody(textMessage("user", "hello"))

	byModel1 := chainOf(t, "m1", body)
	byModel2 := chainOf(t, "m2", body)
	assert.NotEqual(t, byModel1, byModel2, "identical content on different models must not share identity")

	salted := &fwkrh.InferenceRequestBody{
		ChatCompletions: &fwkrh.ChatCompletionsRequest{
			Messages:  []fwkrh.Message{textMessage("user", "hello")},
			CacheSalt: "tenant-a",
		},
	}
	assert.NotEqual(t, byModel1, chainOf(t, "m1", salted), "cache_salt must separate identities")
}

func TestChainToolsAffectIdentity(t *testing.T) {
	plain := chainOf(t, "m1", chatBody(textMessage("user", "hello")))
	withTools := chainOf(t, "m1", &fwkrh.InferenceRequestBody{
		ChatCompletions: &fwkrh.ChatCompletionsRequest{
			Messages: []fwkrh.Message{textMessage("user", "hello")},
			Tools:    []any{map[string]any{"name": "lookup_order"}},
		},
	})
	require.Len(t, withTools, 2, "tools must contribute a leading block")
	assert.NotEqual(t, plain, withTools[1:], "tool catalog must be folded into every downstream append")
}

func completionsBody(text string) *fwkrh.InferenceRequestBody {
	return &fwkrh.InferenceRequestBody{
		Completions: &fwkrh.CompletionsRequest{Prompt: fwkrh.Prompt{Raw: text}},
	}
}

func TestTextRunChunking(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		wantBlocks int
		wantTotal  int
	}{
		{name: "sub-chunk run kept as one block", text: strings.Repeat("a", textChunkBytes-1), wantBlocks: 1, wantTotal: textChunkBytes - 1},
		{name: "exactly one chunk", text: strings.Repeat("a", textChunkBytes), wantBlocks: 1, wantTotal: textChunkBytes},
		{name: "multi-chunk run drops the partial tail", text: strings.Repeat("a", 2*textChunkBytes+10), wantBlocks: 2, wantTotal: 2*textChunkBytes + 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks, total := contentBlocks(completionsBody(tt.text))
			assert.Len(t, blocks, tt.wantBlocks)
			assert.Equal(t, tt.wantTotal, total)
		})
	}
}

func TestRawPromptTailExclusionKeepsPrefixProperty(t *testing.T) {
	base := strings.Repeat("x", 3*textChunkBytes+100)
	grown := base + strings.Repeat("y", textChunkBytes)

	c1 := chainOf(t, "m1", completionsBody(base))
	c2 := chainOf(t, "m1", completionsBody(grown))
	require.Greater(t, len(c2), len(c1))
	assert.Equal(t, c1, c2[:len(c1)], "appending raw text must extend the chain, not fork it")
}

func TestChunkAlignedMessageSplitsStayDistinct(t *testing.T) {
	oneMessage := chainOf(t, "m1", chatBody(
		textMessage("user", strings.Repeat("a", 2*textChunkBytes)),
	))
	twoMessages := chainOf(t, "m1", chatBody(
		textMessage("user", strings.Repeat("a", textChunkBytes)),
		textMessage("user", strings.Repeat("a", textChunkBytes)),
	))
	require.Len(t, oneMessage, 2)
	require.Len(t, twoMessages, 2)
	assert.NotEqual(t, oneMessage, twoMessages,
		"the chunk index in the frame must separate chunk-aligned splits")
}

func TestContentBlocksAnthropicMessages(t *testing.T) {
	body := &fwkrh.InferenceRequestBody{
		Messages: &fwkrh.MessagesRequest{
			System: fwkrh.AnthropicContent{Raw: "you are helpful"},
			Messages: []fwkrh.AnthropicMessage{
				{Role: "user", Content: fwkrh.AnthropicContent{Raw: "hello"}},
			},
			Tools: []any{map[string]any{"name": "lookup_order"}},
		},
	}
	blocks, total := contentBlocks(body)
	require.Len(t, blocks, 3, "tools, system, and one message")
	assert.Equal(t, blocks[len(blocks)-1].cumChars, total)
}

func TestContentBlocksCumCharsMonotone(t *testing.T) {
	body := chatBody(
		textMessage("system", "you are helpful"),
		textMessage("user", "hello"),
		textMessage("assistant", "hi"),
	)
	blocks, total := contentBlocks(body)
	require.Len(t, blocks, 3)
	prev := 0
	for _, b := range blocks {
		assert.Greater(t, b.cumChars, prev)
		prev = b.cumChars
	}
	assert.Equal(t, total, prev, "total chars must equal the last block's cumulative count")
}

func TestContentBlocksUnsupportedBodies(t *testing.T) {
	tests := []struct {
		name string
		body *fwkrh.InferenceRequestBody
	}{
		{name: "nil body", body: nil},
		{name: "responses", body: &fwkrh.InferenceRequestBody{Responses: &fwkrh.ResponsesRequest{Input: "hello"}}},
		{name: "embeddings", body: &fwkrh.InferenceRequestBody{Embeddings: &fwkrh.EmbeddingsRequest{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks, _ := contentBlocks(tt.body)
			assert.Empty(t, blocks, "unsupported bodies derive no identity")
		})
	}
}

func TestChainSingleGrowingMessage(t *testing.T) {
	// Conversations carried as one growing user message (a common agent and
	// benchmark shape) must extend their chain across turns, exactly like the
	// raw completions path.
	base := strings.Repeat("conversation history ", 300) // ~6.3KB
	turn1 := chatBody(textMessage("user", base))
	turn2 := chatBody(textMessage("user", base+strings.Repeat("new turn content ", 150)))

	c1 := chainOf(t, "m1", turn1)
	c2 := chainOf(t, "m1", turn2)
	require.NotEmpty(t, c1, "a large single-message chat must derive identity")
	require.Greater(t, len(c2), len(c1))
	assert.Equal(t, c1, c2[:len(c1)], "a grown single-message chat must extend the chain, not replace it")
}
