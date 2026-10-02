package pricing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFallbackPricing_GPT6Rates(t *testing.T) {
	prices := make(map[string]ModelPricing)
	for _, p := range FallbackPricing() {
		require.NotContains(t, prices, p.ModelPattern, "fallback patterns must be unique")
		prices[p.ModelPattern] = p
	}
	for _, tt := range []struct {
		model, input, output, write, read, bandInput, bandOutput, bandWrite, bandRead string
	}{
		{"gpt-6-astra", "10", "50", "12.5", "1", "20", "75", "25", "2"},
		{"gpt-6-sol", "2", "10", "2.5", "0.2", "4", "15", "5", "0.4"},
		{"gpt-6.1-sol", "2", "10", "2.5", "0.1", "4", "15", "5", "0.2"},
		{GPT6AstraCanonical, "11", "55", "13.75", "1.1", "22", "82.5", "27.5", "2.2"},
		{"gpt-5.6-sol", "4", "20", "5", "0.4", "8", "30", "10", "0.8"},
	} {
		t.Run(tt.model, func(t *testing.T) {
			p, ok := prices[tt.model]
			require.True(t, ok, "embedded pricing missing %s", tt.model)
			assert.Equal(t, testRate(tt.input), p.InputPerMTok)
			assert.Equal(t, testRate(tt.output), p.OutputPerMTok)
			assert.Equal(t, testRate(tt.write), p.CacheCreationPerMTok)
			assert.Equal(t, testRate(tt.read), p.CacheReadPerMTok)
			require.Len(t, p.Bands, 1)
			assert.Equal(t, PricingBand{
				AboveInputTokens: 272_000,
				InputPerMTok:     testRate(tt.bandInput), OutputPerMTok: testRate(tt.bandOutput),
				CacheCreationPerMTok: testRate(tt.bandWrite), CacheReadPerMTok: testRate(tt.bandRead),
			}, p.Bands[0])
		})
	}
	legacy, ok := prices["codex-mini-latest"]
	require.True(t, ok, "retired model must remain priceable")
	assertFlatPricing(t, ModelPricing{
		ModelPattern: "codex-mini-latest", InputPerMTok: testRate("1.5"),
		OutputPerMTok: testRate("6"), CacheReadPerMTok: testRate("0.375"),
	}, legacy)
}

func TestFallbackPricing_BandsReturnIndependentCopy(t *testing.T) {
	first := FallbackPricing()
	for i := range first {
		if first[i].ModelPattern == GPT6AstraCanonical {
			require.NotEmpty(t, first[i].Bands)
			first[i].Bands[0].AboveInputTokens = 1
		}
	}
	for _, p := range FallbackPricing() {
		if p.ModelPattern == GPT6AstraCanonical {
			require.NotEmpty(t, p.Bands)
			assert.Equal(t, 272_000, p.Bands[0].AboveInputTokens)
			return
		}
	}
	require.FailNow(t, "Astra pricing missing")
}
