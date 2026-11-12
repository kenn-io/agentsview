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
	require.NoError(t, os.WriteFile(next, data, 0o600))
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
	before, err := store.GetAnalyticsSummary(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, "alpha", before.MostActive)
	require.NoError(t, swapMirrorFile(next, path))
	store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
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
	errRead := errors.New("read failed")
	load := func(ctx context.Context) (db.AnalyticsSummary, error) {
		calls++
		if calls == 1 {
			return db.AnalyticsSummary{}, errRead
		}
		return db.AnalyticsSummary{TotalSessions: 42}, nil
	}
	_, err = keptAnalytics(t.Context(), store, "summary", db.AnalyticsFilter{}, "", load)
	require.ErrorIs(t, err, errRead)
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
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET started_at = NULL, ended_at = NULL, created_at = TIMESTAMP '2026-01-01 00:00:00', termination_status = '', display_name = ''`)
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
	require.NotNil(t, freshTop.Sessions[0].StartedAt)
	require.Empty(t, *freshTop.Sessions[0].StartedAt)
	require.NotNil(t, freshTop.Sessions[0].EndedAt)
	require.Empty(t, *freshTop.Sessions[0].EndedAt)
	require.NotNil(t, freshTop.Sessions[0].TerminationStatus)
	require.Empty(t, *freshTop.Sessions[0].TerminationStatus)
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
	modelFilter := f
	modelFilter.Model = "model-a"
	freshModel, err := store.analytics().GetAnalyticsTopSessions(t.Context(), modelFilter, "messages")
	require.NoError(t, err)
	require.Len(t, freshModel.Sessions, 1)
	require.NotNil(t, freshModel.Sessions[0].DisplayName)
	require.Empty(t, *freshModel.Sessions[0].DisplayName)
	gotModel, err := store.GetAnalyticsTopSessions(t.Context(), modelFilter, "messages")
	require.NoError(t, err)
	require.Equal(t, freshModel, gotModel)
	other, err := store.GetAnalyticsSummary(t.Context(), db.AnalyticsFilter{Project: "missing"})
	require.NoError(t, err)
	require.Zero(t, other.TotalSessions)
	summary := values["summary"].(db.AnalyticsSummary)
	require.NotEmpty(t, summary.Agents)
	tools := values["tools"].(db.ToolsAnalyticsResponse)
	require.NotEmpty(t, tools.Trend)
	require.Len(t, tools.ByTool, 1)
	require.NotNil(t, tools.ByTool[0].EmptyRate)
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
	warmModel, err := store.GetAnalyticsTopSessions(t.Context(), modelFilter, "messages")
	require.NoError(t, err)
	require.Equal(t, freshModel, warmModel)
	_, err = store.GetAnalyticsSummary(t.Context(), db.AnalyticsFilter{Project: "other"})
	require.Error(t, err)
	t.Run("result isolation", func(t *testing.T) {
		for _, agent := range summary.Agents {
			agent.Sessions = 999
		}
		tools.Trend[0].ByCat["Read"] = 999
		*tools.ByTool[0].EmptyRate = 999
		gotSummary, err := store.GetAnalyticsSummary(t.Context(), f)
		require.NoError(t, err)
		require.Equal(t, 1, gotSummary.Agents["claude"].Sessions)
		gotTools, err := store.GetAnalyticsTools(t.Context(), f)
		require.NoError(t, err)
		require.Equal(t, 1, gotTools.Trend[0].ByCat["Read"])
		require.InDelta(t, 1.0, *gotTools.ByTool[0].EmptyRate, 1e-9)
	})
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
