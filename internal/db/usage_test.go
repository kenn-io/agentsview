package db

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
)

func TestGetDailyUsageEmpty(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-01-01",
		To:   "2024-12-31",
	})
	requireNoError(t, err, "GetDailyUsage empty")

	require.NotNil(t, result.Daily, "Daily should be non-nil empty slice")
	assert.Len(t, result.Daily, 0, "got")
	assert.Equal(t, 0.0, result.Totals.TotalCost, "TotalCost")
}

func TestUsageRowQueryPushesDateBoundsIntoUnion(t *testing.T) {
	query, args := usageRowQuery(UsageFilter{
		From:             "2024-06-01",
		To:               "2024-06-30",
		ExcludeAutomated: true,
	})

	normalized := strings.ToLower(query)
	assert.NotContains(t, normalized, "and u.ts >=")
	assert.NotContains(t, normalized, "and u.ts <=")
	assert.NotContains(t, normalized, " or ")
	assert.NotContains(t, normalized, "display_name")
	assert.NotContains(t, normalized, "first_message")
	assert.NotContains(t, normalized, "cost_status")
	assert.NotContains(t, normalized, "cost_source")
	assert.NotContains(t, normalized, "reasoning_tokens")
	assert.NotContains(t, normalized, "user_message_count")
	assert.NotContains(t, normalized, "session_activity_at")
	assert.NotContains(t, normalized, " as started_at")
	assert.NotContains(t, normalized, "u.machine")
	assert.Contains(t, normalized, "message_timestamp_rows as materialized")
	assert.Contains(t, normalized, "usage_event_timestamp_rows as materialized")
	assert.Contains(t, normalized, "from message_timestamp_rows m\njoin sessions s")
	assert.Contains(t, normalized, "from usage_event_timestamp_rows ue\njoin sessions s")
	assert.Contains(t, normalized, "m.timestamp is not null")
	assert.Contains(t, normalized, "ue.occurred_at is not null")
	assert.Contains(t, normalized, "m.timestamp is null")
	assert.Contains(t, normalized, "ue.occurred_at is null")
	assert.Contains(t, normalized, "m.timestamp >= ?")
	assert.Contains(t, normalized, "ue.occurred_at >= ?")
	assert.Contains(t, normalized, "s.started_at >= ?")
	assert.Contains(t, normalized, "m.timestamp <= ?")
	assert.Contains(t, normalized, "ue.occurred_at <= ?")
	assert.Contains(t, normalized, "s.started_at <= ?")
	require.Len(t, args, 8)
	assert.Equal(t, "2024-05-31T10:00:00Z", args[0])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[1])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[2])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[3])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[4])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[5])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[6])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[7])
}

func TestTopSessionsUsageRowQueryUsesNarrowScan(t *testing.T) {
	query, args := topSessionsUsageRowQuery(UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})

	normalized := strings.ToLower(query)
	assert.NotContains(t, normalized, "display_name")
	assert.NotContains(t, normalized, "first_message")
	assert.NotContains(t, normalized, "cost_status")
	assert.NotContains(t, normalized, "cost_source")
	assert.NotContains(t, normalized, "reasoning_tokens")
	assert.NotContains(t, normalized, "user_message_count")
	assert.NotContains(t, normalized, "session_activity_at")
	assert.NotContains(t, normalized, " as started_at")
	assert.NotContains(t, normalized, "u.machine")
	assert.Contains(t, normalized, "m.timestamp is not null")
	assert.Contains(t, normalized, "ue.occurred_at is not null")
	assert.Contains(t, normalized, "m.timestamp is null")
	assert.Contains(t, normalized, "ue.occurred_at is null")
	assert.Contains(t, normalized, "m.timestamp >= ?")
	assert.Contains(t, normalized, "ue.occurred_at >= ?")
	assert.Contains(t, normalized,
		"m.timestamp is null\n\tand s.started_at >= ?")
	assert.Contains(t, normalized,
		"ue.occurred_at is null\n\tand s.started_at >= ?")
	assert.Contains(t, normalized, "m.timestamp <= ?")
	assert.Contains(t, normalized, "ue.occurred_at <= ?")
	require.Len(t, args, 8)
	assert.Equal(t, "2024-05-31T10:00:00Z", args[0])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[1])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[2])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[3])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[4])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[5])
	assert.Equal(t, "2024-05-31T10:00:00Z", args[6])
	assert.Equal(t, "2024-07-01T13:59:59Z", args[7])
}

func TestUsageEventsReplaceAndList(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "hermes:event", "proj", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-14T10:00:00Z")
		s.UserMessageCount = 2
	})

	cost := 0.02
	ordinal := 3
	events := []UsageEvent{{
		SessionID:                "hermes:event",
		MessageOrdinal:           &ordinal,
		Source:                   "session",
		Model:                    "gpt-5.4",
		InputTokens:              100,
		OutputTokens:             50,
		CacheCreationInputTokens: 7,
		CacheReadInputTokens:     11,
		ReasoningTokens:          13,
		CostUSD:                  &cost,
		CostStatus:               "estimated",
		CostSource:               "hermes",
		OccurredAt:               "2026-05-14T10:05:00Z",
		DedupKey:                 "session:hermes:event",
	}}
	err := d.ReplaceSessionUsageEvents("hermes:event", events)
	require.NoError(t, err, "ReplaceSessionUsageEvents")

	got, err := d.GetUsageEvents(ctx, "hermes:event")
	require.NoError(t, err, "GetUsageEvents")
	require.Len(t, got, 1, "len")
	require.Equal(t, 100, got[0].InputTokens,
		"InputTokens (token fields not round-tripped: %#v)", got[0])
	require.Equal(t, 50, got[0].OutputTokens,
		"OutputTokens (token fields not round-tripped: %#v)", got[0])
	require.Equal(t, 7, got[0].CacheCreationInputTokens,
		"CacheCreationInputTokens (token fields not round-tripped: %#v)", got[0])
	require.Equal(t, 11, got[0].CacheReadInputTokens,
		"CacheReadInputTokens (token fields not round-tripped: %#v)", got[0])
	require.Equal(t, 13, got[0].ReasoningTokens,
		"ReasoningTokens (token fields not round-tripped: %#v)", got[0])
	require.NotNil(t, got[0].MessageOrdinal, "MessageOrdinal want 3")
	require.Equal(t, 3, *got[0].MessageOrdinal, "MessageOrdinal")
	require.NotNil(t, got[0].CostUSD, "CostUSD want %v", cost)
	require.Equal(t, cost, *got[0].CostUSD, "CostUSD")
	require.Equal(t, "session:hermes:event", got[0].DedupKey, "DedupKey")
	fps, err := d.UsageEventFingerprints([]string{"hermes:event", "missing"})
	require.NoError(t, err, "UsageEventFingerprints")
	require.NotEmpty(t, fps["hermes:event"],
		"expected non-empty usage event fingerprint")
	require.Equal(t, "", fps["missing"], "missing fingerprint")

	err = d.ReplaceSessionUsageEvents("hermes:event", nil)
	require.NoError(t, err, "ReplaceSessionUsageEvents clear")
	got, err = d.GetUsageEvents(ctx, "hermes:event")
	require.NoError(t, err, "GetUsageEvents after clear")
	require.Len(t, got, 0, "usage events after clear =")
}

func TestGetDailyUsageWithData(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	err := d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet-4-20250514",
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheCreationPerMTok: 3.75,
		CacheReadPerMTok:     0.30,
	}})
	requireNoError(t, err, "UpsertModelPricing")

	insertSession(t, d, "sess1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
		s.EndedAt = new("2024-06-15T11:00:00Z")
	})

	tokenUsage := `{
		"input_tokens": 1000,
		"output_tokens": 500,
		"cache_creation_input_tokens": 200,
		"cache_read_input_tokens": 300
	}`
	insertMessages(t, d, Message{
		SessionID:  "sess1",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(tokenUsage),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage")

	require.Len(t, result.Daily, 1, "got")

	day := result.Daily[0]
	assert.Equal(t, "2024-06-15", day.Date, "Date")
	assert.Equal(t, 1000, day.InputTokens, "InputTokens")
	assert.Equal(t, 500, day.OutputTokens, "OutputTokens")
	assert.Equal(t, 200, day.CacheCreationTokens, "CacheCreationTokens")
	assert.Equal(t, 300, day.CacheReadTokens, "CacheReadTokens")

	// Cost = (1000*3.0 + 500*15.0 + 200*3.75 + 300*0.30) / 1_000_000
	//      = (3000 + 7500 + 750 + 90) / 1_000_000
	//      = 11340 / 1_000_000
	//      = 0.01134
	wantCost := 0.01134
	assert.InDelta(t, wantCost, day.TotalCost, 1e-9, "TotalCost")

	assert.Equal(t, []string{"claude-sonnet-4-20250514"},
		day.ModelsUsed, "ModelsUsed")

	// Totals should match single day
	assert.Equal(t, 1000, result.Totals.InputTokens, "Totals.InputTokens")
	assert.InDelta(t, wantCost, result.Totals.TotalCost, 1e-9,
		"Totals.TotalCost")
}

func TestUsageQueriesUnionMessageAndUsageEvents(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "claude:msg", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-05-14T09:00:00Z")
		s.UserMessageCount = 2
	})
	insertMessages(t, d, Message{
		SessionID: "claude:msg",
		Ordinal:   0,
		Role:      "assistant",
		Timestamp: "2026-05-14T09:05:00Z",
		Model:     "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(
			`{"input_tokens":100,"output_tokens":40}`,
		),
	})

	insertSession(t, d, "hermes:event", "proj-b", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-14T10:00:00Z")
		s.UserMessageCount = 2
	})
	requireNoError(t, d.ReplaceSessionUsageEvents(
		"hermes:event",
		[]UsageEvent{{
			SessionID:            "hermes:event",
			Source:               "session",
			Model:                "gpt-5.4",
			InputTokens:          300,
			OutputTokens:         70,
			CacheReadInputTokens: 20,
			DedupKey:             "shared-key",
		}},
	), "replace hermes usage event")
	insertSession(t, d, "hermes:event-2", "proj-b", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-14T10:10:00Z")
		s.UserMessageCount = 2
	})
	requireNoError(t, d.ReplaceSessionUsageEvents(
		"hermes:event-2",
		[]UsageEvent{{
			SessionID:    "hermes:event-2",
			Source:       "session",
			Model:        "gpt-5.4",
			InputTokens:  50,
			OutputTokens: 5,
			DedupKey:     "shared-key",
		}},
	), "replace second hermes usage event")

	filter := UsageFilter{
		From:       "2026-05-14",
		To:         "2026-05-14",
		Breakdowns: true,
	}
	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage")
	require.Equal(t, 450, daily.Totals.InputTokens,
		"daily totals: %#v", daily.Totals)
	require.Equal(t, 115, daily.Totals.OutputTokens,
		"daily totals: %#v", daily.Totals)
	require.Equal(t, 20, daily.Totals.CacheReadTokens,
		"daily totals: %#v", daily.Totals)
	require.Len(t, daily.Daily, 1, "daily entries =")
	require.Len(t, daily.Daily[0].AgentBreakdowns, 2,
		"agent breakdowns: %#v", daily.Daily[0].AgentBreakdowns)

	top, err := d.GetTopSessionsByCost(ctx, filter, 10)
	requireNoError(t, err, "GetTopSessionsByCost")
	topByID := make(map[string]TopSessionEntry, len(top))
	for _, entry := range top {
		topByID[entry.SessionID] = entry
	}
	require.Equal(t, 140, topByID["claude:msg"].TotalTokens,
		"claude top tokens: %#v", topByID["claude:msg"])
	require.Equal(t, 390, topByID["hermes:event"].TotalTokens,
		"hermes top tokens: %#v", topByID["hermes:event"])
	require.Equal(t, 55, topByID["hermes:event-2"].TotalTokens,
		"second hermes top tokens: %#v", topByID["hermes:event-2"])

	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts")
	require.Equal(t, 3, counts.Total, "counts: %#v", counts)
	require.Equal(t, 1, counts.ByAgent["claude"], "counts: %#v", counts)
	require.Equal(t, 2, counts.ByAgent["hermes"], "counts: %#v", counts)
	require.Equal(t, 1, counts.ByProject["proj-a"], "counts: %#v", counts)
	require.Equal(t, 2, counts.ByProject["proj-b"], "counts: %#v", counts)
}

// TestGetDailyUsage_CacheSavingsUsesPerModelRates pins down
// that totals.CacheSavings is computed from each row's actual
// per-model pricing, not a hard-coded proxy. A hard-coded
// Sonnet rate would misreport an Opus-heavy workload because
// Opus rates are roughly 5x Sonnet on both sides.
func TestGetDailyUsage_CacheSavingsUsesPerModelRates(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:         "claude-opus-4-6",
			InputPerMTok:         15.0,
			OutputPerMTok:        75.0,
			CacheCreationPerMTok: 18.75,
			CacheReadPerMTok:     1.50,
		},
		{
			ModelPattern:         "claude-sonnet-4-20250514",
			InputPerMTok:         3.0,
			OutputPerMTok:        15.0,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.30,
		},
	}), "UpsertModelPricing")

	// Same 1M/1M mix of cache read + cache creation tokens
	// on both models so the per-model rate difference is the
	// only thing that can move the result.
	tokens := json.RawMessage(
		`{"input_tokens":0,"output_tokens":0,` +
			`"cache_creation_input_tokens":1000000,` +
			`"cache_read_input_tokens":1000000}`)

	insertSession(t, d, "s-opus", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-opus", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-opus-4-6", TokenUsage: tokens,
	})

	insertSession(t, d, "s-sonnet", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:05:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-sonnet", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:35:00Z",
		Model: "claude-sonnet-4-20250514", TokenUsage: tokens,
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01", To: "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage")

	// Opus per-token delta: read earns (15 - 1.50) = 13.50,
	// creation earns (15 - 18.75) = -3.75.
	// Opus savings on 1M + 1M = 13.50 + (-3.75) = 9.75.
	// Sonnet per-token delta: read earns (3 - 0.30) = 2.70,
	// creation earns (3 - 3.75) = -0.75.
	// Sonnet savings on 1M + 1M = 2.70 + (-0.75) = 1.95.
	// Net total savings = 9.75 + 1.95 = 11.70.
	wantSavings := 11.70
	assert.InDelta(t, wantSavings, result.Totals.CacheSavings, 1e-9,
		"Totals.CacheSavings")

	// Falsification: if the code had used Sonnet rates for
	// both rows the total would be 2 * 1.95 = 3.90, which
	// differs from wantSavings by >$7. Assert we're nowhere
	// near that value so a regression to a single-rate path
	// trips the test.
	assert.Greater(t, math.Abs(result.Totals.CacheSavings-3.90), 0.1,
		"CacheSavings looks like single-rate path; expected per-model math")
}

func TestGetDailyUsageAgentFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	err := d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet-4-20250514",
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheCreationPerMTok: 3.75,
		CacheReadPerMTok:     0.30,
	}})
	requireNoError(t, err, "UpsertModelPricing")

	// Claude session
	insertSession(t, d, "sess-claude", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-claude",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	// Codex session
	insertSession(t, d, "sess-codex", "proj1", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-codex",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(`{"input_tokens":2000,"output_tokens":1000}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:  "2024-06-01",
		To:    "2024-06-30",
		Agent: "claude",
	})
	requireNoError(t, err, "GetDailyUsage agent filter")

	require.Len(t, result.Daily, 1, "got")

	day := result.Daily[0]
	assert.Equal(t, 1000, day.InputTokens, "InputTokens")
	assert.Equal(t, 500, day.OutputTokens, "OutputTokens")
}

func TestGetDailyUsageMultipleDaysAndModels(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	err := d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:  "model-a",
			InputPerMTok:  2.0,
			OutputPerMTok: 10.0,
		},
		{
			ModelPattern:  "model-b",
			InputPerMTok:  4.0,
			OutputPerMTok: 20.0,
		},
	})
	requireNoError(t, err, "UpsertModelPricing")

	// Day 1: two models
	insertSession(t, d, "sess-d1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-10T08:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID:  "sess-d1",
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-10T08:30:00Z",
			Model:      "model-a",
			TokenUsage: json.RawMessage(`{"input_tokens":100,"output_tokens":50}`),
		},
		Message{
			SessionID:  "sess-d1",
			Ordinal:    1,
			Role:       "assistant",
			Timestamp:  "2024-06-10T09:00:00Z",
			Model:      "model-b",
			TokenUsage: json.RawMessage(`{"input_tokens":200,"output_tokens":100}`),
		},
	)

	// Day 2: one model
	insertSession(t, d, "sess-d2", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-11T08:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-d2",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-11T08:30:00Z",
		Model:      "model-a",
		TokenUsage: json.RawMessage(`{"input_tokens":300,"output_tokens":150}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage multi")

	require.Len(t, result.Daily, 2, "got")

	// Day 1: check totals
	d1 := result.Daily[0]
	assert.Equal(t, "2024-06-10", d1.Date, "day1 Date")
	assert.Equal(t, 300, d1.InputTokens, "day1 InputTokens")
	assert.Equal(t, 150, d1.OutputTokens, "day1 OutputTokens")
	assert.Len(t, d1.ModelsUsed, 2, "day1 ModelsUsed count")

	// Day 2
	d2 := result.Daily[1]
	assert.Equal(t, "2024-06-11", d2.Date, "day2 Date")
	assert.Equal(t, 300, d2.InputTokens, "day2 InputTokens")

	// Totals should sum both days
	wantTotalInput := 600
	assert.Equal(t, wantTotalInput, result.Totals.InputTokens, "Totals.InputTokens")
	wantTotalOutput := 300
	assert.Equal(t, wantTotalOutput, result.Totals.OutputTokens, "Totals.OutputTokens")

	// Cost check: day1 model-a = (100*2+50*10)/1e6 = 0.0007
	//             day1 model-b = (200*4+100*20)/1e6 = 0.0028
	//             day2 model-a = (300*2+150*10)/1e6 = 0.0021
	//             total = 0.0056
	wantTotalCost := 0.0056
	assert.InDelta(t, wantTotalCost, result.Totals.TotalCost, 1e-9,
		"Totals.TotalCost")
}

func TestGetDailyUsageNoPricing(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "sess1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess1",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "unknown-model",
		TokenUsage: json.RawMessage(`{"input_tokens":500,"output_tokens":250}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage no pricing")

	require.Len(t, result.Daily, 1, "got")

	day := result.Daily[0]
	assert.Equal(t, 500, day.InputTokens, "InputTokens")
	assert.Equal(t, 250, day.OutputTokens, "OutputTokens")
	assert.Equal(t, 0.0, day.TotalCost, "TotalCost")
	assert.Equal(t, []string{"unknown-model"}, day.ModelsUsed,
		"ModelsUsed")
}

// TestGetDailyUsageTruncatedTokenJSON documents what happens when
// a message lands in the DB with truncated token_usage — gjson is
// permissive and still extracts the leading fields, so the valid
// data is preserved. This is why we don't run gjson.Valid on the
// hot aggregation path: the realistic corruption modes reachable
// from our parsers don't produce silent zeros.
func TestGetDailyUsageTruncatedTokenJSON(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "sess1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	insertMessages(t, d,
		Message{
			SessionID: "sess1", Ordinal: 0,
			Role:      "assistant",
			Timestamp: "2024-06-15T10:30:00Z",
			Model:     "claude-sonnet-4-20250514",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`),
		},
		Message{
			SessionID: "sess1", Ordinal: 1,
			Role:      "assistant",
			Timestamp: "2024-06-15T10:31:00Z",
			Model:     "claude-sonnet-4-20250514",
			// Truncated mid-key. gjson still finds the two
			// leading numeric fields and extracts them.
			TokenUsage: json.RawMessage(
				`{"input_tokens":9999,"output_tokens":4242,"ca`),
		},
	)

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage truncated")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	// 1000 (valid row) + 9999 (truncated but still parseable)
	assert.Equal(t, 10999, day.InputTokens,
		"InputTokens want 10999 "+
			"(gjson should extract leading fields from truncated JSON)")
	assert.Equal(t, 4742, day.OutputTokens, "OutputTokens")
}

func TestGetDailyUsage_DedupesByClaudeMessageAndRequestID(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         15.0,
		OutputPerMTok:        75.0,
		CacheCreationPerMTok: 18.75,
		CacheReadPerMTok:     1.50,
	}}), "seed pricing")

	mustExec := func(q string, args ...any) {
		t.Helper()
		_, err := d.getWriter().Exec(q, args...)
		require.NoError(t, err, "exec %q", q)
	}
	mustExec(`INSERT INTO sessions (id, project, machine, agent, started_at, ended_at)
	          VALUES (?, ?, 'local', 'claude', ?, ?)`,
		"s-main", "proj", "2026-04-10T10:00:00Z", "2026-04-10T10:05:00Z")
	mustExec(`INSERT INTO sessions (id, project, machine, agent, started_at, ended_at, parent_session_id, relationship_type)
	          VALUES (?, ?, 'local', 'claude', ?, ?, 's-main', 'fork')`,
		"s-fork", "proj", "2026-04-10T10:01:00Z", "2026-04-10T10:06:00Z")

	shared := `{"input_tokens":100,"output_tokens":500,"cache_creation_input_tokens":1000,"cache_read_input_tokens":50000}`
	unique := `{"input_tokens":20,"output_tokens":80,"cache_creation_input_tokens":200,"cache_read_input_tokens":5000}`

	for _, row := range []struct {
		sid, ts, usage, mid, rid string
		ord                      int
	}{
		{"s-main", "2026-04-10T10:02:00Z", shared, "msg_dup", "req_dup", 0},
		{"s-fork", "2026-04-10T10:02:00Z", shared, "msg_dup", "req_dup", 0},
		{"s-fork", "2026-04-10T10:03:00Z", unique, "msg_uniq", "req_uniq", 1},
	} {
		mustExec(`INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 model, token_usage,
			 claude_message_id, claude_request_id,
			 has_output_tokens, has_context_tokens)
			VALUES (?, ?, 'assistant', '', ?, 'claude-opus-4-6', ?, ?, ?, 1, 1)`,
			row.sid, row.ord, row.ts, row.usage, row.mid, row.rid)
	}

	result, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-04-10", To: "2026-04-10", Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, result.Daily, 1, "daily entries =")
	day := result.Daily[0]
	assert.Equal(t, 120, day.InputTokens, "input")
	assert.Equal(t, 580, day.OutputTokens, "output")
	assert.Equal(t, 1200, day.CacheCreationTokens, "cache_cr")
	assert.Equal(t, 55000, day.CacheReadTokens, "cache_rd")
}

func TestGetDailyUsage_MissingDedupKeysCountedEveryTime(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-opus-4-6",
		OutputPerMTok: 75.0,
	}}), "seed pricing")

	mustExec := func(q string, args ...any) {
		t.Helper()
		_, err := d.getWriter().Exec(q, args...)
		require.NoError(t, err, "exec %q", q)
	}
	mustExec(`INSERT INTO sessions (id, project, machine, agent, started_at, ended_at)
	          VALUES ('s1', 'proj', 'local', 'claude', ?, ?)`,
		"2026-04-10T10:00:00Z", "2026-04-10T10:05:00Z")

	usage := `{"input_tokens":0,"output_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`
	for _, ord := range []int{0, 1} {
		mustExec(`INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 model, token_usage,
			 claude_message_id, claude_request_id,
			 has_output_tokens)
			VALUES ('s1', ?, 'assistant', '', '2026-04-10T10:02:00Z',
			        'claude-opus-4-6', ?, '', '', 1)`, ord, usage)
	}

	result, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-04-10", To: "2026-04-10", Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, result.Daily, 1,
		"output want 20 (both no-key rows counted): %v", result.Daily)
	assert.Equal(t, 20, result.Daily[0].OutputTokens,
		"output want 20 (both no-key rows counted): %v", result.Daily)
}

func TestGetDailyUsageLongLivedSession(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-6",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "upsert pricing")

	// Session started on Apr 1 but has messages on Apr 10.
	requireNoError(t, d.UpsertSession(Session{
		ID: "long-lived", Project: "proj", Machine: "local",
		Agent:     "claude",
		StartedAt: new("2026-04-01T10:00:00Z"),
	}), "upsert session")

	insertMessages(t, d,
		Message{
			SessionID: "long-lived", Ordinal: 0,
			Role: "assistant", Content: "early",
			ContentLength: 5,
			Timestamp:     "2026-04-01T10:00:00Z",
			Model:         "claude-sonnet-4-6",
			TokenUsage: json.RawMessage(
				`{"input_tokens":100,"output_tokens":50}`),
			ContextTokens:    100,
			OutputTokens:     50,
			HasContextTokens: true,
			HasOutputTokens:  true,
		},
		Message{
			SessionID: "long-lived", Ordinal: 1,
			Role: "assistant", Content: "late",
			ContentLength: 4,
			Timestamp:     "2026-04-10T14:00:00Z",
			Model:         "claude-sonnet-4-6",
			TokenUsage: json.RawMessage(
				`{"input_tokens":2000,"output_tokens":500}`),
			ContextTokens:    2000,
			OutputTokens:     500,
			HasContextTokens: true,
			HasOutputTokens:  true,
		},
	)

	// Query Apr 10 only — should include the late message even
	// though the session started on Apr 1.
	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:     "2026-04-10",
		To:       "2026-04-10",
		Timezone: "UTC",
	})
	requireNoError(t, err, "GetDailyUsage long-lived")

	require.Len(t, result.Daily, 1, "expected 1 day")
	assert.Equal(t, 2000, result.Daily[0].InputTokens, "InputTokens")
}

func TestGetDailyUsageProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "sess-a", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-a",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	insertSession(t, d, "sess-b", "proj-b", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-b",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":2000,"output_tokens":1000}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:    "2024-06-01",
		To:      "2024-06-30",
		Project: "proj-a",
	})
	requireNoError(t, err, "GetDailyUsage project filter")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	assert.Equal(t, 1000, day.InputTokens, "InputTokens")
	assert.Equal(t, 1000, result.Totals.InputTokens, "Totals.InputTokens")
}

func TestGetDailyUsageModelFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "claude-sonnet", InputPerMTok: 3.0,
			OutputPerMTok: 15.0},
		{ModelPattern: "gpt-5", InputPerMTok: 2.5,
			OutputPerMTok: 10.0},
	}), "UpsertModelPricing")

	insertSession(t, d, "sess1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID:  "sess1",
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-15T10:30:00Z",
			Model:      "claude-sonnet",
			TokenUsage: json.RawMessage(`{"input_tokens":2000,"output_tokens":800}`),
		},
		Message{
			SessionID:  "sess1",
			Ordinal:    1,
			Role:       "assistant",
			Timestamp:  "2024-06-15T10:31:00Z",
			Model:      "gpt-5",
			TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
		},
	)

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:  "2024-06-01",
		To:    "2024-06-30",
		Model: "gpt-5",
	})
	requireNoError(t, err, "GetDailyUsage model filter")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	assert.Equal(t, 1000, day.InputTokens, "InputTokens")
	assert.Equal(t, []string{"gpt-5"}, day.ModelsUsed, "ModelsUsed")
}

func TestGetDailyUsageProjectBreakdowns(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "sess-a", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-a",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	insertSession(t, d, "sess-b", "proj-b", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-b",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage project breakdowns")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	require.Len(t, day.ProjectBreakdowns, 2, "ProjectBreakdowns len")

	projMap := make(map[string]ProjectBreakdown)
	var projCostSum float64
	for _, pb := range day.ProjectBreakdowns {
		projMap[pb.Project] = pb
		projCostSum += pb.Cost
	}
	for _, name := range []string{"proj-a", "proj-b"} {
		pb, ok := projMap[name]
		if !assert.Truef(t, ok,
			"missing ProjectBreakdown for %s", name) {
			continue
		}
		assert.Equal(t, 1000, pb.InputTokens,
			"%s InputTokens", name)
	}
	assert.InDelta(t, day.TotalCost, projCostSum, 1e-9,
		"sum(ProjectBreakdowns.Cost) want TotalCost")
}

func TestGetDailyUsageAgentBreakdowns(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "sess-claude", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-claude",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	insertSession(t, d, "sess-codex", "proj1", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "sess-codex",
		Ordinal:    0,
		Role:       "assistant",
		Timestamp:  "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage agent breakdowns")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]
	require.Len(t, day.AgentBreakdowns, 2, "AgentBreakdowns len")

	agentMap := make(map[string]AgentBreakdown)
	var agentCostSum float64
	for _, ab := range day.AgentBreakdowns {
		agentMap[ab.Agent] = ab
		agentCostSum += ab.Cost
	}
	for _, name := range []string{"claude", "codex"} {
		ab, ok := agentMap[name]
		if !assert.Truef(t, ok,
			"missing AgentBreakdown for %s", name) {
			continue
		}
		assert.Equal(t, 1000, ab.InputTokens,
			"%s InputTokens", name)
	}
	assert.InDelta(t, day.TotalCost, agentCostSum, 1e-9,
		"sum(AgentBreakdowns.Cost) want TotalCost")
}

func TestGetDailyUsageBreakdownInvariant(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "model-a", InputPerMTok: 2.0,
			OutputPerMTok: 10.0},
		{ModelPattern: "model-b", InputPerMTok: 4.0,
			OutputPerMTok: 20.0},
	}), "UpsertModelPricing")

	// 2 projects x 2 agents = 4 sessions, each with 2 messages
	// from different models.
	type combo struct {
		project string
		agent   string
	}
	combos := []combo{
		{"proj-a", "claude"},
		{"proj-a", "codex"},
		{"proj-b", "claude"},
		{"proj-b", "codex"},
	}
	for i, c := range combos {
		sid := "sess-" + strconv.Itoa(i)
		insertSession(t, d, sid, c.project, func(s *Session) {
			s.Agent = c.agent
			s.StartedAt = new("2024-06-15T10:00:00Z")
		})
		insertMessages(t, d,
			Message{
				SessionID:  sid,
				Ordinal:    0,
				Role:       "assistant",
				Timestamp:  "2024-06-15T10:30:00Z",
				Model:      "model-a",
				TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
			},
			Message{
				SessionID:  sid,
				Ordinal:    1,
				Role:       "assistant",
				Timestamp:  "2024-06-15T10:31:00Z",
				Model:      "model-b",
				TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
			},
		)
	}

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	})
	requireNoError(t, err, "GetDailyUsage breakdown invariant")

	require.Len(t, result.Daily, 1, "got")
	day := result.Daily[0]

	var modelCostSum float64
	for _, mb := range day.ModelBreakdowns {
		modelCostSum += mb.Cost
	}
	var projectCostSum float64
	for _, pb := range day.ProjectBreakdowns {
		projectCostSum += pb.Cost
	}
	var agentCostSum float64
	for _, ab := range day.AgentBreakdowns {
		agentCostSum += ab.Cost
	}

	assert.InDelta(t, day.TotalCost, modelCostSum, 1e-9,
		"sum(ModelBreakdowns.Cost) want TotalCost")
	assert.InDelta(t, day.TotalCost, projectCostSum, 1e-9,
		"sum(ProjectBreakdowns.Cost) want TotalCost")
	assert.InDelta(t, day.TotalCost, agentCostSum, 1e-9,
		"sum(AgentBreakdowns.Cost) want TotalCost")
	assert.InDelta(t, projectCostSum, modelCostSum, 1e-9,
		"model cost sum != project cost sum")
	assert.InDelta(t, agentCostSum, modelCostSum, 1e-9,
		"model cost sum != agent cost sum")
}

// BenchmarkGetDailyUsage measures the hot-path scan over a realistic
// synthetic dataset. The baseline number (captured against the commit
// that introduces this benchmark) is the non-regression budget for all
// subsequent changes to GetDailyUsage: new code must land within +10%.
//
// See docs/specs/2026-04-12-token-usage-ui-design.md for the full
// non-destructive benchmark procedure.
func TestGetTopSessionsByCost(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet",
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheCreationPerMTok: 3.75,
		CacheReadPerMTok:     0.30,
	}}), "UpsertModelPricing")

	// Expensive session
	insertSession(t, d, "sBig", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.SessionName = new("Big Session")
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "sBig", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":5000,"output_tokens":2000,` +
				`"cache_creation_input_tokens":1000,` +
				`"cache_read_input_tokens":3000}`),
	})

	// Cheap session
	insertSession(t, d, "sSmall", "proj-b", func(s *Session) {
		s.Agent = "codex"
		s.SessionName = new("Small Session")
		s.StartedAt = new("2024-06-15T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "sSmall", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T11:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":100,"output_tokens":50,` +
				`"cache_creation_input_tokens":10,` +
				`"cache_read_input_tokens":20}`),
	})

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	}, 20)
	requireNoError(t, err, "GetTopSessionsByCost")

	require.Len(t, top, 2, "len")

	// Ordered cost desc — sBig first
	assert.Equal(t, "sBig", top[0].SessionID, "top[0].SessionID")
	assert.Equal(t, "Big Session", top[0].DisplayName, "top[0].DisplayName")
	assert.Equal(t, "proj-a", top[0].Project, "top[0].Project")
	assert.Equal(t, "claude", top[0].Agent, "top[0].Agent")
	// TotalTokens = 5000 + 2000 + 1000 + 3000 = 11000
	assert.Equal(t, 11000, top[0].TotalTokens, "top[0].TotalTokens")
	assert.Greater(t, top[0].Cost, 0.0, "top[0].Cost want > 0")

	assert.Equal(t, "sSmall", top[1].SessionID, "top[1].SessionID")
	assert.Greater(t, top[0].Cost, top[1].Cost,
		"top[0].Cost should be > top[1].Cost")
}

func TestGetTopSessionsByCost_DisplayNameFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet",
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheCreationPerMTok: 3.75,
		CacheReadPerMTok:     0.30,
	}}), "UpsertModelPricing")

	tokenJSON := `{"input_tokens":100,"output_tokens":50,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`

	// Session with session_name set — should use session_name via COALESCE.
	insertSession(t, d, "s-dn", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.SessionName = new("My Custom Name")
		s.FirstMessage = new("some first message")
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-dn", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:01:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(tokenJSON),
	})

	// Session with no display_name — should fall back to first_message.
	insertSession(t, d, "s-fm", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.FirstMessage = new("fix the login bug")
		s.StartedAt = new("2024-06-15T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-fm", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T11:01:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(tokenJSON),
	})

	// Session with no display_name and no first_message — should
	// fall back to project.
	insertSession(t, d, "s-proj", "my-project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T12:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-proj", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T12:01:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(tokenJSON),
	})

	// Session with no display_name, no first_message, and empty
	// project — should fall back to session ID.
	insertSession(t, d, "s-id", "", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T13:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s-id", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T13:01:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(tokenJSON),
	})

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	}, 20)
	requireNoError(t, err, "GetTopSessionsByCost fallback")

	require.Len(t, top, 4, "len")

	// Build a map for easy lookup (order is by cost, all equal
	// here so secondary sort is by session ID).
	byID := make(map[string]TopSessionEntry)
	for _, e := range top {
		byID[e.SessionID] = e
	}

	assert.Equal(t, "My Custom Name", byID["s-dn"].DisplayName,
		"s-dn DisplayName")
	assert.Equal(t, "fix the login bug", byID["s-fm"].DisplayName,
		"s-fm DisplayName")
	assert.Equal(t, "my-project", byID["s-proj"].DisplayName,
		"s-proj DisplayName")
	assert.Equal(t, "s-id", byID["s-id"].DisplayName,
		"s-id DisplayName")
}

// TestGetTopSessionsByCost_DedupesByClaudeMessageAndRequestID
// mirrors TestGetDailyUsage_DedupesByClaudeMessageAndRequestID
// for the top-sessions query: a parent session and a forked
// session that both replay the same Claude message should only
// count that message once in the per-session totals. The
// earliest-timestamp session wins the credit.
func TestGetTopSessionsByCost_DedupesByClaudeMessageAndRequestID(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-sonnet",
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheCreationPerMTok: 3.75,
		CacheReadPerMTok:     0.30,
	}}), "UpsertModelPricing")

	// Parent session starts first.
	insertSession(t, d, "s-parent", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	// Forked session starts a minute later.
	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:01:00Z")
		s.ParentSessionID = new("s-parent")
		s.RelationshipType = "fork"
	})

	shared := json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500,` +
			`"cache_creation_input_tokens":200,` +
			`"cache_read_input_tokens":3000}`)
	unique := json.RawMessage(
		`{"input_tokens":10,"output_tokens":20,` +
			`"cache_creation_input_tokens":0,` +
			`"cache_read_input_tokens":0}`)

	// The shared message exists on both sessions with the same
	// Claude IDs; the parent's timestamp is earlier so it should
	// win the dedup.
	insertMessages(t, d, Message{
		SessionID: "s-parent", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:02:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", ClaudeRequestID: "req_dup",
	})
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:03:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", ClaudeRequestID: "req_dup",
	})
	// Plus a unique fork-only message so the fork still appears.
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 1,
		Role: "assistant", Timestamp: "2024-06-15T10:04:00Z",
		Model: "claude-sonnet", TokenUsage: unique,
		ClaudeMessageID: "msg_uniq", ClaudeRequestID: "req_uniq",
	})

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-15", To: "2024-06-15", Timezone: "UTC",
	}, 20)
	requireNoError(t, err, "GetTopSessionsByCost")

	require.Len(t, top, 2, "len")

	byID := map[string]TopSessionEntry{}
	for _, e := range top {
		byID[e.SessionID] = e
	}

	parent, ok := byID["s-parent"]
	require.True(t, ok, "s-parent missing from top sessions")
	// Parent owns shared: 1000+500+200+3000 = 4700 tokens.
	assert.Equal(t, 4700, parent.TotalTokens, "parent.TotalTokens")

	fork, ok := byID["s-fork"]
	require.True(t, ok, "s-fork missing from top sessions")
	// Fork should only own the unique message: 10+20 = 30
	// tokens. If the dedup were missing, the shared row would
	// be counted again and this would jump to 4730.
	assert.Equal(t, 30, fork.TotalTokens,
		"fork.TotalTokens want 30 "+
			"(shared message should be deduped)")

	// Total across both entries must equal the undeduped
	// message sum: parent 4700 + fork 30 = 4730.
	total := parent.TotalTokens + fork.TotalTokens
	assert.Equal(t, 4730, total, "sum of per-session totals")
}

func TestGetTopSessionsByCostLimit(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	for i := range 5 {
		sid := "sess-" + strconv.Itoa(i)
		insertSession(t, d, sid, "proj", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new("2024-06-15T10:00:00Z")
		})
		insertMessages(t, d, Message{
			SessionID: sid, Ordinal: 0,
			Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
			Model: "claude-sonnet",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`),
		})
	}

	top, err := d.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	}, 3)
	requireNoError(t, err, "GetTopSessionsByCost limit")

	require.Len(t, top, 3, "len")
}

func TestGetUsageSessionCounts(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// s1: proj-a / claude — TWO messages across TWO days
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "s1", Ordinal: 0,
			Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
			Model: "claude-sonnet",
			TokenUsage: json.RawMessage(
				`{"input_tokens":100,"output_tokens":50}`),
		},
		Message{
			SessionID: "s1", Ordinal: 1,
			Role: "assistant", Timestamp: "2024-06-16T10:30:00Z",
			Model: "claude-sonnet",
			TokenUsage: json.RawMessage(
				`{"input_tokens":200,"output_tokens":100}`),
		},
	)

	// s2: proj-a / codex
	insertSession(t, d, "s2", "proj-a", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = new("2024-06-15T11:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s2", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T11:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":100,"output_tokens":50}`),
	})

	// s3: proj-b / claude
	insertSession(t, d, "s3", "proj-b", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T12:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "s3", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T12:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":100,"output_tokens":50}`),
	})

	counts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetUsageSessionCounts")

	assert.Equal(t, 3, counts.Total, "Total")
	assert.Equal(t, 2, counts.ByProject["proj-a"], "ByProject[proj-a]")
	assert.Equal(t, 1, counts.ByProject["proj-b"], "ByProject[proj-b]")
	assert.Equal(t, 2, counts.ByAgent["claude"], "ByAgent[claude]")
	assert.Equal(t, 1, counts.ByAgent["codex"], "ByAgent[codex]")

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})
	requireNoError(t, err, "GetDailyUsage")
	assert.Equal(t, counts, daily.SessionCounts)
}

func TestNewUsageSessionCounts(t *testing.T) {
	counts := NewUsageSessionCounts(map[string]UsageSessionInfo{
		"s1": {Project: "proj-a", Agent: "claude"},
		"s2": {Project: "proj-a", Agent: "codex"},
		"s3": {Project: "proj-b", Agent: "claude"},
	})

	assert.Equal(t, 3, counts.Total, "Total")
	assert.Equal(t, map[string]int{
		"proj-a": 2,
		"proj-b": 1,
	}, counts.ByProject, "ByProject")
	assert.Equal(t, map[string]int{
		"claude": 2,
		"codex":  1,
	}, counts.ByAgent, "ByAgent")
}

// TestGetUsageSessionCounts_DedupesByClaudeMessageAndRequestID
// mirrors the dedup regression coverage on the other two usage
// queries. A fork session whose only qualifying messages are
// replays of its parent's (same claude_message_id +
// claude_request_id) contributes zero cost after dedup in
// GetDailyUsage, so it must also NOT be counted in
// GetUsageSessionCounts — otherwise the summary cards disagree
// with the charts.
func TestGetUsageSessionCounts_DedupesByClaudeMessageAndRequestID(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	// Parent starts first.
	insertSession(t, d, "s-parent", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	// Fork starts a minute later.
	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:01:00Z")
		s.ParentSessionID = new("s-parent")
		s.RelationshipType = "fork"
	})

	shared := json.RawMessage(
		`{"input_tokens":100,"output_tokens":50}`)

	// Parent has one unique message.
	insertMessages(t, d, Message{
		SessionID: "s-parent", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:02:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", ClaudeRequestID: "req_dup",
	})
	// Fork's ONLY qualifying message is a replay of the parent
	// row — same claude IDs. After dedup the fork contributes
	// nothing and must not be counted.
	insertMessages(t, d, Message{
		SessionID: "s-fork", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:03:00Z",
		Model: "claude-sonnet", TokenUsage: shared,
		ClaudeMessageID: "msg_dup", ClaudeRequestID: "req_dup",
	})

	counts, err := d.GetUsageSessionCounts(ctx, UsageFilter{
		From: "2024-06-15", To: "2024-06-15", Timezone: "UTC",
	})
	requireNoError(t, err, "GetUsageSessionCounts")

	assert.Equal(t, 1, counts.Total,
		"Total want 1 (fork should dedup out)")
	assert.Equal(t, 1, counts.ByProject["proj"], "ByProject[proj]")
	assert.Equal(t, 1, counts.ByAgent["claude"], "ByAgent[claude]")
}

// TestUsageQueryEligibilityParity seeds messages that fail each
// disqualification predicate and asserts all three usage queries
// ignore them. Guardrail against drift between usage queries.
func TestUsageQueryEligibilityParity(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	// Good session — should be visible to all queries.
	insertSession(t, d, "good", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "good", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":1000,"output_tokens":500}`),
	})

	// Bad: empty token_usage
	insertSession(t, d, "bad-empty", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "bad-empty", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model:      "claude-sonnet",
		TokenUsage: json.RawMessage(""),
	})

	// Bad: synthetic model
	insertSession(t, d, "bad-synth", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "bad-synth", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "<synthetic>",
		TokenUsage: json.RawMessage(
			`{"input_tokens":999,"output_tokens":999}`),
	})

	// Bad: soft-deleted session
	insertSession(t, d, "bad-deleted", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "bad-deleted", Ordinal: 0,
		Role: "assistant", Timestamp: "2024-06-15T10:30:00Z",
		Model: "claude-sonnet",
		TokenUsage: json.RawMessage(
			`{"input_tokens":999,"output_tokens":999}`),
	})
	requireNoError(t,
		d.SoftDeleteSession("bad-deleted"),
		"SoftDeleteSession")

	filter := UsageFilter{
		From:       "2024-06-01",
		To:         "2024-06-30",
		Breakdowns: true,
	}

	// GetDailyUsage
	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage parity")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "GetDailyUsage InputTokens")

	// GetUsageSessionCounts
	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts parity")
	assert.Equal(t, 1, counts.Total, "GetUsageSessionCounts Total")

	// GetTopSessionsByCost
	top, err := d.GetTopSessionsByCost(ctx, filter, 20)
	requireNoError(t, err, "GetTopSessionsByCost parity")
	require.Len(t, top, 1, "GetTopSessionsByCost len")
	assert.Equal(t, "good", top[0].SessionID,
		"GetTopSessionsByCost[0].SessionID")
}

// TestExcludeProjectFilter verifies that ExcludeProject removes
// matching projects from all three usage queries.
func TestExcludeProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "sA", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "sB", "proj-b", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "sC", "proj-c", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	usage := `{"input_tokens":1000,"output_tokens":500}`
	insertMessages(t, d,
		Message{SessionID: "sA", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: json.RawMessage(usage)},
		Message{SessionID: "sB", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: json.RawMessage(usage)},
		Message{SessionID: "sC", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: json.RawMessage(usage)},
	)

	base := UsageFilter{From: "2024-06-01", To: "2024-06-30"}

	// Exclude one project.
	f1 := base
	f1.ExcludeProject = "proj-b"
	daily, err := d.GetDailyUsage(ctx, f1)
	requireNoError(t, err, "GetDailyUsage exclude one")
	assert.Equal(t, 2000, daily.Totals.InputTokens, "exclude proj-b: InputTokens")

	// Exclude two projects (comma-separated).
	f2 := base
	f2.ExcludeProject = "proj-a,proj-c"
	daily, err = d.GetDailyUsage(ctx, f2)
	requireNoError(t, err, "GetDailyUsage exclude two")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "exclude a+c: InputTokens")

	// GetTopSessionsByCost with exclude.
	top, err := d.GetTopSessionsByCost(ctx, f1, 10)
	requireNoError(t, err, "GetTopSessionsByCost exclude")
	require.Len(t, top, 2, "exclude proj-b: top len =")
	for _, ts := range top {
		assert.NotEqual(t, "proj-b", ts.Project,
			"excluded proj-b still in top sessions")
	}

	// GetUsageSessionCounts with exclude.
	counts, err := d.GetUsageSessionCounts(ctx, f1)
	requireNoError(t, err, "GetUsageSessionCounts exclude")
	assert.Equal(t, 2, counts.Total, "exclude proj-b: Total")
	assert.Equal(t, 0, counts.ByProject["proj-b"], "excluded proj-b count")
}

func TestUsageSessionFilters(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	tokenUsage := json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500}`,
	)

	insertSession(t, d, "usage-filter-keep", "proj", func(s *Session) {
		s.Machine = "host-a"
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-machine", "proj", func(s *Session) {
		s.Machine = "host-b"
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-prompts", "proj", func(s *Session) {
		s.Machine = "host-a"
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 1
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-one-shot", "proj", func(s *Session) {
		s.Machine = "host-a"
		s.Agent = "claude"
		s.MessageCount = 1
		s.UserMessageCount = 1
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-filter-automated", "proj", func(s *Session) {
		s.Machine = "host-a"
		s.Agent = "claude"
		s.MessageCount = 4
		s.UserMessageCount = 3
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 1 WHERE id = ?",
		"usage-filter-automated",
	)
	require.NoError(t, err, "patch automated fixture")

	for _, sid := range []string{
		"usage-filter-keep",
		"usage-filter-machine",
		"usage-filter-prompts",
		"usage-filter-one-shot",
		"usage-filter-automated",
	} {
		insertMessages(t, d, Message{
			SessionID:  sid,
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-15T10:30:00Z",
			Model:      "claude-sonnet",
			TokenUsage: tokenUsage,
		})
	}

	filter := UsageFilter{
		From:             "2024-06-01",
		To:               "2024-06-30",
		Machine:          "host-a",
		MinUserMessages:  2,
		ExcludeOneShot:   true,
		ExcludeAutomated: true,
	}

	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage session filters")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "InputTokens")

	top, err := d.GetTopSessionsByCost(ctx, filter, 10)
	requireNoError(t, err, "GetTopSessionsByCost session filters")
	require.Len(t, top, 1,
		"top sessions want only usage-filter-keep: %+v", top)
	require.Equal(t, "usage-filter-keep", top[0].SessionID,
		"top sessions want only usage-filter-keep: %+v", top)

	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts session filters")
	assert.Equal(t, 1, counts.Total, "counts.Total")
}

func TestUsageExcludeOneShotUsesUserMessageCount(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	tokenUsage := json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500}`,
	)

	insertSession(t, d, "usage-one-user-message", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "usage-two-user-messages", "proj", func(s *Session) {
		s.Agent = "claude"
		s.MessageCount = 3
		s.UserMessageCount = 2
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	for _, sid := range []string{
		"usage-one-user-message",
		"usage-two-user-messages",
	} {
		insertMessages(t, d, Message{
			SessionID:  sid,
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  "2024-06-15T10:30:00Z",
			Model:      "claude-sonnet",
			TokenUsage: tokenUsage,
		})
	}

	filter := UsageFilter{
		From:           "2024-06-01",
		To:             "2024-06-30",
		ExcludeOneShot: true,
	}

	daily, err := d.GetDailyUsage(ctx, filter)
	requireNoError(t, err, "GetDailyUsage exclude one-shot")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "InputTokens")

	top, err := d.GetTopSessionsByCost(ctx, filter, 10)
	requireNoError(t, err, "GetTopSessionsByCost exclude one-shot")
	require.Len(t, top, 1,
		"top sessions want only usage-two-user-messages: %+v", top)
	require.Equal(t, "usage-two-user-messages", top[0].SessionID,
		"top sessions want only usage-two-user-messages: %+v", top)

	counts, err := d.GetUsageSessionCounts(ctx, filter)
	requireNoError(t, err, "GetUsageSessionCounts exclude one-shot")
	assert.Equal(t, 1, counts.Total, "counts.Total")
}

// TestExcludeAgentFilter verifies ExcludeAgent on GetDailyUsage.
func TestExcludeAgentFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})
	insertSession(t, d, "s2", "proj", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	usage := `{"input_tokens":1000,"output_tokens":500}`
	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: json.RawMessage(usage)},
		Message{SessionID: "s2", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "claude-sonnet",
			TokenUsage: json.RawMessage(usage)},
	)

	f := UsageFilter{
		From:         "2024-06-01",
		To:           "2024-06-30",
		ExcludeAgent: "codex",
	}
	daily, err := d.GetDailyUsage(ctx, f)
	requireNoError(t, err, "GetDailyUsage exclude agent")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "exclude codex: InputTokens")
}

// TestExcludeModelFilter verifies ExcludeModel on GetDailyUsage.
func TestExcludeModelFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "sonnet", InputPerMTok: 3.0,
			OutputPerMTok: 15.0},
		{ModelPattern: "opus", InputPerMTok: 15.0,
			OutputPerMTok: 75.0},
	}), "UpsertModelPricing")

	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-06-15T10:00:00Z")
	})

	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "assistant",
			Timestamp: "2024-06-15T10:30:00Z", Model: "sonnet",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`)},
		Message{SessionID: "s1", Ordinal: 1, Role: "assistant",
			Timestamp: "2024-06-15T11:30:00Z", Model: "opus",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`)},
	)

	f := UsageFilter{
		From:         "2024-06-01",
		To:           "2024-06-30",
		ExcludeModel: "opus",
	}
	daily, err := d.GetDailyUsage(ctx, f)
	requireNoError(t, err, "GetDailyUsage exclude model")
	assert.Equal(t, 1000, daily.Totals.InputTokens, "exclude opus: InputTokens")
	require.Len(t, daily.Daily, 1, "daily len =")
	for _, mb := range daily.Daily[0].ModelBreakdowns {
		assert.NotEqual(t, "opus", mb.ModelName,
			"excluded model opus still in breakdowns")
	}
}

func BenchmarkGetDailyUsage(b *testing.B) {
	d := testDB(&testing.T{})
	ctx := context.Background()

	if err := d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "claude-sonnet-4-20250514",
			InputPerMTok: 3.0, OutputPerMTok: 15.0,
			CacheCreationPerMTok: 3.75, CacheReadPerMTok: 0.30},
		{ModelPattern: "claude-opus-4-20250514",
			InputPerMTok: 15.0, OutputPerMTok: 75.0,
			CacheCreationPerMTok: 18.75, CacheReadPerMTok: 1.50},
		{ModelPattern: "gpt-5",
			InputPerMTok: 2.5, OutputPerMTok: 10.0,
			CacheCreationPerMTok: 2.5, CacheReadPerMTok: 0.25},
		{ModelPattern: "gemini-2.5-pro",
			InputPerMTok: 1.25, OutputPerMTok: 5.0,
			CacheCreationPerMTok: 1.25, CacheReadPerMTok: 0.125},
	}); err != nil {
		b.Fatalf("UpsertModelPricing: %v", err)
	}

	projects := []string{
		"agentsview", "quokka", "arrow-rs", "side-quests",
		"infrastructure", "blog", "experiments", "docs",
		"dotfiles", "playground",
	}
	agents := []string{"claude", "codex", "openhands"}
	models := []string{
		"claude-sonnet-4-20250514",
		"claude-opus-4-20250514",
		"gpt-5",
		"gemini-2.5-pro",
	}

	// 500 sessions × 200 messages each = 100k rows.
	const sessionCount = 500
	const msgsPerSession = 200

	tokenUsage := `{"input_tokens":1200,"output_tokens":480,` +
		`"cache_creation_input_tokens":300,` +
		`"cache_read_input_tokens":2400}`

	// Pre-parse the anchor timestamp once; the seed loop offsets from it.
	startTime, err := time.Parse(time.RFC3339, "2024-06-01T00:00:00Z")
	if err != nil {
		b.Fatalf("parsing start time: %v", err)
	}

	for i := range sessionCount {
		id := "bench-sess-" + strconv.Itoa(i)
		project := projects[i%len(projects)]
		agent := agents[i%len(agents)]
		// Spread sessions across a 60-day window.
		dayOffset := i % 60
		s := Session{
			ID:           id,
			Project:      project,
			Machine:      defaultMachine,
			Agent:        agent,
			MessageCount: msgsPerSession,
			StartedAt:    new(startTime.Format(time.RFC3339)),
		}
		if err := d.UpsertSession(s); err != nil {
			b.Fatalf("UpsertSession: %v", err)
		}
		msgs := make([]Message, msgsPerSession)
		for j := range msgsPerSession {
			msgs[j] = Message{
				SessionID:  id,
				Ordinal:    j,
				Role:       "assistant",
				Timestamp:  startTime.AddDate(0, 0, dayOffset).Format(time.RFC3339),
				Model:      models[(i+j)%len(models)],
				TokenUsage: json.RawMessage(tokenUsage),
			}
		}
		if err := d.InsertMessages(msgs); err != nil {
			b.Fatalf("InsertMessages: %v", err)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := d.GetDailyUsage(ctx, UsageFilter{
			From: "2024-06-01",
			To:   "2024-08-01",
		})
		if err != nil {
			b.Fatalf("GetDailyUsage: %v", err)
		}
	}
}

func TestGetDailyUsage_PricingPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		dbRates  []ModelPricing
		custom   map[string]config.CustomModelRate
		model    string
		input    int // input tokens
		output   int // output tokens
		wantCost float64
	}{
		{
			name:     "db pricing only",
			dbRates:  []ModelPricing{{ModelPattern: "acme-ultra-2.1", InputPerMTok: 1.0, OutputPerMTok: 4.0}},
			model:    "acme-ultra-2.1",
			input:    1_000_000,
			output:   100_000,
			wantCost: 1.4, // 1M*$1/M + 100k*$4/M
		},
		{
			name:     "custom overrides db for same model",
			dbRates:  []ModelPricing{{ModelPattern: "acme-ultra-2.1", InputPerMTok: 1.0, OutputPerMTok: 4.0}},
			custom:   map[string]config.CustomModelRate{"acme-ultra-2.1": {Input: 2.0, Output: 8.0}},
			model:    "acme-ultra-2.1",
			input:    1_000_000,
			output:   100_000,
			wantCost: 2.8, // 1M*$2/M + 100k*$8/M
		},
		{
			name:     "custom for unknown model, no db entry",
			custom:   map[string]config.CustomModelRate{"my-custom-model": {Input: 1.5, Output: 6.0}},
			model:    "my-custom-model",
			input:    500_000,
			output:   50_000,
			wantCost: 1.05, // 500k*$1.5/M + 50k*$6/M
		},
		{
			name:     "no pricing at all yields zero cost",
			model:    "unknown-model",
			input:    1_000_000,
			output:   100_000,
			wantCost: 0.0,
		},
		{
			name:     "custom only affects targeted model",
			dbRates:  []ModelPricing{{ModelPattern: "db-model", InputPerMTok: 3.0, OutputPerMTok: 10.0}},
			custom:   map[string]config.CustomModelRate{"other-model": {Input: 99.0, Output: 99.0}},
			model:    "db-model",
			input:    1_000_000,
			output:   100_000,
			wantCost: 4.0, // 1M*$3/M + 100k*$10/M -- db rates, not custom
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			if len(tt.dbRates) > 0 {
				requireNoError(t, d.UpsertModelPricing(tt.dbRates), "UpsertModelPricing")
			}
			if tt.custom != nil {
				d.SetCustomPricing(tt.custom)
			}

			insertSession(t, d, "s1", "proj", func(s *Session) {
				s.StartedAt = new("2024-06-15T10:00:00Z")
			})
			insertMessages(t, d, Message{
				SessionID:  "s1",
				Ordinal:    0,
				Role:       "assistant",
				Timestamp:  "2024-06-15T10:30:00Z",
				Model:      tt.model,
				TokenUsage: json.RawMessage(`{"input_tokens":` + strconv.Itoa(tt.input) + `,"output_tokens":` + strconv.Itoa(tt.output) + `}`),
			})

			result, err := d.GetDailyUsage(context.Background(), UsageFilter{
				From: "2024-06-01", To: "2024-06-30",
			})
			requireNoError(t, err, "GetDailyUsage")

			assert.InDelta(t, tt.wantCost, result.Totals.TotalCost, 0.01,
				"TotalCost")
		})
	}
}

func seedOpusPricing(t *testing.T, d *DB) {
	t.Helper()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "claude-opus-4-6",
		InputPerMTok: 5.0, OutputPerMTok: 25.0,
		CacheCreationPerMTok: 6.25, CacheReadPerMTok: 0.5,
	}}), "UpsertModelPricing")
}

func TestGetSessionUsage_PricedModel(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)

	insertSession(t, d, "claude:s1", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
		s.TotalOutputTokens = 500
		s.PeakContextTokens = 1200
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})
	insertMessages(t, d, Message{
		SessionID: "claude:s1", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
		TokenUsage: json.RawMessage(
			`{"input_tokens":1000,"output_tokens":500}`),
	})

	u, err := d.GetSessionUsage(ctx, "claude:s1")
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	require.True(t, u.HasCost, "HasCost = false, want true")
	assert.InDelta(t, 0.0175, u.CostUSD, 1e-9, "CostUSD")
	assert.Equal(t, 500, u.TotalOutputTokens,
		"TotalOutputTokens want 500")
	assert.Equal(t, 1200, u.PeakContextTokens,
		"PeakContextTokens want 1200")
	assert.Equal(t, []string{"claude-opus-4-6"}, u.Models, "Models")
	assert.Empty(t, u.UnpricedModels, "UnpricedModels")
}

func TestGetSessionUsage_UnpricedModel(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "claude:s2", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
		s.TotalOutputTokens = 500
		s.HasTotalOutputTokens = true
	})
	insertMessages(t, d, Message{
		SessionID: "claude:s2", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-05-20T10:30:00Z", Model: "local-llama-99",
		TokenUsage: json.RawMessage(
			`{"input_tokens":1000,"output_tokens":500}`),
	})

	u, err := d.GetSessionUsage(ctx, "claude:s2")
	requireNoError(t, err, "GetSessionUsage")
	assert.False(t, u.HasCost, "HasCost = true, want false (unpriced)")
	assert.Equal(t, 0.0, u.CostUSD, "CostUSD")
	assert.Equal(t, []string{"local-llama-99"}, u.UnpricedModels,
		"UnpricedModels")
}

func TestGetSessionUsage_MixedPricedUnpriced(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:s3", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "claude:s3", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`),
		},
		Message{
			SessionID: "claude:s3", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-20T10:31:00Z", Model: "local-llama-99",
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`),
		},
	)

	u, err := d.GetSessionUsage(ctx, "claude:s3")
	requireNoError(t, err, "GetSessionUsage")
	assert.False(t, u.HasCost, "HasCost = true, want false (mixed)")
	assert.Equal(t, 0.0, u.CostUSD, "CostUSD")
	assert.Equal(t, []string{"local-llama-99"}, u.UnpricedModels,
		"UnpricedModels")
}

func TestGetSessionUsage_ExplicitCostOnly(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "hermes:s4", "proj", func(s *Session) {
		s.Agent = "hermes"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	cost := 0.02
	require.NoError(t, d.ReplaceSessionUsageEvents("hermes:s4", []UsageEvent{{
		SessionID: "hermes:s4", Source: "session", Model: "gpt-5.4",
		InputTokens: 100, OutputTokens: 50,
		CostUSD: &cost, CostStatus: "estimated", CostSource: "hermes",
		OccurredAt: "2026-05-20T10:05:00Z", DedupKey: "session:hermes:s4",
	}}), "ReplaceSessionUsageEvents")

	u, err := d.GetSessionUsage(ctx, "hermes:s4")
	requireNoError(t, err, "GetSessionUsage")
	assert.True(t, u.HasCost, "HasCost = false, want true (explicit cost)")
	assert.InDelta(t, 0.02, u.CostUSD, 1e-9, "CostUSD")
	assert.Equal(t, []string{"gpt-5.4"}, u.Models, "Models")
}

func TestGetSessionUsage_DedupesDuplicateClaudeRows(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedOpusPricing(t, d)
	insertSession(t, d, "claude:s6", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
	})
	// Two rows sharing the same claude message+request id (a
	// fork/replay) must be counted once, not doubled.
	insertMessages(t, d,
		Message{
			SessionID: "claude:s6", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-20T10:30:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-1", ClaudeRequestID: "req-1",
			TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
		},
		Message{
			SessionID: "claude:s6", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-20T10:31:00Z", Model: "claude-opus-4-6",
			ClaudeMessageID: "msg-1", ClaudeRequestID: "req-1",
			TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
		},
	)
	u, err := d.GetSessionUsage(ctx, "claude:s6")
	requireNoError(t, err, "GetSessionUsage")
	// One row priced at 1000*5/1e6 + 500*25/1e6 = 0.0175; deduped, not 0.035.
	assert.InDelta(t, 0.0175, u.CostUSD, 1e-9, "CostUSD want 0.0175 (deduped)")
	assert.True(t, u.HasCost, "HasCost = false, want true")
}

func TestGetSessionUsage_NoTokenRowsKeepsMetadata(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "claude:s5", "proj", func(s *Session) {
		s.Agent = "claude-code"
		s.StartedAt = new("2026-05-20T10:00:00Z")
		s.TotalOutputTokens = 700
		s.PeakContextTokens = 3000
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})

	u, err := d.GetSessionUsage(ctx, "claude:s5")
	requireNoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	assert.Equal(t, 700, u.TotalOutputTokens,
		"TotalOutputTokens want 700")
	assert.Equal(t, 3000, u.PeakContextTokens,
		"PeakContextTokens want 3000")
	assert.True(t, u.HasTokenData, "HasTokenData = false, want true")
	assert.False(t, u.HasCost, "HasCost = true, want false (no cost rows)")
	assert.NotNil(t, u.Models, "Models = nil, want non-nil empty slice")
}

func TestGetSessionUsage_NotFound(t *testing.T) {
	d := testDB(t)
	u, err := d.GetSessionUsage(context.Background(), "nope:x")
	requireNoError(t, err, "GetSessionUsage")
	assert.Nil(t, u, "usage")
}
