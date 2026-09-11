package db

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestReportingExportCompletedEmptyDayHas24QuietHours(t *testing.T) {
	d := testDB(t)

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, export.ReportingSchemaVersion, day.SchemaVersion)
	assert.True(t, day.Complete)
	assert.False(t, day.HasData)
	assert.Equal(t, "sha256:308989b5c0df16c9d2d06fd050f3268632327d9faf9088f6dfb85ad9b221fc4c", day.Digest)
	require.Len(t, day.Hours, 24)
	for _, hour := range day.Hours {
		assert.False(t, hour.HasData)
		assert.Zero(t, hour.Activity.Totals.IdleMinutes)
		assert.Empty(t, hour.Activity.ByModel)
		assert.Empty(t, hour.Activity.ByAgent)
		assert.Empty(t, hour.Activity.ByProject)
		assert.Empty(t, hour.Usage.ByModel)
		assert.Empty(t, hour.Usage.ByAgent)
		assert.Empty(t, hour.Usage.ByProject)
		assert.Len(t, hour.Activity.Buckets, 12)
	}
}

func TestReportingExportSplitsActivityAndAssignsFirstSeenOnce(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "cross-hour", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-07-28T10:59:00Z")
		s.EndedAt = Ptr("2026-07-28T11:02:00Z")
	})
	seedMessage(
		t, d, "cross-hour", 1, "user", "2026-07-28T10:59:00Z", "",
	)
	seedMessage(
		t, d, "cross-hour", 2, "assistant", "2026-07-28T11:02:00Z", "opus",
	)

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	hour10 := day.Hours[10]
	hour11 := day.Hours[11]
	assert.True(t, hour10.HasData)
	assert.True(t, hour11.HasData)
	assert.InDelta(t, 1, hour10.Activity.Totals.AgentMinutes, 0.0001)
	assert.InDelta(t, 2, hour11.Activity.Totals.AgentMinutes, 0.0001)
	assert.Equal(t, 1, hour10.Activity.Totals.NewSessions)
	assert.Equal(t, 0, hour11.Activity.Totals.NewSessions)
	assert.Equal(t, 1, hour10.Activity.Totals.NewInteractiveSessions)
	assert.Equal(t, 1, hour10.Activity.Totals.NewProjects)
	assert.Equal(t, 0, hour10.Activity.Totals.NewModels)
	assert.Equal(t, 1, hour11.Activity.Totals.NewModels)
	assert.Equal(t, 1, hour10.Activity.Peak.Agents)
	assert.Equal(t, 1, hour11.Activity.Peak.Agents)
	require.Len(t, hour10.Activity.ByProject, 1)
	assert.Equal(t, "project-a", hour10.Activity.ByProject[0].Project)
	assert.NotEmpty(t, hour10.Activity.ByProject[0].ProjectKey)
	require.Len(t, hour10.Activity.ByModel, 1)
	assert.Equal(t, "opus", hour10.Activity.ByModel[0].Key)

	assert.False(t, day.Hours[9].HasData)
	assert.False(t, day.Hours[12].HasData)

	existing, err := d.GetActivityReport(
		context.Background(),
		AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-07-28", "UTC"),
	)
	require.NoError(t, err)
	var hourlyAgentMinutes float64
	for _, hour := range day.Hours {
		hourlyAgentMinutes += hour.Activity.Totals.AgentMinutes
	}
	assert.InDelta(t, existing.Totals.AgentMinutes, hourlyAgentMinutes, 0.0001)
}

func TestReportingExportPreservesPriorModelsAcrossHourWindows(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "carry", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-07-27T23:58:00Z")
		s.EndedAt = new("2026-07-28T00:02:00Z")
	})
	seedMessage(t, d, "carry", 1, "user", "2026-07-27T23:58:00Z", "")
	seedMessage(t, d, "carry", 2, "assistant", "2026-07-27T23:59:00Z", "prior-model")
	seedMessage(t, d, "carry", 3, "user", "2026-07-28T00:01:00Z", "")
	seedMessage(t, d, "carry", 4, "assistant", "2026-07-28T00:02:00Z", "")

	insertSession(t, d, "unordered", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-07-28T10:58:00Z")
		s.EndedAt = new("2026-07-28T11:02:00Z")
	})
	seedMessage(t, d, "unordered", 1, "user", "2026-07-28T10:58:00Z", "")
	seedMessage(t, d, "unordered", 2, "assistant", "2026-07-28T11:01:00Z", "model-a")
	// This backwards timestamp must not replace the inherited model.
	seedMessage(t, d, "unordered", 3, "assistant", "2026-07-28T10:59:00Z", "backwards-model")
	seedMessage(t, d, "unordered", 4, "user", "2026-07-28T11:02:00Z", "")

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.Len(t, day.Hours, 24)
	for _, tt := range []struct {
		hour  int
		model string
	}{
		{0, "prior-model"},
		{10, "model-a"},
		{11, "model-a"},
	} {
		hour := day.Hours[tt.hour]
		assert.InDelta(t, 2, hour.Activity.Totals.AgentMinutes, 0.0001, "hour %d", tt.hour)
		require.Len(t, hour.Activity.ByModel, 1, "hour %d", tt.hour)
		assert.Equal(t, tt.model, hour.Activity.ByModel[0].Key, "hour %d", tt.hour)
	}
}

func TestReportingExportCurrentDayOmitsOpenHour(t *testing.T) {
	d := testDB(t)

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.False(t, day.Complete)
	assert.Empty(t, day.Digest)
	require.Len(t, day.Hours, 14)
	assert.Equal(t, "2026-07-29-13", day.Hours[13].Period)
}

func TestReportingExportSeparatesSubagentsAndIndependentPeaks(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "model-a", OutputPerMTok: money.MustParseDollars("1"),
	}}))
	for _, session := range []struct {
		id, start, end      string
		subagent, automated bool
	}{
		{"root-a", "10:00:00", "10:02:00", false, false},
		{"root-b", "10:00:00", "10:01:00", false, false},
		{"child-a", "10:01:00", "10:03:00", true, false},
		{"child-b", "10:02:00", "10:04:00", true, false},
		{"automated-child", "10:02:00", "10:03:00", true, true},
		{"automated-a", "10:03:00", "10:05:00", false, true},
		{"automated-b", "10:03:00", "10:04:00", false, true},
		{"untimed-child", "10:04:00", "", true, false},
	} {
		start := "2026-07-28T" + session.start + "Z"
		insertSession(t, d, session.id, "project-a", func(s *Session) {
			s.Agent = "agent-a"
			s.StartedAt = new(start)
			s.IsAutomated = session.automated
			if session.subagent {
				s.ParentSessionID = new("root-a")
				s.RelationshipType = "subagent"
			}
		})
		seedMessage(t, d, session.id, 1, "user", start, "")
		if session.end != "" {
			insertMessages(t, d, Message{
				SessionID: session.id, Ordinal: 2, Role: "assistant",
				Content: "answer", Timestamp: "2026-07-28T" + session.end + "Z",
				Model: "model-a", TokenUsage: jsontext.Value(`{"output_tokens":10}`),
			})
		}
	}

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.Len(t, day.Hours, 24)
	hour := day.Hours[10]
	assert.Equal(t, 11.0, hour.Activity.Totals.AgentMinutes)
	assert.Equal(t, 3.0, hour.Activity.Totals.InteractiveAgentMinutes)
	assert.Equal(t, 5.0, hour.Activity.Totals.SubagentAgentMinutes)
	assert.Equal(t, 3.0, hour.Activity.Totals.AutomatedAgentMinutes)
	assert.Equal(t, money.Money{Microdollars: 70}, hour.Activity.Totals.Cost)
	assert.Equal(t, money.Money{Microdollars: 20}, hour.Activity.Totals.InteractiveCost)
	assert.Equal(t, money.Money{Microdollars: 30}, hour.Activity.Totals.SubagentCost)
	assert.Equal(t, money.Money{Microdollars: 20}, hour.Activity.Totals.AutomatedCost)
	assert.Equal(t, int64(70), hour.Activity.Totals.OutputTokens)
	assert.Equal(t, hour.Activity.Totals.Cost, hour.Usage.Totals.Cost)
	assert.Equal(t, 8, hour.Activity.Totals.NewSessions)
	assert.Equal(t, 2, hour.Activity.Totals.NewInteractiveSessions)
	assert.Equal(t, 4, hour.Activity.Totals.NewSubagentSessions)
	assert.Equal(t, 2, hour.Activity.Totals.NewAutomatedSessions)
	assert.Equal(t, 1, hour.Activity.Totals.NewUntimedSessions)
	assert.Zero(t, day.Hours[11].Activity.Totals.NewSessions)
	assert.Equal(t, export.ReportingActivityPeak{Agents: 3, At: new("2026-07-28T10:02:00Z")}, hour.Activity.Peak)
	assert.Equal(t, export.ReportingActivityPeak{Agents: 2, At: new("2026-07-28T10:00:00Z")}, hour.Activity.InteractivePeak)
	assert.Equal(t, export.ReportingActivityPeak{Agents: 3, At: new("2026-07-28T10:02:00Z")}, hour.Activity.SubagentPeak)
	assert.Equal(t, export.ReportingActivityPeak{Agents: 2, At: new("2026-07-28T10:03:00Z")}, hour.Activity.AutomatedPeak)
	require.Len(t, hour.Activity.Buckets, 12)
	bucket := hour.Activity.Buckets[0]
	assert.Equal(t, 3, bucket.MaxAgents)
	assert.Equal(t, 2, bucket.MaxInteractiveAgents)
	assert.Equal(t, 3, bucket.MaxSubagentAgents)
	assert.Equal(t, 2, bucket.MaxAutomatedAgents)
	assert.Zero(t, bucket.InteractiveAtPeak)
	assert.Equal(t, 3, bucket.SubagentAtPeak)
	assert.Zero(t, bucket.AutomatedAtPeak)
	for _, rows := range [][]export.ReportingActivityBreakdown{hour.Activity.ByModel, hour.Activity.ByAgent} {
		require.Len(t, rows, 1)
		assert.Equal(t, 11.0, rows[0].AgentMinutes)
		assert.Equal(t, 3.0, rows[0].InteractiveAgentMinutes)
		assert.Equal(t, 5.0, rows[0].SubagentAgentMinutes)
		assert.Equal(t, 3.0, rows[0].AutomatedAgentMinutes)
		assert.Equal(t, money.Money{Microdollars: 30}, rows[0].SubagentCost)
	}
	require.Len(t, hour.Activity.ByProject, 1)
	assert.Equal(t, 11.0, hour.Activity.ByProject[0].AgentMinutes)
	assert.Equal(t, 5.0, hour.Activity.ByProject[0].SubagentAgentMinutes)
	assert.Equal(t, money.Money{Microdollars: 30}, hour.Activity.ByProject[0].SubagentCost)
}

func TestReportingUsageBreakdownsIgnoreNilAccumulators(t *testing.T) {
	values := map[string]*reportingUsageAccum{"missing": nil}

	assert.Empty(t, reportingUsageBreakdowns(values))
	assert.Empty(t, reportingUsageProjectBreakdowns(values, nil))
}

func TestReportingExportAllocatesAuthoritativeSessionCostBeforeHourPartition(
	t *testing.T,
) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern: "copilot-model-a",
			InputPerMTok: money.MustParseDollars("10"),
		},
		{
			ModelPattern: "copilot-model-b",
			InputPerMTok: money.MustParseDollars("20"),
		},
	}))
	insertSession(t, d, "copilot:hourly-authoritative", "project-a", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = Ptr("2026-07-28T10:00:00Z")
		s.EndedAt = Ptr("2026-07-28T11:10:00Z")
	})
	reportedCost := money.MustParseDollars("0.03")
	require.NoError(t, d.ReplaceSessionUsageEvents(
		"copilot:hourly-authoritative",
		[]UsageEvent{
			{
				Source:      "shutdown",
				Model:       "copilot-model-a",
				InputTokens: 1_000_000,
				OccurredAt:  "2026-07-28T10:05:00Z",
				DedupKey:    "first",
			},
			{
				Source:      "shutdown",
				Model:       "copilot-model-b",
				InputTokens: 1_000_000,
				Cost:        &reportedCost,
				CostStatus:  "exact",
				CostSource:  CopilotReportedCostSource,
				OccurredAt:  "2026-07-28T11:10:00Z",
				DedupKey:    "final",
			},
		},
	))

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	hour10 := day.Hours[10]
	hour11 := day.Hours[11]
	assert.Equal(t, money.MustParseDollars("0.01"), hour10.Usage.Totals.Cost)
	assert.Equal(t, money.MustParseDollars("0.02"), hour11.Usage.Totals.Cost)
	assert.Equal(t, int64(1_000_000), hour10.Usage.Totals.InputTokens)
	assert.Equal(t, int64(1_000_000), hour11.Usage.Totals.InputTokens)
	assert.Equal(t, hour10.Usage.Totals.Cost, hour10.Activity.Totals.Cost)
	assert.Equal(t, hour11.Usage.Totals.Cost, hour11.Activity.Totals.Cost)
	assert.Equal(t, 1, hour10.Activity.Totals.NewModels)
	assert.Equal(t, 1, hour11.Activity.Totals.NewModels)

	existing, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-07-28", To: "2026-07-28", Timezone: "UTC",
	})
	require.NoError(t, err)
	var hourlyCost money.Money
	var hourlyInputTokens int64
	for _, hour := range day.Hours {
		hourlyCost = money.MustAdd(hourlyCost, hour.Usage.Totals.Cost)
		hourlyInputTokens += hour.Usage.Totals.InputTokens
	}
	assert.Equal(t, existing.Totals.TotalCost, hourlyCost)
	assert.Equal(t, int64(existing.Totals.InputTokens), hourlyInputTokens)
}

func TestReportingExportAllocatesAuthoritativeCostByDailyBreakdownKey(
	t *testing.T,
) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern: "model-a",
			InputPerMTok: money.MustParseDollars("1"),
		},
		{
			ModelPattern: "model-z",
			InputPerMTok: money.MustParseDollars("1"),
		},
	}))
	insertSession(t, d, "fixture-authoritative-key", "project-a", func(s *Session) {
		s.Agent = "agent-a"
		s.StartedAt = Ptr("2026-07-28T10:00:00Z")
		s.EndedAt = Ptr("2026-07-28T10:03:00Z")
	})
	reportedCost := money.Money{Microdollars: 1}
	require.NoError(t, d.ReplaceSessionUsageEvents(
		"fixture-authoritative-key",
		[]UsageEvent{
			{
				Source:      "fixture-source",
				Model:       "model-z",
				InputTokens: 1,
				OccurredAt:  "2026-07-28T10:01:00Z",
				DedupKey:    "first",
			},
			{
				Source:      "fixture-source",
				Model:       "model-a",
				InputTokens: 1,
				Cost:        &reportedCost,
				CostStatus:  "exact",
				CostSource:  CopilotReportedCostSource,
				OccurredAt:  "2026-07-28T10:02:00Z",
				DedupKey:    "second",
			},
		},
	))

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	daily, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From:       "2026-07-28",
		To:         "2026-07-28",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	require.NoError(t, err)

	require.Len(t, daily.Daily, 1)
	exportedCosts := make(map[string]money.Money)
	for _, breakdown := range day.Hours[10].Usage.ByModel {
		exportedCosts[breakdown.Key] = breakdown.Cost
	}
	dailyCosts := make(map[string]money.Money)
	for _, breakdown := range daily.Daily[0].ModelBreakdowns {
		dailyCosts[breakdown.ModelName] = breakdown.Cost
	}
	assert.Equal(t, money.Money{}, exportedCosts["model-a"])
	assert.Equal(t, money.Money{Microdollars: 1}, exportedCosts["model-z"])
	assert.Equal(t, dailyCosts, exportedCosts)
}

func TestReportingExportPreservesZeroWeightAuthoritativeBreakdownKeys(
	t *testing.T,
) {
	d := testDB(t)
	insertSession(t, d, "fixture-authoritative-zero", "project-a", func(s *Session) {
		s.Agent = "agent-a"
		s.StartedAt = Ptr("2026-07-28T10:00:00Z")
		s.EndedAt = Ptr("2026-07-28T10:03:00Z")
	})
	reportedCost := money.Money{Microdollars: 1}
	require.NoError(t, d.ReplaceSessionUsageEvents(
		"fixture-authoritative-zero",
		[]UsageEvent{
			{
				Source:     "fixture-source",
				Model:      "model-z",
				OccurredAt: "2026-07-28T10:01:00Z",
				DedupKey:   "first",
			},
			{
				Source:     "fixture-source",
				Model:      "model-a",
				Cost:       &reportedCost,
				CostStatus: "exact",
				CostSource: CopilotReportedCostSource,
				OccurredAt: "2026-07-28T10:02:00Z",
				DedupKey:   "second",
			},
		},
	))

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	daily, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From:       "2026-07-28",
		To:         "2026-07-28",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	require.NoError(t, err)

	require.Len(t, daily.Daily, 1)
	exportedCosts := make(map[string]money.Money)
	for _, breakdown := range day.Hours[10].Usage.ByModel {
		exportedCosts[breakdown.Key] = breakdown.Cost
	}
	dailyCosts := make(map[string]money.Money)
	for _, breakdown := range daily.Daily[0].ModelBreakdowns {
		dailyCosts[breakdown.ModelName] = breakdown.Cost
	}
	assert.Equal(t, money.Money{}, exportedCosts["model-a"])
	assert.Equal(t, money.Money{Microdollars: 1}, exportedCosts["model-z"])
	assert.Equal(t, dailyCosts, exportedCosts)
}

func TestReportingExportClampsStandaloneUsageTokens(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{
		{
			OccurredAt:       "2026-07-28T09:05:00Z",
			Model:            "model-negative",
			Kind:             "usage",
			InputTokens:      -1,
			OutputTokens:     -2,
			CacheWriteTokens: -3,
			CacheReadTokens:  -4,
			DedupKey:         "negative",
		},
		{
			OccurredAt:       "2026-07-28T09:06:00Z",
			Model:            "model-oversized",
			Kind:             "usage",
			InputTokens:      MaxPlausibleTokens + 1,
			OutputTokens:     MaxPlausibleTokens + 2,
			CacheWriteTokens: MaxPlausibleTokens + 3,
			CacheReadTokens:  MaxPlausibleTokens + 4,
			DedupKey:         "oversized",
		},
	}))

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	daily, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From:       "2026-07-28",
		To:         "2026-07-28",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	require.NoError(t, err)

	hour := day.Hours[9]
	assert.Equal(t, int64(MaxPlausibleTokens), hour.Usage.Totals.InputTokens)
	assert.Equal(t, int64(MaxPlausibleTokens), hour.Usage.Totals.OutputTokens)
	assert.Equal(
		t, int64(MaxPlausibleTokens),
		hour.Usage.Totals.CacheCreationTokens,
	)
	assert.Equal(t, int64(MaxPlausibleTokens), hour.Usage.Totals.CacheReadTokens)
	assert.Equal(t, daily.Totals.InputTokens, int(hour.Usage.Totals.InputTokens))
	assert.Equal(t, daily.Totals.OutputTokens, int(hour.Usage.Totals.OutputTokens))
	assert.Equal(
		t,
		daily.Totals.CacheCreationTokens,
		int(hour.Usage.Totals.CacheCreationTokens),
	)
	assert.Equal(
		t,
		daily.Totals.CacheReadTokens,
		int(hour.Usage.Totals.CacheReadTokens),
	)
	require.Len(t, hour.Usage.ByModel, 2)
	for _, breakdown := range hour.Usage.ByModel {
		switch breakdown.Key {
		case "model-negative":
			assert.Zero(t, breakdown.InputTokens)
			assert.Zero(t, breakdown.OutputTokens)
			assert.Zero(t, breakdown.CacheCreationTokens)
			assert.Zero(t, breakdown.CacheReadTokens)
		case "model-oversized":
			assert.Equal(t, int64(MaxPlausibleTokens), breakdown.InputTokens)
			assert.Equal(t, int64(MaxPlausibleTokens), breakdown.OutputTokens)
			assert.Equal(
				t,
				int64(MaxPlausibleTokens),
				breakdown.CacheCreationTokens,
			)
			assert.Equal(t, int64(MaxPlausibleTokens), breakdown.CacheReadTokens)
		default:
			assert.Fail(t, "unexpected model breakdown", breakdown.Key)
		}
	}
}

func TestReportingExportUsesOneCoherentReadSnapshot(t *testing.T) {
	d := testDB(t)
	opts := ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	}
	opts.afterSnapshot = func() {
		insertSession(t, d, "arrived-after-snapshot", "project-a", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = Ptr("2026-07-28T10:00:00Z")
			s.EndedAt = Ptr("2026-07-28T10:02:00Z")
		})
		seedMessage(
			t, d, "arrived-after-snapshot", 1, "user",
			"2026-07-28T10:00:00Z", "",
		)
		seedMessage(
			t, d, "arrived-after-snapshot", 2, "assistant",
			"2026-07-28T10:02:00Z", "opus",
		)
	}

	first, err := d.ExportReportingDay(context.Background(), opts)
	require.NoError(t, err)
	assert.False(t, first.HasData)

	opts.afterSnapshot = nil
	second, err := d.ExportReportingDay(context.Background(), opts)
	require.NoError(t, err)
	assert.True(t, second.HasData)
	assert.Equal(t, 1, second.Hours[10].Activity.Totals.NewSessions)
}

func TestReportingExportIncludesStandaloneRowsOnlyInUsage(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "session-linked", "project-a", func(s *Session) {
		s.Agent = "agent-a"
		s.StartedAt = Ptr("2026-07-28T11:00:00Z")
		s.EndedAt = Ptr("2026-07-28T11:02:00Z")
	})
	seedMessage(
		t, d, "session-linked", 1, "user", "2026-07-28T11:00:00Z", "",
	)
	seedMessage(
		t, d, "session-linked", 2, "assistant", "2026-07-28T11:02:00Z",
		"session-model",
	)
	sessionCost := money.MustParseDollars("0.007")
	require.NoError(t, d.ReplaceSessionUsageEvents(
		"session-linked",
		[]UsageEvent{{
			Source:       "session-source",
			Model:        "session-model",
			InputTokens:  17,
			OutputTokens: 5,
			Cost:         &sessionCost,
			CostStatus:   "exact",
			CostSource:   "reported",
			OccurredAt:   "2026-07-28T11:01:00Z",
			DedupKey:     "session-usage",
		}},
	))
	insertSession(t, d, "fixture-usage-only", "project usage-only", func(s *Session) {
		s.Agent = "agent usage-only"
		s.StartedAt = Ptr("2026-07-27T08:00:00Z")
		s.EndedAt = Ptr("2026-07-27T08:01:00Z")
	})
	usageOnlyCost := money.MustParseDollars("0.003")
	require.NoError(t, d.ReplaceSessionUsageEvents(
		"fixture-usage-only",
		[]UsageEvent{{
			Source:       "fixture-source",
			Model:        "model usage-only",
			InputTokens:  20,
			OutputTokens: 4,
			Cost:         &usageOnlyCost,
			CostStatus:   "exact",
			CostSource:   "reported",
			OccurredAt:   "2026-07-28T09:10:00Z",
			DedupKey:     "fixture-usage-only",
		}},
	))
	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{
		{
			OccurredAt:       "2026-07-28T09:05:00Z",
			Model:            "standalone-model-a",
			Kind:             "usage",
			InputTokens:      11,
			OutputTokens:     2,
			CacheWriteTokens: 3,
			CacheReadTokens:  5,
			Charged:          money.MustParseDollars("0.001"),
			DedupKey:         "standalone-interactive",
		},
		{
			OccurredAt:       "2026-07-28T10:05:00Z",
			Model:            "standalone-model-b",
			Kind:             "usage",
			InputTokens:      13,
			OutputTokens:     3,
			CacheWriteTokens: 7,
			CacheReadTokens:  9,
			Charged:          money.MustParseDollars("0.002"),
			IsHeadless:       true,
			DedupKey:         "standalone-headless",
		},
	}))

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	hour9 := day.Hours[9]
	assert.True(t, hour9.HasData)
	assert.Equal(t, int64(31), hour9.Usage.Totals.InputTokens)
	assert.Equal(t, int64(6), hour9.Usage.Totals.OutputTokens)
	assert.Equal(t, int64(3), hour9.Usage.Totals.CacheCreationTokens)
	assert.Equal(t, int64(5), hour9.Usage.Totals.CacheReadTokens)
	assert.Equal(t, money.MustParseDollars("0.004"), hour9.Usage.Totals.Cost)
	assert.ElementsMatch(
		t,
		[]string{"standalone-model-a", "model usage-only"},
		reportingUsageBreakdownKeys(hour9.Usage.ByModel),
	)
	assert.ElementsMatch(
		t,
		[]string{"cursor", "agent usage-only"},
		reportingUsageBreakdownKeys(hour9.Usage.ByAgent),
	)
	require.Len(t, hour9.Usage.ByProject, 1)
	assert.Equal(t, "project usage-only", hour9.Usage.ByProject[0].Project)
	assert.NotEmpty(t, hour9.Usage.ByProject[0].ProjectKey)
	assert.Zero(t, hour9.Activity.Totals.AgentMinutes)
	assert.Zero(t, hour9.Activity.Totals.OutputTokens)
	assert.Zero(t, hour9.Activity.Totals.Cost)
	assert.Zero(t, hour9.Activity.Totals.NewSessions)
	assert.Zero(t, hour9.Activity.Totals.NewAutomatedSessions)
	assert.Zero(t, hour9.Activity.Totals.NewInteractiveSessions)
	assert.Zero(t, hour9.Activity.Totals.NewUntimedSessions)
	assert.Zero(t, hour9.Activity.Totals.NewProjects)
	assert.Zero(t, hour9.Activity.Totals.NewModels)

	hour10 := day.Hours[10]
	assert.True(t, hour10.HasData)
	assert.Equal(t, int64(13), hour10.Usage.Totals.InputTokens)
	assert.Equal(t, int64(3), hour10.Usage.Totals.OutputTokens)
	assert.Equal(t, money.MustParseDollars("0.002"), hour10.Usage.Totals.Cost)
	assert.Empty(t, hour10.Usage.ByProject)
	assert.Zero(t, hour10.Activity.Totals.AgentMinutes)
	assert.Zero(t, hour10.Activity.Totals.NewModels)

	daily, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From:       "2026-07-28",
		To:         "2026-07-28",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	require.NoError(t, err)

	var exportedInput, exportedOutput int64
	var exportedCacheCreation, exportedCacheRead int64
	var exportedCost money.Money
	for _, hour := range day.Hours {
		exportedInput += hour.Usage.Totals.InputTokens
		exportedOutput += hour.Usage.Totals.OutputTokens
		exportedCacheCreation += hour.Usage.Totals.CacheCreationTokens
		exportedCacheRead += hour.Usage.Totals.CacheReadTokens
		exportedCost = money.MustAdd(exportedCost, hour.Usage.Totals.Cost)
	}
	assert.Equal(t, int64(daily.Totals.InputTokens), exportedInput)
	assert.Equal(t, int64(daily.Totals.OutputTokens), exportedOutput)
	assert.Equal(
		t, int64(daily.Totals.CacheCreationTokens), exportedCacheCreation,
	)
	assert.Equal(t, int64(daily.Totals.CacheReadTokens), exportedCacheRead)
	assert.Equal(t, daily.Totals.TotalCost, exportedCost)
}

func TestReportingExportUsesAttributedSessionMetadataForCompleteSnapshot(
	t *testing.T,
) {
	d := testDB(t)
	insertSession(t, d, "reporting-parent", "parent-project", func(s *Session) {
		s.Agent = "parent-agent"
		s.Machine = "parent-machine"
		s.StartedAt = Ptr("2026-07-28T09:00:00Z")
		s.EndedAt = Ptr("2026-07-28T09:05:00Z")
	})
	insertSession(t, d, "reporting-child", "child-project", func(s *Session) {
		s.Agent = "child-agent"
		s.Machine = "child-machine"
		s.StartedAt = Ptr("2026-07-28T09:01:00Z")
		s.EndedAt = Ptr("2026-07-28T09:06:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "reporting-parent", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-07-28T09:05:00Z", Model: "partial-model",
			TokenUsage: jsontext.Value(
				`{"input_tokens":10,"output_tokens":5}`),
			ClaudeMessageID: "reporting-message",
			ClaudeRequestID: "reporting-request",
		},
		Message{
			SessionID: "reporting-child", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-07-28T09:06:00Z", Model: "complete-model",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":631}`),
			ClaudeMessageID: "reporting-message",
			ClaudeRequestID: "reporting-request",
		},
	)

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	hour := day.Hours[9]
	assert.Equal(t, int64(631), hour.Usage.Totals.OutputTokens)
	assert.Equal(t, []string{"parent-agent"},
		reportingUsageBreakdownKeys(hour.Usage.ByAgent))
	require.Len(t, hour.Usage.ByProject, 1)
	assert.Equal(t, "parent-project", hour.Usage.ByProject[0].Project)
}

func TestReportingExportDeduplicatesMergedUsageInputs(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "fixture-dedup", "project dedup", func(s *Session) {
		s.Agent = "agent dedup"
		s.StartedAt = Ptr("2026-07-28T09:00:00Z")
		s.EndedAt = Ptr("2026-07-28T09:01:00Z")
	})
	sessionCost := money.MustParseDollars("0.002")
	require.NoError(t, d.ReplaceSessionUsageEvents(
		"fixture-dedup",
		[]UsageEvent{{
			Source:       "merged-source",
			Model:        "model merged-dedup",
			InputTokens:  41,
			OutputTokens: 7,
			Cost:         &sessionCost,
			CostStatus:   "exact",
			CostSource:   "reported",
			OccurredAt:   "2026-07-28T09:05:00Z",
			DedupKey:     "shared",
		}},
	))
	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt:   "2026-07-28T09:05:00Z",
		Model:        "model standalone winner",
		Kind:         "usage",
		InputTokens:  17,
		OutputTokens: 3,
		Charged:      money.MustParseDollars("0.007"),
		DedupKey:     "fixture-dedup:merged-source:shared",
	}}))

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	hour := day.Hours[9]
	assert.Equal(t, int64(17), hour.Usage.Totals.InputTokens)
	assert.Equal(t, int64(3), hour.Usage.Totals.OutputTokens)
	assert.Equal(t, money.MustParseDollars("0.007"), hour.Usage.Totals.Cost)
	require.Len(t, hour.Usage.ByModel, 1)
	assert.Equal(t, "model standalone winner", hour.Usage.ByModel[0].Key)
	assert.Empty(t, hour.Usage.ByProject)

	daily, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From:       "2026-07-28",
		To:         "2026-07-28",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	require.NoError(t, err)
	assert.Equal(t, daily.Totals.InputTokens, int(hour.Usage.Totals.InputTokens))
	assert.Equal(t, daily.Totals.OutputTokens, int(hour.Usage.Totals.OutputTokens))
	assert.Equal(t, daily.Totals.TotalCost, hour.Usage.Totals.Cost)
	assert.Zero(t, hour.Activity.Totals.AgentMinutes)
	assert.Equal(t, 1, hour.Activity.Totals.NewSessions)
	assert.Equal(t, 1, hour.Activity.Totals.NewProjects)
	assert.Zero(t, hour.Activity.Totals.NewModels)
}

func TestReportingExportDedupMatchesDailyUsageForMixedTimestampPrecision(
	t *testing.T,
) {
	d := testDB(t)
	insertSession(t, d, "fixture-mixed-precision", "project precision", func(s *Session) {
		s.Agent = "agent precision"
		s.StartedAt = Ptr("2026-07-28T09:00:00Z")
		s.EndedAt = Ptr("2026-07-28T09:01:00Z")
	})
	sessionCost := money.MustParseDollars("0.002")
	require.NoError(t, d.ReplaceSessionUsageEvents(
		"fixture-mixed-precision",
		[]UsageEvent{{
			Source:       "fixture-source",
			Model:        "model earlier instant",
			InputTokens:  41,
			OutputTokens: 7,
			Cost:         &sessionCost,
			CostStatus:   "exact",
			CostSource:   "reported",
			OccurredAt:   "2026-07-28T09:00:00Z",
			DedupKey:     "shared",
		}},
	))
	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt:   "2026-07-28T09:00:00.123Z",
		Model:        "model text-order winner",
		Kind:         "usage",
		InputTokens:  17,
		OutputTokens: 3,
		Charged:      money.MustParseDollars("0.007"),
		DedupKey:     "fixture-mixed-precision:fixture-source:shared",
	}}))

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	daily, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From:       "2026-07-28",
		To:         "2026-07-28",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	require.NoError(t, err)

	hour := day.Hours[9]
	assert.Equal(t, int64(17), hour.Usage.Totals.InputTokens)
	assert.Equal(t, int64(3), hour.Usage.Totals.OutputTokens)
	assert.Equal(t, money.MustParseDollars("0.007"), hour.Usage.Totals.Cost)
	require.Len(t, hour.Usage.ByModel, 1)
	assert.Equal(t, "model text-order winner", hour.Usage.ByModel[0].Key)
	assert.Equal(t, daily.Totals.InputTokens, int(hour.Usage.Totals.InputTokens))
	assert.Equal(t, daily.Totals.OutputTokens, int(hour.Usage.Totals.OutputTokens))
	assert.Equal(t, daily.Totals.TotalCost, hour.Usage.Totals.Cost)
}

func TestReportingExportPreservesMessageOrdinalForDedup(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "fixture-ordinal", "", func(s *Session) {
		s.Agent = "agent ordinal"
		s.StartedAt = Ptr("2026-07-28T09:00:00Z")
		s.EndedAt = Ptr("2026-07-28T09:06:00Z")
	})
	require.NoError(t, d.InsertMessages([]Message{
		{
			SessionID: "fixture-ordinal",
			Ordinal:   0,
			Role:      "user",
			Content:   "synthetic prompt",
			Timestamp: "2026-07-28T09:00:00Z",
		},
		{
			SessionID:       "fixture-ordinal",
			Ordinal:         1,
			Role:            "assistant",
			Content:         "synthetic first response",
			Timestamp:       "2026-07-28T09:05:00Z",
			Model:           "model-z-ordinal-winner",
			ClaudeMessageID: "fixture-shared-message",
			ClaudeRequestID: "fixture-shared-request",
			TokenUsage: jsontext.Value(
				`{"input_tokens":29,"output_tokens":3}`,
			),
		},
		{
			SessionID:       "fixture-ordinal",
			Ordinal:         2,
			Role:            "assistant",
			Content:         "synthetic rewritten response",
			Timestamp:       "2026-07-28T09:05:00Z",
			Model:           "model-a-semantic-runner-up",
			ClaudeMessageID: "fixture-shared-message",
			ClaudeRequestID: "fixture-shared-request",
			TokenUsage: jsontext.Value(
				`{"input_tokens":11,"output_tokens":2}`,
			),
		},
	}))

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	hour := day.Hours[9]
	assert.Equal(t, int64(29), hour.Usage.Totals.InputTokens)
	assert.Equal(t, int64(3), hour.Usage.Totals.OutputTokens)
	require.Len(t, hour.Usage.ByModel, 1)
	assert.Equal(t, "model-z-ordinal-winner", hour.Usage.ByModel[0].Key)

	daily, err := d.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-07-28", To: "2026-07-28", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, daily.Totals.InputTokens, int(hour.Usage.Totals.InputTokens))
	assert.Equal(t, daily.Totals.OutputTokens, int(hour.Usage.Totals.OutputTokens))
}

func TestFinalizeReportingUsageOrdering(t *testing.T) {
	query := activity.Query{
		RangeStart: time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC),
		RangeEnd:   time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC),
		EffectiveEnd: time.Date(
			2026, 7, 28, 10, 0, 0, 0, time.UTC,
		),
	}
	tests := []struct {
		name      string
		rows      []activity.UsageRow
		wantInput int
		wantCost  money.Money
	}{
		{
			name: "lower ordinal wins before semantic fields",
			rows: []activity.UsageRow{
				{
					SessionID:      "fixture-session",
					MessageOrdinal: 2,
					UsageSource:    "message",
					UsageDedupKey:  "shared-ordinal",
					Timestamp:      "2026-07-28T09:05:00Z",
					Model:          "model-a",
					InputTokens:    11,
					Cost:           money.Money{Microdollars: 1100},
					CostSource:     export.CostSourceReported,
					Priced:         true,
					Contributes:    true,
				},
				{
					SessionID:      "fixture-session",
					MessageOrdinal: 1,
					UsageSource:    "usage_event",
					UsageDedupKey:  "shared-ordinal",
					Timestamp:      "2026-07-28T09:05:00Z",
					Model:          "model-z",
					InputTokens:    29,
					Cost:           money.Money{Microdollars: 2900},
					CostSource:     export.CostSourceReported,
					Priced:         true,
					Contributes:    true,
				},
			},
			wantInput: 29,
			wantCost:  money.Money{Microdollars: 2900},
		},
		{
			name: "source settles an equal primary prefix",
			rows: []activity.UsageRow{
				{
					SessionID:      "fixture-session",
					MessageOrdinal: 1,
					UsageSource:    "z-source",
					UsageDedupKey:  "shared-source",
					Timestamp:      "2026-07-28T09:05:00Z",
					Model:          "model-a",
					InputTokens:    13,
					Cost:           money.Money{Microdollars: 1300},
					CostSource:     export.CostSourceReported,
					Priced:         true,
					Contributes:    true,
				},
				{
					SessionID:      "fixture-session",
					MessageOrdinal: 1,
					UsageSource:    "a-source",
					UsageDedupKey:  "shared-source",
					Timestamp:      "2026-07-28T09:05:00Z",
					Model:          "model-z",
					InputTokens:    23,
					Cost:           money.Money{Microdollars: 2300},
					CostSource:     export.CostSourceReported,
					Priced:         true,
					Contributes:    true,
				},
			},
			wantInput: 23,
			wantCost:  money.Money{Microdollars: 2300},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				rows := append([]activity.UsageRow(nil), tt.rows...)
				if reverse {
					rows[0], rows[1] = rows[1], rows[0]
				}
				survivors, err := finalizeReportingUsage(query, rows, nil)
				require.NoError(t, err)
				require.Len(t, survivors, 1)
				assert.Equal(t, tt.wantInput, survivors[0].InputTokens)
				assert.Equal(t, tt.wantCost, survivors[0].Cost)
			}
		})
	}
}

func TestFinalizeReportingUsageAttributesCompleteSnapshotToEarliestSession(t *testing.T) {
	query := activity.Query{
		RangeStart:   time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC),
		RangeEnd:     time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC),
		EffectiveEnd: time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC),
	}
	rows := []activity.UsageRow{
		{
			SessionID:       "earlier-parent",
			MessageOrdinal:  1,
			UsageSource:     "message",
			Timestamp:       "2026-07-28T09:05:00Z",
			Model:           "model-a",
			OutputTokens:    100,
			Cost:            money.Money{Microdollars: 1000},
			CostSource:      export.CostSourceReported,
			Priced:          true,
			Contributes:     true,
			ClaudeMessageID: "shared-message",
			ClaudeRequestID: "shared-request",
		},
		{
			SessionID:       "later-child",
			MessageOrdinal:  1,
			UsageSource:     "message",
			Timestamp:       "2026-07-28T09:06:00Z",
			Model:           "model-a",
			OutputTokens:    900,
			Cost:            money.Money{Microdollars: 9000},
			CostSource:      export.CostSourceReported,
			Priced:          true,
			Contributes:     true,
			ClaudeMessageID: "shared-message",
			ClaudeRequestID: "shared-request",
		},
	}

	sessionByID := map[string]activity.SessionMeta{
		"earlier-parent": {
			SessionID: "earlier-parent",
			Agent:     "parent-agent",
			Project:   "parent-project",
			Machine:   "parent-machine",
		},
	}
	survivors, err := finalizeReportingUsage(query, rows, sessionByID)
	require.NoError(t, err)
	require.Len(t, survivors, 1)
	assert.Equal(t, "earlier-parent", survivors[0].SessionID)
	assert.Equal(t, "parent-agent", survivors[0].Agent)
	assert.Equal(t, "parent-project", survivors[0].Project)
	assert.Equal(t, "parent-machine", survivors[0].Machine)
	assert.Equal(t, 900, survivors[0].OutputTokens)
	assert.Equal(t, money.Money{Microdollars: 9000}, survivors[0].Cost)
}

func TestFinalizeReportingUsageAttributesEquivalentInstantBySessionID(
	t *testing.T,
) {
	query := activity.Query{
		RangeStart:   time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC),
		RangeEnd:     time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC),
		EffectiveEnd: time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC),
	}
	rows := []activity.UsageRow{
		{
			SessionID:       "a-parent",
			MessageOrdinal:  1,
			UsageSource:     "message",
			Timestamp:       "2026-07-28T09:05:00Z",
			OutputTokens:    100,
			ClaudeMessageID: "shared-message",
			ClaudeRequestID: "shared-request",
		},
		{
			SessionID:       "z-child",
			MessageOrdinal:  1,
			UsageSource:     "message",
			Timestamp:       "2026-07-28T09:05:00.000Z",
			OutputTokens:    900,
			ClaudeMessageID: "shared-message",
			ClaudeRequestID: "shared-request",
		},
	}
	sessionByID := map[string]activity.SessionMeta{
		"a-parent": {
			SessionID: "a-parent",
			Agent:     "parent-agent",
			Project:   "parent-project",
			Machine:   "parent-machine",
		},
	}

	survivors, err := finalizeReportingUsage(query, rows, sessionByID)
	require.NoError(t, err)
	require.Len(t, survivors, 1)
	assert.Equal(t, "a-parent", survivors[0].SessionID)
	assert.Equal(t, "parent-agent", survivors[0].Agent)
	assert.Equal(t, "parent-project", survivors[0].Project)
	assert.Equal(t, "parent-machine", survivors[0].Machine)
	assert.Equal(t, 900, survivors[0].OutputTokens)
}

func TestFinalizeReportingUsageCarriesWebSearchFeeToCompleteSnapshot(
	t *testing.T,
) {
	query := activity.Query{
		RangeStart:   time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC),
		RangeEnd:     time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC),
		EffectiveEnd: time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC),
	}
	rows := []activity.UsageRow{
		{
			SessionID:         "earlier-parent",
			Timestamp:         "2026-07-28T09:05:00Z",
			OutputTokens:      100,
			WebSearchRequests: 2,
			Cost:              money.MustParseDollars("0.22"),
			CostSource:        export.CostSourceComputed,
			Priced:            true,
			Contributes:       true,
			ClaudeMessageID:   "shared-message",
			ClaudeRequestID:   "shared-request",
		},
		{
			SessionID:       "later-child",
			Timestamp:       "2026-07-28T09:06:00Z",
			OutputTokens:    200,
			Cost:            money.MustParseDollars("0.50"),
			CostSource:      export.CostSourceComputed,
			Priced:          true,
			Contributes:     true,
			ClaudeMessageID: "shared-message",
			ClaudeRequestID: "shared-request",
		},
	}

	survivors, err := finalizeReportingUsage(query, rows, nil)
	require.NoError(t, err)
	require.Len(t, survivors, 1)
	assert.Equal(t, 200, survivors[0].OutputTokens)
	assert.Equal(t, 2, survivors[0].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.52"), survivors[0].Cost)
}

func reportingUsageBreakdownKeys(
	rows []export.ReportingUsageBreakdown,
) []string {
	keys := make([]string, len(rows))
	for i, row := range rows {
		keys[i] = row.Key
	}
	return keys
}

func TestReportingExportIsIndependentOfArchiveLayout(t *testing.T) {
	first := testDB(t)
	second := testDB(t)
	seedReportingLayoutArchive(t, first, false)
	seedReportingLayoutArchive(t, second, true)
	opts := ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	}

	firstDay, err := first.ExportReportingDay(context.Background(), opts)
	require.NoError(t, err)
	secondDay, err := second.ExportReportingDay(context.Background(), opts)
	require.NoError(t, err)
	_, firstBytes, err := export.FinalizeReportingDay(firstDay)
	require.NoError(t, err)
	_, secondBytes, err := export.FinalizeReportingDay(secondDay)
	require.NoError(t, err)

	assert.Equal(t, string(firstBytes), string(secondBytes))
	assert.Equal(t, firstDay.Digest, secondDay.Digest)
	assert.Equal(t, int64(10), firstDay.Hours[10].Usage.Totals.InputTokens)

	ids := make([]string, maxSQLVars+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("session-%03d", maxSQLVars-i)
	}
	events, err := first.activityReportActivityFrom(
		context.Background(), first.getReader(), ids,
	)
	require.NoError(t, err)
	require.Len(t, events, 2*(maxSQLVars+1))
	assert.Equal(t, "session-000", events[0].SessionID)
	assert.Equal(t, 1, events[0].Ordinal)
	assert.Equal(
		t,
		fmt.Sprintf("session-%03d", maxSQLVars),
		events[len(events)-1].SessionID,
	)
	assert.Equal(t, 2, events[len(events)-1].Ordinal)
}

func seedReportingLayoutArchive(t *testing.T, d *DB, reverse bool) {
	t.Helper()

	const sessionCount = maxSQLVars + 1
	messages := make([]Message, 0, sessionCount*2)
	for step := range sessionCount {
		index := step
		if reverse {
			index = sessionCount - step - 1
		}
		sessionID := fmt.Sprintf("session-%03d", index)
		insertSession(t, d, sessionID, "", func(s *Session) {
			s.Agent = "agent-a"
			s.StartedAt = Ptr("2026-07-28T10:00:00Z")
			s.EndedAt = Ptr("2026-07-28T10:05:00Z")
		})
		end := "2026-07-28T10:00:00.000000001Z"
		if index == 0 {
			end = "2026-07-28T10:04:59Z"
		}
		messages = append(messages,
			Message{
				SessionID: sessionID,
				Ordinal:   1,
				Role:      "user",
				Content:   "x",
				Timestamp: "2026-07-28T10:00:00Z",
			},
			Message{
				SessionID: sessionID,
				Ordinal:   2,
				Role:      "assistant",
				Content:   "x",
				Timestamp: end,
				Model:     "model-a",
			},
		)
	}
	require.NoError(t, d.InsertMessages(messages))

	firstCost := money.MustParseDollars("0.001")
	secondCost := money.MustParseDollars("0.009")
	usage := []UsageEvent{
		{
			Source:      "source-a",
			Model:       "model-a",
			InputTokens: 1,
			Cost:        &firstCost,
			CostStatus:  "exact",
			CostSource:  "reported",
			OccurredAt:  "2026-07-28T10:01:00Z",
			DedupKey:    "usage-tie",
		},
		{
			Source:      "source-b",
			Model:       "model-b",
			InputTokens: 9,
			Cost:        &secondCost,
			CostStatus:  "exact",
			CostSource:  "reported",
			OccurredAt:  "2026-07-28T10:01:00Z",
			DedupKey:    "usage-tie",
		},
	}
	if reverse {
		usage[0], usage[1] = usage[1], usage[0]
	}
	require.NoError(t, d.ReplaceSessionUsageEvents("session-000", usage))
}

func TestReportingHourDerivesAgentMinutes(t *testing.T) {
	report := activity.Report{
		Totals: activity.Totals{
			AgentMinutes:            0.500000001,
			AutomatedAgentMinutes:   0.25,
			InteractiveAgentMinutes: 0.25,
		},
		ByModel: []activity.KeyMinutes{{
			Key:                     "model α",
			AgentMinutes:            1,
			AutomatedAgentMinutes:   0.4,
			InteractiveAgentMinutes: 0.6,
		}},
		ByAgent: []activity.KeyMinutes{{
			Key:                     "agent β",
			AgentMinutes:            2,
			AutomatedAgentMinutes:   0.75,
			InteractiveAgentMinutes: 1.25,
		}},
		ByProject: []activity.KeyMinutes{{
			Key:                     "project 雪",
			ProjectKey:              "project-fixture-key",
			AgentMinutes:            3,
			AutomatedAgentMinutes:   1.25,
			InteractiveAgentMinutes: 1.75,
		}},
	}

	hour, err := reportingHourFromActivity(
		time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC),
		report,
		export.ReportingSchemaVersion,
	)
	require.NoError(t, err)
	assert.Equal(t, 0.5, hour.Activity.Totals.AgentMinutes)
	require.Len(t, hour.Activity.ByModel, 1)
	assert.Equal(
		t,
		hour.Activity.ByModel[0].AutomatedAgentMinutes+
			hour.Activity.ByModel[0].InteractiveAgentMinutes,
		hour.Activity.ByModel[0].AgentMinutes,
	)
	require.Len(t, hour.Activity.ByAgent, 1)
	assert.Equal(
		t,
		hour.Activity.ByAgent[0].AutomatedAgentMinutes+
			hour.Activity.ByAgent[0].InteractiveAgentMinutes,
		hour.Activity.ByAgent[0].AgentMinutes,
	)
	require.Len(t, hour.Activity.ByProject, 1)
	assert.Equal(
		t,
		hour.Activity.ByProject[0].AutomatedAgentMinutes+
			hour.Activity.ByProject[0].InteractiveAgentMinutes,
		hour.Activity.ByProject[0].AgentMinutes,
	)
}

func TestReportingHourRejectsInvalidAgentMinutes(t *testing.T) {
	tests := []struct {
		name        string
		original    float64
		automated   float64
		interactive float64
		subagent    float64
		want        float64
		wantErr     bool
	}{
		{
			name:        "consistent",
			original:    0.5,
			automated:   0.25,
			interactive: 0.25,
			want:        0.5,
		},
		{
			name: "subagent component", original: 1, automated: 0.25, interactive: 0.25, subagent: 0.5, want: 1,
		},
		{
			name: "negative subagent", subagent: -0.1, wantErr: true,
		},
		{
			name: "nan subagent", subagent: math.NaN(), wantErr: true,
		},
		{
			name: "infinite subagent", subagent: math.Inf(1), wantErr: true,
		},
		{
			name:        "inclusive tolerance boundary",
			original:    0.500000001,
			automated:   0.25,
			interactive: 0.25,
			want:        0.5,
		},
		{
			name:        "above tolerance",
			original:    math.Nextafter(0.500000001, math.Inf(1)),
			automated:   0.25,
			interactive: 0.25,
			wantErr:     true,
		},
		{
			name:        "negative original",
			original:    -0.1,
			automated:   0,
			interactive: 0,
			wantErr:     true,
		},
		{
			name:        "negative automated",
			original:    0,
			automated:   -0.1,
			interactive: 0,
			wantErr:     true,
		},
		{
			name:        "negative interactive",
			original:    0,
			automated:   0,
			interactive: -0.1,
			wantErr:     true,
		},
		{
			name:        "nan original",
			original:    math.NaN(),
			automated:   0,
			interactive: 0,
			wantErr:     true,
		},
		{
			name:        "infinite component",
			original:    0,
			automated:   math.Inf(1),
			interactive: 0,
			wantErr:     true,
		},
		{
			name:        "infinite derived sum",
			original:    math.MaxFloat64,
			automated:   math.MaxFloat64,
			interactive: math.MaxFloat64,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := reportingDerivedAgentMinutes(
				"fixture", tt.original, tt.automated, tt.interactive, tt.subagent,
			)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReportingFirstSeenUsesEffectiveIntervals(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "cross-day-gap", "project-a", func(s *Session) {
		s.Agent = "agent-a"
		s.StartedAt = Ptr("2026-07-27T23:00:00Z")
		s.EndedAt = Ptr("2026-07-28T10:00:00Z")
	})
	seedMessage(
		t, d, "cross-day-gap", 1, "user", "2026-07-27T23:00:00Z", "",
	)
	seedMessage(
		t, d, "cross-day-gap", 2, "assistant", "2026-07-28T10:00:00Z",
		"model-a",
	)

	day, err := d.ExportReportingDay(context.Background(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.False(t, day.Hours[0].HasData)
	assert.Zero(t, day.Hours[0].Activity.Totals.NewSessions)
	assert.Zero(t, day.Hours[0].Activity.Totals.NewProjects)

	hour10 := day.Hours[10]
	assert.True(t, hour10.HasData)
	assert.Zero(t, hour10.Activity.Totals.AgentMinutes)
	assert.Equal(t, 1, hour10.Activity.Totals.NewSessions)
	assert.Equal(t, 1, hour10.Activity.Totals.NewInteractiveSessions)
	assert.Equal(t, 1, hour10.Activity.Totals.NewUntimedSessions)
	assert.Equal(t, 1, hour10.Activity.Totals.NewProjects)
}
