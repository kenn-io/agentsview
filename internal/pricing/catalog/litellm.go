package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/money"
)

const litellmURL = "https://raw.githubusercontent.com/" +
	"BerriAI/litellm/main/" +
	"model_prices_and_context_window.json"

// ModelPricing holds per-model token pricing in cost per million tokens.
type ModelPricing struct {
	ModelPattern         string
	InputPerMTok         money.Money
	OutputPerMTok        money.Money
	CacheCreationPerMTok money.Money
	CacheReadPerMTok     money.Money
}

type litellmEntry struct {
	InputCost         *json.Number `json:"input_cost_per_token"`
	OutputCost        *json.Number `json:"output_cost_per_token"`
	CacheCreationCost *json.Number `json:"cache_creation_input_token_cost"`
	CacheReadCost     *json.Number `json:"cache_read_input_token_cost"`
	Provider          string       `json:"litellm_provider"`
}

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
		var err error
		if entry.InputCost != nil {
			p.InputPerMTok, err = parsePerTokenRate(*entry.InputCost)
			if err != nil {
				return nil, fmt.Errorf("parsing %s input price: %w", model, err)
			}
		}
		if entry.OutputCost != nil {
			p.OutputPerMTok, err = parsePerTokenRate(*entry.OutputCost)
			if err != nil {
				return nil, fmt.Errorf("parsing %s output price: %w", model, err)
			}
		}
		if entry.CacheCreationCost != nil {
			p.CacheCreationPerMTok, err = parsePerTokenRate(*entry.CacheCreationCost)
			if err != nil {
				return nil, fmt.Errorf("parsing %s cache creation price: %w", model, err)
			}
		}
		if entry.CacheReadCost != nil {
			p.CacheReadPerMTok, err = parsePerTokenRate(*entry.CacheReadCost)
			if err != nil {
				return nil, fmt.Errorf("parsing %s cache read price: %w", model, err)
			}
		}
		prices = append(prices, p)
	}
	return prices, nil
}

func parsePerTokenRate(value json.Number) (money.Money, error) {
	microdollars, err := money.ParseScaledDecimal(value.String(), 12)
	if err != nil {
		return money.Money{}, err
	}
	if microdollars < 0 {
		return money.Money{}, money.ErrNegative
	}
	return money.Money{Microdollars: microdollars}, nil
}
