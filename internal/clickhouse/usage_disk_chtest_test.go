//go:build chtest

package clickhouse

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestUsageDiskRestartAndPush(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, first := newPreparedUsageStore(t)
	require.NoError(t, first.openActivityReportDisk(syncer.target))
	filter := db.UsageFilter{Timezone: "UTC", Breakdowns: true, PairwiseSides: true, TopSessionsByGroup: true}
	daily, err := first.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.NotEmpty(t, daily.Daily)
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "clickhouse-usage", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	dailyFile := files[0]
	for _, f := range []db.UsageFilter{
		filter,
		{Timezone: "UTC", Model: "round-test"},
		{Timezone: "America/New_York", Project: "gamma", Breakdowns: true},
		{Timezone: "UTC", Project: "no-such-project", PairwiseSides: true},
	} {
		cached := readPreparedUsage(t, first, f)
		fresh := newTestStore(t, syncer.conn, syncer.target)
		require.Equal(t, readPreparedUsage(t, fresh, f), cached)
		reopened := newTestStore(t, syncer.conn, syncer.target)
		require.NoError(t, reopened.openActivityReportDisk(syncer.target))
		_, before := usageQueryCounts(t, reopened)
		reopenedDaily, err := reopened.GetDailyUsage(t.Context(), f)
		require.NoError(t, err)
		require.Equal(t, cached.daily, reopenedDaily)
		reopenedTop, err := reopened.GetTopSessionsByCost(t.Context(), f, 10)
		require.NoError(t, err)
		require.Equal(t, cached.top, reopenedTop)
		_, after := usageQueryCounts(t, reopened)
		require.Equal(t, before, after)
	}
	body, err := os.ReadFile(dailyFile)
	require.NoError(t, err)
	header, _, ok := bytes.Cut(body, []byte("\n"))
	require.True(t, ok)
	require.NoError(t, os.WriteFile(dailyFile, append(header, []byte("\nbroken")...), 0o600))
	damaged := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, damaged.openActivityReportDisk(syncer.target))
	_, beforeDamage := usageQueryCounts(t, damaged)
	require.Equal(t, readPreparedUsage(t, newTestStore(t, syncer.conn, syncer.target), filter), readPreparedUsage(t, damaged, filter))
	_, afterDamage := usageQueryCounts(t, damaged)
	require.Greater(t, afterDamage, beforeDamage)
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	readPreparedUsage(t, restarted, filter)
	republishRoundSession(t, local, 123)
	pushPreparedUsage(t, syncer)
	got := readPreparedUsage(t, restarted, filter)
	fresh := newTestStore(t, syncer.conn, syncer.target)
	require.Equal(t, readPreparedUsage(t, fresh, filter), got)
}

func TestUsageDiskNextDay(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	_, syncer, first := newPreparedUsageStore(t)
	f := db.UsageFilter{From: "2026-01-01", To: "2026-01-30", Timezone: "America/New_York", Breakdowns: true, PairwiseSides: true, TopSessionsByGroup: true}
	extentCount := func() uint64 {
		t.Helper()
		execPreparedUsage(t, first, "SYSTEM FLUSH LOGS")
		var count uint64
		require.NoError(t, first.DB().QueryRowContext(t.Context(), `SELECT count() FROM system.query_log WHERE current_database = currentDatabase() AND type = 'QueryStart' AND startsWith(query, 'SELECT maxOrNull(ts)')`).Scan(&count))
		return count
	}
	beforeExtent := extentCount()
	_, err := first.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, beforeExtent, extentCount(), "memory-only reads should not scan the source extent")
	first = newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, first.openActivityReportDisk(syncer.target))
	daily, err := first.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.NotEmpty(t, daily.Daily)
	top, err := first.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	extents := extentCount()
	f.To = "2026-01-31"
	memDaily, err := first.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, daily, memDaily)
	memTop, err := first.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, top, memTop)
	require.Equal(t, extents, extentCount(), "next-day memory hits reuse the extent")
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	_, before := usageQueryCounts(t, restarted)
	f.To = "2026-02-01"
	again, err := restarted.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, daily, again)
	againTop, err := restarted.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, top, againTop)
	_, after := usageQueryCounts(t, restarted)
	require.Equal(t, before, after, "next-day reads should reuse both selections")
	independent := newTestStore(t, syncer.conn, syncer.target)
	freshDaily, err := independent.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, freshDaily, again)
	freshTop, err := independent.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, freshTop, againTop)

}

func TestUsageDiskMemoryReopensExactSelection(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	_, syncer, store := newPreparedUsageStore(t)
	require.NoError(t, store.openActivityReportDisk(syncer.target))
	f := db.UsageFilter{Timezone: "UTC", From: "2026-01-01", To: "2026-01-11", TopSessionsByGroup: true}
	_, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	files := func() []string {
		t.Helper()
		out, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "clickhouse-usage", "*", "*.gob"))
		require.NoError(t, err)
		return out
	}
	dailyFile := files()[0]
	unbounded := f
	unbounded.To = ""
	_, err = store.GetDailyUsage(t.Context(), unbounded)
	require.NoError(t, err)
	otherFile := ""
	for _, file := range files() {
		if file != dailyFile {
			otherFile = file
		}
	}
	require.NotEmpty(t, otherFile)
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(otherFile, old, old))
	require.NoError(t, os.Remove(dailyFile))
	_, err = store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	_, err = os.Stat(dailyFile)
	require.NoError(t, err)
	info, err := os.Stat(otherFile)
	require.NoError(t, err)
	require.Equal(t, old.Unix(), info.ModTime().Unix())

	_, err = store.GetTopSessionsByCost(t.Context(), unbounded, 10)
	require.NoError(t, err)
	topFile := ""
	for _, file := range files() {
		if file != dailyFile && file != otherFile {
			topFile = file
		}
	}
	require.NotEmpty(t, topFile)
	require.NoError(t, os.Chtimes(topFile, old, old))
	_, err = store.GetTopSessionsByCost(t.Context(), unbounded, 10)
	require.NoError(t, err)
	info, err = os.Stat(topFile)
	require.NoError(t, err)
	require.True(t, info.ModTime().After(old))
}

func TestUsageDiskTopGroupedCustomPricing(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, store := newPreparedUsageStore(t)
	var writes []db.SessionBatchWrite
	for i, name := range []string{"older name · run", "newer name · run"} {
		id := fmt.Sprintf("group-cache-%d", i)
		ts := fmt.Sprintf("2026-01-12T03:0%d:00Z", i)
		session := fixtureSession(id, "group-cache", id, ts, 1)
		session.Agent, session.GroupKey, session.SessionName = "hermes", "daily:job", &name
		writes = append(writes, db.SessionBatchWrite{Session: session, Messages: []db.Message{usagePriceMessage(id, 0, ts, "round-test", `{"input_tokens":1000000}`)}, DataVersion: 1, ReplaceMessages: true})
	}
	_, err := local.WriteSessionBatchAtomic(t.Context(), writes)
	require.NoError(t, err)
	pushPreparedUsage(t, syncer)
	runPreparedUsageRefresh(t, store)
	rates := map[string]config.CustomModelRate{"round-test": {InputMicrodollarsPerMTok: 5_000_000}}
	store.SetCustomPricing(rates)
	require.NoError(t, store.openActivityReportDisk(syncer.target))
	f := db.UsageFilter{Timezone: "UTC", Project: "group-cache", TopSessionsByGroup: true}
	want, err := store.GetTopSessionsByCost(t.Context(), f, 1)
	require.NoError(t, err)
	require.Len(t, want, 1)
	restarted := newTestStore(t, syncer.conn, syncer.target)
	restarted.SetCustomPricing(rates)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	_, before := usageQueryCounts(t, restarted)
	got, err := restarted.GetTopSessionsByCost(t.Context(), f, 1)
	require.NoError(t, err)
	require.Equal(t, want, got)
	_, after := usageQueryCounts(t, restarted)
	require.Equal(t, before, after)
	changed := newTestStore(t, syncer.conn, syncer.target)
	changed.SetCustomPricing(map[string]config.CustomModelRate{"round-test": {InputMicrodollarsPerMTok: 7_000_000}})
	require.NoError(t, changed.openActivityReportDisk(syncer.target))
	updated, err := changed.GetTopSessionsByCost(t.Context(), f, 1)
	require.NoError(t, err)
	require.NotEqual(t, want[0].Cost, updated[0].Cost)
}

func TestUsageDiskDateProofSources(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, store := newPreparedUsageStore(t)
	require.NoError(t, store.openActivityReportDisk(syncer.target))
	f := db.UsageFilter{Timezone: "America/New_York", To: "2026-01-30"}
	check := func(want string) {
		t.Helper()
		ctx, err := store.withPartsSnapshot(t.Context())
		require.NoError(t, err)
		state, err := store.preparedUsageState(ctx)
		require.NoError(t, err)
		require.True(t, state.ready)
		require.Empty(t, state.raw)
		version, err := store.usageReadFingerprint(ctx, state, state.pricingDigest)
		require.NoError(t, err)
		require.Equal(t, want, store.usageDiskFilter(ctx, state, f, version).To)
	}
	check("")
	appendMessage(t, local, usagePriceRoundID, "later", "2026-01-31T05:00:00Z")
	pushPreparedUsage(t, syncer)
	check(f.To)
	runPreparedUsageRefresh(t, store)
	check(f.To)
	f.To = "2026-02-01"
	check("")
	require.NoError(t, local.InsertCursorUsageEvents(t.Context(), []db.CursorUsageEvent{{OccurredAt: "2026-02-02T05:00:00Z", Model: "round-test", InputTokens: 7, DedupKey: "cursor:date-proof"}}))
	pushPreparedUsage(t, syncer)
	check(f.To)
	execPreparedUsage(t, store, "INSERT INTO cursor_usage_events (dedup_key, input_tokens) VALUES ('unknown-time', 1)")
	f.To = "2026-12-01"
	check(f.To)
}

func TestUsageDiskDeleteDuringDateProof(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newPreparedUsageStore(t)
	require.NoError(t, local.InsertCursorUsageEvents(t.Context(), []db.CursorUsageEvent{{OccurredAt: "2026-02-02T05:00:00Z", Model: "round-test", InputTokens: 12345, DedupKey: "cursor:delete-proof"}}))
	pushPreparedUsage(t, syncer)
	f := db.UsageFilter{Timezone: "UTC", From: "2026-01-01"}
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	var deleteErr error
	deleted := false
	restarted.beforeUsageExtent = func() {
		restarted.beforeUsageExtent = nil
		deleted = true
		_, deleteErr = restarted.DB().ExecContext(t.Context(), `ALTER TABLE cursor_usage_events DELETE WHERE dedup_key = 'cursor:delete-proof' SETTINGS mutations_sync = 2`)
	}
	f.To = "2026-01-30"
	_, err := restarted.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.True(t, deleted)
	require.NoError(t, deleteErr)
	require.False(t, restarted.usageExtent.valid)
}

func TestUsageDiskPartsChangeDuringFill(t *testing.T) {
	for _, change := range []string{"prepared", "push", "reversed", "deleted"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			local, syncer, store := newPreparedUsageStore(t)
			if change == "prepared" {
				appendMessage(t, local, usagePriceRoundID, "refresh", "2026-01-13T05:00:00Z")
				pushPreparedUsage(t, syncer)
			}
			if change == "push" {
				republishRoundSession(t, local, 123)
			}
			if change == "deleted" {
				require.NoError(t, local.InsertCursorUsageEvents(t.Context(), []db.CursorUsageEvent{{OccurredAt: "2026-01-12T05:00:00Z", Model: "round-test", InputTokens: 42, DedupKey: "cursor:fill-delete"}}))
				pushPreparedUsage(t, syncer)
			}
			require.NoError(t, store.openActivityReportDisk(syncer.target))
			f := db.UsageFilter{Timezone: "UTC", Breakdowns: true}
			requestCtx, err := store.withPartsSnapshot(t.Context())
			require.NoError(t, err)
			state, err := store.preparedUsageState(requestCtx)
			require.NoError(t, err)
			pricing, err := store.pricingSnapshot(requestCtx)
			require.NoError(t, err)
			slot, version, err := store.usageRowMemoKey(requestCtx, state, "daily", f, "", pricing.catalog.digest, nil)
			require.NoError(t, err)
			var key, value string
			if change == "reversed" {
				require.NoError(t, store.DB().QueryRowContext(t.Context(), "SELECT key, value FROM sync_metadata WHERE key != ? LIMIT 1", chExchangeProbeKey).Scan(&key, &value))
			}
			var changed atomic.Bool
			var mutationErr error
			// The daily read runs for about 100ms, the server's default
			// interactive_delay. Under it, ClickHouse 26.10 sends the read's
			// only progress packet after the query has left system.processes,
			// so the change below never runs mid-read. A 1ms interval
			// (the setting is in microseconds) makes the server report
			// progress while the read is still running.
			ctx := chdriver.Context(t.Context(), chdriver.WithSettings(chdriver.Settings{"interactive_delay": 1000}), chdriver.WithProgress(func(*chdriver.Progress) {
				if changed.Load() {
					return
				}
				var current uint64
				err := store.DB().QueryRowContext(t.Context(), `SELECT count() FROM system.processes WHERE position(query, 'FROM usage_classified') > 0 AND position(query, 'system.processes') = 0`).Scan(&current)
				if err != nil || current == 0 || !changed.CompareAndSwap(false, true) {
					return
				}
				switch change {
				case "prepared":
					runPreparedUsageRefresh(t, store)
				case "push":
					pushPreparedUsage(t, syncer)
				case "reversed":
					mutationErr = writeMetadata(t.Context(), store.DB(), map[string]string{key: value + "-temporary"})
					if mutationErr == nil {
						mutationErr = writeMetadata(t.Context(), store.DB(), map[string]string{key: value})
					}
				case "deleted":
					_, mutationErr = store.DB().ExecContext(t.Context(), "ALTER TABLE cursor_usage_events DELETE WHERE dedup_key = 'cursor:fill-delete' SETTINGS mutations_sync = 2")
				}
			}))
			_, err = store.GetDailyUsage(ctx, f)
			require.NoError(t, err)
			require.True(t, changed.Load())
			require.NoError(t, mutationErr)
			_, cached := store.dailyUsageRows.Get(slot, version)
			require.False(t, cached)
			files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "clickhouse-usage", "*", "*.gob"))
			require.NoError(t, err)
			require.Empty(t, files)
			restarted := newTestStore(t, syncer.conn, syncer.target)
			require.NoError(t, restarted.openActivityReportDisk(syncer.target))
			got, err := restarted.GetDailyUsage(t.Context(), f)
			require.NoError(t, err)
			want, err := newTestStore(t, syncer.conn, syncer.target).GetDailyUsage(t.Context(), f)
			require.NoError(t, err)
			require.Equal(t, want, got)
			if change == "reversed" {
				freshCtx, err := store.withPartsSnapshot(t.Context())
				require.NoError(t, err)
				freshState, err := store.preparedUsageState(freshCtx)
				require.NoError(t, err)
				freshVersion, err := store.usageReadFingerprint(freshCtx, freshState, pricing.catalog.digest)
				require.NoError(t, err)
				require.Equal(t, version, freshVersion)
			}
		})
	}
}
