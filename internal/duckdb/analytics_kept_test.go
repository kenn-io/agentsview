//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestAnalyticsKeptMirrorReplacement(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	next := filepath.Join(dir, "next.duckdb")
	buildMirrorFixture(t, path, "session-a")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(next, data, 0600))
	conn, err := Open(t.Context(), next)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET project = 'beta', message_count = 5, health_score = 80, health_grade = 'B', outcome = 'success', ended_at = TIMESTAMP '2026-01-01 00:05:00'`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE messages SET timestamp = TIMESTAMP '2026-01-02 01:00:00', content = 'changed'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	f := db.AnalyticsFilter{Timezone: "UTC"}
	cases := keptPanelCases(t.Context(), f)
	for _, tc := range cases {
		_, err := tc.read(store)
		require.NoError(t, err, tc.name)
	}
	probeBefore, err := store.ActivityReportSourceProbe(t.Context())
	require.NoError(t, err)
	before, err := store.GetAnalyticsSummary(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, "alpha", before.MostActive)
	require.NoError(t, swapMirrorFile(next, path))
	store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
	probeAfter, err := store.ActivityReportSourceProbe(t.Context())
	require.NoError(t, err)
	require.Equal(t, probeBefore, probeAfter)
	after, err := store.GetAnalyticsSummary(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, "beta", after.MostActive)
	for _, filter := range []db.AnalyticsFilter{f, {Timezone: "UTC", Project: "missing"}} {
		for _, tc := range keptPanelCases(t.Context(), filter) {
			want, err := tc.read(store.analytics())
			require.NoError(t, err, tc.name)
			got, err := tc.read(store)
			require.NoError(t, err, tc.name)
			require.Equal(t, want, got, tc.name)
			again, err := tc.read(store)
			require.NoError(t, err, tc.name)
			require.Equal(t, want, again, tc.name)
		}
	}
}

func TestAnalyticsKeptErrorsAreRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	buildMirrorFixture(t, path, "session-a")
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	calls := 0
	load := func(ctx context.Context) (db.AnalyticsSummary, error) {
		calls++
		if calls == 1 {
			return db.AnalyticsSummary{}, errors.New("read failed")
		}
		return db.AnalyticsSummary{TotalSessions: 42}, nil
	}
	_, err = keptAnalytics(t.Context(), store, "summary", db.AnalyticsFilter{}, "", load)
	require.Error(t, err)
	got, err := keptAnalytics(t.Context(), store, "summary", db.AnalyticsFilter{}, "", load)
	require.NoError(t, err)
	require.Equal(t, 42, got.TotalSessions)
	require.Equal(t, 2, calls)
}

type analyticsReports interface {
	GetAnalyticsSummary(context.Context, db.AnalyticsFilter) (db.AnalyticsSummary, error)
	GetAnalyticsActivity(context.Context, db.AnalyticsFilter, string) (db.ActivityResponse, error)
	GetAnalyticsHeatmap(context.Context, db.AnalyticsFilter, string) (db.HeatmapResponse, error)
	GetAnalyticsProjects(context.Context, db.AnalyticsFilter) (db.ProjectsAnalyticsResponse, error)
	GetAnalyticsHourOfWeek(context.Context, db.AnalyticsFilter) (db.HourOfWeekResponse, error)
	GetAnalyticsSessionShape(context.Context, db.AnalyticsFilter) (db.SessionShapeResponse, error)
	GetAnalyticsTools(context.Context, db.AnalyticsFilter) (db.ToolsAnalyticsResponse, error)
	GetAnalyticsSkills(context.Context, db.AnalyticsFilter, string) (db.SkillsAnalyticsResponse, error)
	GetAnalyticsVelocity(context.Context, db.AnalyticsFilter) (db.VelocityResponse, error)
	GetAnalyticsTopSessions(context.Context, db.AnalyticsFilter, string) (db.TopSessionsResponse, error)
	GetAnalyticsSignals(context.Context, db.AnalyticsFilter) (db.SignalsAnalyticsResponse, error)
}

type keptPanelCase struct {
	name string
	read func(analyticsReports) (any, error)
}

func keptPanelCases(ctx context.Context, f db.AnalyticsFilter) []keptPanelCase {
	return []keptPanelCase{
		{"summary", func(s analyticsReports) (any, error) { return s.GetAnalyticsSummary(ctx, f) }},
		{"activity-default", func(s analyticsReports) (any, error) { return s.GetAnalyticsActivity(ctx, f, "") }},
		{"activity-day", func(s analyticsReports) (any, error) { return s.GetAnalyticsActivity(ctx, f, "day") }},
		{"activity-week", func(s analyticsReports) (any, error) { return s.GetAnalyticsActivity(ctx, f, "week") }},
		{"activity-month", func(s analyticsReports) (any, error) { return s.GetAnalyticsActivity(ctx, f, "month") }},
		{"heatmap-default", func(s analyticsReports) (any, error) { return s.GetAnalyticsHeatmap(ctx, f, "") }},
		{"heatmap-messages", func(s analyticsReports) (any, error) { return s.GetAnalyticsHeatmap(ctx, f, "messages") }},
		{"heatmap-sessions", func(s analyticsReports) (any, error) { return s.GetAnalyticsHeatmap(ctx, f, "sessions") }},
		{"heatmap-tokens", func(s analyticsReports) (any, error) { return s.GetAnalyticsHeatmap(ctx, f, "output_tokens") }},
		{"projects", func(s analyticsReports) (any, error) { return s.GetAnalyticsProjects(ctx, f) }},
		{"hours", func(s analyticsReports) (any, error) { return s.GetAnalyticsHourOfWeek(ctx, f) }},
		{"shape", func(s analyticsReports) (any, error) { return s.GetAnalyticsSessionShape(ctx, f) }},
		{"tools", func(s analyticsReports) (any, error) { return s.GetAnalyticsTools(ctx, f) }},
		{"skills-default", func(s analyticsReports) (any, error) { return s.GetAnalyticsSkills(ctx, f, "") }},
		{"skills-day", func(s analyticsReports) (any, error) { return s.GetAnalyticsSkills(ctx, f, "day") }},
		{"skills-week", func(s analyticsReports) (any, error) { return s.GetAnalyticsSkills(ctx, f, "week") }},
		{"skills-month", func(s analyticsReports) (any, error) { return s.GetAnalyticsSkills(ctx, f, "month") }},
		{"velocity", func(s analyticsReports) (any, error) { return s.GetAnalyticsVelocity(ctx, f) }},
		{"top-default", func(s analyticsReports) (any, error) { return s.GetAnalyticsTopSessions(ctx, f, "") }},
		{"top-messages", func(s analyticsReports) (any, error) { return s.GetAnalyticsTopSessions(ctx, f, "messages") }},
		{"top-duration", func(s analyticsReports) (any, error) { return s.GetAnalyticsTopSessions(ctx, f, "duration") }},
		{"top-tokens", func(s analyticsReports) (any, error) { return s.GetAnalyticsTopSessions(ctx, f, "output_tokens") }},
		{"signals", func(s analyticsReports) (any, error) { return s.GetAnalyticsSignals(ctx, f) }},
	}
}

func TestAnalyticsKeptAllPanelsAndVariants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	local := newLocalDB(t)
	id := "session-a"
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:     syncSession(id, "alpha", "prompt", "2026-01-01T00:00:00Z", 2),
		Messages:    []db.Message{syncMessage(id, 0, "user", "prompt", "2026-01-01T00:00:00Z"), duckModelMessage(id, 1, "assistant", "read", "2026-01-01T00:01:00Z", "model-a", db.ToolCall{ToolName: "Read", Category: "Read", SkillName: "review"})},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	_, err = rebuildMirror(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, "test", nil)
	require.NoError(t, err)
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET quality_signal_version = ?`, db.CurrentQualitySignalVersion)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET ended_at = NULL`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE tool_calls SET observed_outcome = 'empty'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	f := db.AnalyticsFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
	freshTop, err := store.analytics().GetAnalyticsTopSessions(t.Context(), f, "messages")
	require.NoError(t, err)
	require.Len(t, freshTop.Sessions, 1)
	require.NotNil(t, freshTop.Sessions[0].EndedAt)
	require.Empty(t, *freshTop.Sessions[0].EndedAt)
	cases := keptPanelCases(t.Context(), f)
	values := make(map[string]any)
	for _, tc := range cases {
		got, err := tc.read(store)
		require.NoError(t, err, tc.name)
		fresh, err := tc.read(store.analytics())
		require.NoError(t, err, tc.name)
		require.Equal(t, fresh, got, tc.name)
		values[tc.name] = got
	}
	require.Equal(t, freshTop, values["top-messages"])
	require.Equal(t, 1, values["summary"].(db.AnalyticsSummary).TotalSessions)
	other, err := store.GetAnalyticsSummary(t.Context(), db.AnalyticsFilter{Project: "missing"})
	require.NoError(t, err)
	require.Zero(t, other.TotalSessions)
	activity := values["activity-day"].(db.ActivityResponse)
	require.Equal(t, "day", values["activity-default"].(db.ActivityResponse).Granularity)
	require.Len(t, activity.Series, 1)
	require.Equal(t, 1, activity.Series[0].Sessions)
	require.Equal(t, 2, activity.Series[0].Messages)
	require.Equal(t, "2025-12-29", values["activity-week"].(db.ActivityResponse).Series[0].Date)
	require.Equal(t, "2026-01-01", values["activity-month"].(db.ActivityResponse).Series[0].Date)
	heat := values["heatmap-messages"].(db.HeatmapResponse)
	require.Equal(t, "messages", values["heatmap-default"].(db.HeatmapResponse).Metric)
	require.Len(t, heat.Entries, 1)
	require.Equal(t, 2, heat.Entries[0].Value)
	require.Greater(t, heat.Entries[0].Level, 0)
	require.Len(t, values["hours"].(db.HourOfWeekResponse).Cells, 168)
	require.Equal(t, db.HourOfWeekCell{DayOfWeek: 3, Hour: 0, Messages: 2}, values["hours"].(db.HourOfWeekResponse).Cells[72])
	require.Equal(t, 1, values["signals"].(db.SignalsAnalyticsResponse).UnscoredSessions)
	require.Len(t, values["velocity"].(db.VelocityResponse).ByAgent, 1)
	require.Equal(t, 1, values["velocity"].(db.VelocityResponse).ByAgent[0].Sessions)
	require.Equal(t, 1, values["heatmap-sessions"].(db.HeatmapResponse).Entries[0].Value)
	require.Empty(t, values["heatmap-tokens"].(db.HeatmapResponse).Entries)
	require.Equal(t, "alpha", values["projects"].(db.ProjectsAnalyticsResponse).Projects[0].Name)
	require.Equal(t, 1, values["shape"].(db.SessionShapeResponse).Count)
	require.Equal(t, []db.DistributionBucket{{Label: "1-5", Count: 1}}, values["shape"].(db.SessionShapeResponse).LengthDistribution)
	require.Equal(t, "messages", values["top-default"].(db.TopSessionsResponse).Metric)
	require.Equal(t, "session-a", values["top-messages"].(db.TopSessionsResponse).Sessions[0].ID)
	require.Equal(t, values["skills-week"], values["skills-default"])
	require.Equal(t, "2025-12-29", values["skills-week"].(db.SkillsAnalyticsResponse).Trend[0].Date)
	require.Equal(t, "2026-01-01", values["skills-day"].(db.SkillsAnalyticsResponse).Trend[0].Date)
	require.Equal(t, "2026-01-01", values["skills-month"].(db.SkillsAnalyticsResponse).Trend[0].Date)
	require.Equal(t, "duration", values["top-duration"].(db.TopSessionsResponse).Metric)
	require.Equal(t, "output_tokens", values["top-tokens"].(db.TopSessionsResponse).Metric)
	require.Equal(t, "sessions", values["heatmap-sessions"].(db.HeatmapResponse).Metric)
	require.Equal(t, "output_tokens", values["heatmap-tokens"].(db.HeatmapResponse).Metric)
	tools := values["tools"].(db.ToolsAnalyticsResponse)
	require.Equal(t, 1, tools.TotalCalls)
	require.Len(t, tools.ByTool, 1)
	require.NotNil(t, tools.ByTool[0].EmptyRate)
	require.Equal(t, 1.0, *tools.ByTool[0].EmptyRate)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range keptPanelCases(canceled, f) {
		_, err := tc.read(store)
		require.ErrorIs(t, err, context.Canceled, tc.name)
	}
	require.NoError(t, store.DB().Close())
	for _, tc := range cases {
		got, err := tc.read(store)
		require.NoError(t, err, tc.name)
		require.Equal(t, values[tc.name], got, tc.name)
	}
	warmTop, err := store.GetAnalyticsTopSessions(t.Context(), f, "messages")
	require.NoError(t, err)
	require.Equal(t, freshTop, warmTop)
	for _, change := range []struct {
		name  string
		apply func(*db.AnalyticsFilter)
	}{
		{"from", func(f *db.AnalyticsFilter) { f.From = "2025-12-31" }},
		{"to", func(f *db.AnalyticsFilter) { f.To = "2026-01-02" }},
		{"machine", func(f *db.AnalyticsFilter) { f.Machine = "other" }},
		{"project", func(f *db.AnalyticsFilter) { f.Project = "other" }},
		{"agent", func(f *db.AnalyticsFilter) { f.Agent = "other" }},
		{"minimum messages", func(f *db.AnalyticsFilter) { f.MinUserMessages = 2 }},
		{"one shot", func(f *db.AnalyticsFilter) { f.ExcludeOneShot = true }},
	} {
		changed := f
		change.apply(&changed)
		_, err := store.GetAnalyticsSummary(t.Context(), changed)
		require.Error(t, err, change.name)
	}

	summary := values["summary"].(db.AnalyticsSummary)
	for _, agent := range summary.Agents {
		agent.Sessions = 999
	}
	tools.Trend[0].ByCat["Read"] = 999
	*tools.ByTool[0].EmptyRate = 999
	skills := values["skills-day"].(db.SkillsAnalyticsResponse)
	skills.Trend[0].BySkill["review"] = 999
	gotSummary, err := store.GetAnalyticsSummary(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 1, gotSummary.Agents["claude"].Sessions)
	gotTools, err := store.GetAnalyticsTools(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 1, gotTools.Trend[0].ByCat["Read"])
	require.Equal(t, 1.0, *gotTools.ByTool[0].EmptyRate)
	gotSkills, err := store.GetAnalyticsSkills(t.Context(), f, "day")
	require.NoError(t, err)
	require.Equal(t, 1, gotSkills.Trend[0].BySkill["review"])
	activity.Series[0].ByAgent["test"] = 999
	hours := values["hours"].(db.HourOfWeekResponse)
	hours.Cells[0].Messages = 999
	heat.Entries[0].Value = 999
	gotActivity, err := store.GetAnalyticsActivity(t.Context(), f, "day")
	require.NoError(t, err)
	require.NotContains(t, gotActivity.Series[0].ByAgent, "test")
	gotHours, err := store.GetAnalyticsHourOfWeek(t.Context(), f)
	require.NoError(t, err)
	require.NotEqual(t, 999, gotHours.Cells[0].Messages)
	gotHeat, err := store.GetAnalyticsHeatmap(t.Context(), f, "messages")
	require.NoError(t, err)
	require.Equal(t, 2, gotHeat.Entries[0].Value)
}

func TestAnalyticsKeptMovingSelectionsReadFresh(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter db.AnalyticsFilter
	}{
		{"termination", db.AnalyticsFilter{Termination: "active"}},
		{"active since", db.AnalyticsFilter{ActiveSince: "2026-01-01T00:00:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mirror.duckdb")
			buildMirrorFixture(t, path, "session-a")
			store, err := NewStore(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { store.Close() })
			_, err = store.GetAnalyticsSummary(t.Context(), tc.filter)
			require.NoError(t, err)
			require.NoError(t, store.DB().Close())
			_, err = store.GetAnalyticsSummary(t.Context(), tc.filter)
			require.Error(t, err)
		})
	}
}
