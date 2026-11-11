package dbtest

import (
	"context"
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
	// Comparison is the expected computed-versus-recorded summary.
	Comparison *db.SessionRecordedCost
}

func comparison(recorded, computed int64, rows, fallback, unpriced int) *db.SessionRecordedCost {
	return &db.SessionRecordedCost{
		Recorded: money.Money{Microdollars: recorded}, Computed: money.Money{Microdollars: computed},
		Difference: money.Money{Microdollars: computed - recorded},
		Rows:       rows, FallbackRows: fallback, UnpricedRows: unpriced,
	}
}

// RecordedCostCases covers how a cost a harness recorded itself, and a
// costed row without a model, are priced. rc-priced is $1/MTok input and
// $2/MTok output, so 1,000 input and 500 output tokens cost 2,000
// microdollars at catalog rates. rc-custom has no rate.
var RecordedCostCases = []RecordedCostCase{
	// The catalog prices the model, so its price beats the recorded 9,000.
	{
		SessionID: "rc-catalog", Cost: 2_000, HasCost: true, Models: []string{"rc-priced"},
		Comparison: comparison(9_000, 2_000, 1, 0, 0),
	},
	// The catalog cannot price the model, so the recorded cost is the price.
	{
		SessionID: "rc-fallback", Cost: 7_000, HasCost: true, Models: []string{"rc-custom"},
		Comparison: comparison(7_000, 7_000, 1, 1, 0),
	},
	// A recorded $0 for an unpriced model stays unpriced.
	{
		SessionID: "rc-zero", Cost: 0, HasCost: false, Models: []string{"rc-custom"}, Unpriced: []string{"rc-custom"},
		Comparison: comparison(0, 0, 1, 0, 1),
	},
	// A recorded aggregate event follows the same rule as a message.
	{
		SessionID: "rc-event", Cost: 3_000 + 2_000, HasCost: true, Models: []string{"rc-custom", "rc-priced"},
		Comparison: comparison(3_000+6_000, 3_000+2_000, 2, 1, 0),
	},
	// Any other status keeps the historical rule: the reported cost wins.
	{SessionID: "rc-exact", Cost: 4_000, HasCost: true, Models: []string{"rc-priced"}},
	// A cost-only message whose recorded cost loses to the catalog adds
	// nothing: no cost, no model, and no priced request.
	{SessionID: "rc-evidence", Cost: 0, HasCost: false},
	// A costed event without a model is kept under the unknown-model label.
	{SessionID: "rc-unknown", Cost: 1_500, HasCost: true, Models: []string{usagefacts.UnknownModel}},
}

// RecordedCostDailyTotal is the sum of RecordedCostCases costs.
const RecordedCostDailyTotal = 2_000 + 7_000 + 0 + 5_000 + 4_000 + 1_500

// SeedRecordedCostSessions writes the RecordedCostCases sessions and the
// rc-priced catalog row through the normal batch write path.
func SeedRecordedCostSessions(tb testing.TB, local *db.DB) {
	tb.Helper()
	require.NoError(tb, local.UpsertModelPricing([]db.ModelPricing{{
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
		{Session: session("rc-evidence"), Messages: []db.Message{
			message("rc-evidence", "rc-priced", `{"recorded_cost_microdollars":800}`),
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
	_, err := local.WriteSessionBatchAtomic(tb.Context(), writes)
	require.NoError(tb, err, "seed recorded-cost sessions")
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
		summary, err := db.SummarizeRecordedCost(got.Breakdown)
		require.NoError(t, err)
		assert.Equal(t, want.Comparison, summary, "%s recorded-cost comparison", want.SessionID)
		if rows, ok := store.(interface {
			GetSessionUsageRows(context.Context, []string) (*activity.SessionUsageRows, error)
		}); ok {
			// Subagent totals are rebuilt from these rows, so they must carry
			// the same recorded costs as the breakdown.
			rowSet, err := rows.GetSessionUsageRows(ctx, []string{want.SessionID})
			require.NoError(t, err, "session usage rows %s", want.SessionID)
			var fromRows, fromBreakdown []int64
			for _, row := range rowSet.Rows {
				if row.Contributes && row.RecordedCost != nil {
					fromRows = append(fromRows, row.RecordedCost.Microdollars)
				}
			}
			for _, entry := range got.Breakdown {
				if entry.RecordedCost != nil {
					fromBreakdown = append(fromBreakdown, entry.RecordedCost.Microdollars)
				}
			}
			assert.ElementsMatch(t, fromBreakdown, fromRows, "%s recorded costs on usage rows", want.SessionID)
		}
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
	// rc-priced is charged at catalog rates for one message request
	// (rc-catalog) and one session aggregate (rc-event). The rc-evidence
	// message is not a priced request.
	require.NotNil(t, daily.Pricing, "daily pricing provenance")
	var requests, aggregates int
	for _, resolution := range daily.Pricing.Models["rc-priced"].Resolutions {
		requests += resolution.Application.BaseRequestCount
		for _, band := range resolution.Application.Bands {
			requests += band.RequestCount
		}
		aggregates += resolution.Application.AggregateRowCount
	}
	assert.Equal(t, 1, requests, "rc-priced priced requests")
	assert.Equal(t, 1, aggregates, "rc-priced priced aggregate rows")

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

// SessionTotalSessionID is the session SeedSessionUsageTotal writes.
const SessionTotalSessionID = "rc-session-total"

// SessionUsageTotalFixture is the total SeedSessionUsageTotal stores.
func SessionUsageTotalFixture() *db.SessionUsageTotal {
	return &db.SessionUsageTotal{
		Source: "claude-cost-state", SubagentScope: "included",
		Cost: &money.Money{Microdollars: 750_000}, Incomplete: true,
		Models: []db.SessionUsageTotalModel{
			{Model: "rc-custom", InputTokens: 10, OutputTokens: 4},
			{
				Model: "rc-priced", InputTokens: 1_500, OutputTokens: 530, ReasoningTokens: 30,
				CacheCreationInputTokens: 7, CacheReadInputTokens: 9, WebSearchRequests: 1,
				Cost: &money.Money{Microdollars: 750_000},
			},
		},
	}
}

// SeedSessionUsageTotal writes a session with one priced message and a
// recorded session total through the full-parse batch write.
func SeedSessionUsageTotal(tb testing.TB, local *db.DB) {
	tb.Helper()
	require.NoError(tb, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "rc-priced",
		InputPerMTok:  money.Money{Microdollars: 1_000_000},
		OutputPerMTok: money.Money{Microdollars: 2_000_000},
	}}), "seed session-total pricing")
	started := RecordedCostDay + "T11:00:00Z"
	at := RecordedCostDay + "T11:05:00Z"
	_, err := local.WriteSessionBatchAtomic(tb.Context(), []db.SessionBatchWrite{{
		Session: db.Session{
			ID: SessionTotalSessionID, Project: "recorded-cost", Machine: "local", Agent: "claude",
			StartedAt: &started, EndedAt: &at, MessageCount: 1, UserMessageCount: 1,
		},
		Messages: []db.Message{{
			SessionID: SessionTotalSessionID, Ordinal: 0, Role: "assistant", Timestamp: at,
			Model: "rc-priced", TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
		}},
		SessionTotal: SessionUsageTotalFixture(), ReplaceSessionTotal: true,
	}})
	require.NoError(tb, err, "seed session total")
}

// AssertSessionUsageTotal checks that a store holding SeedSessionUsageTotal
// reports the total beside, and never inside, the session's usage.
func AssertSessionUsageTotal(t *testing.T, store db.Store) {
	t.Helper()
	ctx := t.Context()
	got, err := store.GetSessionUsage(ctx, SessionTotalSessionID, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int64(2_000), got.Cost.Microdollars, "the total is not added to cost")
	assert.Equal(t, SessionUsageTotalFixture(), got.SessionTotal)
	daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: RecordedCostDay, To: RecordedCostDay, Timezone: "UTC", Project: "recorded-cost",
		Agent: "claude",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2_000), daily.Totals.TotalCost.Microdollars, "usage totals exclude the session total")
	assert.Equal(t, 500, daily.Totals.OutputTokens)
}
