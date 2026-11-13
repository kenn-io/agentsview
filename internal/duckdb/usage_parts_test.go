//go:build !(windows && arm64)

package duckdb

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func openUsagePartsStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, store.OpenActivityReportDisk(t.Context()))
	return store
}

func writeUsageMirror(t *testing.T, path string, statements ...string) {
	t.Helper()
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	for _, statement := range statements {
		_, err := conn.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
	require.NoError(t, conn.Close())
}

func TestUsagePartsAcrossRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	writeUsageMirror(t, path,
		`UPDATE sessions SET agentsview_push_fingerprint = 'first'`,
		`UPDATE messages SET token_usage = '{"output_tokens":500,"recorded_cost_microdollars":2000}' WHERE id = 2`,
	)
	store := openUsagePartsStore(t, path)
	f := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
	first, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	top, err := store.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, 500, first.Totals.OutputTokens)
	require.Equal(t, money.MustParseDollars("0.002"), first.Totals.TotalCost)
	require.NoError(t, store.Close())

	restarted := openUsagePartsStore(t, path)
	defer restarted.Close()
	reads := 0
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(string) { reads++ }
	t.Cleanup(func() { analyticsQueryObserver = previous })
	got, err := restarted.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	gotTop, err := restarted.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, first, got)
	require.Equal(t, top, gotTop)
	require.Zero(t, reads)
	require.NoError(t, restarted.Close())

	writeUsageMirror(t, path,
		`UPDATE sessions SET agentsview_push_fingerprint = 'second'`,
		`UPDATE messages SET token_usage = '{"output_tokens":900}' WHERE id = 2`,
	)
	changed := openUsagePartsStore(t, path)
	defer changed.Close()
	got, err = changed.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 900, got.Totals.OutputTokens)
	require.Equal(t, 1, reads)
	gotTop, err = changed.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, 2, reads)
	fresh := NewStoreFromDB(changed.DB())
	want, err := fresh.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, want, got)
	wantTop, err := fresh.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, wantTop, gotTop)
	reads = 0
	changed.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 10_000_000}})
	repriced, err := changed.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.009"), repriced.Totals.TotalCost)
	require.Equal(t, 1, reads)
	require.NoError(t, changed.Close())
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "duckdb-usage", "*", "*.gob"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, file := range files {
		require.NoError(t, os.WriteFile(file, []byte("truncated"), 0600))
	}
	restarted, err = NewStore(t.Context(), path)
	require.NoError(t, err)
	defer restarted.Close()
	restarted.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 10_000_000}})
	require.NoError(t, restarted.OpenActivityReportDisk(t.Context()))
	reads = 0
	got, err = restarted.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, repriced, got)
	require.Equal(t, 1, reads)
}

func TestUsagePartsRefreshesDuplicatePeers(t *testing.T) {
	for _, tc := range []struct {
		name, update string
		first        int
	}{
		{"claude", "claude_message_id = 'message', claude_request_id = 'request'", 700},
		{"source", "source_uuid = 'shared'", 500},
	} {
		t.Run(tc.name, func(t *testing.T) { testUsagePartsPeers(t, tc.update, tc.first) })
	}
}

func testUsagePartsPeers(t *testing.T, update string, firstOutput int) {
	t.Helper()
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	writeUsageMirror(t, path,
		`UPDATE sessions SET agentsview_push_fingerprint = 'a1'`,
		`INSERT INTO sessions SELECT * REPLACE ('session-b' AS id, 'b1' AS agentsview_push_fingerprint, 'beta' AS project) FROM sessions WHERE id = 'session-a'`,
		`UPDATE messages SET `+update+` WHERE id = 2`,
		`INSERT INTO messages SELECT * REPLACE (3 AS id, 'session-b' AS session_id, '{"output_tokens":700}' AS token_usage) FROM messages WHERE id = 2`,
		`INSERT INTO messages SELECT * REPLACE (id + 10 AS id, ordinal + 10 AS ordinal, '{}' AS token_usage, CASE WHEN claude_message_id != '' THEN claude_message_id || '-other' ELSE '' END AS claude_message_id, CASE WHEN source_uuid != '' THEN source_uuid || '-other' ELSE '' END AS source_uuid) FROM messages WHERE id IN (2, 3)`,
		`INSERT INTO sessions SELECT * REPLACE ('session-c' AS id, 'c1' AS agentsview_push_fingerprint) FROM sessions WHERE id = 'session-b'`,
		`INSERT INTO messages SELECT * REPLACE (23 AS id, 'session-c' AS session_id) FROM messages WHERE id = 13`,
	)
	f := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
	store := openUsagePartsStore(t, path)
	peers, err := store.usagePartPeers(t.Context())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"session-a": "session-a", "session-b": "session-a", "session-c": "session-a"}, peers)
	first, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, firstOutput, first.Totals.OutputTokens)
	filtered := f
	filtered.Project = "alpha"
	selected, err := store.GetDailyUsage(t.Context(), filtered)
	require.NoError(t, err)
	require.Equal(t, firstOutput, selected.Totals.OutputTokens)
	require.NoError(t, store.Close())
	writeUsageMirror(t, path,
		`UPDATE sessions SET agentsview_push_fingerprint = 'a2' WHERE id = 'session-a'`,
		`UPDATE messages SET token_usage = '{"output_tokens":900}' WHERE id = 2`,
	)
	store = openUsagePartsStore(t, path)
	defer store.Close()
	var refreshed []string
	previous := usagePartReadObserver
	usagePartReadObserver = func(ids []string) { refreshed = append(refreshed, ids...) }
	t.Cleanup(func() { usagePartReadObserver = previous })
	got, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"session-a", "session-b", "session-c"}, refreshed)
	require.Equal(t, 900, got.Totals.OutputTokens)
	fresh := NewStoreFromDB(store.DB())
	want, err := fresh.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, want, got)
	refreshed = nil
	selected, err = store.GetDailyUsage(t.Context(), filtered)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"session-a", "session-b", "session-c"}, refreshed)
	require.Equal(t, 900, selected.Totals.OutputTokens)
	require.NoError(t, store.Close())
	writeUsageMirror(t, path,
		`UPDATE sessions SET agentsview_push_fingerprint = agentsview_push_fingerprint || '-split' WHERE id IN ('session-a', 'session-b')`,
		`UPDATE messages SET claude_message_id = '', source_uuid = '' WHERE id IN (12, 13)`,
	)
	store = openUsagePartsStore(t, path)
	refreshed = nil
	_, err = store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"session-a", "session-b", "session-c"}, refreshed)
	require.NoError(t, store.Close())
	writeUsageMirror(t, path, `UPDATE sessions SET deleted_at = CURRENT_TIMESTAMP WHERE id = 'session-a'`)
	store = openUsagePartsStore(t, path)
	defer store.Close()
	refreshed = nil
	got, err = store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"session-b"}, refreshed)
	require.Equal(t, 700, got.Totals.OutputTokens)
	fresh = NewStoreFromDB(store.DB())
	want, err = fresh.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestUsagePartsMirrorPricingChange(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	pricingpkg.SetConfiguredAliases(map[string]string{"kept-model": "price-a"})
	t.Cleanup(func() { pricingpkg.SetConfiguredAliases(nil) })
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	writeUsageMirror(t, path,
		`UPDATE sessions SET agentsview_push_fingerprint = 'a1'`,
		`INSERT INTO model_pricing (model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok, cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at) VALUES ('price-a', 0, 10000000, 0, 0, ''), ('price-b', 0, 20000000, 0, 0, '')`,
	)
	f := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
	store := openUsagePartsStore(t, path)
	first, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.005"), first.Totals.TotalCost)
	_, err = store.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	pricingpkg.SetConfiguredAliases(map[string]string{"kept-model": "price-b"})
	store = openUsagePartsStore(t, path)
	aliased, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.01"), aliased.Totals.TotalCost)
	top, err := store.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.01"), top[0].Cost)
	require.NoError(t, store.Close())
	writeUsageMirror(t, path, `UPDATE model_pricing SET output_microdollars_per_mtok = 30000000 WHERE model_pattern = 'price-b'`)
	store = openUsagePartsStore(t, path)
	defer store.Close()
	reads := 0
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(string) { reads++ }
	t.Cleanup(func() { analyticsQueryObserver = previous })
	got, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.015"), got.Totals.TotalCost)
	require.Equal(t, 1, reads)
}

func TestUsagePartsRefreshesCursorAccount(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	writeUsageMirror(t, path, `UPDATE sessions SET agentsview_push_fingerprint = 'a1'`)
	f := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
	store := openUsagePartsStore(t, path)
	_, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	writeUsageMirror(t, path, `INSERT INTO cursor_usage_events (id, occurred_at, model, output_tokens, dedup_key) VALUES (1, TIMESTAMP '2026-01-01 01:00:00', 'kept-model', 200, 'account-1')`)
	store = openUsagePartsStore(t, path)
	defer store.Close()
	var refreshed []string
	previous := usagePartReadObserver
	usagePartReadObserver = func(ids []string) { refreshed = append(refreshed, ids...) }
	t.Cleanup(func() { usagePartReadObserver = previous })
	got, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, []string{""}, refreshed)
	require.Equal(t, 700, got.Totals.OutputTokens)
	fresh := NewStoreFromDB(store.DB())
	want, err := fresh.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.NoError(t, store.Close())
	writeUsageMirror(t, path, `UPDATE cursor_usage_events SET output_tokens = 300 WHERE id = 1`)
	store = openUsagePartsStore(t, path)
	defer store.Close()
	refreshed = nil
	got, err = store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, []string{""}, refreshed)
	require.Equal(t, 800, got.Totals.OutputTokens)
}

func TestUsagePartsRefreshesAuthoritativeCost(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	writeUsageMirror(t, path,
		`UPDATE sessions SET agentsview_push_fingerprint = 'a1', agent = 'copilot'`,
		`INSERT INTO messages SELECT * REPLACE (3 AS id, 2 AS ordinal, TIMESTAMP '2026-01-02 00:01:00' AS timestamp) FROM messages WHERE id = 2`,
		`INSERT INTO usage_events (id, session_id, source, model, cost_microdollars, cost_status, cost_source, occurred_at, dedup_key) VALUES (1, 'session-a', 'shutdown', 'kept-model', 10000, 'exact', 'copilot-reported', TIMESTAMP '2026-01-02 00:02:00', 'cost')`,
	)
	f := db.UsageFilter{From: "2026-01-01", To: "2026-01-02", Timezone: "UTC"}
	rates := map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 10_000_000}}
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	store.SetCustomPricing(rates)
	require.NoError(t, store.OpenActivityReportDisk(t.Context()))
	first, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.01"), first.Totals.TotalCost)
	require.NoError(t, store.Close())
	writeUsageMirror(t, path,
		`UPDATE sessions SET agentsview_push_fingerprint = 'a2'`,
		`UPDATE usage_events SET cost_microdollars = 20000 WHERE id = 1`,
	)
	store, err = NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	store.SetCustomPricing(rates)
	require.NoError(t, store.OpenActivityReportDisk(t.Context()))
	got, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, got.Daily, 2)
	require.Equal(t, money.MustParseDollars("0.01"), got.Daily[0].TotalCost)
	require.Equal(t, money.MustParseDollars("0.01"), got.Daily[1].TotalCost)
	fresh := NewStoreFromDB(store.DB())
	fresh.SetCustomPricing(rates)
	want, err := fresh.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestUsagePartsReplacementDuringPublish(t *testing.T) {
	skipReopenTestOnWindows(t)
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	dir := t.TempDir()
	path, next := filepath.Join(dir, "mirror.duckdb"), filepath.Join(dir, "next.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	keptActivityMirror(t, next, "session-a", 900)
	for _, file := range []string{path, next} {
		conn, err := Open(t.Context(), file)
		require.NoError(t, err)
		_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET agentsview_push_fingerprint = 'same'`)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	}
	info, err := os.Stat(path)
	require.NoError(t, err)
	nextInfo, err := os.Stat(next)
	require.NoError(t, err)
	require.Equal(t, info.Size(), nextInfo.Size())
	require.NoError(t, os.Chtimes(next, info.ModTime(), info.ModTime()))
	store := openUsagePartsStore(t, path)
	initial := store
	t.Cleanup(func() { _ = initial.Close() })
	paused, resume := make(chan struct{}), make(chan struct{})
	previous := usagePartPublishObserver
	usagePartPublishObserver = func() { close(paused); <-resume }
	t.Cleanup(func() { usagePartPublishObserver = previous })
	done := make(chan struct{})
	var readErr error
	var resumeOnce sync.Once
	unpause := func() { resumeOnce.Do(func() { close(resume) }) }
	go func() { defer close(done); _, readErr = store.GetDailyUsage(t.Context(), db.UsageFilter{}) }()
	t.Cleanup(func() {
		unpause()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("usage read did not finish")
		}
	})
	select {
	case <-paused:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "usage read did not reach publication")
	}
	require.NoError(t, swapMirrorFile(next, path))
	store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
	unpause()
	select {
	case <-done:
		require.NoError(t, readErr)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "usage read did not finish")
	}
	usagePartPublishObserver = nil
	require.NoError(t, store.Close())
	store = openUsagePartsStore(t, path)
	defer store.Close()
	reads := 0
	prevQuery := analyticsQueryObserver
	analyticsQueryObserver = func(string) { reads++ }
	t.Cleanup(func() { analyticsQueryObserver = prevQuery })
	got, err := store.GetDailyUsage(t.Context(), db.UsageFilter{})
	require.NoError(t, err)
	require.Equal(t, 900, got.Totals.OutputTokens)
	require.Equal(t, 1, reads)
}

func TestUsagePartsDefaultTimezone(t *testing.T) {
	if path := os.Getenv("USAGE_PARTS_TZ_MIRROR"); path != "" {
		store := openUsagePartsStore(t, path)
		defer store.Close()
		result, err := store.GetDailyUsage(t.Context(), db.UsageFilter{})
		require.NoError(t, err)
		require.Len(t, result.Daily, 1)
		require.Equal(t, os.Getenv("USAGE_PARTS_TZ_DATE"), result.Daily[0].Date)
		return
	}
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	writeUsageMirror(t, path, `UPDATE sessions SET agentsview_push_fingerprint = 'a1'`, `UPDATE messages SET timestamp = TIMESTAMP '2026-01-02 02:00:00' WHERE id = 2`)
	for _, tc := range []struct{ zone, day string }{{"America/New_York", "2026-01-01"}, {"UTC", "2026-01-02"}} {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUsagePartsDefaultTimezone$")
		cmd.Env = append(os.Environ(), "TZ="+tc.zone, "USAGE_PARTS_TZ_MIRROR="+path, "USAGE_PARTS_TZ_DATE="+tc.day)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
}
