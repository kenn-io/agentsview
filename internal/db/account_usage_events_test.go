package db

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

func TestAccountUsageEventsKeepAgentsDatesAndReportedZero(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	events := []CursorUsageEvent{
		{Agent: "grok", OccurredAt: "2026-11-05T00:30:00Z", Model: "", InputTokens: 12, CacheReadTokens: 128, OutputTokens: 7, DedupKey: "xai:item-a"},
		{Agent: "grok-bot", OccurredAt: "2026-11-04T13:00:00Z", Model: "", InputTokens: 11, CacheWriteTokens: 32, CacheReadTokens: 64, OutputTokens: 5, DedupKey: "xai:item-b"},
		{OccurredAt: "2026-11-04T13:00:00Z", Model: "grok-4.7-build", InputTokens: 9, OutputTokens: 3, DedupKey: "cursor:item-c"},
	}
	require.NoError(t, d.InsertCursorUsageEvents(ctx, events))
	require.NoError(t, d.InsertCursorUsageEvents(ctx, events))
	stored, err := d.GetCursorUsageEvents(ctx, 0)
	require.NoError(t, err)
	require.Len(t, stored, 3, "reimport must not duplicate billing items")
	stats, err := d.GetStats(ctx, false, false)
	require.NoError(t, err)
	assert.Nil(t, stats.EarliestSession)
	require.NotNil(t, stats.EarliestAccountUsage)
	assert.Equal(t, "2026-11-04T13:00:00Z", *stats.EarliestAccountUsage)
	for _, tc := range []struct {
		agent                    string
		input, output, cacheRead int
	}{
		{"grok", 12, 7, 128}, {"grok-bot", 11, 5, 64}, {"cursor", 9, 3, 0},
	} {
		result, err := d.GetDailyUsage(ctx, UsageFilter{From: "2026-11-04", To: "2026-11-04", Timezone: "America/New_York", Agent: tc.agent, Breakdowns: true})
		require.NoError(t, err)
		require.Len(t, result.Daily, 1, tc.agent)
		assert.Equal(t, tc.input, result.Daily[0].InputTokens)
		assert.Equal(t, tc.output, result.Daily[0].OutputTokens)
		assert.Equal(t, tc.cacheRead, result.Daily[0].CacheReadTokens)
		assert.Zero(t, result.Daily[0].TotalCost.Microdollars)
		require.Len(t, result.Daily[0].AgentBreakdowns, 1)
		assert.Equal(t, tc.agent, result.Daily[0].AgentBreakdowns[0].Agent)
		assert.Zero(t, result.SessionCounts.Total)
		assert.Empty(t, result.Projects)
	}
	result, err := d.GetDailyUsage(ctx, UsageFilter{From: "2026-11-04", To: "2026-11-04", Timezone: "America/New_York", ExcludeAgent: "grok-bot"})
	require.NoError(t, err)
	require.Len(t, result.Daily, 1)
	assert.Equal(t, 21, result.Daily[0].InputTokens)
	result, err = d.GetDailyUsage(ctx, UsageFilter{From: "2026-11-04", To: "2026-11-04", Timezone: "America/New_York", Agent: "grok, grok-bot", Breakdowns: true})
	require.NoError(t, err)
	require.Len(t, result.Daily, 1)
	assert.Len(t, result.Daily[0].AgentBreakdowns, 2, "unknown models from different products must not merge their agents")
	assert.Equal(t, 23, result.Daily[0].InputTokens)
	assert.Equal(t, 32, result.Daily[0].CacheCreationTokens)
	day, err := d.ExportReportingDay(ctx, ReportingExportOptions{
		Date: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.EqualValues(t, 12, day.Hours[0].Usage.Totals.InputTokens)
	require.Len(t, day.Hours[0].Usage.ByAgent, 1)
	assert.Equal(t, "grok", day.Hours[0].Usage.ByAgent[0].Key)
}

func TestAccountUsageAgentMigrationPreservesCursorRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "migration")
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	event := CursorUsageEvent{OccurredAt: "2026-11-04T13:00:00Z", Model: "model-a", InputTokens: 12, DedupKey: "cursor:item"}
	require.NoError(t, d.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{event}))
	_, err := d.rawWriter().ExecContext(t.Context(), "ALTER TABLE cursor_usage_events DROP COLUMN agent")
	require.NoError(t, err)
	require.NoError(t, d.Close())
	_, err = OpenReadOnly(t.Context(), path)
	require.Error(t, err)
	assert.True(t, IsSchemaUpgradeRequired(err))
	migrated, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, migrated.Close()) })
	rows, err := migrated.GetCursorUsageEvents(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "cursor", rows[0].Agent)
	assert.Equal(t, event.OccurredAt, rows[0].OccurredAt)
	assert.Equal(t, event.Model, rows[0].Model)
	assert.Equal(t, event.DedupKey, rows[0].DedupKey)
	assert.Equal(t, 12, rows[0].InputTokens)
}

func TestAccountUsageUsesBaseRatesForCacheSavings(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "billing-banded", InputPerMTok: money.MustParseDollars("10"),
		CacheCreationPerMTok: money.MustParseDollars("12"), CacheReadPerMTok: money.MustParseDollars("1"),
		Bands: []PricingBand{{
			AboveInputTokens: 200_000, InputPerMTok: money.MustParseDollars("20"),
			CacheCreationPerMTok: money.MustParseDollars("24"), CacheReadPerMTok: money.MustParseDollars("2"),
		}},
	}}))
	require.NoError(t, d.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{{
		Agent: "grok", OccurredAt: "2026-11-04T13:00:00Z", Model: "billing-banded",
		InputTokens: 300_000, CacheWriteTokens: 100_000, CacheReadTokens: 400_000, DedupKey: "xai:banded",
	}}))
	filter := UsageFilter{From: "2026-11-04", To: "2026-11-04", Timezone: "UTC", Agent: "grok"}
	got, err := d.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Zero(t, got.Totals.TotalCost.Microdollars)
	assert.Equal(t, money.MustParseDollars("3.40"), got.Totals.CacheSavings)
	live, err := d.getDailyUsageLegacy(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, live.Totals, got.Totals)
}
