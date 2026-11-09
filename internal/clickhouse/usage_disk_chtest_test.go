//go:build chtest

package clickhouse

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/stretchr/testify/require"
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
		require.Equal(t, want, store.usageDiskFilter(ctx, state, f).To)
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
		err := restarted.DB().QueryRowContext(t.Context(), `SELECT count() FROM system.processes WHERE position(query, 'cursor_usage_events WHERE occurred_at') > 0 AND position(query, 'system.processes') = 0`).Scan(&current)
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
	fresh := newTestStore(t, syncer.conn, syncer.target)
	want, err := fresh.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	got, err := restarted.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, want, got)
}
