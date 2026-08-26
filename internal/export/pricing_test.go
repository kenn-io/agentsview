package export

import (
	"encoding/json/v2"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func TestPricingResolverUsesHistoricalGenAIPricesBeforeFlatFallback(t *testing.T) {
	embedded := pricingpkg.EmbeddedGenAIDocument()
	genAI := EffectivePricingRow{
		GenAI: embedded.Prices, GenAIVersion: embedded.Version,
		GenAISource: PricingRowSourceEmbedded,
	}
	flat := EffectivePricingRow{
		ModelPattern: "gpt-5.6-luna",
		Rates: ModelRates{
			InputPerMTok: money.MustParseDollars("9"),
			Source:       PricingRowSourceFetched,
		},
	}
	resolver := NewPricingResolver([]EffectivePricingRow{flat, genAI})

	beforeModel, before := resolver.ResolveAt(
		"gpt-5.6-luna", "gpt-5.6-luna",
		time.Date(2026, 7, 29, 23, 59, 59, 0, time.UTC),
	)
	afterModel, after := resolver.ResolveAt(
		"gpt-5.6-luna", "gpt-5.6-luna",
		time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
	)

	require.True(t, before.OK)
	require.True(t, after.OK)
	assert.Equal(t, "gpt-5.6-luna", beforeModel)
	assert.Equal(t, "gpt-5.6-luna", afterModel)
	assert.Equal(t, money.MustParseDollars("1"), before.Rates.InputPerMTok)
	assert.Equal(t, money.MustParseDollars("6"), before.Rates.OutputPerMTok)
	assert.Equal(t, money.MustParseDollars("0.2"), after.Rates.InputPerMTok)
	assert.Equal(t, money.MustParseDollars("1.2"), after.Rates.OutputPerMTok)

	_, withoutTimestamp := resolver.Resolve("gpt-5.6-luna", "gpt-5.6-luna")
	require.True(t, withoutTimestamp.OK)
	assert.Equal(t, money.MustParseDollars("9"), withoutTimestamp.Rates.InputPerMTok,
		"usage without an event timestamp falls back to the flat catalog")

	resolver.RecordResolvedComputed("gpt-5.6-luna", beforeModel, before)
	resolver.RecordResolvedComputed("gpt-5.6-luna", afterModel, after)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	resolutions := block.Models["gpt-5.6-luna"].Resolutions
	require.Len(t, resolutions, 2)
	assert.ElementsMatch(t, []money.Money{
		money.MustParseDollars("1"),
		money.MustParseDollars("0.2"),
	}, []money.Money{
		resolutions[0].InputCostPerMTok,
		resolutions[1].InputCostPerMTok,
	})

	custom := EffectivePricingRow{
		ModelPattern: "gpt-5.6-luna",
		Rates: ModelRates{
			InputPerMTok: money.MustParseDollars("7"),
			Source:       PricingRowSourceCustom,
		},
	}
	_, customLookup := NewPricingResolver(
		[]EffectivePricingRow{flat, genAI, custom},
	).ResolveAt(
		"gpt-5.6-luna", "gpt-5.6-luna",
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	)
	require.True(t, customLookup.OK)
	assert.Equal(t, money.MustParseDollars("7"), customLookup.Rates.InputPerMTok)
	assert.Equal(t, PricingRowSourceCustom, customLookup.Rates.Source)
}

func TestPricingResolverBuildBlockUsesRecordedLookup(t *testing.T) {
	updatedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	resolver := NewPricingResolver([]EffectivePricingRow{{
		ModelPattern: "claude-test",
		Rates: ModelRates{
			InputPerMTok:      money.MustParseDollars("3"),
			OutputPerMTok:     money.MustParseDollars("15"),
			CacheWritePerMTok: money.MustParseDollars("3.75"),
			CacheReadPerMTok:  money.MustParseDollars("0.30"),
			UpdatedAt:         &updatedAt,
			Source:            PricingRowSourceFetched,
		},
	}})

	lookup := resolver.Lookup("claude-test-20260703")
	require.True(t, lookup.OK)
	require.Equal(t, "claude-test", lookup.Pattern)
	cost, err := lookup.Rates.CostForTokens(
		1_000_000, 2_000_000, 500_000, 3_000_000, 4_000_000)
	require.NoError(t, err)

	resolver.RecordComputed("claude-test-20260703", lookup)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)

	require.Contains(t, block.Models, "claude-test-20260703")
	model := onlyPricingResolution(
		t, block.Models["claude-test-20260703"])
	require.NotNil(t, model.MatchedPattern)
	assert.Equal(t, lookup.Pattern, *model.MatchedPattern)
	assert.Equal(t, lookup.Rates.InputPerMTok, model.InputCostPerMTok)
	assert.Equal(t, lookup.Rates.OutputPerMTok, model.OutputCostPerMTok)
	assert.Equal(t, lookup.Rates.CacheWritePerMTok, model.CacheWriteCostPerMTok)
	assert.Equal(t, lookup.Rates.CacheReadPerMTok, model.CacheReadCostPerMTok)
	assert.Equal(t, money.MustParseDollars("45.45"), cost)
}

func TestPricingResolverResolvePrefersExactCustomReportedModel(t *testing.T) {
	resolver := NewPricingResolver([]EffectivePricingRow{
		{
			ModelPattern: "kimi-for-coding",
			Rates: ModelRates{
				InputPerMTok: money.MustParseDollars("7"),
				Source:       PricingRowSourceCustom,
			},
		},
		{
			ModelPattern: "moonshot/kimi-k3",
			Rates: ModelRates{
				InputPerMTok: money.MustParseDollars("2"),
				Source:       PricingRowSourceFetched,
			},
		},
	})

	pricedModel, lookup := resolver.Resolve(
		"kimi-for-coding", "moonshot/kimi-k3")

	assert.Equal(t, "kimi-for-coding", pricedModel)
	require.True(t, lookup.OK)
	assert.Equal(t, "kimi-for-coding", lookup.Pattern)
	assert.Equal(t, money.MustParseDollars("7"), lookup.Rates.InputPerMTok)
}

func TestPricingResolverResolveUsesCanonicalWithoutExactCustom(t *testing.T) {
	resolver := NewPricingResolver([]EffectivePricingRow{
		{
			ModelPattern: "kimi-for-coding",
			Rates: ModelRates{
				InputPerMTok: money.MustParseDollars("9"),
				Source:       PricingRowSourceFetched,
			},
		},
		{
			ModelPattern: "moonshot/kimi-k3",
			Rates: ModelRates{
				InputPerMTok: money.MustParseDollars("2"),
				Source:       PricingRowSourceFetched,
			},
		},
	})

	pricedModel, lookup := resolver.Resolve(
		"kimi-for-coding", "moonshot/kimi-k3")

	assert.Equal(t, "moonshot/kimi-k3", pricedModel)
	require.True(t, lookup.OK)
	assert.Equal(t, "moonshot/kimi-k3", lookup.Pattern)
	assert.Equal(t, money.MustParseDollars("2"), lookup.Rates.InputPerMTok)
}

func TestPricingResolverResolveAtPrefersNormalizedCanonicalCustomOverGenAI(
	t *testing.T,
) {
	embedded := pricingpkg.EmbeddedGenAIDocument()
	resolver := NewPricingResolver([]EffectivePricingRow{
		{
			ModelPattern: "gpt-5-6-luna",
			Rates: ModelRates{
				InputPerMTok: money.MustParseDollars("7"),
				Source:       PricingRowSourceCustom,
			},
		},
		{
			GenAI: embedded.Prices, GenAIVersion: embedded.Version,
			GenAISource: PricingRowSourceEmbedded,
		},
	})

	pricedModel, lookup := resolver.ResolveAt(
		"luna-runtime-alias", "gpt-5.6-luna",
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	)

	assert.Equal(t, "gpt-5.6-luna", pricedModel)
	require.True(t, lookup.OK)
	assert.Equal(t, "gpt-5-6-luna", lookup.Pattern)
	assert.Equal(t, money.MustParseDollars("7"), lookup.Rates.InputPerMTok)
	assert.Equal(t, PricingRowSourceCustom, lookup.Rates.Source)
}

func TestPricingResolverBuildBlockKeepsReportedModelResolutions(t *testing.T) {
	resolver := NewPricingResolver([]EffectivePricingRow{
		{
			ModelPattern: "moonshot/kimi-k2.6",
			Rates: ModelRates{
				InputPerMTok: money.MustParseDollars("1"),
				Source:       PricingRowSourceFetched,
			},
		},
		{
			ModelPattern: "moonshot/kimi-k3",
			Rates: ModelRates{
				InputPerMTok: money.MustParseDollars("2"),
				Source:       PricingRowSourceFetched,
			},
		},
	})

	k26 := resolver.Lookup("moonshot/kimi-k2.6")
	k3 := resolver.Lookup("moonshot/kimi-k3")
	require.True(t, k26.OK)
	require.True(t, k3.OK)
	resolver.RecordResolvedComputed(
		"kimi-for-coding", "moonshot/kimi-k3", k3)
	resolver.RecordResolvedReported(
		"kimi-for-coding", "moonshot/kimi-k2.6", k26)

	block, err := resolver.BuildBlock()
	require.NoError(t, err)

	require.Contains(t, block.Models, "kimi-for-coding")
	provenance := block.Models["kimi-for-coding"]
	assert.Equal(t, CostSourceMixed, provenance.CostSource)
	require.Len(t, provenance.Resolutions, 2)
	assert.Equal(t, "moonshot/kimi-k2.6",
		provenance.Resolutions[0].PricedModel)
	assert.Equal(t, CostSourceReported,
		provenance.Resolutions[0].CostSource)
	assert.Equal(t, money.MustParseDollars("1"),
		provenance.Resolutions[0].InputCostPerMTok)
	assert.Equal(t, "moonshot/kimi-k3",
		provenance.Resolutions[1].PricedModel)
	assert.Equal(t, CostSourceComputed,
		provenance.Resolutions[1].CostSource)
	assert.Equal(t, money.MustParseDollars("2"),
		provenance.Resolutions[1].InputCostPerMTok)
	assert.NotContains(t, block.Models, "moonshot/kimi-k2.6")
	assert.NotContains(t, block.Models, "moonshot/kimi-k3")
}

func TestModelRatesCostForTokensTreatsReasoningAsOutputBreakdown(t *testing.T) {
	rates := ModelRates{
		InputPerMTok:  money.MustParseDollars("1"),
		OutputPerMTok: money.MustParseDollars("10"),
	}

	cost, err := rates.CostForTokens(1_000_000, 2_000_000, 500_000, 0, 0)
	require.NoError(t, err)

	assert.Equal(t, money.MustParseDollars("21"), cost)
}

func TestModelRatesCostForTokensBillsReasoningOnlyRowsAsOutput(t *testing.T) {
	rates := ModelRates{
		OutputPerMTok: money.MustParseDollars("10"),
	}

	cost, err := rates.CostForTokens(0, 0, 500_000, 0, 0)
	require.NoError(t, err)

	assert.Equal(t, money.MustParseDollars("5"), cost)
}

func TestModelRatesCostForTokensReturnsOverflow(t *testing.T) {
	rates := ModelRates{
		InputPerMTok: money.Money{Microdollars: math.MaxInt64},
	}

	_, err := rates.CostForTokens(2_000_000, 0, 0, 0, 0)

	require.ErrorIs(t, err, money.ErrOverflow)
}

func TestModelRatesCostForTokensPricingBandBoundary(t *testing.T) {
	rates := ModelRates{
		InputPerMTok:      money.MustParseDollars("1"),
		OutputPerMTok:     money.MustParseDollars("2"),
		CacheWritePerMTok: money.MustParseDollars("0.50"),
		CacheReadPerMTok:  money.MustParseDollars("0.10"),
		Bands: []PricingBand{{
			AboveInputTokens:  200_000,
			InputPerMTok:      money.MustParseDollars("2"),
			OutputPerMTok:     money.MustParseDollars("3"),
			CacheWritePerMTok: money.MustParseDollars("1"),
			CacheReadPerMTok:  money.MustParseDollars("0.20"),
		}},
	}

	atBoundary, err := rates.CostForTokens(100_000, 10_000, 0, 50_000, 50_000)
	require.NoError(t, err)
	aboveBoundary, err := rates.CostForTokens(100_001, 10_000, 0, 50_000, 50_000)
	require.NoError(t, err)

	assert.Equal(t, money.Money{Microdollars: 150_000}, atBoundary)
	assert.Equal(t, money.Money{Microdollars: 290_002}, aboveBoundary)
}

func TestModelRatesRatesForTokensUsesHighestPricingBand(t *testing.T) {
	rates := ModelRates{
		InputPerMTok: money.MustParseDollars("1"),
		Bands: []PricingBand{
			{AboveInputTokens: 200_000, InputPerMTok: money.MustParseDollars("2")},
			{AboveInputTokens: 272_000, InputPerMTok: money.MustParseDollars("4")},
		},
	}

	selected := rates.RatesForTokens(272_001, 0, 0)

	assert.Equal(t, money.MustParseDollars("4"), selected.InputPerMTok)
}

func TestModelRatesPricesRequestsBeforeAggregation(t *testing.T) {
	rates := ModelRates{
		InputPerMTok: money.MustParseDollars("1"),
		Bands: []PricingBand{{
			AboveInputTokens: 200_000,
			InputPerMTok:     money.MustParseDollars("2"),
		}},
	}

	first, err := rates.CostForTokens(150_000, 0, 0, 0, 0)
	require.NoError(t, err)
	second, err := rates.CostForTokens(150_000, 0, 0, 0, 0)
	require.NoError(t, err)

	assert.Equal(t, money.Money{Microdollars: 300_000}, money.MustAdd(first, second))
}

func TestModelRatesCostForTokensScopedAggregateUsesBaseRate(t *testing.T) {
	rates := ModelRates{
		InputPerMTok: money.MustParseDollars("1"),
		Bands: []PricingBand{{
			AboveInputTokens: 200_000,
			InputPerMTok:     money.MustParseDollars("2"),
		}},
	}

	cost, err := rates.CostForTokensScoped(false, 300_000, 0, 0, 0, 0)
	require.NoError(t, err)

	assert.Equal(t, money.Money{Microdollars: 300_000}, cost)
}

func TestPricingResolverBuildBlockPricingBandsAndApplicationCounts(t *testing.T) {
	baseUpdatedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	bandUpdatedAt := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	resolver := NewPricingResolver([]EffectivePricingRow{{
		ModelPattern: "banded-model",
		Rates: ModelRates{
			InputPerMTok: money.MustParseDollars("1"),
			UpdatedAt:    &baseUpdatedAt,
			Source:       PricingRowSourceFetched,
			Bands: []PricingBand{{
				AboveInputTokens: 200_000,
				InputPerMTok:     money.MustParseDollars("2"),
				UpdatedAt:        &bandUpdatedAt,
			}},
		},
	}})
	lookup := resolver.Lookup("banded-model")
	require.True(t, lookup.OK)

	resolver.RecordComputedRequest("banded-model", lookup, 150_000, 0, 0)
	resolver.RecordComputedRequest("banded-model", lookup, 200_001, 0, 0)
	resolver.RecordComputedAggregate("banded-model", lookup)
	resolver.RecordReported("banded-model", lookup)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)

	require.NotNil(t, block.LatestRowUpdatedAt)
	assert.Equal(t, bandUpdatedAt, *block.LatestRowUpdatedAt)
	model := onlyPricingResolution(t, block.Models["banded-model"])
	assert.Equal(t, []PricingBand{{
		AboveInputTokens: 200_000,
		InputPerMTok:     money.MustParseDollars("2"),
		UpdatedAt:        &bandUpdatedAt,
	}}, model.Bands)
	assert.Equal(t, PricingApplication{
		BaseRequestCount:  1,
		AggregateRowCount: 1,
		Bands: []AppliedPricingBand{{
			AboveInputTokens: 200_000,
			RequestCount:     1,
		}},
	}, model.Application)
}

func TestPricingResolverReportedOnlyRowDoesNotCountPricingApplication(t *testing.T) {
	resolver := NewPricingResolver([]EffectivePricingRow{{
		ModelPattern: "reported-model",
		Rates: ModelRates{
			InputPerMTok: money.MustParseDollars("1"),
			Bands: []PricingBand{{
				AboveInputTokens: 200_000,
				InputPerMTok:     money.MustParseDollars("2"),
			}},
		},
	}})
	lookup := resolver.Lookup("reported-model")
	resolver.RecordReported("reported-model", lookup)

	block, err := resolver.BuildBlock()
	require.NoError(t, err)

	model := onlyPricingResolution(t, block.Models["reported-model"])
	assert.Equal(t, PricingApplication{}, model.Application)
}

func TestPricingResolverUnresolvedRequestPreservesComputedProvenanceWithoutApplication(t *testing.T) {
	resolver := NewPricingResolver(nil)
	lookup := resolver.Lookup("unpriced-request-model")
	require.False(t, lookup.OK)

	resolver.RecordComputedRequest("unpriced-request-model", lookup, 150_000, 0, 0)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)

	model := onlyPricingResolution(t, block.Models["unpriced-request-model"])
	assert.Equal(t, CostSourceComputed, model.CostSource)
	assert.Nil(t, model.MatchedPattern)
	assert.Equal(t, PricingApplication{}, model.Application)
}

func TestPricingResolverUnresolvedAggregatePreservesComputedProvenanceWithoutApplication(t *testing.T) {
	resolver := NewPricingResolver(nil)
	lookup := resolver.Lookup("unpriced-aggregate-model")
	require.False(t, lookup.OK)

	resolver.RecordComputedAggregate("unpriced-aggregate-model", lookup)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)

	model := onlyPricingResolution(t, block.Models["unpriced-aggregate-model"])
	assert.Equal(t, CostSourceComputed, model.CostSource)
	assert.Nil(t, model.MatchedPattern)
	assert.Equal(t, PricingApplication{}, model.Application)
}

func TestPricingResolverBuildBlockModelsAndFallback(t *testing.T) {
	resolver := NewPricingResolver([]EffectivePricingRow{
		{
			ModelPattern: "claude-test",
			Rates: ModelRates{
				InputPerMTok: money.MustParseDollars("3"), OutputPerMTok: money.MustParseDollars("15"),
				CacheWritePerMTok: money.MustParseDollars("3.75"), CacheReadPerMTok: money.MustParseDollars("0.30"),
				Source: PricingRowSourceEmbedded,
			},
		},
		{
			ModelPattern: "unused-model",
			Rates: ModelRates{
				InputPerMTok: money.MustParseDollars("100"), OutputPerMTok: money.MustParseDollars("200"),
				Source: PricingRowSourceCustom,
			},
		},
	})

	claudeLookup := resolver.Lookup("claude-test")
	require.True(t, claudeLookup.OK)
	resolver.RecordComputed("claude-test", claudeLookup)
	unknownLookup := resolver.Lookup("unpriced-model")
	require.False(t, unknownLookup.OK)
	resolver.RecordComputed("unpriced-model", unknownLookup)

	block, err := resolver.BuildBlock()
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"claude-test", "unpriced-model"}, mapKeys(block.Models))
	assert.True(t, block.Fallback.Used)
	assert.Equal(t, []string{"claude-test"}, block.Fallback.Models)
	assert.NotContains(t, block.Fallback.Models, "unpriced-model")
	assert.NotContains(t, block.Models, "unused-model")

	unpriced := onlyPricingResolution(
		t, block.Models["unpriced-model"])
	assert.Nil(t, unpriced.MatchedPattern)
	assert.Zero(t, unpriced.InputCostPerMTok)
	assert.Zero(t, unpriced.OutputCostPerMTok)
	assert.Zero(t, unpriced.CacheWriteCostPerMTok)
	assert.Zero(t, unpriced.CacheReadCostPerMTok)
}

func TestPricingResolverReportedCostWithoutMatchingRateIsExplicit(t *testing.T) {
	resolver := NewPricingResolver(nil)
	lookup := resolver.Lookup("provider-opaque-model")
	require.False(t, lookup.OK)
	resolver.RecordReported("provider-opaque-model", lookup)

	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	require.Contains(t, block.Models, "provider-opaque-model")
	provenance := block.Models["provider-opaque-model"]
	assert.Equal(t, CostSourceReported, provenance.CostSource)
	model := onlyPricingResolution(t, provenance)
	assert.Equal(t, CostSourceReported, model.CostSource)
	assert.Nil(t, model.MatchedPattern)
	assert.Zero(t, model.InputCostPerMTok)
	assert.Zero(t, model.OutputCostPerMTok)
	assert.Zero(t, model.CacheWriteCostPerMTok)
	assert.Zero(t, model.CacheReadCostPerMTok)
}

func TestPricingResolverCostSource(t *testing.T) {
	tests := []struct {
		name string
		acts func(*PricingResolver, PricingLookup)
		want CostSource
	}{
		{
			name: "computed",
			acts: func(r *PricingResolver, l PricingLookup) {
				r.RecordComputed("claude-test", l)
			},
			want: CostSourceComputed,
		},
		{
			name: "reported",
			acts: func(r *PricingResolver, l PricingLookup) {
				r.RecordReported("claude-test", l)
			},
			want: CostSourceReported,
		},
		{
			name: "mixed",
			acts: func(r *PricingResolver, l PricingLookup) {
				r.RecordComputed("claude-test", l)
				r.RecordReported("claude-test", l)
			},
			want: CostSourceMixed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := NewPricingResolver([]EffectivePricingRow{{
				ModelPattern: "claude-test",
				Rates: ModelRates{
					InputPerMTok: money.MustParseDollars("3"), OutputPerMTok: money.MustParseDollars("15"),
					Source: PricingRowSourceCustom,
				},
			}})
			lookup := resolver.Lookup("claude-test")
			require.True(t, lookup.OK)

			tt.acts(resolver, lookup)
			block, err := resolver.BuildBlock()
			require.NoError(t, err)

			assert.Equal(t, tt.want, block.CostSource)
			assert.Equal(t, tt.want, block.Models["claude-test"].CostSource)
		})
	}
}

func TestPricingResolverCostSourceDefaultsComputedWithoutModels(t *testing.T) {
	resolver := NewPricingResolver([]EffectivePricingRow{{
		ModelPattern: "claude-test",
		Rates: ModelRates{
			InputPerMTok: money.MustParseDollars("3"), OutputPerMTok: money.MustParseDollars("15"),
			Source: PricingRowSourceCustom,
		},
	}})

	block, err := resolver.BuildBlock()
	require.NoError(t, err)

	assert.Equal(t, CostSourceComputed, block.CostSource)
	assert.Empty(t, block.Models)
}

func TestAllocateCostByWeightReconcilesToReportedTotal(t *testing.T) {
	total := money.Money{Microdollars: 30_000}
	allocated := AllocateCostByWeight(total, []money.Money{
		{Microdollars: 10},
		{Microdollars: 20},
	})

	require.Len(t, allocated, 2)
	assert.Equal(t, money.Money{Microdollars: 10_000}, allocated[0])
	assert.Equal(t, money.Money{Microdollars: 20_000}, allocated[1])
	assert.Equal(t, total, money.MustAdd(allocated[0], allocated[1]))
}

func TestPricingResolverLookupCachesByReportedModel(t *testing.T) {
	resolver := NewPricingResolver([]EffectivePricingRow{{
		ModelPattern: "claude-test",
		Rates: ModelRates{
			InputPerMTok: money.MustParseDollars("3"), OutputPerMTok: money.MustParseDollars("15"),
			Source: PricingRowSourceCustom,
		},
	}})

	first := resolver.Lookup("claude-test-20260703")
	require.True(t, first.OK)
	require.Equal(t, "claude-test", first.Pattern)
	require.Len(t, resolver.lookupCache, 1)

	second := resolver.Lookup("claude-test-20260703")

	assert.Equal(t, first, second)
	assert.Len(t, resolver.lookupCache, 1)
}

func TestPricingResolverDeepClonesPricingBands(t *testing.T) {
	rows := []EffectivePricingRow{{
		ModelPattern: "banded-model",
		Rates: ModelRates{Bands: []PricingBand{{
			AboveInputTokens: 200_000,
			InputPerMTok:     money.MustParseDollars("2"),
		}}},
	}}
	resolver := NewPricingResolver(rows)
	rows[0].Rates.Bands[0].InputPerMTok = money.MustParseDollars("99")

	lookup := resolver.Lookup("banded-model")
	require.True(t, lookup.OK)
	lookup.Rates.Bands[0].InputPerMTok = money.MustParseDollars("88")
	second := resolver.Lookup("banded-model")

	assert.Equal(t, money.MustParseDollars("2"), second.Rates.Bands[0].InputPerMTok)
}

func TestPricingResolverSourceCanonicalOrder(t *testing.T) {
	tests := []struct {
		name string
		rows []EffectivePricingRow
		want string
	}{
		{
			name: "custom fetched",
			rows: []EffectivePricingRow{
				rowWithSource("custom", PricingRowSourceCustom),
				rowWithSource("fetched", PricingRowSourceFetched),
			},
			want: "custom+fetched",
		},
		{
			name: "custom embedded",
			rows: []EffectivePricingRow{
				rowWithSource("embedded", PricingRowSourceEmbedded),
				rowWithSource("custom", PricingRowSourceCustom),
			},
			want: "custom+embedded",
		},
		{
			name: "custom",
			rows: []EffectivePricingRow{rowWithSource("custom", PricingRowSourceCustom)},
			want: "custom",
		},
		{
			name: "fetched",
			rows: []EffectivePricingRow{rowWithSource("fetched", PricingRowSourceFetched)},
			want: "fetched",
		},
		{
			name: "fetched wins base source over embedded",
			rows: []EffectivePricingRow{
				rowWithSource("embedded", PricingRowSourceEmbedded),
				rowWithSource("fetched", PricingRowSourceFetched),
			},
			want: "fetched",
		},
		{
			name: "embedded",
			rows: []EffectivePricingRow{rowWithSource("embedded", PricingRowSourceEmbedded)},
			want: "embedded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block, err := NewPricingResolver(tt.rows).BuildBlock()
			require.NoError(t, err)
			assert.Equal(t, tt.want, block.Source)
		})
	}
}

func TestPricingResolverTableVersionFollowsBaseSource(t *testing.T) {
	updatedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		rows []EffectivePricingRow
		want string
	}{
		{
			name: "fetched uses latest row timestamp",
			rows: []EffectivePricingRow{{
				ModelPattern: "fetched",
				Rates: ModelRates{
					InputPerMTok: money.MustParseDollars("1"), Source: PricingRowSourceFetched,
					UpdatedAt: &updatedAt,
				},
			}},
			want: "2026-07-03T12:00:00Z",
		},
		{
			name: "custom fetched uses fetched timestamp",
			rows: []EffectivePricingRow{
				{
					ModelPattern: "custom",
					Rates: ModelRates{
						InputPerMTok: money.MustParseDollars("1"), Source: PricingRowSourceCustom,
					},
				},
				{
					ModelPattern: "fetched",
					Rates: ModelRates{
						InputPerMTok: money.MustParseDollars("1"), Source: PricingRowSourceFetched,
						UpdatedAt: &updatedAt,
					},
				},
			},
			want: "2026-07-03T12:00:00Z",
		},
		{
			name: "custom only",
			rows: []EffectivePricingRow{{
				ModelPattern: "custom",
				Rates: ModelRates{
					InputPerMTok: money.MustParseDollars("1"), Source: PricingRowSourceCustom,
				},
			}},
			want: "custom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block, err := NewPricingResolver(tt.rows).BuildBlock()
			require.NoError(t, err)
			assert.Equal(t, tt.want, block.TableVersion)
		})
	}
}

func TestPricingResolverJSONNesting(t *testing.T) {
	resolver := NewPricingResolver([]EffectivePricingRow{{
		ModelPattern: "claude-test",
		Rates: ModelRates{
			InputPerMTok: money.MustParseDollars("3"), OutputPerMTok: money.MustParseDollars("15"),
			Source: PricingRowSourceCustom,
		},
	}})
	lookup := resolver.Lookup("claude-test")
	require.True(t, lookup.OK)
	resolver.RecordComputed("claude-test", lookup)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)

	got, err := json.Marshal(struct {
		Pricing PricingBlock `json:"pricing"`
	}{Pricing: block})
	require.NoError(t, err)

	assert.Contains(t, string(got), `"pricing":{"source":`)
	assert.Contains(t, string(got), `"models":{"claude-test":`)
	assert.NotContains(t, string(got), `"effective_model_rates"`)
}

func rowWithSource(pattern string, source PricingRowSource) EffectivePricingRow {
	return EffectivePricingRow{
		ModelPattern: pattern,
		Rates: ModelRates{
			InputPerMTok: money.MustParseDollars("1"), OutputPerMTok: money.MustParseDollars("2"),
			Source: source,
		},
	}
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func onlyPricingResolution(
	t *testing.T, provenance ModelPricingProvenance,
) EffectiveModelRate {
	t.Helper()
	require.Len(t, provenance.Resolutions, 1)
	return provenance.Resolutions[0]
}
