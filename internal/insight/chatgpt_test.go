package insight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestChatGPTDoesNotFallbackToCLIOrEndpoint(t *testing.T) {
	opts := GenerateOptions{ChatGPT: &ChatGPTConfig{DataDir: t.TempDir(), Model: "model"}, Endpoint: &EndpointConfig{Endpoint: "https://example.invalid", Model: "other"}}
	_, err := GenerateStreamWithOptions(t.Context(), "copilot", "prompt", nil, opts)
	require.ErrorContains(t, err, "ChatGPT is not connected")
	assert.Zero(t, PromptArgLimit("copilot", opts))
}

func TestChatGPTCacheIdentityProperty(t *testing.T) {
	key := func(agent string, opts GenerateOptions) string {
		k, err := CannedCacheKey(CannedPromptMaturityReview, "2026-01-01", "2026-01-31", "project", agent, "focus", "hash", "all", CannedSessionFilters{}, opts)
		require.NoError(t, err)
		return k
	}
	hegel.Test(t, func(ht *hegel.T) {
		model := "model-" + hegel.Draw(ht, hegel.Text())
		opts := GenerateOptions{ChatGPT: &ChatGPTConfig{DataDir: "unused", Model: model}}
		assert.Equal(ht, key("claude", opts), key("codex", opts))
		assert.NotEqual(ht, key("claude", opts), key("claude", GenerateOptions{Endpoint: &EndpointConfig{Endpoint: "https://chatgpt.com/backend-api/codex/responses", Model: model}}))
		assert.NotEqual(ht, key("claude", opts), key("claude", GenerateOptions{ChatGPT: &ChatGPTConfig{Model: "other-" + model}}))
	})
}
