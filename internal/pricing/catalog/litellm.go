package catalog

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ccoveille/go-safecast/v2"

	"go.kenn.io/agentsview/internal/money"
)

const litellmBaseURL = "https://raw.githubusercontent.com/BerriAI/litellm/"
const litellmPricingFile = "model_prices_and_context_window.json"
const litellmURL = litellmBaseURL + "main/" + litellmPricingFile

// ModelPricing holds per-model token pricing in cost per million tokens.
type ModelPricing struct {
	ModelPattern         string
	InputPerMTok         money.Money
	OutputPerMTok        money.Money
	CacheCreationPerMTok money.Money
	CacheReadPerMTok     money.Money
	Bands                []PricingBand
}

type PricingBand struct {
	AboveInputTokens     int         `json:"above_input_tokens"`
	InputPerMTok         money.Money `json:"input_per_mtok"`
	OutputPerMTok        money.Money `json:"output_per_mtok"`
	CacheCreationPerMTok money.Money `json:"cache_creation_per_mtok"`
	CacheReadPerMTok     money.Money `json:"cache_read_per_mtok"`
}

var inputThresholdRatePattern = regexp.MustCompile(
	`^input_cost_per_token_above_([0-9]+)(k?)_tokens$`,
)

// FetchLiteLLMPricingContext downloads the LiteLLM pricing JSON and binds the
// request lifetime to ctx.
func FetchLiteLLMPricingContext(
	ctx context.Context,
) ([]ModelPricing, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return fetchLiteLLMPricing(ctx, client, litellmURL)
}

// FetchLiteLLMPricingAtRef downloads the catalog from an immutable upstream
// Git ref for reproducible fallback snapshot generation.
func FetchLiteLLMPricingAtRef(
	ctx context.Context, ref string,
) ([]ModelPricing, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return fetchLiteLLMPricing(
		ctx,
		client,
		litellmBaseURL+ref+"/"+litellmPricingFile,
	)
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
	var raw map[string]map[string]jsontext.Value
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing litellm JSON: %w", err)
	}

	var prices []ModelPricing
	for model, fields := range raw {
		input, hasInput, err := parseOptionalRate(fields, "input_cost_per_token")
		if err != nil {
			return nil, fmt.Errorf("parsing %s input price: %w", model, err)
		}
		output, hasOutput, err := parseOptionalRate(fields, "output_cost_per_token")
		if err != nil {
			return nil, fmt.Errorf("parsing %s output price: %w", model, err)
		}
		if !hasInput && !hasOutput {
			continue
		}
		cacheCreation, _, err := parseOptionalRate(fields, "cache_creation_input_token_cost")
		if err != nil {
			return nil, fmt.Errorf("parsing %s cache creation price: %w", model, err)
		}
		cacheRead, _, err := parseOptionalRate(fields, "cache_read_input_token_cost")
		if err != nil {
			return nil, fmt.Errorf("parsing %s cache read price: %w", model, err)
		}

		p := ModelPricing{
			ModelPattern:         model,
			InputPerMTok:         input,
			OutputPerMTok:        output,
			CacheCreationPerMTok: cacheCreation,
			CacheReadPerMTok:     cacheRead,
		}
		p.Bands, err = parsePricingBands(model, fields, p)
		if err != nil {
			return nil, err
		}
		prices = append(prices, p)
	}
	return prices, nil
}

func parsePricingBands(
	model string,
	fields map[string]jsontext.Value,
	base ModelPricing,
) ([]PricingBand, error) {
	var bands []PricingBand
	thresholds := make(map[int]struct{})
	for key := range fields {
		matches := inputThresholdRatePattern.FindStringSubmatch(key)
		if matches == nil {
			continue
		}

		threshold, err := parseThreshold(matches[1], matches[2] == "k")
		if err != nil {
			return nil, fmt.Errorf("parsing %s pricing threshold %q: %w", model, key, err)
		}
		if _, exists := thresholds[threshold]; exists {
			return nil, fmt.Errorf(
				"parsing %s: duplicate pricing threshold %d",
				model,
				threshold,
			)
		}

		input, present, err := parseOptionalRate(fields, key)
		if err != nil {
			return nil, fmt.Errorf("parsing %s %s: %w", model, key, err)
		}
		if !present {
			continue
		}
		thresholds[threshold] = struct{}{}

		suffix := strings.TrimPrefix(key, "input_cost_per_token")
		band := PricingBand{
			AboveInputTokens:     threshold,
			InputPerMTok:         input,
			OutputPerMTok:        base.OutputPerMTok,
			CacheCreationPerMTok: base.CacheCreationPerMTok,
			CacheReadPerMTok:     base.CacheReadPerMTok,
		}
		companions := []struct {
			prefix string
			dest   *money.Money
		}{
			{prefix: "output_cost_per_token", dest: &band.OutputPerMTok},
			{prefix: "cache_creation_input_token_cost", dest: &band.CacheCreationPerMTok},
			{prefix: "cache_read_input_token_cost", dest: &band.CacheReadPerMTok},
		}
		for _, companion := range companions {
			rate, exists, err := parseOptionalRate(fields, companion.prefix+suffix)
			if err != nil {
				return nil, fmt.Errorf(
					"parsing %s %s: %w",
					model,
					companion.prefix+suffix,
					err,
				)
			}
			if exists {
				*companion.dest = rate
			}
		}
		bands = append(bands, band)
	}

	if err := NormalizePricingBands(model, bands); err != nil {
		return nil, err
	}
	return bands, nil
}

// NormalizePricingBands validates and sorts a model's complete request-pricing
// bands for deterministic transport and persistence.
func NormalizePricingBands(model string, bands []PricingBand) error {
	slices.SortFunc(bands, func(a, b PricingBand) int {
		return cmp.Compare(a.AboveInputTokens, b.AboveInputTokens)
	})
	for i, band := range bands {
		if band.AboveInputTokens <= 0 {
			return fmt.Errorf(
				"model %s pricing threshold must be positive",
				model,
			)
		}
		if i > 0 && bands[i-1].AboveInputTokens == band.AboveInputTokens {
			return fmt.Errorf(
				"model %s has duplicate pricing threshold %d",
				model,
				band.AboveInputTokens,
			)
		}
	}
	return nil
}

func parseThreshold(raw string, thousands bool) (int, error) {
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	if thousands {
		maxInt := uint64(^uint(0) >> 1)
		if value > maxInt/1000 {
			return 0, strconv.ErrRange
		}
		value *= 1000
	}
	if value == 0 {
		return 0, fmt.Errorf("must be positive")
	}
	converted, err := safecast.Convert[int](value)
	if err != nil {
		return 0, strconv.ErrRange
	}
	return converted, nil
}

func parseOptionalRate(
	fields map[string]jsontext.Value,
	key string,
) (money.Money, bool, error) {
	raw, ok := fields[key]
	if !ok || string(raw) == "null" {
		return money.Money{}, false, nil
	}
	if raw.Kind() != jsontext.KindNumber {
		return money.Money{}, false, fmt.Errorf("pricing rate is not a JSON number")
	}
	rate, err := parsePerTokenRate(string(raw))
	if err != nil {
		return money.Money{}, false, err
	}
	return rate, true, nil
}

func parsePerTokenRate(value string) (money.Money, error) {
	microdollars, err := money.ParseScaledDecimal(value, 12)
	if err != nil {
		return money.Money{}, err
	}
	if microdollars < 0 {
		return money.Money{}, money.ErrNegative
	}
	return money.Money{Microdollars: microdollars}, nil
}
