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
