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

func assertClose(
	t *testing.T,
	name string,
	got, want float64,
) {
	t.Helper()
	assert.LessOrEqual(t, math.Abs(got-want), 0.001, "%s: got %f, want %f", name, got, want)
}
