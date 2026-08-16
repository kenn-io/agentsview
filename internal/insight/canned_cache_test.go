package insight

import "testing"

func TestCannedCacheKeyIncludesEffectiveGenerationIdentity(t *testing.T) {
	key := func(agent, focus string, generation GenerateOptions) string {
		t.Helper()
		key, err := CannedCacheKey(
			CannedPromptMaturityReview,
			"2026-01-01", "2026-01-31", "project", agent,
			focus, "aggregate", "all", CannedSessionFilters{}, generation,
		)
		if err != nil {
			t.Fatalf("CannedCacheKey: %v", err)
		}
		return key
	}

	if key("claude", "focus", GenerateOptions{}) ==
		key("codex", "focus", GenerateOptions{}) {
		t.Fatal("CLI agent identity must partition the cache")
	}
	if key("claude", "focus", GenerateOptions{
		Endpoint: &EndpointConfig{Endpoint: "https://one.example/v1", Model: "model"},
	}) == key("claude", "focus", GenerateOptions{
		Endpoint: &EndpointConfig{Endpoint: "https://two.example/v1", Model: "model"},
	}) {
		t.Fatal("endpoint identity must partition the cache")
	}
	if key("claude", "focus", GenerateOptions{
		Endpoint: &EndpointConfig{Endpoint: "https://one.example/v1", Model: "model-a"},
	}) == key("claude", "focus", GenerateOptions{
		Endpoint: &EndpointConfig{Endpoint: "https://one.example/v1", Model: "model-b"},
	}) {
		t.Fatal("endpoint model must partition the cache")
	}
	if key("claude", "focus", GenerateOptions{
		Endpoint: &EndpointConfig{Endpoint: "https://one.example/v1", Model: "model"},
	}) != key("codex", "focus", GenerateOptions{
		Endpoint: &EndpointConfig{Endpoint: "https://one.example/v1", Model: "model"},
	}) {
		t.Fatal("selected CLI agent must not partition endpoint-mode cache keys")
	}
}
