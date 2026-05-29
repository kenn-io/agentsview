package pricing

// FallbackVersion must be bumped whenever FallbackPricing
// rates change so the startup seeder knows to re-upsert.
const FallbackVersion = "2026-05-29"

// FallbackPricing returns hardcoded pricing for key Claude
// models. Used when the LiteLLM fetch fails.
// Prices in USD per million tokens, current as of 2026-05.
func FallbackPricing() []ModelPricing {
	return []ModelPricing{
		// Current model names (used by Claude Code / Codex)
		{
			ModelPattern:         "claude-sonnet-4-6",
			InputPerMTok:         3.0,
			OutputPerMTok:        15.0,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.30,
		},
		{
			ModelPattern:         "claude-opus-4-6",
			InputPerMTok:         5.0,
			OutputPerMTok:        25.0,
			CacheCreationPerMTok: 6.25,
			CacheReadPerMTok:     0.50,
		},
		{
			// Opus 4.8 launched at the Opus 4.6/4.7 rates and is
			// not yet in the LiteLLM catalog, so it ships here so
			// usage is priced until LiteLLM adds it.
			ModelPattern:         "claude-opus-4-8",
			InputPerMTok:         5.0,
			OutputPerMTok:        25.0,
			CacheCreationPerMTok: 6.25,
			CacheReadPerMTok:     0.50,
		},
		{
			ModelPattern:         "claude-haiku-4-5-20251001",
			InputPerMTok:         1.0,
			OutputPerMTok:        5.0,
			CacheCreationPerMTok: 1.25,
			CacheReadPerMTok:     0.10,
		},
		// Codex / OpenAI models
		{
			ModelPattern:     "gpt-5.5",
			InputPerMTok:     5.0,
			OutputPerMTok:    30.0,
			CacheReadPerMTok: 0.50,
		},
		{
			ModelPattern:  "gpt-5.4",
			InputPerMTok:  2.50,
			OutputPerMTok: 15.0,
		},
		{
			ModelPattern:  "gpt-5.2-codex",
			InputPerMTok:  1.75,
			OutputPerMTok: 14.0,
		},
		{
			ModelPattern:  "gpt-5.3-codex",
			InputPerMTok:  1.75,
			OutputPerMTok: 14.0,
		},
		{
			ModelPattern:  "gpt-5.4-mini",
			InputPerMTok:  0.75,
			OutputPerMTok: 4.50,
		},
		{
			ModelPattern:  "gpt-5.4-nano",
			InputPerMTok:  0.20,
			OutputPerMTok: 1.25,
		},
		{
			ModelPattern:  "gpt-5.1-codex-max",
			InputPerMTok:  1.25,
			OutputPerMTok: 10.0,
		},
		// Older model names (still in some session logs)
		{
			ModelPattern:         "claude-sonnet-4-20250514",
			InputPerMTok:         3.0,
			OutputPerMTok:        15.0,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.30,
		},
		{
			ModelPattern:         "claude-sonnet-4-5-20250514",
			InputPerMTok:         3.0,
			OutputPerMTok:        15.0,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.30,
		},
		{
			ModelPattern:         "claude-opus-4-20250514",
			InputPerMTok:         15.0,
			OutputPerMTok:        75.0,
			CacheCreationPerMTok: 18.75,
			CacheReadPerMTok:     1.50,
		},
		{
			ModelPattern:         "claude-haiku-3-5-20241022",
			InputPerMTok:         0.80,
			OutputPerMTok:        4.0,
			CacheCreationPerMTok: 1.0,
			CacheReadPerMTok:     0.08,
		},
		// Free OpenRouter model
		{
			ModelPattern:  "openrouter/owl-alpha",
			InputPerMTok:  0,
			OutputPerMTok: 0,
		},
	}
}
