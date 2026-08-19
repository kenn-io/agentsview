//go:build fts5

package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/usagefacts"
)

func TestUsageRollupExceptionClassification(t *testing.T) {
	ordinary := rollupSnapshotFact(1, 0, "session-a", "2026-08-01", "model-a", 10)
	ordinary.Fact.ClaudeMessageID = ""
	ordinary.Fact.ClaudeRequestID = ""
	assert.False(t, isUsageRollupException(ordinary))
	assert.True(t, isUsageRollupException(
		rollupSnapshotFact(1, 1, "session-a", "2026-08-01", "model-a", 20)))
	assert.True(t, isUsageRollupException(
		rollupGeneralFact(1, 2, "session-a", "2026-08-01", "model-a", "shared", 30)))
}

func TestPriceUsageFactRoundsEachSurvivor(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "model-a",
		Rates: export.ModelRates{
			InputPerMTok: money.Money{Microdollars: 1},
		},
	}})
	input := usagePriceInput{ReportedModel: "model-a", Fact: usagefacts.Fact{
		Model: "model-a", InputTokens: 500_000, RequestScoped: true,
	}}

	first, err := priceUsageFact(input, resolver)
	require.NoError(t, err)
	second, err := priceUsageFact(input, resolver)
	require.NoError(t, err)

	assert.Equal(t, int64(1), first.Cost.Microdollars)
	assert.Equal(t, int64(2), first.Cost.Microdollars+second.Cost.Microdollars)
}

func TestPriceUsageFactSelectsRequestBand(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "model-a",
		Rates: export.ModelRates{
			InputPerMTok: money.Money{Microdollars: 1_000_000},
			Bands: []export.PricingBand{{
				AboveInputTokens: 100,
				InputPerMTok:     money.Money{Microdollars: 2_000_000},
			}},
		},
	}})

	got, err := priceUsageFact(usagePriceInput{
		ReportedModel: "model-a",
		Fact: usagefacts.Fact{
			Model: "model-a", InputTokens: 101, RequestScoped: true,
		},
	}, resolver)
	require.NoError(t, err)

	require.NotNil(t, got.BandThreshold)
	assert.Equal(t, 100, *got.BandThreshold)
	assert.Equal(t, int64(202), got.Cost.Microdollars)
	assert.Equal(t, 1, got.ComputedRequest)
	assert.Equal(t, 0, got.BaseRequest)
}

func TestPriceUsageFactPreservesReportedAndAuthoritativeCosts(t *testing.T) {
	resolver := export.NewPricingResolver(nil)
	reported := int64(77)

	ordinary, err := priceUsageFact(usagePriceInput{
		ReportedModel: "model-a",
		Fact: usagefacts.Fact{
			Model: "model-a", ReportedCostMicrodollars: &reported,
			CostSource: "provider-reported",
		},
	}, resolver)
	require.NoError(t, err)
	assert.Equal(t, int64(77), ordinary.Cost.Microdollars)
	assert.Nil(t, ordinary.AuthoritativeCost)
	assert.Equal(t, 1, ordinary.Reported)

	authoritative, err := priceUsageFact(usagePriceInput{
		ReportedModel: "model-a",
		Fact: usagefacts.Fact{
			Model: "model-a", ReportedCostMicrodollars: &reported,
			CostSource: CopilotReportedCostSource,
		},
	}, resolver)
	require.NoError(t, err)
	require.NotNil(t, authoritative.AuthoritativeCost)
	assert.Equal(t, int64(77), authoritative.AuthoritativeCost.Microdollars)
	assert.Equal(t, 1, authoritative.ComputedAggregate)
}

func TestPriceUsageFactPricesReasoningAsOutputWhenOutputIsZero(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "model-a",
		Rates: export.ModelRates{
			OutputPerMTok: money.Money{Microdollars: 1_000_000},
		},
	}})

	got, err := priceUsageFact(usagePriceInput{
		ReportedModel: "model-a",
		Fact: usagefacts.Fact{
			Model: "model-a", ReasoningTokens: 7, RequestScoped: true,
		},
	}, resolver)
	require.NoError(t, err)
	assert.Equal(t, int64(7), got.Cost.Microdollars)
}

func TestPriceUsageFactComputesCacheSavingsAndWebSearchFee(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "model-a",
		Rates: export.ModelRates{
			InputPerMTok:     money.Money{Microdollars: 2_000_000},
			CacheReadPerMTok: money.Money{Microdollars: 500_000},
		},
	}})

	got, err := priceUsageFact(usagePriceInput{
		ReportedModel: "model-a",
		Fact: usagefacts.Fact{
			Model: "model-a", CacheReadTokens: 1_000_000,
			WebSearchRequests: 2, RequestScoped: true,
		},
	}, resolver)
	require.NoError(t, err)
	assert.Equal(t, int64(500_000)+2*export.WebSearchRequestMicrodollars,
		got.Cost.Microdollars)
	assert.Equal(t, int64(1_500_000), got.Savings.Microdollars)
}

func TestBuildUsageDailyContributionsDefersSnapshotsToExceptionQuery(t *testing.T) {
	first := rollupSnapshotFact(1, 0, "session-a", "2026-08-01", "model-a", 10)
	first.Fact.WebSearchRequests = 3
	second := rollupSnapshotFact(2, 0, "session-b", "2026-08-01", "model-a", 20)
	second.Fact.WebSearchRequests = 1
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "model-a",
		Rates: export.ModelRates{
			OutputPerMTok: money.Money{Microdollars: 1_000_000},
		},
	}})

	daily, exceptions, err := buildUsageDailyContributions(
		[]usageRollupFact{second, first}, resolver,
	)
	require.NoError(t, err)
	assert.Empty(t, daily)
	assert.Equal(t, []string{"1:0", "2:0"}, rollupFactIDs(exceptions))
}

func TestBuildUsageDailyContributionsPricesBeforeSumming(t *testing.T) {
	first := rollupGeneralFact(1, 0, "session-a", "2026-08-01", "model-a", "", 1)
	first.Fact.InputTokens = 500_000
	second := rollupGeneralFact(1, 1, "session-a", "2026-08-01", "model-a", "", 1)
	second.Fact.InputTokens = 500_000
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "model-a",
		Rates: export.ModelRates{
			InputPerMTok: money.Money{Microdollars: 1},
		},
	}})

	daily, _, err := buildUsageDailyContributions(
		[]usageRollupFact{first, second}, resolver,
	)
	require.NoError(t, err)
	require.Len(t, daily, 1)
	assert.Equal(t, int64(2), daily[0].CostMicrodollars)
}

func rollupSnapshotFact(
	cachedID int64, factIndex int, sessionID, date, model string, output int64,
) usageRollupFact {
	millis := int64(factIndex + 1)
	ordinal := factIndex
	return usageRollupFact{
		CachedSessionID: cachedID, FactIndex: factIndex,
		SourceSessionID: sessionID, AttributionSessionID: sessionID,
		LocalDate: date, Model: model, Agent: "claude",
		Fact: usagefacts.Fact{
			Source: "message", MessageOrdinal: &ordinal, TimestampMillis: &millis,
			RawTimestamp: "2026-08-01T00:00:00Z", Model: model,
			OutputTokens: output, ClaudeMessageID: "message-id",
			ClaudeRequestID: "request-id", TokenEligible: true,
		},
	}
}

func rollupGeneralFact(
	cachedID int64, factIndex int, sessionID, date, model, dedup string, output int64,
) usageRollupFact {
	millis := int64(factIndex + 1)
	ordinal := factIndex
	return usageRollupFact{
		CachedSessionID: cachedID, FactIndex: factIndex,
		SourceSessionID: sessionID, AttributionSessionID: sessionID,
		LocalDate: date, Model: model, Agent: "codex",
		Fact: usagefacts.Fact{
			Source: "usage", MessageOrdinal: &ordinal, TimestampMillis: &millis,
			RawTimestamp: "2026-08-01T00:00:00Z", Model: model,
			OutputTokens: output, UsageDedupKey: dedup, TokenEligible: true,
		},
	}
}

func rollupFactIDs(facts []usageRollupFact) []string {
	result := make([]string, 0, len(facts))
	for _, fact := range facts {
		result = append(result, usageRollupFactIdentity(fact))
	}
	return result
}
