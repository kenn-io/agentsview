package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const litellmURL = "https://raw.githubusercontent.com/" +
	"BerriAI/litellm/main/" +
	"model_prices_and_context_window.json"

// ModelPricing holds per-model token pricing in cost per million tokens.
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
	return FetchLiteLLMPricingContext(context.Background())
}

// FetchLiteLLMPricingContext downloads the LiteLLM pricing JSON and binds the
// request lifetime to ctx.
func FetchLiteLLMPricingContext(
	ctx context.Context,
) ([]ModelPricing, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return fetchLiteLLMPricing(ctx, client, litellmURL)
}

func fetchLiteLLMPricing(
	ctx context.Context,
	client *http.Client,
	url string,
) ([]ModelPricing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating litellm pricing request: %w", err)
	}
	resp, err := client.Do(req)
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
