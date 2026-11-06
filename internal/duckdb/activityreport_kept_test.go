//go:build !(windows && arm64)

package duckdb

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

// Extend the real rebuilt mirror with known usage for the ended day.
func keptActivityMirror(t *testing.T, path, session string, output int) {
	t.Helper()
	buildMirrorFixture(t, path, session)
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `UPDATE messages SET model = '', token_usage = '', output_tokens = 0, context_tokens = 0, has_output_tokens = false, has_context_tokens = false`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO messages
  (id, session_id, ordinal, role, content, timestamp, model, token_usage, output_tokens)
  VALUES (2, ?, 1, 'assistant', 'answer', TIMESTAMP '2026-01-01 00:01:00', 'kept-model', ?, ?)`,
		session, `{"output_tokens":`+fmt.Sprint(output)+`}`, output)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET ended_at = TIMESTAMP '2026-01-01 00:01:00', message_count = 2`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func keptActivityBuild(t *testing.T, s *Store, q activity.Query, wantUsage bool) activity.CandidateArtifacts {
	t.Helper()
	var progress []activity.Progress
	artifacts, err := s.BuildActivityReportArtifacts(t.Context(), db.AnalyticsFilter{Timezone: "UTC"}, q,
		func(p activity.Progress) { progress = append(progress, p) })
	require.NoError(t, err)
	require.NotEmpty(t, progress)
	require.Equal(t, activity.ProgressLoadingSessions, progress[0].Phase)
	require.Equal(t, activity.ProgressDone, progress[len(progress)-1].Phase)
	loaded := false
	for _, p := range progress {
		loaded = loaded || p.Phase == activity.ProgressLoadingUsage
	}
	require.Equal(t, wantUsage, loaded)
	return artifacts
}

func requireKeptActivityEqual(t *testing.T, want, got activity.CandidateArtifacts) {
	t.Helper()
	a, err := activity.ArtifactDigest(want)
	require.NoError(t, err)
	b, err := activity.ArtifactDigest(got)
	require.NoError(t, err)
	require.Equal(t, a, b)
}

func TestActivityReportKeptAcrossReopensAndRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	q := duckDayQuery(t, "2026-01-01", "UTC")
	rates := map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 10_000_000}}
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	store.SetCustomPricing(rates)
	require.NoError(t, store.OpenActivityReportDisk(t.Context()))
	first := keptActivityBuild(t, store, q, true)
	require.Equal(t, 500, activity.ReportWithSessions(first).Totals.OutputTokens)
	require.Equal(t, money.MustParseDollars("0.005"), activity.ReportWithSessions(first).Totals.Cost)
	requireKeptActivityEqual(t, first, keptActivityBuild(t, store, q, false))
	require.NoError(t, store.Close())

	restarted, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	restarted.SetCustomPricing(rates)
	require.NoError(t, restarted.OpenActivityReportDisk(t.Context()))
	requireKeptActivityEqual(t, first, keptActivityBuild(t, restarted, q, false))
	require.NoError(t, restarted.Close())

	repriced, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repriced.Close()) })
	repriced.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 20_000_000}})
	require.NoError(t, repriced.OpenActivityReportDisk(t.Context()))
	changed := keptActivityBuild(t, repriced, q, true)
	require.Equal(t, money.MustParseDollars("0.01"), activity.ReportWithSessions(changed).Totals.Cost)
	requireKeptActivityEqual(t, changed, keptActivityBuild(t, repriced, q, false))
}

func TestActivityReportKeptMirrorReplacement(t *testing.T) {
	skipReopenTestOnWindows(t)
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.OpenActivityReportDisk(t.Context()))
	q := duckDayQuery(t, "2026-01-01", "UTC")
	first := keptActivityBuild(t, store, q, true)
	requireKeptActivityEqual(t, first, keptActivityBuild(t, store, q, false))
	next := filepath.Join(dir, "next.duckdb")
	keptActivityMirror(t, next, "session-b", 900)
	require.NoError(t, swapMirrorFile(next, path))
	store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
	changed := keptActivityBuild(t, store, q, true)
	require.Equal(t, 900, activity.ReportWithSessions(changed).Totals.OutputTokens)
	fresh := keptActivityBuild(t, NewStoreFromDB(store.DB()), q, true)
	requireKeptActivityEqual(t, fresh, changed)
	requireKeptActivityEqual(t, changed, keptActivityBuild(t, store, q, false))
}

func TestActivityReportKeptExcludesPartialAndBorrowedHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	q := duckDayQuery(t, "2026-01-01", "UTC")
	borrowed := NewStoreFromDB(store.DB())
	for range 2 {
		keptActivityBuild(t, borrowed, q, true)
	}
	q, err = activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	for range 2 {
		keptActivityBuild(t, store, q, true)
	}
}
