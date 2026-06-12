package pricing

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLiteLLMPricing(t *testing.T) {
	data := []byte(`{
		"sample_spec": {"max_tokens": 4096},
		"claude-sonnet-4-20250514": {
			"input_cost_per_token": 0.000003,
			"output_cost_per_token": 0.000015,
			"cache_creation_input_token_cost": 0.00000375,
			"cache_read_input_token_cost": 0.0000003,
			"litellm_provider": "anthropic"
		}
	}`)

	prices, err := ParseLiteLLMPricing(data)
	require.NoError(t, err)

	var found *ModelPricing
	for i := range prices {
		if prices[i].ModelPattern == "claude-sonnet-4-20250514" {
			found = &prices[i]
			break
		}
	}
	require.NotNil(t, found, "claude-sonnet-4-20250514 not found in results")

	assertClose(t, "InputPerMTok", found.InputPerMTok, 3.0)
	assertClose(t, "OutputPerMTok", found.OutputPerMTok, 15.0)
	assertClose(t, "CacheCreationPerMTok",
		found.CacheCreationPerMTok, 3.75)
	assertClose(t, "CacheReadPerMTok",
		found.CacheReadPerMTok, 0.30)
}

func TestParseLiteLLMPricingMultipleProviders(t *testing.T) {
	data := []byte(`{
		"claude-sonnet-4-20250514": {
			"input_cost_per_token": 0.000003,
			"output_cost_per_token": 0.000015,
			"litellm_provider": "anthropic"
		},
		"gpt-4o": {
			"input_cost_per_token": 0.0000025,
			"output_cost_per_token": 0.00001,
			"litellm_provider": "openai"
		}
	}`)

	prices, err := ParseLiteLLMPricing(data)
	require.NoError(t, err)

	foundAnthropic := false
	foundOpenAI := false
	for _, p := range prices {
		switch p.ModelPattern {
		case "claude-sonnet-4-20250514":
			foundAnthropic = true
		case "gpt-4o":
			foundOpenAI = true
		}
	}
	assert.True(t, foundAnthropic, "anthropic model not found")
	assert.True(t, foundOpenAI, "openai model not found")
}

func TestParseLiteLLMPricingSkipsNoCost(t *testing.T) {
	data := []byte(`{
		"claude-sonnet-4-20250514": {
			"input_cost_per_token": 0.000003,
			"output_cost_per_token": 0.000015,
			"litellm_provider": "anthropic"
		},
		"no-cost-model": {
			"litellm_provider": "anthropic"
		}
	}`)

	prices, err := ParseLiteLLMPricing(data)
	require.NoError(t, err)

	require.Len(t, prices, 1)
	assert.Equal(t, "claude-sonnet-4-20250514", prices[0].ModelPattern)
}

func TestFallbackPricing(t *testing.T) {
	prices := FallbackPricing()
	require.NotEmpty(t, prices, "FallbackPricing returned empty")

	required := map[string]bool{
		"claude-sonnet-4-6":         false,
		"claude-opus-4-6":           false,
		"claude-opus-4-7":           false,
		"claude-opus-4-8":           false,
		"claude-fable-5":            false,
		"claude-haiku-4-5-20251001": false,
		"claude-sonnet-4-20250514":  false,
		"claude-opus-4-20250514":    false,
		"claude-haiku-3-5-20241022": false,
	}
	for _, p := range prices {
		if _, ok := required[p.ModelPattern]; ok {
			required[p.ModelPattern] = true
		}
	}
	for model, found := range required {
		assert.True(t, found, "required model %s missing", model)
	}
}

func TestFallbackPricing_Opus46Rates(t *testing.T) {
	prices := FallbackPricing()
	var got *ModelPricing
	for i := range prices {
		if prices[i].ModelPattern == "claude-opus-4-6" {
			got = &prices[i]
			break
		}
	}
	require.NotNil(t, got, "claude-opus-4-6 entry missing from FallbackPricing")

	// Source: https://claude.com/pricing — Opus 4.5/4.6 tier.
	want := ModelPricing{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         5.0,
		OutputPerMTok:        25.0,
		CacheCreationPerMTok: 6.25,
		CacheReadPerMTok:     0.50,
	}
	assert.Equal(t, want, *got)
}

func TestFallbackPricing_Opus47Rates(t *testing.T) {
	prices := FallbackPricing()
	var got *ModelPricing
	for i := range prices {
		if prices[i].ModelPattern == "claude-opus-4-7" {
			got = &prices[i]
			break
		}
	}
	require.NotNil(t, got, "claude-opus-4-7 entry missing from FallbackPricing")

	// Opus 4.7 shares the Opus 4.6/4.8 tier. LiteLLM already lists
	// it, but the fallback ships it too so offline and fresh-seed
	// pricing covers the whole current Opus generation.
	want := ModelPricing{
		ModelPattern:         "claude-opus-4-7",
		InputPerMTok:         5.0,
		OutputPerMTok:        25.0,
		CacheCreationPerMTok: 6.25,
		CacheReadPerMTok:     0.50,
	}
	assert.Equal(t, want, *got)
}

func TestFallbackPricing_Opus48Rates(t *testing.T) {
	prices := FallbackPricing()
	var got *ModelPricing
	for i := range prices {
		if prices[i].ModelPattern == "claude-opus-4-8" {
			got = &prices[i]
			break
		}
	}
	require.NotNil(t, got, "claude-opus-4-8 entry missing from FallbackPricing")

	// Opus 4.8 launched at the same rates as Opus 4.6/4.7 and is
	// not yet in the LiteLLM catalog, so the shipped fallback must
	// price it at the current Opus tier.
	want := ModelPricing{
		ModelPattern:         "claude-opus-4-8",
		InputPerMTok:         5.0,
		OutputPerMTok:        25.0,
		CacheCreationPerMTok: 6.25,
		CacheReadPerMTok:     0.50,
	}
	assert.Equal(t, want, *got)
}

func TestFallbackPricing_Fable5Rates(t *testing.T) {
	prices := FallbackPricing()
	var got *ModelPricing
	for i := range prices {
		if prices[i].ModelPattern == "claude-fable-5" {
			got = &prices[i]
			break
		}
	}
	require.NotNil(t, got, "claude-fable-5 entry missing from FallbackPricing")

	// Source: https://platform.claude.com/docs/en/about-claude/pricing
	// Fable 5 launched at double the Opus 4.8 rates and is not yet in
	// the LiteLLM catalog, so the shipped fallback must price it.
	want := ModelPricing{
		ModelPattern:         "claude-fable-5",
		InputPerMTok:         10.0,
		OutputPerMTok:        50.0,
		CacheCreationPerMTok: 12.50,
		CacheReadPerMTok:     1.0,
	}
	assert.Equal(t, want, *got)
}

func TestFallbackPricing_HermesModels(t *testing.T) {
	byPattern := make(map[string]ModelPricing)
	for _, p := range FallbackPricing() {
		byPattern[p.ModelPattern] = p
	}

	// gpt-5.5 (Hermes). Source: https://developers.openai.com/api/docs/pricing
	// standard tier — input $5.00, cached input $0.50, output $30.00 per MTok.
	gpt, ok := byPattern["gpt-5.5"]
	require.True(t, ok, "gpt-5.5 entry missing from FallbackPricing")
	assert.Equal(t, 5.0, gpt.InputPerMTok)
	assert.Equal(t, 30.0, gpt.OutputPerMTok)
	assert.Equal(t, 0.50, gpt.CacheReadPerMTok)

	// openrouter/owl-alpha is a free model: a known $0 (present with
	// zero rates) rather than an unpriced/unknown model.
	owl, ok := byPattern["openrouter/owl-alpha"]
	require.True(t, ok, "openrouter/owl-alpha entry missing from FallbackPricing")
	assert.Zero(t, owl.InputPerMTok)
	assert.Zero(t, owl.OutputPerMTok)
}

func assertClose(
	t *testing.T,
	name string,
	got, want float64,
) {
	t.Helper()
	assert.LessOrEqual(t, math.Abs(got-want), 0.001, "%s: got %f, want %f", name, got, want)
}
