package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// openrouterURL is the public OpenRouter models endpoint. Unlike
// most providers, OpenRouter publishes a single JSON document
// listing every model they proxy along with its prompt/completion
// cost, so we can fetch a stable snapshot of dozens of model
// prices in one HTTP call. There is no auth requirement and no
// rate-limit worth respecting for normal polling.
const openrouterURL = "https://openrouter.ai/api/v1/models"

// FetchOpenRouterPricing downloads the OpenRouter public model
// catalog and converts each entry into ModelPricing. The same
// per-million-token convention used by FetchLiteLLMPricing applies.
// OpenRouter prices are quoted in USD per token, not per million,
// so they are multiplied by perMTok before being stored.
func FetchOpenRouterPricing() ([]ModelPricing, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(openrouterURL)
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
	ID         string  `json:"id"`
	Architecture struct {
		Modality string `json:"modality"`
	} `json:"architecture"`
	Pricing struct {
		Prompt            string `json:"prompt"`
		Completion        string `json:"completion"`
		InputCacheRead    string `json:"input_cache_read"`
		InputCacheWrite   string `json:"input_cache_write"`
	} `json:"pricing"`
}

// ParseOpenRouterPricing parses the OpenRouter /models JSON
// envelope into ModelPricing entries. The endpoint returns a top
// level {"data": [...]} array; text-generation entries are
// kept, image/audio/embedding entries are dropped since the rest
// of agentsview only knows how to price text token counters.
func ParseOpenRouterPricing(data []byte) ([]ModelPricing, error) {
	var envelope struct {
		Data []openrouterEntry `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parsing openrouter JSON: %w", err)
	}

	var prices []ModelPricing
	for _, e := range envelope.Data {
		modality := e.Architecture.Modality
		if modality != "" && modality != "text->text" {
			continue
		}
		prompt, okPrompt := parsePricePerToken(e.Pricing.Prompt)
		completion, okCompletion := parsePricePerToken(e.Pricing.Completion)
		if !okPrompt && !okCompletion {
			continue
		}
		p := ModelPricing{ModelPattern: e.ID}
		if okPrompt {
			p.InputPerMTok = prompt * perMTok
		}
		if okCompletion {
			p.OutputPerMTok = completion * perMTok
		}
		if cr, ok := parsePricePerToken(e.Pricing.InputCacheRead); ok {
			p.CacheReadPerMTok = cr * perMTok
		}
		if cw, ok := parsePricePerToken(e.Pricing.InputCacheWrite); ok {
			p.CacheCreationPerMTok = cw * perMTok
		}
		prices = append(prices, p)
	}
	return prices, nil
}

// parsePricePerToken turns OpenRouter's quoted string
// ("0.000003") into a float64 USD-per-token. Empty strings
// return ok=false so the caller can fall back to the
// input or output rate if only one of the two is published.
func parsePricePerToken(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
		return 0, false
	}
	if f <= 0 {
		return 0, false
	}
	return f, true
}