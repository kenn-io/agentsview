package catalog

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

func TestParseGenAIRateRejectsNegativePrices(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "scalar", raw: `-0.0000004`},
		{name: "tier base", raw: `{"base": -0.0000004, "tiers": []}`},
		{
			name: "tier price",
			raw:  `{"base": 0, "tiers": [{"start": 0, "price": -0.0000004}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseGenAIRate(jsontext.Value(tt.raw))
			assert.ErrorContains(t, err, "must be non-negative")
		})
	}
}

func TestParseGenAIRateAcceptsZeroPrices(t *testing.T) {
	for _, raw := range []string{
		`0`,
		`{"base": 0, "tiers": [{"start": 0, "price": 0}]}`,
	} {
		_, err := parseGenAIRate(jsontext.Value(raw))
		assert.NoError(t, err)
	}
}

func TestParseGenAIPricesResolvesMatchingConditionsAndTiers(t *testing.T) {
	data := []byte(`[
		{
			"id": "openai",
			"name": "OpenAI",
			"api_pattern": "https://example.invalid",
			"model_match": {"starts_with": "gpt-"},
			"provider_match": {"contains": "openai"},
			"models": [{
				"id": "gpt-5.6-luna",
				"match": {"or": [
					{"equals": "gpt-5.6-luna"},
					{"regex": "^gpt-5\\.6-luna-\\d{4}-\\d{2}-\\d{2}$"}
				]},
				"prices": [
					{"prices": {
						"input_mtok": {"base": 1, "tiers": [{"start": 272000, "price": 2}]},
						"output_mtok": 6,
						"cache_write_mtok": 1.25,
						"cache_read_mtok": 0.1,
						"input_audio_mtok": 99
					}},
					{"constraint": {"start_date": "2026-07-30"}, "prices": {
						"input_mtok": {"base": 0.2, "tiers": [{"start": 272000, "price": 0.4}]},
						"output_mtok": {"base": 1.2, "tiers": [{"start": 272000, "price": 1.8}]},
						"cache_write_mtok": 0.25,
						"cache_read_mtok": 0.02
					}}
				]
			}]
		},
		{
			"id": "azure",
			"name": "Azure",
			"api_pattern": "https://example.invalid",
			"provider_match": {"and": [
				{"starts_with": "azure"},
				{"ends_with": "gateway"}
			]},
			"fallback_model_providers": ["openai"],
			"models": []
		}
	]`)

	prices, err := ParseGenAIPrices(data)
	require.NoError(t, err)
	assert.Equal(t, data, prices.RawJSON(), "the persisted document stays upstream JSON")
	assert.NotEmpty(t, prices.Version())

	before, ok := prices.Resolve(
		"openai-compatible", "gpt-5.6-luna-2026-07-13",
		time.Date(2026, 7, 29, 16, 59, 59, 0, time.FixedZone("west", -7*60*60)),
	)
	require.True(t, ok)
	assert.Equal(t, money.MustParseDollars("1"), before.InputPerMTok)
	assert.Equal(t, money.MustParseDollars("6"), before.OutputPerMTok)
	require.Len(t, before.Bands, 1)
	assert.Equal(t, 272000, before.Bands[0].AboveInputTokens)
	assert.Equal(t, money.MustParseDollars("2"), before.Bands[0].InputPerMTok)
	assert.Equal(t, money.MustParseDollars("6"), before.Bands[0].OutputPerMTok)

	after, ok := prices.Resolve(
		"azure-private-gateway", "gpt-5.6-luna",
		time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
	)
	require.True(t, ok, "Azure finds the OpenAI model through its fallback provider")
	assert.Equal(t, money.MustParseDollars("0.2"), after.InputPerMTok)
	assert.Equal(t, money.MustParseDollars("1.2"), after.OutputPerMTok)
	require.Len(t, after.Bands, 1)
	assert.Equal(t, money.MustParseDollars("0.4"), after.Bands[0].InputPerMTok)
	assert.Equal(t, money.MustParseDollars("1.8"), after.Bands[0].OutputPerMTok)
}

func TestGenAIPricesConditionalPrecedenceAndUTCTimeOfDay(t *testing.T) {
	prices, err := ParseGenAIPrices([]byte(`[
		{
			"id": "offpeak",
			"name": "Off Peak",
			"api_pattern": "https://example.invalid",
			"model_match": {"contains": "night-model"},
			"models": [{
				"id": "night-model",
				"match": {"equals": "night-model"},
				"prices": [
					{"prices": {"input_mtok": 10}},
					{"constraint": {"start_time": "22:00:00+00:00", "end_time": "02:00:00+00:00"},
					 "prices": {"input_mtok": 2}},
					{"constraint": {"start_time": "23:00:00+00:00", "end_time": "00:00:00+00:00"},
					 "prices": {"input_mtok": 1}}
				]
			}]
		}
	]`))
	require.NoError(t, err)

	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{"before interval", time.Date(2026, 8, 1, 21, 59, 59, 0, time.UTC), "10"},
		{"cross-midnight start", time.Date(2026, 8, 1, 22, 0, 0, 0, time.UTC), "2"},
		{"last active wins", time.Date(2026, 8, 1, 23, 30, 0, 0, time.UTC), "1"},
		{"after midnight", time.Date(2026, 8, 2, 1, 59, 59, 0, time.UTC), "2"},
		{"end exclusive", time.Date(2026, 8, 2, 2, 0, 0, 0, time.UTC), "10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := prices.Resolve("", "night-model", tt.at)
			require.True(t, ok)
			assert.Equal(t, money.MustParseDollars(tt.want), got.InputPerMTok)
		})
	}
}

func TestParseGenAIPricesAcceptsCurrentUpstreamRegexFeatures(t *testing.T) {
	prices, err := ParseGenAIPrices([]byte(`[
		{
			"id": "mistral",
			"name": "Mistral",
			"api_pattern": "https://example.invalid",
			"model_match": {"regex": "^(?![^/]+/)(?:mistral|codestral)"},
			"models": [{
				"id": "mistral-small",
				"match": {"starts_with": "mistral-small"},
				"prices": {"input_mtok": 1}
			}]
		}
	]`))
	require.NoError(t, err)

	_, ok := prices.Resolve("", "mistral-small-latest", time.Time{})
	assert.True(t, ok)
	_, ok = prices.Resolve("", "other/mistral-small-latest", time.Time{})
	assert.False(t, ok, "the upstream negative lookahead excludes provider prefixes")
}

func TestGenAIPricesRegexMatchingPreservesUpstreamCaseSensitivity(t *testing.T) {
	prices, err := ParseGenAIPrices([]byte(`[
		{
			"id": "case-sensitive",
			"name": "Case Sensitive",
			"api_pattern": "https://example.invalid",
			"model_match": {"regex": "^Model-[A-Z]+$"},
			"models": [{
				"id": "Model-ABC",
				"match": {"regex": "^Model-[A-Z]+$"},
				"prices": {"input_mtok": 1}
			}]
		}
	]`))
	require.NoError(t, err)

	_, ok := prices.Resolve("", "Model-ABC", time.Time{})
	assert.True(t, ok)
	_, ok = prices.Resolve("", "model-abc", time.Time{})
	assert.False(t, ok)
}
