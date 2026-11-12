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

func TestAnalyticsKeptReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	local := newLocalDB(t)
	id := "session-a"
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:     syncSession(id, "alpha", "prompt", "2026-01-01T00:00:00Z", 1),
		Messages:    []db.Message{duckModelMessage(id, 0, "assistant", "read", "2026-01-01T00:00:00Z", "model-a", db.ToolCall{ToolName: "Read", Category: "Read", SkillName: "review"})},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	_, err = rebuildMirror(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, "test", nil)
	require.NoError(t, err)
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET quality_signal_version = ?`, db.CurrentQualitySignalVersion)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE tool_calls SET observed_outcome = 'empty'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	f := db.AnalyticsFilter{Timezone: "UTC"}
	summary, err := store.GetAnalyticsSummary(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 1, summary.TotalSessions)
	tools, err := store.GetAnalyticsTools(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 1, tools.TotalCalls)
	skills, err := store.GetAnalyticsSkills(t.Context(), f, "day")
	require.NoError(t, err)
	require.Equal(t, 1, skills.TotalSkillCalls)
	require.NotEmpty(t, summary.Agents)
	require.NotEmpty(t, tools.Trend)
	require.Len(t, tools.ByTool, 1)
	require.NotNil(t, tools.ByTool[0].EmptyRate)
	require.Equal(t, 1.0, *tools.ByTool[0].EmptyRate)
	require.NotEmpty(t, skills.Trend)
	// A closed handle makes an unexpected SQL read fail.
	require.NoError(t, store.DB().Close())
	againSummary, err := store.GetAnalyticsSummary(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, summary, againSummary)
	againTools, err := store.GetAnalyticsTools(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, tools, againTools)
	againSkills, err := store.GetAnalyticsSkills(t.Context(), f, "day")
	require.NoError(t, err)
	require.Equal(t, skills, againSkills)
	for _, agent := range againSummary.Agents {
		agent.Sessions = 999
	}
	againTools.Trend[0].ByCat["Read"] = 999
	*againTools.ByTool[0].EmptyRate = 999
	againSkills.Trend[0].BySkill["review"] = 999
	gotSummary, err := store.GetAnalyticsSummary(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, summary, gotSummary)
	gotTools, err := store.GetAnalyticsTools(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, tools, gotTools)
	gotSkills, err := store.GetAnalyticsSkills(t.Context(), f, "day")
	require.NoError(t, err)
	require.Equal(t, skills, gotSkills)
	_, err = store.GetAnalyticsSummary(t.Context(), db.AnalyticsFilter{Project: "other"})
	require.Error(t, err)
	_, err = store.GetAnalyticsSkills(t.Context(), f, "week")
	require.Error(t, err)
	_, err = store.GetAnalyticsSummary(t.Context(), db.AnalyticsFilter{Timezone: "UTC", Termination: "active"})
	require.Error(t, err)
}

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
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET project = 'beta'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	f := db.AnalyticsFilter{Timezone: "UTC"}
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
	fresh, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { fresh.Close() })
	expected, err := fresh.GetAnalyticsSummary(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, expected, after)
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
