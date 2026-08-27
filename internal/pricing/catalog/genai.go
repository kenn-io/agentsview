package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"

	"go.kenn.io/agentsview/internal/money"
)

const genAIPricesBaseURL = "https://raw.githubusercontent.com/pydantic/" +
	"genai-prices/"
const genAIPricesPath = "prices/new_data/v2/data.json"

// GenAIPricesURL is the live Pydantic GenAI Prices v2 document refreshed by
// Agentsview.
const GenAIPricesURL = genAIPricesBaseURL + "main/" + genAIPricesPath

const maxGenAIPricesBytes = 8 << 20

// GenAIPrices is a parsed Pydantic GenAI Prices v2 document. The original
// JSON remains the persistence and snapshot format; these values are only the
// compiled runtime representation used for matching and price selection.
type GenAIPrices struct {
	providers []genAIProvider
	raw       []byte
	version   string
}

type genAIProvider struct {
	id                     string
	modelMatch             genAIMatch
	providerMatch          genAIMatch
	fallbackModelProviders []string
	models                 []genAIModel
}

type genAIModel struct {
	id     string
	match  genAIMatch
	prices []genAIConditionalPrice
}

type genAIConditionalPrice struct {
	constraint genAIConstraint
	prices     map[string]genAIRate
}

type genAIConstraint struct {
	kind      genAIConstraintKind
	startDate time.Time
	startTime time.Duration
	endTime   time.Duration
}

type genAIConstraintKind uint8

const (
	genAIConstraintAlways genAIConstraintKind = iota
	genAIConstraintStartDate
	genAIConstraintTimeOfDay
)

type genAIRate struct {
	base  money.Money
	tiers []genAITier
}

type genAITier struct {
	start int
	price money.Money
}

type genAIMatch struct {
	kind    genAIMatchKind
	value   string
	regex   *regexp2.Regexp
	clauses []genAIMatch
}

type genAIMatchKind uint8

const (
	genAIMatchMissing genAIMatchKind = iota
	genAIMatchEquals
	genAIMatchStartsWith
	genAIMatchEndsWith
	genAIMatchContains
	genAIMatchRegex
	genAIMatchOr
	genAIMatchAnd
)

type rawGenAIProvider struct {
	ID                     string          `json:"id"`
	ModelMatch             jsontext.Value  `json:"model_match"`
	ProviderMatch          jsontext.Value  `json:"provider_match"`
	FallbackModelProviders []string        `json:"fallback_model_providers"`
	Models                 []rawGenAIModel `json:"models"`
}

type rawGenAIModel struct {
	ID     string         `json:"id"`
	Match  jsontext.Value `json:"match"`
	Prices jsontext.Value `json:"prices"`
}

type rawGenAIConditionalPrice struct {
	Constraint jsontext.Value            `json:"constraint"`
	Prices     map[string]jsontext.Value `json:"prices"`
}

type rawGenAIConstraint struct {
	StartDate string `json:"start_date"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
}

type rawGenAITieredPrice struct {
	Base  jsontext.Value `json:"base"`
	Tiers []rawGenAITier `json:"tiers"`
}

type rawGenAITier struct {
	Start int            `json:"start"`
	Price jsontext.Value `json:"price"`
}

type rawGenAIMatch struct {
	Equals     *string          `json:"equals"`
	StartsWith *string          `json:"starts_with"`
	EndsWith   *string          `json:"ends_with"`
	Contains   *string          `json:"contains"`
	Regex      *string          `json:"regex"`
	Or         []jsontext.Value `json:"or"`
	And        []jsontext.Value `json:"and"`
}

// FetchGenAIPricesContext downloads and parses the current GenAI Prices v2
// document. The returned value retains the upstream JSON for persistence.
func FetchGenAIPricesContext(ctx context.Context) (*GenAIPrices, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return fetchGenAIPrices(ctx, client, GenAIPricesURL)
}

// FetchGenAIPricesAtRef downloads GenAI Prices from an immutable upstream Git
// ref for reproducible embedded snapshot generation.
func FetchGenAIPricesAtRef(
	ctx context.Context, ref string,
) (*GenAIPrices, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return fetchGenAIPrices(
		ctx, client, genAIPricesBaseURL+ref+"/"+genAIPricesPath,
	)
}

func fetchGenAIPrices(
	ctx context.Context, client *http.Client, url string,
) (*GenAIPrices, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating GenAI Prices request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching GenAI Prices: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"fetching GenAI Prices: status %d", resp.StatusCode,
		)
	}
	data, err := readLimitedGenAIPrices(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading GenAI Prices response: %w", err)
	}
	return ParseGenAIPrices(data)
}

func readLimitedGenAIPrices(reader io.Reader) ([]byte, error) {
	limited := io.LimitReader(reader, maxGenAIPricesBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxGenAIPricesBytes {
		return nil, fmt.Errorf(
			"GenAI Prices document exceeds %d bytes", maxGenAIPricesBytes,
		)
	}
	return data, nil
}

// ParseGenAIPrices parses a Pydantic GenAI Prices v2 data document. It keeps
// the original JSON intact and compiles the match expressions, conditional
// prices, and tier thresholds needed at lookup time.
func ParseGenAIPrices(data []byte) (*GenAIPrices, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("parsing GenAI Prices JSON: empty document")
	}
	if len(data) > maxGenAIPricesBytes {
		return nil, fmt.Errorf(
			"parsing GenAI Prices JSON: document exceeds %d bytes",
			maxGenAIPricesBytes,
		)
	}
	var raw []rawGenAIProvider
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing GenAI Prices JSON: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("parsing GenAI Prices JSON: no providers")
	}

	prices := &GenAIPrices{
		providers: make([]genAIProvider, len(raw)),
		raw:       slices.Clone(data),
	}
	for i, provider := range raw {
		parsed, err := parseGenAIProvider(provider)
		if err != nil {
			return nil, err
		}
		prices.providers[i] = parsed
	}
	prices.version = genAIPricesVersion(data)
	return prices, nil
}

func parseGenAIProvider(raw rawGenAIProvider) (genAIProvider, error) {
	id := strings.TrimSpace(raw.ID)
	if id == "" {
		return genAIProvider{}, fmt.Errorf(
			"parsing GenAI Prices JSON: provider has empty id",
		)
	}
	modelMatch, err := parseOptionalGenAIMatch(raw.ModelMatch)
	if err != nil {
		return genAIProvider{}, fmt.Errorf(
			"parsing GenAI Prices provider %q model_match: %w", id, err,
		)
	}
	providerMatch, err := parseOptionalGenAIMatch(raw.ProviderMatch)
	if err != nil {
		return genAIProvider{}, fmt.Errorf(
			"parsing GenAI Prices provider %q provider_match: %w", id, err,
		)
	}
	provider := genAIProvider{
		id:                     id,
		modelMatch:             modelMatch,
		providerMatch:          providerMatch,
		fallbackModelProviders: slices.Clone(raw.FallbackModelProviders),
		models:                 make([]genAIModel, len(raw.Models)),
	}
	for i, model := range raw.Models {
		parsed, parseErr := parseGenAIModel(id, model)
		if parseErr != nil {
			return genAIProvider{}, parseErr
		}
		provider.models[i] = parsed
	}
	return provider, nil
}

func parseGenAIModel(provider string, raw rawGenAIModel) (genAIModel, error) {
	id := strings.TrimSpace(raw.ID)
	if id == "" {
		return genAIModel{}, fmt.Errorf(
			"parsing GenAI Prices provider %q: model has empty id", provider,
		)
	}
	match, err := parseRequiredGenAIMatch(raw.Match)
	if err != nil {
		return genAIModel{}, fmt.Errorf(
			"parsing GenAI Prices model %s/%s match: %w", provider, id, err,
		)
	}
	conditionals, err := parseGenAIModelPrices(provider+"/"+id, raw.Prices)
	if err != nil {
		return genAIModel{}, err
	}
	return genAIModel{id: id, match: match, prices: conditionals}, nil
}

func parseGenAIModelPrices(
	model string, raw jsontext.Value,
) ([]genAIConditionalPrice, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return nil, fmt.Errorf("parsing GenAI Prices model %s: missing prices", model)
	}
	if !strings.HasPrefix(value, "[") {
		var fields map[string]jsontext.Value
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf(
				"parsing GenAI Prices model %s prices: %w", model, err,
			)
		}
		prices, err := parseGenAIPriceFields(model, fields)
		if err != nil {
			return nil, err
		}
		return []genAIConditionalPrice{{prices: prices}}, nil
	}

	var rawConditionals []rawGenAIConditionalPrice
	if err := json.Unmarshal(raw, &rawConditionals); err != nil {
		return nil, fmt.Errorf(
			"parsing GenAI Prices model %s conditional prices: %w", model, err,
		)
	}
	if len(rawConditionals) == 0 {
		return nil, fmt.Errorf(
			"parsing GenAI Prices model %s: empty conditional prices", model,
		)
	}
	out := make([]genAIConditionalPrice, len(rawConditionals))
	for i, conditional := range rawConditionals {
		constraint, err := parseGenAIConstraint(conditional.Constraint)
		if err != nil {
			return nil, fmt.Errorf(
				"parsing GenAI Prices model %s condition %d: %w", model, i, err,
			)
		}
		prices, err := parseGenAIPriceFields(model, conditional.Prices)
		if err != nil {
			return nil, err
		}
		out[i] = genAIConditionalPrice{constraint: constraint, prices: prices}
	}
	return out, nil
}

func parseGenAIConstraint(raw jsontext.Value) (genAIConstraint, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return genAIConstraint{}, nil
	}
	var decoded rawGenAIConstraint
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return genAIConstraint{}, err
	}
	switch {
	case decoded.StartDate != "" && decoded.StartTime == "" && decoded.EndTime == "":
		start, err := time.Parse(time.DateOnly, decoded.StartDate)
		if err != nil {
			return genAIConstraint{}, fmt.Errorf(
				"invalid start_date %q: %w", decoded.StartDate, err,
			)
		}
		return genAIConstraint{
			kind: genAIConstraintStartDate, startDate: start.UTC(),
		}, nil
	case decoded.StartDate == "" && decoded.StartTime != "" && decoded.EndTime != "":
		start, err := parseGenAITimeOfDay(decoded.StartTime)
		if err != nil {
			return genAIConstraint{}, fmt.Errorf(
				"invalid start_time %q: %w", decoded.StartTime, err,
			)
		}
		end, err := parseGenAITimeOfDay(decoded.EndTime)
		if err != nil {
			return genAIConstraint{}, fmt.Errorf(
				"invalid end_time %q: %w", decoded.EndTime, err,
			)
		}
		return genAIConstraint{
			kind: genAIConstraintTimeOfDay, startTime: start, endTime: end,
		}, nil
	default:
		return genAIConstraint{}, fmt.Errorf(
			"constraint must contain start_date or start_time and end_time",
		)
	}
}

func parseGenAITimeOfDay(value string) (time.Duration, error) {
	parsed, err := time.Parse("15:04:05Z07:00", value)
	if err != nil {
		return 0, err
	}
	utc := parsed.UTC()
	return time.Duration(utc.Hour())*time.Hour +
		time.Duration(utc.Minute())*time.Minute +
		time.Duration(utc.Second())*time.Second +
		time.Duration(utc.Nanosecond()), nil
}

func parseGenAIPriceFields(
	model string, fields map[string]jsontext.Value,
) (map[string]genAIRate, error) {
	prices := make(map[string]genAIRate, len(fields))
	for unit, raw := range fields {
		rate, err := parseGenAIRate(raw)
		if err != nil {
			return nil, fmt.Errorf(
				"parsing GenAI Prices model %s unit %q: %w", model, unit, err,
			)
		}
		prices[unit] = rate
	}
	return prices, nil
}

func parseGenAIRate(raw jsontext.Value) (genAIRate, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return genAIRate{}, fmt.Errorf("price must be a non-negative number")
	}
	if !strings.HasPrefix(value, "{") {
		base, err := parseNonnegativeGenAIPrice(value)
		if err != nil {
			return genAIRate{}, err
		}
		return genAIRate{base: base}, nil
	}
	var tiered rawGenAITieredPrice
	if err := json.Unmarshal(raw, &tiered); err != nil {
		return genAIRate{}, err
	}
	base, err := parseNonnegativeGenAIPrice(
		strings.TrimSpace(string(tiered.Base)),
	)
	if err != nil {
		return genAIRate{}, fmt.Errorf("invalid base price: %w", err)
	}
	tiers := make([]genAITier, len(tiered.Tiers))
	for i, tier := range tiered.Tiers {
		if tier.Start < 0 {
			return genAIRate{}, fmt.Errorf(
				"pricing threshold must be non-negative",
			)
		}
		price, parseErr := parseNonnegativeGenAIPrice(
			strings.TrimSpace(string(tier.Price)),
		)
		if parseErr != nil {
			return genAIRate{}, fmt.Errorf("invalid tier price: %w", parseErr)
		}
		tiers[i] = genAITier{start: tier.Start, price: price}
	}
	slices.SortFunc(tiers, func(a, b genAITier) int {
		return a.start - b.start
	})
	return genAIRate{base: base, tiers: tiers}, nil
}

func parseNonnegativeGenAIPrice(value string) (money.Money, error) {
	price, err := money.ParseDollars(value)
	if err != nil {
		return money.Money{}, err
	}

	mantissa := value
	if exponent := strings.IndexAny(mantissa, "eE"); exponent >= 0 {
		mantissa = mantissa[:exponent]
	}
	if strings.HasPrefix(mantissa, "-") &&
		strings.ContainsAny(mantissa, "123456789") {
		return money.Money{}, fmt.Errorf("price must be non-negative")
	}
	return price, nil
}

func parseOptionalGenAIMatch(raw jsontext.Value) (genAIMatch, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return genAIMatch{}, nil
	}
	return parseRequiredGenAIMatch(raw)
}

func parseRequiredGenAIMatch(raw jsontext.Value) (genAIMatch, error) {
	var decoded rawGenAIMatch
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return genAIMatch{}, err
	}
	type scalar struct {
		kind  genAIMatchKind
		value *string
	}
	scalars := []scalar{
		{genAIMatchEquals, decoded.Equals},
		{genAIMatchStartsWith, decoded.StartsWith},
		{genAIMatchEndsWith, decoded.EndsWith},
		{genAIMatchContains, decoded.Contains},
		{genAIMatchRegex, decoded.Regex},
	}
	var matches []genAIMatch
	for _, candidate := range scalars {
		if candidate.value != nil {
			matches = append(matches, genAIMatch{
				kind: candidate.kind, value: *candidate.value,
			})
		}
	}
	for _, group := range []struct {
		kind genAIMatchKind
		raw  []jsontext.Value
	}{{genAIMatchOr, decoded.Or}, {genAIMatchAnd, decoded.And}} {
		if group.raw == nil {
			continue
		}
		clauses := make([]genAIMatch, len(group.raw))
		for i, clause := range group.raw {
			parsed, err := parseRequiredGenAIMatch(clause)
			if err != nil {
				return genAIMatch{}, err
			}
			clauses[i] = parsed
		}
		matches = append(matches, genAIMatch{
			kind: group.kind, clauses: clauses,
		})
	}
	if len(matches) != 1 {
		return genAIMatch{}, fmt.Errorf(
			"match clause must contain exactly one operation",
		)
	}
	match := matches[0]
	if match.kind == genAIMatchRegex {
		compiled, err := regexp2.Compile(match.value, regexp2.RE2)
		if err != nil {
			return genAIMatch{}, fmt.Errorf("invalid regex %q: %w", match.value, err)
		}
		compiled.MatchTimeout = 100 * time.Millisecond
		match.regex = compiled
	}
	return match, nil
}

func (m genAIMatch) matches(text string) bool {
	switch m.kind {
	case genAIMatchEquals:
		return strings.EqualFold(text, m.value)
	case genAIMatchStartsWith:
		return strings.HasPrefix(strings.ToLower(text), strings.ToLower(m.value))
	case genAIMatchEndsWith:
		return strings.HasSuffix(strings.ToLower(text), strings.ToLower(m.value))
	case genAIMatchContains:
		return strings.Contains(strings.ToLower(text), strings.ToLower(m.value))
	case genAIMatchRegex:
		matched, err := m.regex.MatchString(text)
		return err == nil && matched
	case genAIMatchOr:
		return slices.ContainsFunc(m.clauses, func(clause genAIMatch) bool {
			return clause.matches(text)
		})
	case genAIMatchAnd:
		return !slices.ContainsFunc(m.clauses, func(clause genAIMatch) bool {
			return !clause.matches(text)
		})
	default:
		return false
	}
}

// Resolve selects a GenAI Prices model and its active conditional price. It
// follows the upstream provider/model matching and one-level fallback rules.
// The bool is false when no model matches or the selected price has none of
// the token units Agentsview records.
func (p *GenAIPrices) Resolve(
	providerID, modelID string, timestamp time.Time,
) (ModelPricing, bool) {
	if p == nil || modelID == "" {
		return ModelPricing{}, false
	}
	provider := p.findProvider(providerID, modelID)
	if provider == nil {
		return ModelPricing{}, false
	}
	model := provider.findModel(modelID)
	if model == nil {
		for _, fallbackID := range provider.fallbackModelProviders {
			fallback := p.providerByID(fallbackID)
			if fallback != nil {
				model = fallback.findModel(modelID)
			}
			if model != nil {
				break
			}
		}
	}
	if model == nil {
		return ModelPricing{}, false
	}
	selected := model.activePrices(timestamp)
	rates, ok := genAIModelPricing(provider.id+"/"+model.id, selected)
	return rates, ok
}

func (p *GenAIPrices) findProvider(
	providerID, modelID string,
) *genAIProvider {
	if providerID != "" {
		normalized := strings.ToLower(strings.TrimSpace(providerID))
		if exact := p.providerByID(normalized); exact != nil {
			return exact
		}
		for i := range p.providers {
			if p.providers[i].providerMatch.matches(normalized) {
				return &p.providers[i]
			}
		}
		if normalized != "litellm" {
			return nil
		}
	}
	for i := range p.providers {
		if p.providers[i].modelMatch.matches(modelID) {
			return &p.providers[i]
		}
	}
	return nil
}

func (p *GenAIPrices) providerByID(id string) *genAIProvider {
	for i := range p.providers {
		if p.providers[i].id == id {
			return &p.providers[i]
		}
	}
	return nil
}

func (p *genAIProvider) findModel(modelID string) *genAIModel {
	for i := range p.models {
		if p.models[i].match.matches(modelID) {
			return &p.models[i]
		}
	}
	return nil
}

func (m *genAIModel) activePrices(timestamp time.Time) map[string]genAIRate {
	if len(m.prices) == 1 {
		return m.prices[0].prices
	}
	for _, v := range slices.Backward(m.prices) {
		if v.constraint.active(timestamp) {
			return v.prices
		}
	}
	return m.prices[0].prices
}

func (c genAIConstraint) active(timestamp time.Time) bool {
	switch c.kind {
	case genAIConstraintAlways:
		return true
	case genAIConstraintStartDate:
		return !timestamp.UTC().Before(c.startDate)
	case genAIConstraintTimeOfDay:
		utc := timestamp.UTC()
		clock := time.Duration(utc.Hour())*time.Hour +
			time.Duration(utc.Minute())*time.Minute +
			time.Duration(utc.Second())*time.Second +
			time.Duration(utc.Nanosecond())
		if c.endTime < c.startTime {
			return clock >= c.startTime || clock < c.endTime
		}
		return clock >= c.startTime && clock < c.endTime
	default:
		return false
	}
}

func genAIModelPricing(
	pattern string, prices map[string]genAIRate,
) (ModelPricing, bool) {
	input, hasInput := prices["input_mtok"]
	output, hasOutput := prices["output_mtok"]
	cacheWrite, hasCacheWrite := prices["cache_write_mtok"]
	cacheRead, hasCacheRead := prices["cache_read_mtok"]
	if !hasInput && !hasOutput && !hasCacheWrite && !hasCacheRead {
		return ModelPricing{}, false
	}
	rates := ModelPricing{
		ModelPattern:         pattern,
		InputPerMTok:         input.base,
		OutputPerMTok:        output.base,
		CacheCreationPerMTok: cacheWrite.base,
		CacheReadPerMTok:     cacheRead.base,
	}
	thresholds := make(map[int]struct{})
	for _, rate := range []genAIRate{input, output, cacheWrite, cacheRead} {
		for _, tier := range rate.tiers {
			thresholds[tier.start] = struct{}{}
		}
	}
	ordered := make([]int, 0, len(thresholds))
	for threshold := range thresholds {
		ordered = append(ordered, threshold)
	}
	slices.Sort(ordered)
	for _, threshold := range ordered {
		rates.Bands = append(rates.Bands, PricingBand{
			AboveInputTokens:     threshold,
			InputPerMTok:         input.at(threshold),
			OutputPerMTok:        output.at(threshold),
			CacheCreationPerMTok: cacheWrite.at(threshold),
			CacheReadPerMTok:     cacheRead.at(threshold),
		})
	}
	return rates, true
}

func (r genAIRate) at(totalInputThreshold int) money.Money {
	price := r.base
	for _, tier := range r.tiers {
		if tier.start > totalInputThreshold {
			break
		}
		price = tier.price
	}
	return price
}

// RawJSON returns a copy of the original GenAI Prices v2 document.
func (p *GenAIPrices) RawJSON() []byte {
	if p == nil {
		return nil
	}
	return slices.Clone(p.raw)
}

// Version is the content-derived identity of the original JSON document.
func (p *GenAIPrices) Version() string {
	if p == nil {
		return ""
	}
	return p.version
}

func genAIPricesVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return "genai-prices-" + hex.EncodeToString(sum[:])[:12]
}
