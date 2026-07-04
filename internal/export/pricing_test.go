package export

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPricingResolverBuildBlockUsesRecordedLookup(t *testing.T) {
	updatedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	resolver := NewPricingResolver([]EffectivePricingRow{{
		ModelPattern: "claude-test",
		Rates: ModelRates{
			InputPerMTok:      3,
			OutputPerMTok:     15,
			CacheWritePerMTok: 3.75,
			CacheReadPerMTok:  0.30,
			UpdatedAt:         &updatedAt,
			Source:            PricingRowSourceFetched,
		},
	}})

	lookup := resolver.Lookup("claude-test-20260703")
	require.True(t, lookup.OK)
	require.Equal(t, "claude-test", lookup.Pattern)
	cost := lookup.Rates.CostForTokens(1_000_000, 2_000_000, 500_000, 3_000_000, 4_000_000)

	resolver.RecordComputed("claude-test-20260703", lookup)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)

	require.Contains(t, block.Models, "claude-test-20260703")
	model := block.Models["claude-test-20260703"]
	require.NotNil(t, model.MatchedPattern)
	assert.Equal(t, lookup.Pattern, *model.MatchedPattern)
	assert.Equal(t, lookup.Rates.InputPerMTok, model.InputCostPerMTok)
	assert.Equal(t, lookup.Rates.OutputPerMTok, model.OutputCostPerMTok)
	assert.Equal(t, lookup.Rates.CacheWritePerMTok, model.CacheWriteCostPerMTok)
	assert.Equal(t, lookup.Rates.CacheReadPerMTok, model.CacheReadCostPerMTok)
	assert.Equal(t, 45.45, cost)
}

func TestModelRatesCostForTokensTreatsReasoningAsOutputBreakdown(t *testing.T) {
	rates := ModelRates{
		InputPerMTok:  1,
		OutputPerMTok: 10,
	}

	cost := rates.CostForTokens(1_000_000, 2_000_000, 500_000, 0, 0)

	assert.Equal(t, 21.0, cost)
}

func TestModelRatesCostForTokensBillsReasoningOnlyRowsAsOutput(t *testing.T) {
	rates := ModelRates{
		OutputPerMTok: 10,
	}

	cost := rates.CostForTokens(0, 0, 500_000, 0, 0)

	assert.Equal(t, 5.0, cost)
}

func TestPricingResolverBuildBlockModelsAndFallback(t *testing.T) {
	resolver := NewPricingResolver([]EffectivePricingRow{
		{
			ModelPattern: "claude-test",
			Rates: ModelRates{
				InputPerMTok: 3, OutputPerMTok: 15,
				CacheWritePerMTok: 3.75, CacheReadPerMTok: 0.30,
				Source: PricingRowSourceEmbedded,
			},
		},
		{
			ModelPattern: "unused-model",
			Rates: ModelRates{
				InputPerMTok: 100, OutputPerMTok: 200,
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

	unpriced := block.Models["unpriced-model"]
	assert.Nil(t, unpriced.MatchedPattern)
	assert.Zero(t, unpriced.InputCostPerMTok)
	assert.Zero(t, unpriced.OutputCostPerMTok)
	assert.Zero(t, unpriced.CacheWriteCostPerMTok)
	assert.Zero(t, unpriced.CacheReadCostPerMTok)
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
					InputPerMTok: 3, OutputPerMTok: 15,
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
			InputPerMTok: 3, OutputPerMTok: 15,
			Source: PricingRowSourceCustom,
		},
	}})

	block, err := resolver.BuildBlock()
	require.NoError(t, err)

	assert.Equal(t, CostSourceComputed, block.CostSource)
	assert.Empty(t, block.Models)
}

func TestPricingResolverLookupCachesByReportedModel(t *testing.T) {
	resolver := NewPricingResolver([]EffectivePricingRow{{
		ModelPattern: "claude-test",
		Rates: ModelRates{
			InputPerMTok: 3, OutputPerMTok: 15,
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
					InputPerMTok: 1, Source: PricingRowSourceFetched,
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
						InputPerMTok: 1, Source: PricingRowSourceCustom,
					},
				},
				{
					ModelPattern: "fetched",
					Rates: ModelRates{
						InputPerMTok: 1, Source: PricingRowSourceFetched,
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
					InputPerMTok: 1, Source: PricingRowSourceCustom,
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
			InputPerMTok: 3, OutputPerMTok: 15,
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
			InputPerMTok: 1, OutputPerMTok: 2,
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
