//go:build chtest

package clickhouse

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

func TestUsageMemosSurviveMerges(t *testing.T) {
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := NewStoreFromDB(syncer.conn)
	refresh := func() {
		t.Helper()
		execPreparedUsage(t, store, "SYSTEM START VIEW prepare_usage", "SYSTEM REFRESH VIEW prepare_usage",
			"SYSTEM WAIT VIEW prepare_usage", "SYSTEM STOP VIEW prepare_usage")
	}
	execPreparedUsage(t, store, "SYSTEM STOP VIEW prepare_usage")
	pushPreparedUsage(t, syncer)
	require.NoError(t, syncer.syncModelPricing(ctx))
	refresh()
	for _, table := range chUsageReadTables {
		execPreparedUsage(t, store, "SYSTEM STOP MERGES "+table)
		var count uint64
		require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT count() FROM "+table).Scan(&count))
		require.NotZero(t, count, table)
	}
	f := db.UsageFilter{Timezone: "UTC", Breakdowns: true}
	read := func() preparedUsageRead {
		t.Helper()
		return readPreparedUsage(t, store, f)
	}
	before := read()
	require.NotEmpty(t, store.dailyUsageRows.entries)
	require.NotEmpty(t, store.topSessionTotals.entries)
	require.NotEmpty(t, store.usageSessionRows.entries)
	probes := store.usageRowIdentityQueries.Load()
	require.Equal(t, before, read())
	require.Equal(t, probes, store.usageRowIdentityQueries.Load(), "unchanged parts need no probes")
	for _, table := range chUsageReadTables {
		t.Run(table, func(t *testing.T) {
			daily := maps.Clone(store.dailyUsageRows.entries)
			top := maps.Clone(store.topSessionTotals.entries)
			counts := maps.Clone(store.usageSessionRows.entries)
			parts, err := store.readPartsSnapshot(ctx)
			require.NoError(t, err)
			execPreparedUsage(t, store, "INSERT INTO "+table+" SELECT * FROM "+table,
				"SYSTEM START MERGES "+table, "OPTIMIZE TABLE "+table+" FINAL", "SYSTEM STOP MERGES "+table)
			merged, err := store.readPartsSnapshot(ctx)
			require.NoError(t, err)
			require.NotEqual(t, parts[table], merged[table])
			probes := store.usageRowIdentityQueries.Load()
			require.Equal(t, before, read())
			require.Equal(t, probes+1, store.usageRowIdentityQueries.Load())
			// Reusing the backing rows proves the reads hit all three memos.
			for slot, entry := range daily {
				require.Same(t, &entry.rows[0], &store.dailyUsageRows.entries[slot].rows[0])
			}
			for slot, entry := range top {
				require.Same(t, &entry.rows[0], &store.topSessionTotals.entries[slot].rows[0])
			}
			for slot, entry := range counts {
				require.Same(t, &entry.rows[0], &store.usageSessionRows.entries[slot].rows[0])
			}
		})
	}
	for _, table := range chUsageReadTables {
		execPreparedUsage(t, store, "SYSTEM START MERGES "+table)
	}
	// Each real change must update every memo, even when a
	// particular result, such as session counts after repricing, stays equal.
	requireMiss := func(change func()) preparedUsageRead {
		t.Helper()
		daily := maps.Clone(store.dailyUsageRows.entries)
		top := maps.Clone(store.topSessionTotals.entries)
		counts := maps.Clone(store.usageSessionRows.entries)
		change()
		after := read()
		require.NotEqual(t, daily, store.dailyUsageRows.entries)
		require.NotEqual(t, top, store.topSessionTotals.entries)
		require.NotEqual(t, counts, store.usageSessionRows.entries)
		return after
	}
	afterPush := requireMiss(func() {
		appendMessage(t, local, usagePriceRoundID, "more usage", "2026-01-12T04:00:00.000Z")
		pushPreparedUsage(t, syncer)
	})
	require.Greater(t, afterPush.daily.Totals.InputTokens, before.daily.Totals.InputTokens)
	require.NotEqual(t, before.top, afterPush.top)
	afterDelete := requireMiss(func() {
		require.NoError(t, local.SoftDeleteSession(ctx, usagePriceTierID))
		deleted, err := local.DeleteSessionIfTrashed(ctx, usagePriceTierID)
		require.NoError(t, err)
		require.EqualValues(t, 1, deleted)
		pushPreparedUsage(t, syncer)
	})
	require.Equal(t, afterPush.counts.Total-1, afterDelete.counts.Total)
	require.Less(t, afterDelete.daily.Totals.InputTokens, afterPush.daily.Totals.InputTokens)
	afterPricing := requireMiss(func() {
		require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "round-test",
			InputPerMTok: money.MustParseDollars("2"), OutputPerMTok: money.MustParseDollars("3")}}))
		require.NoError(t, syncer.syncModelPricing(ctx))
	})
	require.NotEqual(t, afterDelete.daily.Totals.TotalCost, afterPricing.daily.Totals.TotalCost)
	require.NotEqual(t, afterDelete.top, afterPricing.top)
	state, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	afterRefresh := requireMiss(refresh)
	refreshed, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.NotEqual(t, state.stamp, refreshed.stamp)
	require.Equal(t, afterPricing, afterRefresh)
}

// A request can take its parts snapshot before another request's merge.
// Both coverage and range memos must still reuse the unchanged rows.
func TestUsageRowsVersionRetriesAcrossMerge(t *testing.T) {
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := NewStoreFromDB(syncer.conn)
	execPreparedUsage(t, store, "SYSTEM STOP VIEW prepare_usage")
	pushPreparedUsage(t, syncer)
	execPreparedUsage(t, store, "SYSTEM START VIEW prepare_usage", "SYSTEM REFRESH VIEW prepare_usage",
		"SYSTEM WAIT VIEW prepare_usage", "SYSTEM STOP VIEW prepare_usage", "SYSTEM STOP MERGES sessions")
	appendMessage(t, local, usagePriceRoundID, "delta usage", "2026-01-12T03:50:00.000Z")
	pushPreparedUsage(t, syncer)
	f := db.UsageFilter{Timezone: "UTC"}
	before := readPreparedUsage(t, store, f)
	kept := maps.Clone(store.dailyUsageRows.entries)
	state, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	live := store.coverageCache.live
	// The insert gives the request a changed snapshot that a later merge replaces.
	execPreparedUsage(t, store, "INSERT INTO sessions SELECT * FROM sessions")
	oldCtx, err := store.withPartsSnapshot(ctx)
	require.NoError(t, err)
	execPreparedUsage(t, store, "SYSTEM START MERGES sessions", "OPTIMIZE TABLE sessions FINAL", "SYSTEM STOP MERGES sessions")
	probes := store.usageRowIdentityQueries.Load()
	got, err := store.preparedUsageState(oldCtx)
	require.NoError(t, err)
	require.Equal(t, state, got)
	require.Equal(t, probes+2, store.usageRowIdentityQueries.Load())
	require.NotEmpty(t, live.deltaRows)
	require.Same(t, &live.deltaRows[0], &store.coverageCache.live.deltaRows[0])
	// The same old request snapshot also reaches the range memo path.
	probes = store.usageRowIdentityQueries.Load()
	slot, version, err := store.usageRowMemoKey(oldCtx, state, "daily", f, "", "", nil)
	require.NoError(t, err)
	require.NotEmpty(t, slot)
	require.NotEmpty(t, version)
	require.Equal(t, probes+1, store.usageRowIdentityQueries.Load())
	require.Equal(t, before, readPreparedUsage(t, store, f))
	for slot, entry := range kept {
		require.Same(t, &entry.rows[0], &store.dailyUsageRows.entries[slot].rows[0])
	}
	// A real write during the same interval must produce a new version.
	execPreparedUsage(t, store, "INSERT INTO sessions SELECT * FROM sessions")
	oldCtx, err = store.withPartsSnapshot(ctx)
	require.NoError(t, err)
	appendMessage(t, local, usagePriceRoundID, "more usage", "2026-01-12T04:00:00.000Z")
	pushPreparedUsage(t, syncer)
	_, changed, err := store.usageRowMemoKey(oldCtx, state, "daily", f, "", "", nil)
	require.NoError(t, err)
	require.NotEmpty(t, changed)
	require.NotEqual(t, version, changed)
	after := readPreparedUsage(t, store, f)
	require.Greater(t, after.daily.Totals.InputTokens, before.daily.Totals.InputTokens)
}

func TestUsageUnpreparedSkipsIdentityProbe(t *testing.T) {
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := NewStoreFromDB(syncer.conn)
	execPreparedUsage(t, store, "SYSTEM STOP VIEW prepare_usage")
	pushPreparedUsage(t, syncer)
	f := db.UsageFilter{Timezone: "UTC"}
	raw := readPreparedUsage(t, store, f)
	require.NotZero(t, raw.daily.Totals.InputTokens)
	require.Zero(t, store.usageRowIdentityQueries.Load())
	// Published snapshots with an empty prepared table take the second exit.
	execPreparedUsage(t, store, "TRUNCATE TABLE prepared_usage")
	require.Equal(t, raw, readPreparedUsage(t, store, f))
	require.Zero(t, store.usageRowIdentityQueries.Load())
	// Older compatible mirrors have neither snapshots nor a ready marker.
	execPreparedUsage(t, store, "DELETE FROM sync_metadata WHERE startsWith(key, '"+usageSnapshotReadyKeyBase+":') SETTINGS mutations_sync=2",
		"DROP TABLE usage_session_snapshots")
	require.NoError(t, CheckSchemaCompat(ctx, store.DB()))
	require.Equal(t, raw, readPreparedUsage(t, store, f))
	require.Zero(t, store.usageRowIdentityQueries.Load())
}
