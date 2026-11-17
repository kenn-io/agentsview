//go:build !(windows && arm64)

package duckdb

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/readbase"
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

func TestActivityUsagePartsBoundaryFailureFallsBack(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "month", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	store := openUsagePartsStore(t, path)
	defer store.Close()
	fresh := NewStoreFromDB(store.DB())
	want := keptActivityBuild(t, fresh, q, true)
	reads := observeActivityUsageReads(t)
	activityUsagePartNextHook = func() error { return errors.New("injected boundary query failure") }
	t.Cleanup(func() { activityUsagePartNextHook = nil })
	got := keptActivityBuild(t, store, q, true)
	require.Equal(t, 2, *reads)
	requireKeptActivityEqual(t, want, got)
}

func TestActivityUsagePartsRemapRates(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	writeUsageMirror(t, path,
		`UPDATE sessions SET agentsview_push_fingerprint = 'a1'`,
		`INSERT INTO model_pricing (model_pattern, output_microdollars_per_mtok) VALUES ('kept-model', 10000000), ('a-new-model', 20000000)`,
		`INSERT INTO sessions SELECT * REPLACE ('session-b' AS id, 'b1' AS agentsview_push_fingerprint) FROM sessions WHERE id = 'session-a'`,
		`INSERT INTO messages SELECT * REPLACE (3 AS id, 'session-b' AS session_id) FROM messages WHERE id = 2`,
	)
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "month", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	store := openUsagePartsStore(t, path)
	keptActivityBuild(t, store, q, true)
	require.NoError(t, store.Close())
	writeUsageMirror(t, path, `UPDATE sessions SET agentsview_push_fingerprint = 'b2' WHERE id = 'session-b'`, `UPDATE messages SET model = 'a-new-model', usage_output_tokens = 900 WHERE id = 3`)
	store = openUsagePartsStore(t, path)
	defer store.Close()
	var refreshed []string
	previous := usagePartReadObserver
	usagePartReadObserver = func(ids []string) { refreshed = append(refreshed, ids...) }
	t.Cleanup(func() { usagePartReadObserver = previous })
	got := keptActivityBuild(t, store, q, true)
	require.Equal(t, 1400, activity.ReportWithSessions(got).Totals.OutputTokens)
	require.Equal(t, money.MustParseDollars("0.023"), activity.ReportWithSessions(got).Totals.Cost)
	require.Equal(t, []string{"session-b"}, refreshed)
	fresh := NewStoreFromDB(store.DB())
	requireKeptActivityEqual(t, keptActivityBuild(t, fresh, q, true), got)
}

func TestActivityUsagePartsInvalidRateRebuilds(t *testing.T) {
	for _, invalidRate := range []int64{-1, 1} {
		t.Run(strconv.FormatInt(invalidRate, 10), func(t *testing.T) {
			cache := t.TempDir()
			t.Setenv("CACHE_DIRECTORY", cache)
			path := filepath.Join(t.TempDir(), "mirror.duckdb")
			keptActivityMirror(t, path, "session-a", 500)
			q, err := activity.ResolveQuery(activity.QueryInput{Preset: "month", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
			require.NoError(t, err)
			store := openUsagePartsStore(t, path)
			want := keptActivityBuild(t, store, q, true)
			require.NoError(t, store.Close())
			rewritten := 0
			require.NoError(t, filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
				if err != nil || filepath.Ext(path) != ".gob" {
					return err
				}
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				headerEnd := bytes.IndexByte(data, '\n')
				require.GreaterOrEqual(t, headerEnd, 0)
				rows, ok := kept.DecodeRows[kept.Part[*readbase.ActivityUsageSums]](data[headerEnd+1:])
				if !ok || len(rows) == 0 || rows[0].Value == nil || len(rows[0].Value.Groups) == 0 {
					return nil
				}
				require.Len(t, rows[0].Value.Schedule.Rates, 1)
				rows[0].Value.Groups[0].Rate = invalidRate
				payload, err := kept.EncodeRows(rows, 128<<20, func(part kept.Part[*readbase.ActivityUsageSums]) int64 { return part.Value.Bytes() })
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, append(data[:headerEnd+1], payload...), 0o600))
				rewritten++
				return nil
			}))
			require.Equal(t, 1, rewritten)
			store = openUsagePartsStore(t, path)
			defer store.Close()
			var refreshed []string
			previous := usagePartReadObserver
			usagePartReadObserver = func(ids []string) { refreshed = append(refreshed, ids...) }
			t.Cleanup(func() { usagePartReadObserver = previous })
			got := keptActivityBuild(t, store, q, true)
			require.Equal(t, []string{"session-a"}, refreshed)
			requireKeptActivityEqual(t, want, got)
		})
	}
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

func TestActivityRunsReplacementDuringReadThenRestore(t *testing.T) {
	skipReopenTestOnWindows(t)
	for _, phase := range []string{"runs", "messages"} {
		t.Run(phase, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			dir := t.TempDir()
			path, next, old := filepath.Join(dir, "mirror.duckdb"), filepath.Join(dir, "next.duckdb"), filepath.Join(dir, "old.duckdb")
			keptActivityMirror(t, path, "session-a", 500)
			keptActivityMirror(t, next, "session-a", 500)
			writeUsageMirror(t, path, `UPDATE sessions SET agentsview_push_fingerprint = 'old'`)
			writeUsageMirror(t, next, `UPDATE sessions SET agentsview_push_fingerprint = 'new'`, `UPDATE messages SET timestamp = TIMESTAMP '2026-01-01 00:04:00' WHERE id = 2`)
			store, err := NewStore(t.Context(), path)
			require.NoError(t, err)
			require.NoError(t, store.OpenActivityReportDisk(t.Context()))
			q := duckDayQuery(t, "2026-01-01", "UTC")
			previous := analyticsQueryObserver
			analyticsQueryObserver = func(query string) {
				if (phase == "runs" && strings.Contains(query, "activity_run_pieces")) || (phase == "messages" && strings.Contains(query, "SELECT session_id, role, timestamp FROM messages")) {
					analyticsQueryObserver = previous
					require.NoError(t, os.Rename(path, old))
					require.NoError(t, swapMirrorFile(next, path))
					store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
				}
			}
			t.Cleanup(func() { analyticsQueryObserver = previous })
			_, _, err = store.activityReportRuns(t.Context(), []string{"session-a"}, q)
			require.NoError(t, err)
			require.NoError(t, store.Close())
			require.NoError(t, swapMirrorFile(old, path))
			restarted, err := NewStore(t.Context(), path)
			require.NoError(t, err)
			defer restarted.Close()
			require.NoError(t, restarted.OpenActivityReportDisk(t.Context()))
			runs, entries, err := restarted.activityReportRuns(t.Context(), []string{"session-a"}, q)
			require.NoError(t, err)
			require.Len(t, runs, 1)
			require.Equal(t, time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC), runs[0].End)
			require.Equal(t, []int64{time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC).UnixMicro()}, entries["session-a"].assistants)
		})
	}
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
	for _, tc := range []struct {
		name string
		peer bool
	}{{"own", false}, {"peer", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			path := filepath.Join(t.TempDir(), "mirror.duckdb")
			keptActivityMirror(t, path, "session-a", 500)
			writeUsageMirror(t, path,
				`UPDATE sessions SET agentsview_push_fingerprint = 'a1', project = 'included'`,
				`INSERT INTO sessions SELECT * REPLACE ('session-b' AS id, 'b1' AS agentsview_push_fingerprint, TIMESTAMP '2026-01-01 12:30:00' AS ended_at, 1 AS message_count, 0 AS user_message_count) FROM sessions WHERE id = 'session-a'`,
				`INSERT INTO messages SELECT * REPLACE (3 AS id, 'session-b' AS session_id, TIMESTAMP '2026-01-01 12:30:00' AS timestamp) FROM messages WHERE id = 2`,
			)
			filter := db.AnalyticsFilter{Timezone: "UTC"}
			if tc.peer {
				writeUsageMirror(t, path,
					`UPDATE sessions SET project = 'excluded' WHERE id = 'session-b'`,
					`UPDATE messages SET claude_message_id = 'message', claude_request_id = 'request' WHERE id IN (2, 3)`,
					`UPDATE messages SET token_usage = '{"output_tokens":900}', output_tokens = 900, usage_output_tokens = 900 WHERE id = 3`,
				)
				filter.Project = "included"
			}
			store := openUsagePartsStore(t, path)
			defer store.Close()
			q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
			require.NoError(t, err)
			reads := observeActivityUsageReads(t)
			first, err := store.GetActivityReport(t.Context(), filter, q)
			require.NoError(t, err)
			require.Equal(t, 500, first.Totals.OutputTokens)
			q.EffectiveEnd = q.EffectiveEnd.Add(time.Minute)
			repeat, err := store.GetActivityReport(t.Context(), filter, q)
			require.NoError(t, err)
			require.Equal(t, 500, repeat.Totals.OutputTokens)
			require.Equal(t, 1, *reads)
			var refreshed []string
			previous := usagePartReadObserver
			usagePartReadObserver = func(ids []string) { refreshed = append(refreshed, ids...) }
			t.Cleanup(func() { usagePartReadObserver = previous })
			q.EffectiveEnd = q.EffectiveEnd.Add(time.Hour)
			updated, err := store.GetActivityReport(t.Context(), filter, q)
			require.NoError(t, err)
			if tc.peer {
				require.Equal(t, 900, updated.Totals.OutputTokens)
				require.ElementsMatch(t, []string{"session-a", "session-b"}, refreshed)
			} else {
				require.Equal(t, 1000, updated.Totals.OutputTokens)
				require.Equal(t, []string{"session-b"}, refreshed)
			}
			require.Equal(t, 2, *reads)
			q.EffectiveEnd = q.EffectiveEnd.Add(-time.Hour)
			third, err := store.GetActivityReport(t.Context(), filter, q)
			require.NoError(t, err)
			require.Equal(t, 500, third.Totals.OutputTokens)
			require.Equal(t, 3, *reads)
		})
	}
}

func TestPartialActivityInputsSurviveRestart(t *testing.T) {
	for _, scenario := range []string{"unchanged", "custom pricing", "mirror pricing", "alias pricing", "unrelated mirror write", "cursor account"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			path := filepath.Join(t.TempDir(), "mirror.duckdb")
			keptActivityMirror(t, path, "session-a", 500)
			writeUsageMirror(t, path, `INSERT INTO model_pricing (model_pattern, output_microdollars_per_mtok) VALUES ('kept-model', 10000000), ('price-a', 10000000), ('price-b', 20000000)`)
			if scenario == "alias pricing" {
				pricingpkg.SetConfiguredAliases(map[string]string{"kept-model": "price-a"})
				t.Cleanup(func() { pricingpkg.SetConfiguredAliases(nil) })
			}
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
			case "alias pricing":
				pricingpkg.SetConfiguredAliases(map[string]string{"kept-model": "price-b"})
			case "mirror pricing":
				writeUsageMirror(t, path, `UPDATE model_pricing SET output_microdollars_per_mtok = 20000000 WHERE model_pattern = 'kept-model'`)
			case "unrelated mirror write":
				writeUsageMirror(t, path, `INSERT INTO sync_metadata (key, value) VALUES ('unrelated', '1')`)
			case "cursor account":
				writeUsageMirror(t, path, `INSERT INTO cursor_usage_events (id, occurred_at, model, output_tokens, dedup_key) VALUES (1, TIMESTAMP '2026-01-01 01:00:00', 'kept-model', 200, 'account-1')`)
			}
			restarted := open()
			defer restarted.Close()
			if scenario == "custom pricing" {
				restarted.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 20_000_000}})
			}
			q.EffectiveEnd = q.EffectiveEnd.Add(time.Minute)
			usage := 0
			previous := analyticsQueryObserver
			analyticsQueryObserver = func(query string) {
				if strings.Contains(query, "activity_usage_keyed") {
					usage++
				}
			}
			t.Cleanup(func() { analyticsQueryObserver = previous })
			got := keptActivityBuild(t, restarted, q, true)
			wantReads := 0
			if scenario == "custom pricing" || scenario == "mirror pricing" || scenario == "alias pricing" {
				wantReads = 1
			}
			require.Equal(t, wantReads, usage)
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
	rewritten := 0
	require.NoError(t, filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filepath.Ext(path) == ".gob" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			headerEnd := bytes.IndexByte(data, '\n')
			require.GreaterOrEqual(t, headerEnd, 0)
			if err := os.WriteFile(path, append(data[:headerEnd+1], []byte("corrupt")...), 0o600); err != nil {
				return err
			}
			rewritten++
		}
		return nil
	}))
	require.Positive(t, rewritten)
	restarted, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer restarted.Close()
	require.NoError(t, restarted.OpenActivityReportDisk(t.Context()))
	usageReads, runReads := 0, 0
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(query string) {
		if strings.Contains(query, "activity_usage_keyed") {
			usageReads++
		}
		if strings.Contains(query, "activity_run_pieces") {
			runReads++
		}
	}
	t.Cleanup(func() { analyticsQueryObserver = previous })
	got := keptActivityBuild(t, restarted, q, true)
	require.Equal(t, 1, usageReads)
	require.Equal(t, 1, runReads)
	require.Equal(t, 500, activity.ReportWithSessions(got).Totals.OutputTokens)
}

func TestPartialActivityUsageSameStampReplacement(t *testing.T) {
	skipReopenTestOnWindows(t)
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
	store := openUsagePartsStore(t, path)
	first := keptActivityBuild(t, store, q, true)
	require.Equal(t, 500, activity.ReportWithSessions(first).Totals.OutputTokens)
	require.NoError(t, store.Close())
	require.NoError(t, swapMirrorFile(next, path))
	store = openUsagePartsStore(t, path)
	changed := keptActivityBuild(t, store, q, true)
	require.Equal(t, 900, activity.ReportWithSessions(changed).Totals.OutputTokens)
	require.NoError(t, store.Close())
	reopened := openUsagePartsStore(t, path)
	defer reopened.Close()
	reads := observeActivityUsageReads(t)
	got := keptActivityBuild(t, reopened, q, true)
	require.Equal(t, 900, activity.ReportWithSessions(got).Totals.OutputTokens)
	require.Zero(t, *reads)
}

func TestFinishedDayThenPartialMonthSurvivesRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	open := func() *Store {
		s, err := NewStore(t.Context(), path)
		require.NoError(t, err)
		require.NoError(t, s.OpenActivityReportDisk(t.Context()))
		return s
	}
	store := open()
	keptActivityBuild(t, store, duckDayQuery(t, "2026-01-01", "UTC"), true)
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "month", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	want := keptActivityBuild(t, store, q, true)
	require.NoError(t, store.Close())
	restarted := open()
	defer restarted.Close()
	reads := 0
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(query string) {
		if strings.Contains(query, "activity_run_pieces") || strings.Contains(query, "SELECT session_id, role, timestamp FROM messages") {
			reads++
		}
	}
	defer func() { analyticsQueryObserver = previous }()
	got := keptActivityBuild(t, restarted, q, true)
	require.Equal(t, 0, reads)
	requireKeptActivityEqual(t, want, got)
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

func TestPartialActivityUsageLocalTimezoneChangesAcrossRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	writeUsageMirror(t, path, `UPDATE messages SET timestamp = TIMESTAMP '2026-01-01 10:00:00' WHERE id = 2`, `UPDATE sessions SET ended_at = TIMESTAMP '2026-01-01 10:00:00'`, `INSERT INTO messages SELECT * REPLACE (3 AS id, 2 AS ordinal, TIMESTAMP '2026-01-01 02:00:00' AS timestamp, '{"output_tokens":200}' AS token_usage, 200 AS output_tokens, 200 AS usage_output_tokens) FROM messages WHERE id = 2`)
	local := time.Local //nolint:forbidigo // Exercise the process-local timezone across a restart.
	_ = time.Now().In(local)
	originalLocal := *local
	t.Cleanup(func() { *local = originalLocal })
	input := activity.QueryInput{Preset: "custom", From: "2025-12-31T00:00:00Z", To: "2026-01-03T00:00:00Z", Timezone: "Local", BucketOverride: "1d"}
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	*local = *time.UTC
	q, err := activity.ResolveQuery(input, now)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, store.OpenActivityReportDisk(t.Context()))
	first, err := store.GetActivityReport(t.Context(), db.AnalyticsFilter{Timezone: "Local"}, q)
	require.NoError(t, err)
	require.Equal(t, 700, first.Buckets[1].OutputTokens)
	require.NoError(t, store.Close())

	*local = *time.FixedZone("new local", -8*60*60)
	q, err = activity.ResolveQuery(input, now)
	require.NoError(t, err)
	restarted, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer restarted.Close()
	require.NoError(t, restarted.OpenActivityReportDisk(t.Context()))
	report, err := restarted.GetActivityReport(t.Context(), db.AnalyticsFilter{Timezone: "Local"}, q)
	require.NoError(t, err)
	require.Equal(t, 200, report.Buckets[1].OutputTokens)
	require.Equal(t, 500, report.Buckets[2].OutputTokens)
}
