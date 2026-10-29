package catalog

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
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

const (
	litellmBaseURL     = "https://raw.githubusercontent.com/BerriAI/litellm/"
	litellmPricingFile = "model_prices_and_context_window.json"
	litellmURL         = litellmBaseURL + "main/" + litellmPricingFile
	priorityTier       = "priority"
)

// PriorityPattern identifies a model's Fast pricing row.
func PriorityPattern(pattern string) string { return pattern + "@" + priorityTier }

// ModelPricing holds per-model token pricing in cost per million tokens.
// CacheCreation1hPerMTok prices 1-hour-TTL cache writes; zero means the
// catalog publishes no separate 1h rate and writes bill at
// CacheCreationPerMTok.
type ModelPricing struct {
	ModelPattern           string
	InputPerMTok           money.Money
	OutputPerMTok          money.Money
	CacheCreationPerMTok   money.Money
	CacheCreation1hPerMTok money.Money
	CacheReadPerMTok       money.Money
	Bands                  []PricingBand
}

type PricingBand struct {
	AboveInputTokens       int         `json:"above_input_tokens"`
	InputPerMTok           money.Money `json:"input_per_mtok"`
	OutputPerMTok          money.Money `json:"output_per_mtok"`
	CacheCreationPerMTok   money.Money `json:"cache_creation_per_mtok"`
	CacheCreation1hPerMTok money.Money `json:"cache_creation_1h_per_mtok"`
	CacheReadPerMTok       money.Money `json:"cache_read_per_mtok"`
}

var inputThresholdRatePattern = regexp.MustCompile(
	`^input_cost_per_token_above_([0-9]+)(k?)_tokens$`,
)

var priorityInputThresholdRatePattern = regexp.MustCompile(
	`^input_cost_per_token_above_([0-9]+)(k?)_tokens_` + priorityTier + `$`,
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
		cacheCreation1h, _, err := parseOptionalRate(
			fields, "cache_creation_input_token_cost_above_1hr")
		if err != nil {
			return nil, fmt.Errorf("parsing %s 1h cache creation price: %w", model, err)
		}
		cacheRead, _, err := parseOptionalRate(fields, "cache_read_input_token_cost")
		if err != nil {
			return nil, fmt.Errorf("parsing %s cache read price: %w", model, err)
		}

		p := ModelPricing{
			ModelPattern:           model,
			InputPerMTok:           input,
			OutputPerMTok:          output,
			CacheCreationPerMTok:   cacheCreation,
			CacheCreation1hPerMTok: cacheCreation1h,
			CacheReadPerMTok:       cacheRead,
		}
		p.Bands, err = parsePricingBands(model, fields, p, inputThresholdRatePattern, nil)
		if err != nil {
			return nil, err
		}
		prices = append(prices, p)
		priority, present, err := parsePriorityPricing(model, fields, p)
		if err != nil {
			return nil, err
		}
		if present {
			prices = append(prices, priority)
		}
	}
	return prices, nil
}

func parsePriorityPricing(model string, fields map[string]jsontext.Value, p ModelPricing) (ModelPricing, bool, error) {
	var provider string
	openAI := json.Unmarshal(fields["litellm_provider"], &provider) == nil && provider == "openai"
	if !openAI {
		return ModelPricing{}, false, nil
	}
	input, present, err := parseOptionalRate(fields, "input_cost_per_token_"+priorityTier)
	if err != nil {
		return ModelPricing{}, false, fmt.Errorf("parsing %s %s input price: %w", model, priorityTier, err)
	}
	if !present {
		return ModelPricing{}, false, nil
	}
	output, present, err := parseOptionalRate(fields, "output_cost_per_token_"+priorityTier)
	if err != nil {
		return ModelPricing{}, false, fmt.Errorf("parsing %s %s output price: %w", model, priorityTier, err)
	}
	tp := p
	tp.ModelPattern = PriorityPattern(model)
	tp.InputPerMTok = input
	if present {
		tp.OutputPerMTok = output
	}
	for field, dest := range map[string]*money.Money{
		"cache_read_input_token_cost":     &tp.CacheReadPerMTok,
		"cache_creation_input_token_cost": &tp.CacheCreationPerMTok,
	} {
		rate, present, err := parseOptionalRate(fields, field+"_"+priorityTier)
		if err != nil {
			return ModelPricing{}, false, fmt.Errorf("parsing %s %s: %w", tp.ModelPattern, field, err)
		}
		if present {
			*dest = rate
		}
	}
	tp.Bands, err = parsePricingBands(model, fields, tp,
		priorityInputThresholdRatePattern, p.Bands)
	if err != nil {
		return ModelPricing{}, false, err
	}
	return tp, true, nil
}

// parsePricingBands fills unpublished tier thresholds from standard bands when
// fallbackBands is provided; tier-specific companion rates still take priority.
func parsePricingBands(
	model string,
	fields map[string]jsontext.Value,
	base ModelPricing,
	thresholdPattern *regexp.Regexp,
	fallbackBands []PricingBand,
) ([]PricingBand, error) {
	var bands []PricingBand
	thresholds := make(map[int]struct{})
	for key := range fields {
		threshold, matched, err := parsePricingThreshold(model, key, thresholdPattern)
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
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
		fallbackSuffix := strings.TrimSuffix(suffix, "_"+priorityTier)
		suffixes := []string{suffix}
		if fallbackSuffix != suffix {
			suffixes = append(suffixes, fallbackSuffix)
		}
		band, err := parsePricingBand(model, fields, base, threshold, input, suffixes...)
		if err != nil {
			return nil, err
		}
		bands = append(bands, band)
	}
	if len(fallbackBands) > 0 {
		standardByThreshold := make(map[int]PricingBand, len(fallbackBands))
		for _, band := range fallbackBands {
			standardByThreshold[band.AboveInputTokens] = band
		}
		for key := range fields {
			threshold, matched, err := parsePricingThreshold(
				model, key, inputThresholdRatePattern,
			)
			if err != nil {
				return nil, err
			}
			if !matched {
				continue
			}
			if _, exists := thresholds[threshold]; exists {
				continue
			}
			standard, exists := standardByThreshold[threshold]
			if !exists {
				continue
			}
			suffix := strings.TrimPrefix(key, "input_cost_per_token")
			band, err := parsePricingBand(
				model, fields, base, threshold, standard.InputPerMTok,
				suffix+"_"+priorityTier, suffix,
			)
			if err != nil {
				return nil, err
			}
			bands = append(bands, band)
			thresholds[threshold] = struct{}{}
		}
	}

	if err := NormalizePricingBands(model, bands); err != nil {
		return nil, err
	}
	return bands, nil
}

func parsePricingThreshold(
	model, key string,
	pattern *regexp.Regexp,
) (int, bool, error) {
	matches := pattern.FindStringSubmatch(key)
	if matches == nil {
		return 0, false, nil
	}
	threshold, err := parseThreshold(matches[1], matches[2] == "k")
	if err != nil {
		return 0, true, fmt.Errorf("parsing %s pricing threshold %q: %w", model, key, err)
	}
	return threshold, true, nil
}

func parsePricingBand(
	model string,
	fields map[string]jsontext.Value,
	base ModelPricing,
	threshold int,
	input money.Money,
	suffixes ...string,
) (PricingBand, error) {
	band := PricingBand{
		AboveInputTokens:       threshold,
		InputPerMTok:           input,
		OutputPerMTok:          base.OutputPerMTok,
		CacheCreationPerMTok:   base.CacheCreationPerMTok,
		CacheCreation1hPerMTok: base.CacheCreation1hPerMTok,
		CacheReadPerMTok:       base.CacheReadPerMTok,
	}
	companions := []struct {
		prefix string
		dest   *money.Money
	}{
		{prefix: "output_cost_per_token", dest: &band.OutputPerMTok},
		{prefix: "cache_creation_input_token_cost", dest: &band.CacheCreationPerMTok},
		{prefix: "cache_creation_input_token_cost_above_1hr", dest: &band.CacheCreation1hPerMTok},
		{prefix: "cache_read_input_token_cost", dest: &band.CacheReadPerMTok},
	}
	for _, companion := range companions {
		for _, suffix := range suffixes {
			key := companion.prefix + suffix
			rate, exists, err := parseOptionalRate(fields, key)
			if err != nil {
				return PricingBand{}, fmt.Errorf("parsing %s %s: %w", model, key, err)
			}
			if exists {
				*companion.dest = rate
				break
			}
		}
	}
	return band, nil
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
		return 0, errors.New("must be positive")
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
		return money.Money{}, false, errors.New("pricing rate is not a JSON number")
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
