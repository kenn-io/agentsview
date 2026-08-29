package db

import (
	"context"
	"encoding/json/jsontext"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestRecordUsageFactsPricingSeparatesReportedAndBilledProvenance(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "model",
		Rates:        export.ModelRates{OutputPerMTok: money.Money{Microdollars: 1_000_000}},
	}})
	require.NoError(t, recordUsageFactsPricing(resolver, usageFactsGroup{
		Agent: "posit-assistant", ProviderID: "positai", Model: "model", PricedModel: "model",
		ReportedCount: 1, ComputedAggregateCount: 1,
	}))
	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	resolutions := block.Models["model"].Resolutions
	require.Len(t, resolutions, 2)
	asserted := map[int64]bool{}
	for _, resolution := range resolutions {
		asserted[resolution.OutputCostPerMTok.Microdollars] = true
	}
	if !asserted[1_000_000] || !asserted[1_100_000] {
		t.Fatalf("reported and billed provenance merged: %#v", resolutions)
	}
}

func TestRecordUsageFactsPricingSkipsUnusedOverflowingReportedRate(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "model",
		Rates:        export.ModelRates{OutputPerMTok: money.Money{Microdollars: math.MaxInt64}},
	}})
	require.NoError(t, recordUsageFactsPricing(resolver, usageFactsGroup{
		Agent: "posit-assistant", ProviderID: "positai", Model: "model", PricedModel: "model",
		ReportedCount: 1,
	}))
	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	require.Contains(t, block.Models, "model")
	resolutions := block.Models["model"].Resolutions
	require.Len(t, resolutions, 1)
	asserted := resolutions[0].OutputCostPerMTok.Microdollars
	require.Equal(t, int64(math.MaxInt64), asserted)
}

func TestAssembleDailyUsageFactsPropagatesBillingError(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "model",
		Rates:        export.ModelRates{OutputPerMTok: money.Money{Microdollars: math.MaxInt64}},
	}})
	_, err := (&DB{}).assembleDailyUsageFacts(
		context.Background(), UsageFilter{}, usageFactsResult{
			Groups: []usageFactsGroup{{
				Agent: "posit-assistant", ProviderID: "positai", Model: "model", PricedModel: "model",
				ComputedAggregateCount: 1,
			}},
		}, resolver)
	require.Error(t, err)
	require.ErrorContains(t, err, "recording daily usage pricing")
}

func TestPositBillingPublicAPIReproduction(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "repro-model",
		InputPerMTok: money.MustParseDollars("1"),
	}}))
	insertSession(t, d, "posit-repro", "repro", func(s *Session) {
		s.Agent = "posit-assistant"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{SessionID: "posit-repro", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "repro-model", ProviderID: "positai",
		TokenUsage: jsontext.Value(`{"input_tokens":1000000}`)})
	insertSession(t, d, "plain-repro", "repro", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{SessionID: "plain-repro", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "repro-model", TokenUsage: jsontext.Value(`{"input_tokens":1000000}`)})
	filter := UsageFilter{
		From: "2024-06-01", To: "2024-06-30", Breakdowns: true,
	}
	for range 2 {
		result, err := d.GetDailyUsage(context.Background(), filter)
		require.NoError(t, err)
		require.Len(t, result.Daily, 1)
		costs := make(map[string]money.Money, len(result.Daily[0].AgentBreakdowns))
		for _, breakdown := range result.Daily[0].AgentBreakdowns {
			costs[breakdown.Agent] = breakdown.Cost
		}
		require.Equal(t, money.MustParseDollars("1.1"), costs["posit-assistant"])
		require.Equal(t, money.MustParseDollars("1"), costs["claude"])
		t.Logf("observed cost: posit=$%.6f", float64(costs["posit-assistant"].Microdollars)/1_000_000)
		t.Logf("observed non-Posit cost: $%.6f", float64(costs["claude"].Microdollars)/1_000_000)
	}
}

func TestPositBillingUsesProviderPerUsageRow(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "mixed-provider-model",
		InputPerMTok: money.MustParseDollars("1"),
	}}))
	insertSession(t, d, "mixed-provider", "repro", func(s *Session) {
		s.Agent = "posit-assistant"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d,
		Message{SessionID: "mixed-provider", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "mixed-provider-model",
			ProviderID: "positai",
			TokenUsage: jsontext.Value(`{"input_tokens":1000000}`)},
		Message{SessionID: "mixed-provider", Ordinal: 1, Role: "assistant",
			Timestamp: "2024-06-15T10:31:00Z", Model: "mixed-provider-model",
			ProviderID: "anthropic",
			TokenUsage: jsontext.Value(`{"input_tokens":1000000}`)},
	)

	usage, err := d.GetSessionUsage(context.Background(), "mixed-provider", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Equal(t, money.MustParseDollars("2.1"), usage.Cost)

	daily, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From: "2024-06-01", To: "2024-06-30", Timezone: "UTC",
	})
	require.NoError(t, err)
	require.Len(t, daily.Daily, 1)
	require.Equal(t, money.MustParseDollars("2.1"), daily.Totals.TotalCost)
}
