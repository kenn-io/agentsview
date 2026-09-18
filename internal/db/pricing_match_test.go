package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestLookupModelRates_DotDashFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	resolver := export.NewPricingResolver([]export.EffectivePricingRow{
		{
			ModelPattern: "claude-opus-4-7",
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("5"), OutputPerMTok: money.MustParseDollars("25"),
			},
		},
		{
			ModelPattern: "claude-opus-4.6",
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("99"), OutputPerMTok: money.MustParseDollars("99"),
			},
		},
	})

	lookup := resolver.Lookup("claude-opus-4.7")
	require.True(lookup.OK, "dotted model should resolve via normalized key")
	assert.Equal(money.MustParseDollars("5.0"), lookup.Rates.InputPerMTok)
	assert.Equal(money.MustParseDollars("25.0"), lookup.Rates.OutputPerMTok)

	dashed := resolver.Lookup("claude-opus-4-7")
	require.True(dashed.OK, "already-dashed model should resolve exactly")
	assert.Equal(money.MustParseDollars("5.0"), dashed.Rates.InputPerMTok)

	exact := resolver.Lookup("claude-opus-4.6")
	require.True(exact.OK)
	assert.Equal(money.MustParseDollars("99.0"), exact.Rates.InputPerMTok,
		"exact match must win over normalized fallback")

	unknown := resolver.Lookup("gpt-5.5")
	assert.False(unknown.OK, "unknown model stays unpriced")
}

func TestModelRateResolverCachesResolvedModels(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "gemini-3.5-flash",
		Rates: export.ModelRates{
			InputPerMTok: money.MustParseDollars("1.25"), OutputPerMTok: money.MustParseDollars("10"),
		},
	}})

	first := resolver.Lookup("Gemini 3.5 Flash (High)")
	require.True(first.OK)
	assert.Equal(money.MustParseDollars("1.25"), first.Rates.InputPerMTok)

	second := resolver.Lookup("Gemini 3.5 Flash (High)")
	require.True(second.OK)
	assert.Equal(first, second)

	unknown := resolver.Lookup("unknown-model")
	assert.False(unknown.OK)

	unknown = resolver.Lookup("unknown-model")
	assert.False(unknown.OK)
}
