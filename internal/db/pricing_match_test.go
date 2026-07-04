package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

func TestLookupModelRates_DotDashFallback(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{
		{
			ModelPattern: "claude-opus-4-7",
			Rates: export.ModelRates{
				InputPerMTok: 5, OutputPerMTok: 25,
			},
		},
		{
			ModelPattern: "claude-opus-4.6",
			Rates: export.ModelRates{
				InputPerMTok: 99, OutputPerMTok: 99,
			},
		},
	})

	lookup := resolver.Lookup("claude-opus-4.7")
	require.True(t, lookup.OK, "dotted model should resolve via normalized key")
	assert.Equal(t, 5.0, lookup.Rates.InputPerMTok)
	assert.Equal(t, 25.0, lookup.Rates.OutputPerMTok)

	dashed := resolver.Lookup("claude-opus-4-7")
	require.True(t, dashed.OK, "already-dashed model should resolve exactly")
	assert.Equal(t, 5.0, dashed.Rates.InputPerMTok)

	exact := resolver.Lookup("claude-opus-4.6")
	require.True(t, exact.OK)
	assert.Equal(t, 99.0, exact.Rates.InputPerMTok,
		"exact match must win over normalized fallback")

	unknown := resolver.Lookup("gpt-5.5")
	assert.False(t, unknown.OK, "unknown model stays unpriced")
}

func TestModelRateResolverCachesResolvedModels(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "gemini-3.5-flash",
		Rates: export.ModelRates{
			InputPerMTok: 1.25, OutputPerMTok: 10,
		},
	}})

	first := resolver.Lookup("Gemini 3.5 Flash (High)")
	require.True(t, first.OK)
	assert.Equal(t, 1.25, first.Rates.InputPerMTok)

	second := resolver.Lookup("Gemini 3.5 Flash (High)")
	require.True(t, second.OK)
	assert.Equal(t, first, second)

	unknown := resolver.Lookup("unknown-model")
	assert.False(t, unknown.OK)

	unknown = resolver.Lookup("unknown-model")
	assert.False(t, unknown.OK)
}
