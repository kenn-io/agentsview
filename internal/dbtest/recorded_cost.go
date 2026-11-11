package dbtest

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// RecordedCostDay is the local date every RecordedCostSessions row falls on.
const RecordedCostDay = "2026-08-12"

// RecordedCostCase is one session in the recorded-cost fixture and the
// session usage every backend must report for it.
type RecordedCostCase struct {
	SessionID string
	// Cost is the session's expected cost in microdollars.
	Cost    int64
	HasCost bool
	Models  []string
	// Unpriced lists models the catalog cannot price and no cost covers.
	Unpriced []string
}

// RecordedCostCases covers how a cost a harness recorded itself, and a
// costed row without a model, are priced. rc-priced is $1/MTok input and
// $2/MTok output, so 1,000 input and 500 output tokens cost 2,000
// microdollars at catalog rates. rc-custom has no rate.
var RecordedCostCases = []RecordedCostCase{
	// The catalog prices the model, so its price beats the recorded 9,000.
	{SessionID: "rc-catalog", Cost: 2_000, HasCost: true, Models: []string{"rc-priced"}},
	// The catalog cannot price the model, so the recorded cost is the price.
	{SessionID: "rc-fallback", Cost: 7_000, HasCost: true, Models: []string{"rc-custom"}},
	// A recorded $0 for an unpriced model stays unpriced.
	{SessionID: "rc-zero", Cost: 0, HasCost: false, Models: []string{"rc-custom"}, Unpriced: []string{"rc-custom"}},
	// A recorded aggregate event follows the same rule as a message.
	{SessionID: "rc-event", Cost: 3_000 + 2_000, HasCost: true, Models: []string{"rc-custom", "rc-priced"}},
	// Any other status keeps the historical rule: the reported cost wins.
	{SessionID: "rc-exact", Cost: 4_000, HasCost: true, Models: []string{"rc-priced"}},
	// A costed event without a model is kept under the unknown-model label.
	{SessionID: "rc-unknown", Cost: 1_500, HasCost: true, Models: []string{usagefacts.UnknownModel}},
}

// RecordedCostDailyTotal is the sum of RecordedCostCases costs.
const RecordedCostDailyTotal = 2_000 + 7_000 + 0 + 5_000 + 4_000 + 1_500

// SeedRecordedCostSessions writes the RecordedCostCases sessions and the
// rc-priced catalog row through the normal batch write path.
func SeedRecordedCostSessions(t testing.TB, local *db.DB) {
	t.Helper()
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "rc-priced",
		InputPerMTok:  money.Money{Microdollars: 1_000_000},
		OutputPerMTok: money.Money{Microdollars: 2_000_000},
	}}), "seed recorded-cost pricing")

	started := RecordedCostDay + "T10:00:00Z"
	at := RecordedCostDay + "T10:05:00Z"
	session := func(id string) db.Session {
		return db.Session{
			ID: id, Project: "recorded-cost", Machine: "local", Agent: "opencode",
			StartedAt: &started, EndedAt: &at, MessageCount: 1, UserMessageCount: 1,
		}
	}
	message := func(id, model, usage string) db.Message {
		return db.Message{
			SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: at,
			Model: model, TokenUsage: jsontext.Value(usage),
		}
	}
	cost := func(microdollars int64) *money.Money {
		return &money.Money{Microdollars: microdollars}
	}
	tokens := `"input_tokens":1000,"output_tokens":500`
	writes := []db.SessionBatchWrite{
		{Session: session("rc-catalog"), Messages: []db.Message{
			message("rc-catalog", "rc-priced", `{`+tokens+`,"recorded_cost_microdollars":9000}`),
		}},
		{Session: session("rc-fallback"), Messages: []db.Message{
			message("rc-fallback", "rc-custom", `{`+tokens+`,"recorded_cost_microdollars":7000}`),
		}},
		{Session: session("rc-zero"), Messages: []db.Message{
			message("rc-zero", "rc-custom", `{`+tokens+`,"recorded_cost_microdollars":0}`),
		}},
		{Session: session("rc-event"), UsageEvents: []db.UsageEvent{
			{
				SessionID: "rc-event", Source: "session", Model: "rc-custom",
				InputTokens: 10, OutputTokens: 5, Cost: cost(3_000),
				CostStatus: usagefacts.CostStatusRecorded, CostSource: "fixture",
				OccurredAt: at, DedupKey: "rc-event-custom",
			},
			{
				SessionID: "rc-event", Source: "session", Model: "rc-priced",
				InputTokens: 1_000, OutputTokens: 500, Cost: cost(6_000),
				CostStatus: usagefacts.CostStatusRecorded, CostSource: "fixture",
				OccurredAt: at, DedupKey: "rc-event-priced",
			},
		}},
		{Session: session("rc-exact"), UsageEvents: []db.UsageEvent{{
			SessionID: "rc-exact", Source: "session", Model: "rc-priced",
			InputTokens: 1_000, OutputTokens: 500, Cost: cost(4_000),
			CostStatus: "exact", CostSource: "fixture",
			OccurredAt: at, DedupKey: "rc-exact",
		}}},
		{Session: session("rc-unknown"), UsageEvents: []db.UsageEvent{{
			SessionID: "rc-unknown", Source: "session",
			InputTokens: 20, OutputTokens: 10, Cost: cost(1_500),
			CostSource: "fixture", OccurredAt: at, DedupKey: "rc-unknown",
		}}},
	}
	_, err := local.WriteSessionBatchAtomic(t.Context(), writes)
	require.NoError(t, err, "seed recorded-cost sessions")
}

// AssertRecordedCostUsage checks a store holding SeedRecordedCostSessions
// against RecordedCostCases through session usage, daily usage, and top
// sessions.
func AssertRecordedCostUsage(t *testing.T, store db.Store) {
	t.Helper()
	ctx := t.Context()
	for _, want := range RecordedCostCases {
		got, err := store.GetSessionUsage(ctx, want.SessionID, true)
		require.NoError(t, err, "session usage %s", want.SessionID)
		require.NotNil(t, got, "session usage %s", want.SessionID)
		assert.Equal(t, want.Cost, got.Cost.Microdollars, "%s cost", want.SessionID)
		assert.Equal(t, want.HasCost, got.HasCost, "%s has_cost", want.SessionID)
		assert.ElementsMatch(t, want.Models, got.Models, "%s models", want.SessionID)
		assert.ElementsMatch(t, want.Unpriced, got.UnpricedModels, "%s unpriced", want.SessionID)
		var breakdown int64
		for _, entry := range got.Breakdown {
			breakdown += entry.Cost.Microdollars
		}
		assert.Equal(t, want.Cost, breakdown, "%s breakdown cost", want.SessionID)
		counted, err := store.GetSessionUsage(ctx, want.SessionID, false)
		require.NoError(t, err, "session usage count %s", want.SessionID)
		require.NotNil(t, counted, "session usage count %s", want.SessionID)
		assert.Equal(t, len(got.Breakdown), counted.BreakdownCount,
			"%s breakdown count without rows", want.SessionID)
	}

	filter := db.UsageFilter{From: RecordedCostDay, To: RecordedCostDay, Timezone: "UTC"}
	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err, "daily usage")
	assert.Equal(t, int64(RecordedCostDailyTotal), daily.Totals.TotalCost.Microdollars,
		"daily total cost")
	require.Len(t, daily.Daily, 1, "daily rows")
	models := map[string]int64{}
	for _, model := range daily.Daily[0].ModelBreakdowns {
		models[model.ModelName] = model.Cost.Microdollars
	}
	assert.Equal(t, map[string]int64{
		"rc-priced":             2_000 + 2_000 + 4_000,
		"rc-custom":             7_000 + 3_000,
		usagefacts.UnknownModel: 1_500,
	}, models, "daily model costs")

	top, err := store.GetTopSessionsByCost(ctx, filter, 100)
	require.NoError(t, err, "top sessions")
	costs := map[string]int64{}
	for _, row := range top {
		costs[row.SessionID] = row.Cost.Microdollars
	}
	for _, want := range RecordedCostCases {
		assert.Equal(t, want.Cost, costs[want.SessionID], "%s top-session cost", want.SessionID)
	}

	day, err := time.Parse(time.DateOnly, RecordedCostDay)
	require.NoError(t, err)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: RecordedCostDay, Timezone: "UTC",
	}, day.Add(48*time.Hour))
	require.NoError(t, err, "activity query")
	report, err := store.GetActivityReport(ctx, db.AnalyticsFilter{Timezone: "UTC"}, q)
	require.NoError(t, err, "activity report")
	assert.Equal(t, int64(RecordedCostDailyTotal), report.Totals.Cost.Microdollars,
		"activity report cost")
}
