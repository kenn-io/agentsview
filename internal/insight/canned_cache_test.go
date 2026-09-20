package insight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCannedCacheKeyIncludesEffectiveGenerationIdentity(t *testing.T) {
	key := func(agent, focus string, generation GenerateOptions) string {
		t.Helper()
		key, err := CannedCacheKey(
			CannedPromptMaturityReview,
			"2026-01-01", "2026-01-31", "project", agent,
			focus, "aggregate", "all", CannedSessionFilters{}, generation,
		)
		require.NoError(t, err)
		return key
	}

	assert.NotEqual(t, key("claude", "focus", GenerateOptions{}), key("codex", "focus", GenerateOptions{}))
	assert.NotEqual(t, key("claude", "focus", GenerateOptions{Endpoint: &EndpointConfig{Endpoint: "https://one.example/v1", Model: "model"}}), key("claude", "focus", GenerateOptions{Endpoint: &EndpointConfig{Endpoint: "https://two.example/v1", Model: "model"}}))
	assert.NotEqual(t, key("claude", "focus", GenerateOptions{Endpoint: &EndpointConfig{Endpoint: "https://one.example/v1", Model: "model-a"}}), key("claude", "focus", GenerateOptions{Endpoint: &EndpointConfig{Endpoint: "https://one.example/v1", Model: "model-b"}}))
	assert.Equal(t, key("claude", "focus", GenerateOptions{Endpoint: &EndpointConfig{Endpoint: "https://one.example/v1", Model: "model"}}), key("codex", "focus", GenerateOptions{Endpoint: &EndpointConfig{Endpoint: "https://one.example/v1", Model: "model"}}))
}
