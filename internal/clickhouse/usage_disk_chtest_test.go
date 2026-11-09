//go:build chtest

package clickhouse

import (
	"context"
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
	"go.kenn.io/agentsview/internal/money"
)

func TestUsageDiskRestartAndPush(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, first := newPreparedUsageStore(t)
	require.NoError(t, first.openActivityReportDisk(syncer.target))
	filter := db.UsageFilter{Timezone: "UTC", Breakdowns: true, PairwiseSides: true, TopSessionsByGroup: true}
	daily, err := first.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.NotEmpty(t, daily.Daily)
	top, err := first.GetTopSessionsByCost(t.Context(), filter, 10)
	require.NoError(t, err)
	_, queries := usageQueryCounts(t, first)
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	afterDaily, err := restarted.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, daily, afterDaily)
	afterTop, err := restarted.GetTopSessionsByCost(t.Context(), filter, 10)
	require.NoError(t, err)
	require.Equal(t, top, afterTop)
	_, after := usageQueryCounts(t, restarted)
	require.Equal(t, queries, after, "restart should reuse grouped selections")
	require.NoError(t, EnsureSchemaOn(t.Context(), restarted.DB()))
	execPreparedUsage(t, restarted, "OPTIMIZE TABLE sessions FINAL")
	merged := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, merged.openActivityReportDisk(syncer.target))
	_, queries = usageQueryCounts(t, merged)
	mergedDaily, err := merged.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, daily, mergedDaily)
	mergedTop, err := merged.GetTopSessionsByCost(t.Context(), filter, 10)
	require.NoError(t, err)
	require.Equal(t, top, mergedTop)
	_, after = usageQueryCounts(t, merged)
	require.Equal(t, queries, after, "schema checks and merges should preserve selections")
	for _, f := range []db.UsageFilter{
		{Timezone: "America/New_York", Project: "gamma", Breakdowns: true},
		{Timezone: "UTC", Project: "no-such-project", PairwiseSides: true},
	} {
		cached := readPreparedUsage(t, merged, f)
		fresh := newTestStore(t, syncer.conn, syncer.target)
		require.Equal(t, readPreparedUsage(t, fresh, f), cached)
		reopened := newTestStore(t, syncer.conn, syncer.target)
		require.NoError(t, reopened.openActivityReportDisk(syncer.target))
		require.Equal(t, cached, readPreparedUsage(t, reopened, f))
	}
	roundFilter := db.UsageFilter{Timezone: "UTC", Model: "round-test"}
	roundDaily, err := merged.GetDailyUsage(t.Context(), roundFilter)
	require.NoError(t, err)
	require.Equal(t, 100011, roundDaily.Totals.InputTokens)
	roundRestart := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, roundRestart.openActivityReportDisk(syncer.target))
	roundAgain, err := roundRestart.GetDailyUsage(t.Context(), roundFilter)
	require.NoError(t, err)
	require.Equal(t, 100011, roundAgain.Totals.InputTokens)
	require.Equal(t, roundDaily, roundAgain)
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "clickhouse-usage", "*", "*.gob"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	require.NoError(t, os.WriteFile(files[0], []byte("broken"), 0o600))
	damaged := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, damaged.openActivityReportDisk(syncer.target))
	require.Equal(t, readPreparedUsage(t, newTestStore(t, syncer.conn, syncer.target), filter), readPreparedUsage(t, damaged, filter))
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = damaged.GetDailyUsage(canceled, db.UsageFilter{Timezone: "UTC", Project: "other"})
	require.Error(t, err)
	republishRoundSession(t, local, 123)
	pushPreparedUsage(t, syncer)
	got := readPreparedUsage(t, restarted, filter)
	fresh := newTestStore(t, syncer.conn, syncer.target)
	require.Equal(t, readPreparedUsage(t, fresh, filter), got)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "round-test", InputPerMTok: money.MustParseDollars("2"), OutputPerMTok: money.MustParseDollars("3")}}))
	require.NoError(t, syncer.syncModelPricing(t.Context()))
	afterPricing := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, afterPricing.openActivityReportDisk(syncer.target))
	freshPricing := newTestStore(t, syncer.conn, syncer.target)
	require.Equal(t, readPreparedUsage(t, freshPricing, filter), readPreparedUsage(t, afterPricing, filter))
	require.NoError(t, local.SoftDeleteSession(t.Context(), usagePriceTierID))
	deleted, err := local.DeleteSessionIfTrashed(t.Context(), usagePriceTierID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	pushPreparedUsage(t, syncer)
	afterDelete := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, afterDelete.openActivityReportDisk(syncer.target))
	freshDelete := newTestStore(t, syncer.conn, syncer.target)
	require.Equal(t, readPreparedUsage(t, freshDelete, filter), readPreparedUsage(t, afterDelete, filter))
	require.NoError(t, local.SoftDeleteSession(t.Context(), usagePriceSnapBID))
	deleted, err = local.DeleteSessionIfTrashed(t.Context(), usagePriceSnapBID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	pushPreparedUsage(t, syncer)
	snapshotChanged := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, snapshotChanged.openActivityReportDisk(syncer.target))
	freshSnapshot := newTestStore(t, syncer.conn, syncer.target)
	require.Equal(t, readPreparedUsage(t, freshSnapshot, filter), readPreparedUsage(t, snapshotChanged, filter))
	execPreparedUsage(t, snapshotChanged, "INSERT INTO source_archives (source_archive_id, source_archive_salt) VALUES ('archive-test', 'salt')")
	archiveChanged := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, archiveChanged.openActivityReportDisk(syncer.target))
	_, queries = usageQueryCounts(t, archiveChanged)
	archiveDaily, err := archiveChanged.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	archiveTop, err := archiveChanged.GetTopSessionsByCost(t.Context(), filter, 10)
	require.NoError(t, err)
	_, after = usageQueryCounts(t, archiveChanged)
	require.Equal(t, queries, after, "archive metadata changes should preserve grouped selections")
	independent := newTestStore(t, syncer.conn, syncer.target)
	freshDaily, err := independent.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, freshDaily, archiveDaily)
	freshTop, err := independent.GetTopSessionsByCost(t.Context(), filter, 10)
	require.NoError(t, err)
	require.Equal(t, freshTop, archiveTop)
}

func TestUsageDiskNextDay(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	_, syncer, first := newPreparedUsageStore(t)
	require.NoError(t, first.openActivityReportDisk(syncer.target))
	f := db.UsageFilter{From: "2026-01-01", To: "2026-01-30", Timezone: "America/New_York", Breakdowns: true, PairwiseSides: true, TopSessionsByGroup: true}
	daily, err := first.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.NotEmpty(t, daily.Daily)
	top, err := first.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	extentCount := func() uint64 {
		t.Helper()
		execPreparedUsage(t, first, "SYSTEM FLUSH LOGS")
		var count uint64
		require.NoError(t, first.DB().QueryRowContext(t.Context(), `SELECT count() FROM system.query_log WHERE current_database = currentDatabase() AND type = 'QueryStart' AND startsWith(query, 'SELECT maxOrNull(ts)')`).Scan(&count))
		return count
	}
	extents := extentCount()
	keptFiles, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "clickhouse-usage", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, keptFiles, 2)
	for _, file := range keptFiles {
		require.NoError(t, os.Remove(file))
	}
	f.To = "2026-01-31"
	memDaily, err := first.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, daily, memDaily)
	memTop, err := first.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, top, memTop)
	require.Equal(t, extents, extentCount(), "next-day memory hits reuse the extent")
	for _, file := range keptFiles {
		_, err := os.Stat(file)
		require.NoError(t, err)
	}
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	_, before := usageQueryCounts(t, restarted)
	f.To = "2026-01-31"
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

	// Earlier end dates must retain their bounds when later usage exists.
	f.Timezone = "UTC"
	f.To = "2026-01-11"
	earlier, err := first.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	f.To = "2026-01-12"
	got, err := restarted.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	fresh, err := newTestStore(t, syncer.conn, syncer.target).GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, fresh, got)
	require.NotEqual(t, earlier.Totals.InputTokens, got.Totals.InputTokens)
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "clickhouse-usage", "*", "*.gob"))
	require.NoError(t, err)
	f.Termination = "active"
	_, err = first.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	_, err = first.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	afterFiles, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "clickhouse-usage", "*", "*.gob"))
	require.NoError(t, err)
	require.Equal(t, files, afterFiles)
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
	require.Equal(t, "newer name", want[0].DisplayName)
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
	local, syncer, store := newPreparedUsageStore(t)
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
	local, syncer, first := newPreparedUsageStore(t)
	require.NoError(t, local.InsertCursorUsageEvents(t.Context(), []db.CursorUsageEvent{{OccurredAt: "2026-02-02T05:00:00Z", Model: "round-test", InputTokens: 12345, DedupKey: "cursor:delete-proof"}}))
	pushPreparedUsage(t, syncer)
	require.NoError(t, first.openActivityReportDisk(syncer.target))
	f := db.UsageFilter{Timezone: "UTC", From: "2026-01-01"}
	_, err := first.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	var deleted atomic.Bool
	var deleteErr error
	ctx := chdriver.Context(t.Context(), chdriver.WithProgress(func(*chdriver.Progress) {
		if deleted.Load() {
			return
		}
		var current uint64
		err := restarted.DB().QueryRowContext(t.Context(), `SELECT count() FROM system.processes WHERE position(query, 'SELECT maxOrNull(ts)') > 0 AND position(query, 'system.processes') = 0`).Scan(&current)
		if err == nil && current > 0 && deleted.CompareAndSwap(false, true) {
			_, deleteErr = restarted.DB().ExecContext(t.Context(), `ALTER TABLE cursor_usage_events DELETE WHERE dedup_key = 'cursor:delete-proof' SETTINGS mutations_sync = 2`)
		}
	}))
	f.To = "2026-01-30"
	got, err := restarted.GetDailyUsage(ctx, f)
	require.NoError(t, err)
	require.True(t, deleted.Load())
	require.NoError(t, deleteErr)
	want, err := newTestStore(t, syncer.conn, syncer.target).GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestUsageDiskEvictionRebuildsSelection(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	_, syncer, first := newPreparedUsageStore(t)
	require.NoError(t, first.usageDisk.Open("usage-eviction", "mirror", 128<<20, 1))
	f := db.UsageFilter{Timezone: "UTC", Project: "gamma"}
	want, err := first.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	f.Project = "alpha"
	_, err = first.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.usageDisk.Open("usage-eviction", "mirror", 128<<20, 1))
	_, before := usageQueryCounts(t, restarted)
	f.Project = "gamma"
	got, err := restarted.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, want, got)
	_, after := usageQueryCounts(t, restarted)
	require.Greater(t, after, before)
}

func TestUsageDiskPushDuringFill(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, store := newPreparedUsageStore(t)
	require.NoError(t, store.openActivityReportDisk(syncer.target))
	f := db.UsageFilter{Timezone: "UTC", Breakdowns: true, PairwiseSides: true}
	republishRoundSession(t, local, 123)
	var pushed atomic.Bool
	ctx := chdriver.Context(t.Context(), chdriver.WithProgress(func(*chdriver.Progress) {
		if pushed.Load() {
			return
		}
		var current uint64
		err := store.DB().QueryRowContext(t.Context(), `SELECT count() FROM system.processes WHERE position(query, 'FROM usage_classified') > 0 AND position(query, 'system.processes') = 0`).Scan(&current)
		if err == nil && current > 0 && pushed.CompareAndSwap(false, true) {
			pushPreparedUsage(t, syncer)
		}
	}))
	_, err := store.GetDailyUsage(ctx, f)
	require.NoError(t, err)
	require.True(t, pushed.Load())
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "clickhouse-usage", "*", "*.gob"))
	require.NoError(t, err)
	require.Empty(t, files, "a changing fill must not publish its rows")
	fresh := newTestStore(t, syncer.conn, syncer.target)
	want, err := fresh.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	got, err := restarted.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestUsageDiskPartsChangeDuringFill(t *testing.T) {
	for _, change := range []string{"prepared", "reversed", "deleted"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			local, syncer, store := newPreparedUsageStore(t)
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
			ctx := chdriver.Context(t.Context(), chdriver.WithProgress(func(*chdriver.Progress) {
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
					_, mutationErr = store.DB().ExecContext(t.Context(), "INSERT INTO prepared_usage SELECT * FROM prepared_usage LIMIT 1")
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
			got, err := store.GetDailyUsage(t.Context(), f)
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
