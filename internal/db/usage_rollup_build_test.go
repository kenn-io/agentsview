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

func TestClassifyUsageRollupFactsFinalizesSafeGroups(t *testing.T) {
	plain := rollupSnapshotFact(1, 0, "session-a", "2026-08-01", "model-a", 5)
	plain.Fact.ClaudeMessageID = ""
	plain.Fact.ClaudeRequestID = ""
	singleton := rollupSnapshotFact(1, 1, "session-a", "2026-08-01", "model-a", 10)
	loser := rollupSnapshotFact(1, 2, "session-a", "2026-08-01", "model-a", 20)
	loser.Fact.ClaudeMessageID = "duplicated"
	loser.Fact.WebSearchRequests = 4
	winner := rollupSnapshotFact(1, 3, "session-a", "2026-08-01", "model-a", 30)
	winner.Fact.ClaudeMessageID = "duplicated"
	winner.Fact.WebSearchRequests = 1

	survivors, exceptions := classifyUsageRollupFacts(
		[]usageRollupFact{plain, singleton, loser, winner},
		newUsageDedupIdentitySet(),
	)

	assert.Empty(t, exceptions)
	require.Len(t, survivors, 3)
	assert.Equal(t, "1:0", usageRollupFactIdentity(survivors[0].Fact))
	assert.Equal(t, "1:1", usageRollupFactIdentity(survivors[1].Fact))
	assert.Zero(t, survivors[1].DiscardedSnapshotOutputTokens)
	ranked := survivors[2]
	assert.Equal(t, "1:3", usageRollupFactIdentity(ranked.Fact),
		"the greater output snapshot must win")
	assert.Equal(t, int64(20), ranked.DiscardedSnapshotOutputTokens)
	assert.Equal(t, int64(4), ranked.Fact.Fact.WebSearchRequests,
		"the maximum web-search count must carry across snapshots")
	assert.Equal(t, "session-a", ranked.Fact.AttributionSessionID)
}

func TestClassifyUsageRollupFactsFinalizesSafeGeneralGroups(t *testing.T) {
	early := rollupGeneralFact(1, 0, "session-a", "2026-08-01", "model-a", "shared", 10)
	early.Fact.RawTimestamp = "2026-08-01T01:00:00Z"
	early.DedupTimestamp = early.Fact.RawTimestamp
	late := rollupGeneralFact(1, 1, "session-a", "2026-08-01", "model-a", "shared", 20)
	late.Fact.RawTimestamp = "2026-08-01T02:00:00Z"
	late.DedupTimestamp = late.Fact.RawTimestamp

	survivors, exceptions := classifyUsageRollupFacts(
		[]usageRollupFact{late, early}, newUsageDedupIdentitySet(),
	)

	assert.Empty(t, exceptions)
	require.Len(t, survivors, 1)
	assert.Equal(t, "1:0", usageRollupFactIdentity(survivors[0].Fact),
		"the earliest row must win general dedup")
}

func TestClassifyUsageRollupFactsKeepsIrreducibleGroups(t *testing.T) {
	crossSnapshot := newUsageDedupIdentitySet()
	crossSnapshot.add("message-id", "request-id", "", "")
	crossSource := newUsageDedupIdentitySet()
	crossSource.add("", "", "shared-uuid", "")
	crossUsage := newUsageDedupIdentitySet()
	crossUsage.add("", "", "", "shared-key")

	sourceFact := func(cachedID int64, index int, date, model string) usageRollupFact {
		fact := rollupGeneralFact(cachedID, index, "session-a", date, model, "", 10)
		fact.Fact.Source = "message"
		fact.Fact.SourceUUID = "shared-uuid"
		fact.Agent = "codex"
		return fact
	}
	copilot := rollupGeneralFact(1, 0, "session-a", "2026-08-01", "model-a", "cp", 10)
	reported := int64(55)
	copilot.Fact.ReportedCostMicrodollars = &reported
	copilot.Fact.CostSource = CopilotReportedCostSource
	undated := rollupGeneralFact(1, 0, "session-a", "", "model-a", "shared", 10)
	undated.Fact.RawTimestamp = ""
	undated.DedupTimestamp = ""

	cases := []struct {
		name       string
		facts      []usageRollupFact
		cross      usageDedupIdentitySet
		exceptions int
	}{
		{"cross-session snapshot identity",
			[]usageRollupFact{rollupSnapshotFact(1, 0, "session-a", "2026-08-01", "model-a", 10)},
			crossSnapshot, 1},
		{"snapshot group spanning days", []usageRollupFact{
			rollupSnapshotFact(1, 0, "session-a", "2026-08-01", "model-a", 10),
			rollupSnapshotFact(1, 1, "session-a", "2026-08-02", "model-a", 20),
		}, newUsageDedupIdentitySet(), 2},
		{"cross-session source identity",
			[]usageRollupFact{sourceFact(1, 0, "2026-08-01", "model-a")},
			crossSource, 1},
		{"general group spanning models", []usageRollupFact{
			rollupGeneralFact(1, 0, "session-a", "2026-08-01", "model-a", "shared", 10),
			rollupGeneralFact(1, 1, "session-a", "2026-08-01", "model-b", "shared", 20),
		}, newUsageDedupIdentitySet(), 2},
		{"general group spanning days", []usageRollupFact{
			rollupGeneralFact(1, 0, "session-a", "2026-08-01", "model-a", "shared", 10),
			rollupGeneralFact(1, 1, "session-a", "2026-08-02", "model-a", "shared", 20),
		}, newUsageDedupIdentitySet(), 2},
		{"cross usage key",
			[]usageRollupFact{rollupGeneralFact(1, 0, "session-a", "2026-08-01", "model-a", "shared-key", 10)},
			crossUsage, 1},
		{"authoritative copilot cost",
			[]usageRollupFact{copilot}, newUsageDedupIdentitySet(), 1},
		{"general group with mixed empty dates", []usageRollupFact{
			undated,
			rollupGeneralFact(1, 1, "session-a", "2026-08-01", "model-a", "shared", 20),
		}, newUsageDedupIdentitySet(), 2},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			survivors, exceptions := classifyUsageRollupFacts(item.facts, item.cross)
			assert.Empty(t, survivors)
			assert.Len(t, exceptions, item.exceptions)
		})
	}
}

func TestClassifyUsageRollupFactsFinalizesUndatedSameKeyGroup(t *testing.T) {
	first := rollupGeneralFact(1, 0, "session-a", "", "model-a", "shared", 10)
	first.Fact.RawTimestamp = ""
	first.DedupTimestamp = ""
	second := rollupGeneralFact(1, 1, "session-a", "", "model-a", "shared", 20)
	second.Fact.RawTimestamp = ""
	second.DedupTimestamp = ""

	survivors, exceptions := classifyUsageRollupFacts(
		[]usageRollupFact{first, second}, newUsageDedupIdentitySet(),
	)

	assert.Empty(t, exceptions)
	require.Len(t, survivors, 1)
	assert.Equal(t, "1:0", usageRollupFactIdentity(survivors[0].Fact))
}

func TestClassifyUsageRollupFactsClosesSnapshotToGeneralEdges(t *testing.T) {
	snapshotWithKey := rollupSnapshotFact(1, 0, "session-a", "2026-08-01", "model-a", 10)
	snapshotWithKey.Fact.UsageDedupKey = "linked"
	general := rollupGeneralFact(1, 1, "session-a", "2026-08-01", "model-a", "linked", 20)

	survivors, exceptions := classifyUsageRollupFacts(
		[]usageRollupFact{snapshotWithKey, general}, newUsageDedupIdentitySet(),
	)

	assert.Empty(t, survivors,
		"a snapshot fact linked into a general group is irreducible")
	assert.Len(t, exceptions, 2)
}

func TestClassifyUsageRollupFactsSkipsNonTokenFacts(t *testing.T) {
	activity := rollupSnapshotFact(1, 0, "session-a", "2026-08-01", "model-a", 10)
	activity.Fact.TokenEligible = false

	survivors, exceptions := classifyUsageRollupFacts(
		[]usageRollupFact{activity}, newUsageDedupIdentitySet(),
	)

	assert.Empty(t, survivors)
	assert.Empty(t, exceptions)
}

func TestCompareUsageFactTiesUsesAscendingIdentity(t *testing.T) {
	left := rollupGeneralFact(1, 0, "session-a", "2026-08-01", "model-a", "key", 1)
	right := rollupGeneralFact(2, 0, "session-b", "2026-08-01", "model-a", "key", 1)

	assert.Negative(t, compareUsageFactTies(left, right))
	assert.Positive(t, compareUsageFactTies(right, left))
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

func TestPriceUsageFactUsesBilledRatesForReportedCacheSavings(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "model-a",
		Rates: export.ModelRates{
			InputPerMTok:     money.MustParseDollars("1"),
			CacheReadPerMTok: money.MustParseDollars("0.1"),
		},
	}})
	reported := int64(77)

	got, err := priceUsageFact(usagePriceInput{
		ProviderID:    "positai",
		ReportedModel: "model-a",
		Fact: usagefacts.Fact{
			Model: "model-a", CacheReadTokens: 1_000_000,
			ReportedCostMicrodollars: &reported,
			CostSource:               "provider-reported",
		},
	}, resolver)
	require.NoError(t, err)
	assert.Equal(t, int64(77), got.Cost.Microdollars)
	assert.Equal(t, int64(990_000), got.Savings.Microdollars)
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

func TestBuildUsageDailyContributionsRecordsDiscardedSnapshots(t *testing.T) {
	loser := rollupSnapshotFact(1, 0, "session-a", "2026-08-01", "model-a", 10)
	winner := rollupSnapshotFact(1, 1, "session-a", "2026-08-01", "model-a", 20)
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "model-a",
		Rates: export.ModelRates{
			OutputPerMTok: money.Money{Microdollars: 1_000_000},
		},
	}})

	survivors, exceptions := classifyUsageRollupFacts(
		[]usageRollupFact{loser, winner}, newUsageDedupIdentitySet(),
	)
	require.Empty(t, exceptions)
	daily, err := buildUsageDailyContributions(survivors, resolver)
	require.NoError(t, err)
	require.Len(t, daily, 1)
	assert.Equal(t, int64(20), daily[0].OutputTokens)
	assert.Equal(t, int64(10), daily[0].DiscardedSnapshotOutputTokens)
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

	daily, err := buildUsageDailyContributions(
		[]usageRollupSurvivor{{Fact: first}, {Fact: second}}, resolver,
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
