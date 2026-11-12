//go:build fts5

package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/money"
)

func TestDailyUsagePairwiseSharedSessions(t *testing.T) {
	d := openDailyUsageFixtureDB(t)
	d.SetCustomPricing(map[string]config.CustomModelRate{
		"model-a": {InputMicrodollarsPerMTok: 2000000, OutputMicrodollarsPerMTok: 10000000},
		"gpt-5":   {InputMicrodollarsPerMTok: 2500000, OutputMicrodollarsPerMTok: 10000000},
	})
	filter := UsageFilter{From: "2024-06-15", To: "2024-06-15", Timezone: "UTC", Breakdowns: true, PairwiseSides: true}
	ranking, err := d.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.NotNil(t, ranking.Pairwise)
	models, ok := ranking.Pairwise.Side("model", []string{"model-a", "gpt-5"})
	require.True(t, ok)
	assert.Equal(t, 8000, models.Totals.InputTokens)
	assert.EqualValues(t, 58000, models.Totals.TotalCost.Microdollars)
	assert.Equal(t, 4, models.SessionCounts.Total)
	project, ok := ranking.Pairwise.Side("project", []string{"proj-a"})
	require.True(t, ok)
	assert.Equal(t, 4000, project.Totals.InputTokens)
	assert.EqualValues(t, 29000, project.Totals.TotalCost.Microdollars)
	assert.Equal(t, 2, project.SessionCounts.Total)
	filtered := filter
	filtered.PairwiseSides = false
	filtered.ProjectLabels = []string{"proj-a"}
	independent, err := d.GetDailyUsage(t.Context(), filtered)
	require.NoError(t, err)
	assert.Equal(t, independent.Totals.TotalCost, project.Totals.TotalCost)
	assert.Equal(t, independent.Totals.InputTokens, project.Totals.InputTokens)
	assert.Equal(t, independent.SessionCounts.Total, project.SessionCounts.Total)
	filter.ActiveSince = "2024-06-01T00:00:00Z"
	filter.ExcludeAgent = "codex"
	result, err := d.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.NotNil(t, result.Pairwise)
	side, ok := result.Pairwise.Side("model", []string{"model-a", "gpt-5"})
	require.True(t, ok)
	assert.Equal(t, 4000, side.Totals.InputTokens)
	assert.Equal(t, 2, side.SessionCounts.Total)
}

func TestDailyUsagePairwiseReportedCost(t *testing.T) {
	d := testDB(t)
	d.SetCustomPricing(map[string]config.CustomModelRate{"model-a": {InputMicrodollarsPerMTok: 1000000}})
	insertSession(t, d, "copilot", "project", func(s *Session) { s.Agent = "copilot"; s.StartedAt = Ptr("2026-08-10T08:00:00Z") })
	reported := money.MustParseDollars("3")
	require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), "copilot", []UsageEvent{{
		Source: "shutdown", Model: "model-a", InputTokens: 30, Cost: &reported,
		CostStatus: "exact", CostSource: CopilotReportedCostSource,
		OccurredAt: "2026-08-10T09:00:00Z", DedupKey: "shutdown",
	}}))
	result, err := d.GetDailyUsage(t.Context(), UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", PairwiseSides: true})
	require.NoError(t, err)
	require.NotNil(t, result.Pairwise)
	model, ok := result.Pairwise.Side("model", []string{"model-a"})
	require.True(t, ok)
	assert.EqualValues(t, 30, model.Totals.TotalCost.Microdollars)
	project, ok := result.Pairwise.Side("project", []string{"project"})
	require.True(t, ok)
	assert.Equal(t, reported, project.Totals.TotalCost)
	assert.Equal(t, 1, project.SessionCounts.Total)
}

func TestDailyUsagePairwiseSnapshotAttribution(t *testing.T) {
	d := testDB(t)
	seedUsageSnapshotSession(t, d, "earlier", "project-a", "2026-08-10T09:00:00Z", 0, 10, "model-a")
	seedUsageSnapshotSession(t, d, "later", "project-b", "2026-08-10T10:00:00Z", 0, 20, "model-a")
	insertMessages(t, d, Message{SessionID: "earlier", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T09:05:00Z", Model: "model-b", TokenUsage: []byte(`{"input_tokens":40}`)})
	ranking, err := d.GetDailyUsage(t.Context(), UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", PairwiseSides: true})
	require.NoError(t, err)
	require.NotNil(t, ranking.Pairwise)
	side, ok := ranking.Pairwise.Side("project", []string{"project-a"})
	require.True(t, ok)
	assert.Equal(t, 20, side.Totals.OutputTokens)
	assert.Equal(t, 40, side.Totals.InputTokens)
	assert.Equal(t, 1, side.SessionCounts.Total)
	ordinary, ok := ranking.Pairwise.Side("model", []string{"model-b"})
	require.True(t, ok)
	assert.Equal(t, 40, ordinary.Totals.InputTokens)
	assert.Equal(t, 1, ordinary.SessionCounts.Total)
	other, ok := ranking.Pairwise.Side("project", []string{"project-b"})
	require.True(t, ok)
	assert.Zero(t, other.Totals.OutputTokens)
	assert.Zero(t, other.SessionCounts.Total)
}

func TestDailyUsagePairwiseDuplicateAndCursor(t *testing.T) {
	d := testDB(t)
	for _, row := range []struct{ id, project, model, at string }{
		{"first", "project-a", "model-a", "2026-11-01T05:30:00Z"},
		{"second", "project-b", "model-b", "2026-11-02T01:30:00Z"},
	} {
		insertSession(t, d, row.id, row.project, func(s *Session) { s.Agent = "codex"; s.StartedAt = &row.at })
		insertMessages(t, d, Message{SessionID: row.id, Ordinal: 0, Role: "assistant", Timestamp: row.at, Model: row.model, SourceUUID: "shared", TokenUsage: []byte(`{"input_tokens":100}`)})
	}
	require.NoError(t, d.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{{OccurredAt: "2026-11-01T06:40:00Z", Model: "cursor-model", InputTokens: 30, DedupKey: "cursor"}}))
	filter := UsageFilter{From: "2026-11-01", To: "2026-11-01", Timezone: "America/Chicago", Breakdowns: true, PairwiseSides: true}
	ranking, err := d.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.NotNil(t, ranking.Pairwise)
	_, ok := ranking.Pairwise.Side("model", []string{"model-b"})
	assert.False(t, ok)
	_, ok = ranking.Pairwise.Side("project", []string{"project-b"})
	assert.False(t, ok)
	cursor, ok := ranking.Pairwise.Side("model", []string{"cursor-model"})
	require.True(t, ok)
	assert.Equal(t, 30, cursor.Totals.InputTokens)
	assert.Zero(t, cursor.SessionCounts.Total)
}
