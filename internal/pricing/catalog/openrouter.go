package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
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
	// Count how many OpenRouter entries share each "bare" suffix
	// (the part after the last '/'). OpenRouter ids are provider
	// qualified (`minimax/minimax-m3`), but agentsview sessions
	// often record the bare model name (`MiniMax-M3`) with no
	// provider prefix. Emitting an unqualified alias lets the
	// canonical resolver rank the OpenRouter row at the same
	// tier as an inherently unqualified pricing key, so a bare
	// user-side model name resolves cleanly. We only emit the
	// alias when the bare suffix is unique inside OpenRouter, so
	// two providers publishing the same base model do not fabricate
	// an ambiguous unqualified row.
	suffixCounts := make(map[string]int)
	for _, e := range envelope.Data {
		if bare := bareSuffix(e.ID); bare != "" && bare != e.ID {
			suffixCounts[bare]++
		}
	}
	for _, e := range envelope.Data {
		if !producesText(e.Architecture.Modality) {
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
		if bare := bareSuffix(e.ID); bare != "" && bare != e.ID &&
			suffixCounts[bare] == 1 {
			alias := p
			alias.ModelPattern = bare
			prices = append(prices, alias)
		}
	}
	return prices, nil
}

// bareSuffix returns the substring after the last '/' in an
// OpenRouter model id, or "" if there is no '/'. Used to derive
// an unqualified alias for OpenRouter entries so users who record
// bare model names (no provider prefix) can still resolve pricing.
func bareSuffix(id string) string {
	i := strings.LastIndex(id, "/")
	if i < 0 || i == len(id)-1 {
		return ""
	}
	return id[i+1:]
}

// producesText reports whether an OpenRouter modality string
// describes a model whose output side is text. The earlier strict
// text->text filter dropped multimodal-input, text-output models
// like `minimax/minimax-m3` (text+image+video->text) and
// `moonshotai/kimi-k2.5` (text+image->text) — the very models
// users reach via bare names such as `MiniMax-M3`. They still bill
// prompt/completion in tokens, so the filter is output-side only.
// An empty modality is treated as text->text since OpenRouter
// omits the field for plain text models.
func producesText(modality string) bool {
	if modality == "" {
		return true
	}
	_, output, ok := strings.Cut(modality, "->")
	if !ok {
		return false
	}
	return strings.Contains(output, "text")
}

// parsePricePerToken turns OpenRouter's quoted string
// ("0.000003") into a float64 USD-per-token. Empty strings
// and malformed, negative, or non-finite values return
// ok=false. Zero is valid for free models.
func parsePricePerToken(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if f < 0 {
		return 0, false
	}
	return f, true
}
