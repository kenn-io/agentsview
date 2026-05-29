package pricing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
