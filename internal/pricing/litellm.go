package pricing

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// FallbackVersion must be bumped whenever FallbackPricing
// rates change so the startup seeder knows to re-upsert.
const FallbackVersion = "2026-06-10"

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
			ModelPattern:         "claude-opus-4-7",
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
			// Fable 5 launched at double the Opus 4.8 rates and is
			// not yet in the LiteLLM catalog, so it ships here so
			// usage is priced until LiteLLM adds it.
			ModelPattern:         "claude-fable-5",
			InputPerMTok:         10.0,
			OutputPerMTok:        50.0,
			CacheCreationPerMTok: 12.50,
			CacheReadPerMTok:     1.0,
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

const litellmURL = "https://raw.githubusercontent.com/" +
	"BerriAI/litellm/main/" +
	"model_prices_and_context_window.json"

// ModelPricing holds per-model token pricing in cost per
// million tokens. Separate from db.ModelPricing — the CLI
// command converts between the two.
type ModelPricing struct {
	ModelPattern         string
	InputPerMTok         float64
	OutputPerMTok        float64
	CacheCreationPerMTok float64
	CacheReadPerMTok     float64
}

type litellmEntry struct {
	InputCost         *float64 `json:"input_cost_per_token"`
	OutputCost        *float64 `json:"output_cost_per_token"`
	CacheCreationCost *float64 `json:"cache_creation_input_token_cost"`
	CacheReadCost     *float64 `json:"cache_read_input_token_cost"`
	Provider          string   `json:"litellm_provider"`
}

const perMTok = 1_000_000

// FetchLiteLLMPricing downloads the LiteLLM pricing JSON
// and parses it into ModelPricing entries.
func FetchLiteLLMPricing() ([]ModelPricing, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(litellmURL)
	if err != nil {
		return nil, fmt.Errorf("fetching litellm pricing: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"fetching litellm pricing: status %d", resp.StatusCode,
		)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading litellm response: %w", err)
	}

	return ParseLiteLLMPricing(data)
}

// ParseLiteLLMPricing parses the LiteLLM JSON map into
// ModelPricing entries. Per-token costs are converted to
// per-million-token costs. Entries missing both input and
// output cost are skipped.
func ParseLiteLLMPricing(
	data []byte,
) ([]ModelPricing, error) {
	var raw map[string]litellmEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing litellm JSON: %w", err)
	}

	var prices []ModelPricing
	for model, entry := range raw {
		if entry.InputCost == nil && entry.OutputCost == nil {
			continue
		}
		p := ModelPricing{ModelPattern: model}
		if entry.InputCost != nil {
			p.InputPerMTok = *entry.InputCost * perMTok
		}
		if entry.OutputCost != nil {
			p.OutputPerMTok = *entry.OutputCost * perMTok
		}
		if entry.CacheCreationCost != nil {
			p.CacheCreationPerMTok =
				*entry.CacheCreationCost * perMTok
		}
		if entry.CacheReadCost != nil {
			p.CacheReadPerMTok = *entry.CacheReadCost * perMTok
		}
		prices = append(prices, p)
	}
	return prices, nil
}
