//go:build !(windows && arm64)

package duckdb

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
  (id, session_id, ordinal, role, content, timestamp, model, token_usage, output_tokens, usage_output_tokens)
  VALUES (2, ?, 1, 'assistant', 'answer', TIMESTAMP '2026-01-01 00:01:00', 'kept-model', ?, ?, ?)`,
		session, `{"output_tokens":`+strconv.Itoa(output)+`}`, output, output)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET ended_at = TIMESTAMP '2026-01-01 00:01:00', message_count = 2`)
	require.NoError(t, err)
	// These edits keep the push fingerprints a push would change, so the
	// usage rollup built for the old rows would still count as current.
	_, err = conn.ExecContext(t.Context(), `DELETE FROM sync_metadata WHERE key = ?`, usageRollupMetadataKey)
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

func observeActivityUsageReads(t *testing.T) *int {
	t.Helper()
	reads := 0
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(query string) {
		if strings.Contains(query, "activity_usage_keyed") {
			reads++
		}
	}
	t.Cleanup(func() { analyticsQueryObserver = previous })
	return &reads
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
	reads := observeActivityUsageReads(t)
	for range 2 {
		keptActivityBuild(t, borrowed, q, true)
	}
	require.Equal(t, 2, *reads)
}

func TestActivityReportKeptExcludesTimeDependentTermination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET termination_status = 'tool_call_pending'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	q := duckDayQuery(t, "2026-01-01", "UTC")
	for _, termination := range []string{"active", "stale", "unclean", "clean, active"} {
		t.Run(termination, func(t *testing.T) {
			for range 2 {
				var phases []activity.ProgressPhase
				_, err := store.BuildActivityReportArtifacts(t.Context(), db.AnalyticsFilter{
					Timezone: "UTC", Termination: termination,
				}, q, func(p activity.Progress) { phases = append(phases, p.Phase) })
				require.NoError(t, err)
				require.Contains(t, phases, activity.ProgressLoadingUsage,
					"time-dependent session membership must be rebuilt on every read")
			}
		})
	}
	q, err = activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	reads := observeActivityUsageReads(t)
	for range 2 {
		_, err := store.BuildActivityReportArtifacts(t.Context(), db.AnalyticsFilter{Timezone: "UTC", Termination: "unclean"}, q, nil)
		require.NoError(t, err)
	}
	require.Equal(t, 2, *reads)
}

func TestPartialActivityUsageKeptUntilPush(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	store.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 10_000_000}})
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	reads := observeActivityUsageReads(t)
	first := keptActivityBuild(t, store, q, true)
	require.Equal(t, 1, *reads)
	q.EffectiveEnd = q.EffectiveEnd.Add(time.Minute)
	repeat := keptActivityBuild(t, store, q, true)
	require.Equal(t, 1, *reads)
	freshStore := NewStoreFromDB(store.DB())
	freshStore.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 10_000_000}})
	fresh := keptActivityBuild(t, freshStore, q, true)
	requireKeptActivityEqual(t, fresh, repeat)
	require.Equal(t, activity.ReportWithSessions(first).Totals.OutputTokens, activity.ReportWithSessions(repeat).Totals.OutputTokens)
	require.Equal(t, activity.ReportWithSessions(first).Totals.Cost, activity.ReportWithSessions(repeat).Totals.Cost)
	store.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 20_000_000}})
	*reads = 0
	repriced := keptActivityBuild(t, store, q, true)
	require.Equal(t, 1, *reads)
	require.Equal(t, money.MustParseDollars("0.01"), activity.ReportWithSessions(repriced).Totals.Cost)
	next := filepath.Join(dir, "next.duckdb")
	keptActivityMirror(t, next, "session-b", 900)
	require.NoError(t, swapMirrorFile(next, path))
	store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
	*reads = 0
	changed := keptActivityBuild(t, store, q, true)
	require.Equal(t, 1, *reads)
	require.Equal(t, 900, activity.ReportWithSessions(changed).Totals.OutputTokens)
}

func TestPartialActivityUsageRereadsPastNextRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE messages SET timestamp = TIMESTAMP '2026-01-01 12:30:00' WHERE id = 2`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	reads := observeActivityUsageReads(t)
	first := keptActivityBuild(t, store, q, true)
	require.Equal(t, 0, activity.ReportWithSessions(first).Totals.OutputTokens)
	q.EffectiveEnd = q.EffectiveEnd.Add(time.Hour)
	second := keptActivityBuild(t, store, q, true)
	require.Equal(t, 500, activity.ReportWithSessions(second).Totals.OutputTokens)
	require.Equal(t, 2, *reads)
	q.EffectiveEnd = q.EffectiveEnd.Add(-time.Hour)
	third := keptActivityBuild(t, store, q, true)
	require.Equal(t, 0, activity.ReportWithSessions(third).Totals.OutputTokens)
	require.Equal(t, 3, *reads)
}

func TestPartialActivityUsageRereadsPastClaudePeer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET project = 'included'`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO sessions SELECT * REPLACE ('session-b' AS id, 'excluded' AS project, TIMESTAMP '2026-01-01 12:30:00' AS ended_at, 1 AS message_count) FROM sessions WHERE id = 'session-a'`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE messages SET claude_message_id = 'message', claude_request_id = 'request' WHERE id = 2`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO messages SELECT * REPLACE (3 AS id, 'session-b' AS session_id, TIMESTAMP '2026-01-01 12:30:00' AS timestamp, '{"output_tokens":900}' AS token_usage, 900 AS output_tokens, 900 AS usage_output_tokens) FROM messages WHERE id = 2`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	reads := observeActivityUsageReads(t)
	filter := db.AnalyticsFilter{Project: "included", Timezone: "UTC"}
	first, err := store.GetActivityReport(t.Context(), filter, q)
	require.NoError(t, err)
	require.Equal(t, 500, first.Totals.OutputTokens)
	q.EffectiveEnd = q.EffectiveEnd.Add(time.Minute)
	repeat, err := store.GetActivityReport(t.Context(), filter, q)
	require.NoError(t, err)
	require.Equal(t, 500, repeat.Totals.OutputTokens)
	require.Equal(t, 1, *reads)
	q.EffectiveEnd = q.EffectiveEnd.Add(time.Hour)
	updated, err := store.GetActivityReport(t.Context(), filter, q)
	require.NoError(t, err)
	require.Equal(t, 900, updated.Totals.OutputTokens)
	require.Equal(t, 2, *reads)
}

func TestPartialActivityInputsSurviveRestart(t *testing.T) {
	for _, scenario := range []string{"unchanged", "custom pricing", "mirror pricing", "cursor usage"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			path := filepath.Join(t.TempDir(), "mirror.duckdb")
			keptActivityMirror(t, path, "session-a", 500)
			writeUsageMirror(t, path, `INSERT INTO model_pricing (model_pattern, output_microdollars_per_mtok) VALUES ('kept-model', 10000000)`)
			q, err := activity.ResolveQuery(activity.QueryInput{Preset: "month", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
			require.NoError(t, err)
			open := func() *Store {
				s, err := NewStore(t.Context(), path)
				require.NoError(t, err)
				require.NoError(t, s.OpenActivityReportDisk(t.Context()))
				return s
			}
			store := open()
			want := keptActivityBuild(t, store, q, true)
			require.Equal(t, money.MustParseDollars("0.005"), activity.ReportWithSessions(want).Totals.Cost)
			require.NoError(t, store.Close())
			switch scenario {
			case "mirror pricing":
				writeUsageMirror(t, path, `UPDATE model_pricing SET output_microdollars_per_mtok = 20000000 WHERE model_pattern = 'kept-model'`)
			case "cursor usage":
				writeUsageMirror(t, path, `INSERT INTO cursor_usage_events (id, occurred_at, model, output_tokens, dedup_key) VALUES (1, TIMESTAMP '2026-01-01 01:00:00', 'kept-model', 200, 'account-1')`)
			}
			restarted := open()
			defer restarted.Close()
			if scenario == "custom pricing" {
				restarted.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 20_000_000}})
			}
			q.EffectiveEnd = q.EffectiveEnd.Add(time.Minute)
			usage, runs, messages := 0, 0, 0
			previous := analyticsQueryObserver
			analyticsQueryObserver = func(query string) {
				if strings.Contains(query, "activity_usage_keyed") {
					usage++
				}
				if strings.Contains(query, "activity_run_pieces") {
					runs++
				}
				if strings.Contains(query, "SELECT session_id, role, timestamp FROM messages") {
					messages++
				}
			}
			t.Cleanup(func() { analyticsQueryObserver = previous })
			got := keptActivityBuild(t, restarted, q, true)
			wantReads := 0
			if scenario == "custom pricing" || scenario == "mirror pricing" {
				wantReads = 1
			}
			require.Equal(t, []int{wantReads, 0, 0}, []int{usage, runs, messages})
			if wantReads == 1 {
				require.Equal(t, money.MustParseDollars("0.01"), activity.ReportWithSessions(got).Totals.Cost)
			} else {
				require.Equal(t, 500, activity.ReportWithSessions(got).Totals.OutputTokens)
				require.Equal(t, money.MustParseDollars("0.005"), activity.ReportWithSessions(got).Totals.Cost)
				fresh := NewStoreFromDB(restarted.DB())
				requireKeptActivityEqual(t, keptActivityBuild(t, fresh, q, true), got)
			}
		})
	}
}

func TestPartialActivityInvalidDiskEntryIsMiss(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("CACHE_DIRECTORY", cache)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, store.OpenActivityReportDisk(t.Context()))
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "month", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	keptActivityBuild(t, store, q, true)
	require.NoError(t, store.Close())
	require.NoError(t, filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filepath.Ext(path) == ".gob" {
			return os.WriteFile(path, []byte("corrupt"), 0600)
		}
		return nil
	}))
	restarted, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer restarted.Close()
	require.NoError(t, restarted.OpenActivityReportDisk(t.Context()))
	reads := observeActivityUsageReads(t)
	got := keptActivityBuild(t, restarted, q, true)
	require.Equal(t, 1, *reads)
	require.Equal(t, 500, activity.ReportWithSessions(got).Totals.OutputTokens)
}

func TestPartialActivityUsageSameStampReplacement(t *testing.T) {
	skipReopenTestOnWindows(t)
	for _, duringPublish := range []bool{false, true} {
		t.Run(strconv.FormatBool(duringPublish), func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			dir := t.TempDir()
			path, next := filepath.Join(dir, "mirror.duckdb"), filepath.Join(dir, "next.duckdb")
			keptActivityMirror(t, path, "session-a", 500)
			keptActivityMirror(t, next, "session-b", 900)
			oldInfo, err := os.Stat(path)
			require.NoError(t, err)
			nextInfo, err := os.Stat(next)
			require.NoError(t, err)
			require.Equal(t, oldInfo.Size(), nextInfo.Size())
			require.NoError(t, os.Chtimes(next, oldInfo.ModTime(), oldInfo.ModTime()))
			q, err := activity.ResolveQuery(activity.QueryInput{Preset: "month", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
			require.NoError(t, err)
			store, err := NewStore(t.Context(), path)
			require.NoError(t, err)
			require.NoError(t, store.OpenActivityReportDisk(t.Context()))
			if duringPublish {
				paused, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var once sync.Once
				unpause := func() { once.Do(func() { close(resume) }) }
				var firstPublish atomic.Bool
				activityUsagePublishObserver = func() {
					if firstPublish.CompareAndSwap(false, true) {
						close(paused)
						<-resume
					}
				}
				t.Cleanup(func() { unpause(); <-done; activityUsagePublishObserver = nil })
				var first activity.CandidateArtifacts
				var buildErr error
				go func() {
					defer close(done)
					first, buildErr = store.BuildActivityReportArtifacts(t.Context(), db.AnalyticsFilter{Timezone: "UTC"}, q, nil)
				}()
				<-paused
				require.NoError(t, swapMirrorFile(next, path))
				store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
				changed := keptActivityBuild(t, store, q, true)
				require.Equal(t, 900, activity.ReportWithSessions(changed).Totals.OutputTokens)
				unpause()
				<-done
				require.NoError(t, buildErr)
				require.Equal(t, 500, activity.ReportWithSessions(first).Totals.OutputTokens)
				activityUsagePublishObserver = nil
			} else {
				first := keptActivityBuild(t, store, q, true)
				require.Equal(t, 500, activity.ReportWithSessions(first).Totals.OutputTokens)
				require.NoError(t, store.Close())
				require.NoError(t, swapMirrorFile(next, path))
				store, err = NewStore(t.Context(), path)
				require.NoError(t, err)
				require.NoError(t, store.OpenActivityReportDisk(t.Context()))
			}
			if !duringPublish {
				changed := keptActivityBuild(t, store, q, true)
				require.Equal(t, 900, activity.ReportWithSessions(changed).Totals.OutputTokens)
			}
			require.NoError(t, store.Close())
			reopened, err := NewStore(t.Context(), path)
			require.NoError(t, err)
			defer reopened.Close()
			require.NoError(t, reopened.OpenActivityReportDisk(t.Context()))
			reads := observeActivityUsageReads(t)
			got := keptActivityBuild(t, reopened, q, true)
			require.Equal(t, 900, activity.ReportWithSessions(got).Totals.OutputTokens)
			require.Zero(t, *reads)
		})
	}
}

func TestPartialActivityEmptyFingerprintDoesNotReuseDisk(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET agentsview_push_fingerprint = ''`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "month", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	reads := observeActivityUsageReads(t)
	for range 2 {
		store, err := NewStore(t.Context(), path)
		require.NoError(t, err)
		require.NoError(t, store.OpenActivityReportDisk(t.Context()))
		got := keptActivityBuild(t, store, q, true)
		require.Equal(t, 500, activity.ReportWithSessions(got).Totals.OutputTokens)
		require.NoError(t, store.Close())
	}
	require.Equal(t, 2, *reads)
}
