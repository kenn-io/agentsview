//go:build duckdb && !(windows && arm64)

package duckdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/storage"
)

func primePanelDigests(t *testing.T, store *Store, f db.AnalyticsFilter, panels ...string) {
	t.Helper()
	for _, panel := range panels {
		digest, _ := store.dashboardDigest(t.Context(), panel, f)
		require.NotEmpty(t, digest)
	}
}

func TestAnalyticsKeptMirrorReplacement(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
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
	// Trends uses the push fingerprint to invalidate its saved per-session data.
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET agentsview_push_fingerprint = 'changed', project = 'beta', message_count = 5, health_score = 80, health_grade = 'B', outcome = 'success', ended_at = TIMESTAMP '2026-01-01 00:05:00'`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE messages SET timestamp = TIMESTAMP '2026-01-02 01:00:00', content = 'changed'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.OpenActivityReportDisk(t.Context()))
	f := db.AnalyticsFilter{Timezone: "UTC"}
	cases := keptPanelCases(t.Context(), f)
	for _, tc := range cases {
		_, err := tc.read(store)
		require.NoError(t, err, tc.name)
	}
	before, err := store.GetAnalyticsSummary(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, "alpha", before.MostActive)
	trendsFilter := db.AnalyticsFilter{From: "2026-01-01", To: "2026-01-02", Timezone: "UTC"}
	terms := []db.TrendTermInput{{Term: "changed", Variants: []string{"changed"}, Matchers: []string{"changed"}}}
	trends, err := store.GetTrendsTerms(t.Context(), trendsFilter, terms, "day")
	require.NoError(t, err)
	require.Zero(t, trends.Series[0].Total)
	invalid := filepath.Join(dir, "invalid.duckdb")
	require.NoError(t, os.WriteFile(invalid, []byte("invalid mirror"), 0o600))
	require.NoError(t, swapMirrorFile(invalid, path))
	unchanged, err := store.GetTrendsTerms(t.Context(), trendsFilter, terms, "day")
	require.NoError(t, err)
	require.Equal(t, trends, unchanged)
	require.NoError(t, swapMirrorFile(next, path))
	trends, err = store.GetTrendsTerms(t.Context(), trendsFilter, terms, "day")
	require.NoError(t, err)
	require.Equal(t, 1, trends.Series[0].Total)
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
	GetTrendsTerms(context.Context, db.AnalyticsFilter, []db.TrendTermInput, string) (db.TrendsTermsResponse, error)
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
		{"trends", func(s analyticsReports) (any, error) {
			return s.GetTrendsTerms(ctx, f, []db.TrendTermInput{{Term: "prompt", Variants: []string{"prompt"}, Matchers: []string{"prompt"}}}, "week")
		}},
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

func TestAnalyticsConcurrentPanelsMatchSerial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	buildMirrorFixture(t, path, "session-a")
	serial, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { serial.Close() })
	concurrent, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { concurrent.Close() })

	cases := keptPanelCases(t.Context(), db.AnalyticsFilter{Timezone: "UTC"})
	want := make([]any, len(cases))
	for i, tc := range cases {
		want[i], err = tc.read(serial)
		require.NoError(t, err, tc.name)
	}
	type result struct {
		value any
		err   error
	}
	results := make([]chan result, len(cases))
	start := make(chan struct{})
	for i, tc := range cases {
		results[i] = make(chan result, 1)
		go func() {
			<-start
			value, err := tc.read(concurrent)
			results[i] <- result{value, err}
		}()
	}
	close(start)
	got := make([]result, len(cases))
	for i := range cases {
		got[i] = <-results[i]
	}
	for i, tc := range cases {
		require.NoError(t, got[i].err, tc.name)
		require.Equal(t, want[i], got[i].value, tc.name)
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

func TestAnalyticsKeptRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	pricing.SetConfiguredAliases(map[string]string{"model": "first"})
	t.Cleanup(func() { pricing.SetConfiguredAliases(nil) })
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	buildMirrorFixture(t, path, "session-a")
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, store.OpenActivityReportDisk(t.Context()))
	store.SetCustomPricing(map[string]config.CustomModelRate{"model": {OutputMicrodollarsPerMTok: 100}})
	f := db.AnalyticsFilter{Timezone: "UTC"}
	var expected []any
	for _, panel := range keptPanelCases(t.Context(), f) {
		if db.SavedPanel(panel.name) {
			value, err := panel.read(store)
			require.NoError(t, err)
			expected = append(expected, value)
		}
	}
	require.NoError(t, store.Close())
	pricing.SetConfiguredAliases(map[string]string{"model": "second"})
	reopened, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { reopened.Close() })
	reopened.SetCustomPricing(map[string]config.CustomModelRate{"model": {OutputMicrodollarsPerMTok: 200}})
	require.NoError(t, reopened.OpenActivityReportDisk(t.Context()))
	primePanelDigests(t, reopened, f, "tools", "velocity", "signals")
	require.NoError(t, reopened.DB().Close())
	i := 0
	for _, panel := range keptPanelCases(t.Context(), f) {
		if db.SavedPanel(panel.name) {
			value, err := panel.read(reopened)
			require.NoError(t, err, panel.name)
			require.Equal(t, expected[i], value)
			i++
		}
	}
}

func TestAnalyticsKeptChangedMirrorInputs(t *testing.T) {
	skipReopenTestOnWindows(t)
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	dir := t.TempDir()
	path, next := filepath.Join(dir, "mirror.duckdb"), filepath.Join(dir, "next.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	writeUsageMirror(t, path,
		`UPDATE sessions SET health_score=30`,
		`UPDATE messages SET content_length=10 WHERE id=2`,
		`INSERT INTO tool_calls(id,message_id,session_id,tool_name,category) VALUES(1,2,'session-a','Read','Read')`)
	ctx := t.Context()
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.OpenActivityReportDisk(ctx))
	f := db.AnalyticsFilter{Timezone: "UTC"}
	var loads [3]int
	read := func() (db.ToolsAnalyticsResponse, db.VelocityResponse, db.SignalsAnalyticsResponse) {
		tools, err := keptAnalytics(ctx, store, "tools", f, "", func(ctx context.Context) (db.ToolsAnalyticsResponse, error) {
			loads[0]++
			return store.analytics().GetAnalyticsTools(ctx, f)
		})
		require.NoError(t, err)
		velocity, err := keptAnalytics(ctx, store, "velocity", f, "", func(ctx context.Context) (db.VelocityResponse, error) {
			loads[1]++
			return store.analytics().GetAnalyticsVelocity(ctx, f)
		})
		require.NoError(t, err)
		signals, err := keptAnalytics(ctx, store, "signals", f, "", func(ctx context.Context) (db.SignalsAnalyticsResponse, error) {
			loads[2]++
			return store.analytics().GetAnalyticsSignals(ctx, f)
		})
		require.NoError(t, err)
		return tools, velocity, signals
	}
	read()
	for _, tc := range []struct {
		query string
		loads [3]int
		tool  string
		chars float64
		score float64
	}{
		{`UPDATE tool_calls SET tool_name='Grep'`, [3]int{1, 0, 0}, "Grep", 10, 30},
		{`UPDATE messages SET content_length=20 WHERE id=2`, [3]int{0, 1, 0}, "Grep", 20, 30},
		{`UPDATE sessions SET health_score=80`, [3]int{0, 0, 1}, "Grep", 20, 80},
	} {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(next, data, 0o600))
		writeUsageMirror(t, next, tc.query)
		require.NoError(t, swapMirrorFile(next, path))
		store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
		loads = [3]int{}
		tools, velocity, signals := read()
		require.Equal(t, tc.loads, loads, tc.query)
		require.Equal(t, tc.tool, tools.ByTool[0].ToolName)
		require.InDelta(t, tc.chars, velocity.Overall.CharsPerActiveMin, 0)
		require.NotNil(t, signals.AvgHealthScore)
		require.InDelta(t, tc.score, *signals.AvgHealthScore, 0)
	}
}

func TestAnalyticsKeptDigestAssociatesRows(t *testing.T) {
	for _, tc := range []struct {
		name          string
		setup         []string
		change        string
		filter        db.AnalyticsFilter
		read          func(*testing.T, *Store, db.AnalyticsFilter) any
		before, after any
	}{
		{"velocity message", []string{`INSERT INTO messages SELECT * REPLACE (3 AS id, 2 AS ordinal, 'other-model' AS model) FROM messages WHERE id=2`, `INSERT INTO tool_calls(id,message_id,session_id,tool_name,category) VALUES(1,2,'session-a','Read','Read')`}, `UPDATE messages SET id=CASE id WHEN 2 THEN 3 WHEN 3 THEN 2 ELSE id END WHERE id IN (2,3)`, db.AnalyticsFilter{Timezone: "UTC", Model: "kept-model"}, func(t *testing.T, store *Store, f db.AnalyticsFilter) any {
			t.Helper()
			result, err := store.GetAnalyticsVelocity(t.Context(), f)
			require.NoError(t, err)
			return result.Overall.ToolCallsPerActiveMin
		}, float64(1), float64(0)},
		{"signals session", []string{`UPDATE sessions SET project='alpha', health_score=20`, `INSERT INTO sessions SELECT * REPLACE ('session-b' AS id, 'beta' AS project, 80 AS health_score) FROM sessions WHERE id='session-a'`}, `UPDATE sessions SET health_score=100-health_score`, db.AnalyticsFilter{Timezone: "UTC"}, func(t *testing.T, store *Store, f db.AnalyticsFilter) any {
			t.Helper()
			result, err := store.GetAnalyticsSignals(t.Context(), f)
			require.NoError(t, err)
			scores := make(map[string]float64)
			for _, row := range result.ByProject {
				require.NotNil(t, row.AvgHealthScore)
				scores[row.Project] = *row.AvgHealthScore
			}
			return scores
		}, map[string]float64{"alpha": 20, "beta": 80}, map[string]float64{"alpha": 80, "beta": 20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skipReopenTestOnWindows(t)
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			dir := t.TempDir()
			path, next := filepath.Join(dir, "mirror.duckdb"), filepath.Join(dir, "next.duckdb")
			keptActivityMirror(t, path, "session-a", 500)
			writeUsageMirror(t, path, tc.setup...)
			store, err := NewStore(t.Context(), path)
			require.NoError(t, err)
			require.NoError(t, store.OpenActivityReportDisk(t.Context()))
			require.Equal(t, tc.before, tc.read(t, store, tc.filter))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(next, data, 0o600))
			writeUsageMirror(t, next, tc.change)
			require.NoError(t, store.Close())
			require.NoError(t, swapMirrorFile(next, path))
			reopened, err := NewStore(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { reopened.Close() })
			require.NoError(t, reopened.OpenActivityReportDisk(t.Context()))
			require.Equal(t, tc.after, tc.read(t, reopened, tc.filter))
		})
	}
}

func TestAnalyticsKeptSameStampReplacement(t *testing.T) {
	skipReopenTestOnWindows(t)
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	next := filepath.Join(dir, "next.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	writeUsageMirror(t, path, `UPDATE sessions SET project='alpha'`)
	keptActivityMirror(t, next, "session-b", 900)
	writeUsageMirror(t, next, `UPDATE sessions SET project='beta'`)
	info, err := os.Stat(path)
	require.NoError(t, err)
	nextInfo, err := os.Stat(next)
	require.NoError(t, err)
	require.Equal(t, info.Size(), nextInfo.Size())
	require.NoError(t, os.Chtimes(next, info.ModTime(), info.ModTime()))
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, store.OpenActivityReportDisk(t.Context()))
	filter := db.AnalyticsFilter{Timezone: "UTC", Project: "beta"}
	before, err := store.GetAnalyticsSignals(t.Context(), filter)
	require.NoError(t, err)
	require.Zero(t, before.ScoredSessions+before.UnscoredSessions)
	stamp := store.mirrorStamp()
	require.NoError(t, swapMirrorFile(next, path))
	store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
	require.Equal(t, stamp, store.mirrorStamp())
	changed, err := store.GetAnalyticsSignals(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, 1, changed.ScoredSessions+changed.UnscoredSessions)
	require.NoError(t, store.Close())
}
