package catalog

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/money"
)

// openrouterURL is OpenRouter's public model list. It needs no auth and
// returns every proxied model with its per-token prompt/completion price.
const openrouterURL = "https://openrouter.ai/api/v1/models"

// FetchOpenRouterPricingContext downloads the OpenRouter model catalog and
// binds the request lifetime to ctx.
func FetchOpenRouterPricingContext(
	ctx context.Context,
) ([]ModelPricing, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return fetchOpenRouterPricing(ctx, client, openrouterURL)
}

func fetchOpenRouterPricing(
	ctx context.Context, client *http.Client, url string,
) ([]ModelPricing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating openrouter pricing request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching openrouter pricing: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"fetching openrouter pricing: status %d", resp.StatusCode,
		)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading openrouter response: %w", err)
	}
	return ParseOpenRouterPricing(data)
}

type openrouterEntry struct {
	ID           string `json:"id"`
	Architecture struct {
		Modality string `json:"modality"`
	} `json:"architecture"`
	Pricing struct {
		Prompt          string `json:"prompt"`
		Completion      string `json:"completion"`
		InputCacheRead  string `json:"input_cache_read"`
		InputCacheWrite string `json:"input_cache_write"`
	} `json:"pricing"`
}

// ParseOpenRouterPricing parses the OpenRouter /models envelope
// ({"data": [...]}) into ModelPricing entries keyed by the
// provider-qualified OpenRouter id. Only models that produce text are
// kept: agentsview prices token counters, so image, audio, and embedding
// outputs are skipped. Prices are quoted in USD per token as decimal
// strings and are converted to microdollars per million tokens. Entries
// without a usable prompt or completion price are skipped rather than
// failing the parse: OpenRouter marks dynamically priced routers with
// "-1", and one odd entry must not take the whole catalog offline.
func ParseOpenRouterPricing(data []byte) ([]ModelPricing, error) {
	var envelope struct {
		Data []openrouterEntry `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parsing openrouter JSON: %w", err)
	}

	var prices []ModelPricing
	for _, e := range envelope.Data {
		if !producesText(e.Architecture.Modality) {
			continue
		}
		p, ok := openrouterModelPricing(e)
		if ok {
			prices = append(prices, p)
		}
	}
	return prices, nil
}

func openrouterModelPricing(e openrouterEntry) (ModelPricing, bool) {
	prompt, hasPrompt, err := parseOpenRouterRate(e.Pricing.Prompt)
	if err != nil {
		return ModelPricing{}, false
	}
	completion, hasCompletion, err := parseOpenRouterRate(e.Pricing.Completion)
	if err != nil || (!hasPrompt && !hasCompletion) {
		return ModelPricing{}, false
	}
	cacheRead, _, err := parseOpenRouterRate(e.Pricing.InputCacheRead)
	if err != nil {
		return ModelPricing{}, false
	}
	cacheWrite, _, err := parseOpenRouterRate(e.Pricing.InputCacheWrite)
	if err != nil {
		return ModelPricing{}, false
	}
	return ModelPricing{
		ModelPattern:         e.ID,
		InputPerMTok:         prompt,
		OutputPerMTok:        completion,
		CacheCreationPerMTok: cacheWrite,
		CacheReadPerMTok:     cacheRead,
	}, true
}

// producesText reports whether an OpenRouter modality ("text+image->text")
// has text on the output side. An empty modality is treated as text: the
// field is omitted for plain text models.
func producesText(modality string) bool {
	if modality == "" {
		return true
	}
	_, output, ok := strings.Cut(modality, "->")
	return ok && strings.Contains(output, "text")
}

// parseOpenRouterRate converts a per-token USD price to per-million-token
// microdollars. An empty value means the field is absent; zero is a valid
// price for free models; negative or malformed values are errors.
func parseOpenRouterRate(value string) (money.Money, bool, error) {
	if value == "" {
		return money.Money{}, false, nil
	}
	rate, err := parsePerTokenRate(value)
	if err != nil {
		return money.Money{}, false, err
	}
	return rate, true, nil
}
