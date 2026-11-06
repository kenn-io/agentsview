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
	exec := func(stmts ...string) {
		t.Helper()
		for _, stmt := range stmts {
			_, err := store.DB().ExecContext(ctx, stmt)
			require.NoError(t, err, stmt)
		}
	}
	push := func() {
		t.Helper()
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
	}
	refresh := func() {
		t.Helper()
		exec("SYSTEM START VIEW prepare_usage", "SYSTEM REFRESH VIEW prepare_usage",
			"SYSTEM WAIT VIEW prepare_usage", "SYSTEM STOP VIEW prepare_usage")
	}
	exec("SYSTEM STOP VIEW prepare_usage")
	push()
	require.NoError(t, syncer.syncModelPricing(ctx))
	refresh()
	for _, table := range chUsageReadTables {
		exec("SYSTEM STOP MERGES " + table)
		var count uint64
		require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT count() FROM "+table).Scan(&count))
		require.NotZero(t, count, table)
	}
	f := db.UsageFilter{Timezone: "UTC", Breakdowns: true}
	type reads struct {
		daily  db.DailyUsageResult
		top    []db.TopSessionEntry
		counts db.UsageSessionCounts
	}
	read := func() reads {
		t.Helper()
		daily, err := store.GetDailyUsage(ctx, f)
		require.NoError(t, err)
		top, err := store.GetTopSessionsByCost(ctx, f, 100)
		require.NoError(t, err)
		counts, err := store.GetUsageSessionCounts(ctx, f)
		require.NoError(t, err)
		return reads{daily, top, counts}
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
			exec("INSERT INTO "+table+" SELECT * FROM "+table,
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
		exec("SYSTEM START MERGES " + table)
	}
	// Each real change must update every memo, even when a
	// particular result, such as session counts after repricing, stays equal.
	requireMiss := func(change func()) reads {
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
		push()
	})
	require.Greater(t, afterPush.daily.Totals.InputTokens, before.daily.Totals.InputTokens)
	require.NotEqual(t, before.top, afterPush.top)
	afterDelete := requireMiss(func() {
		require.NoError(t, local.SoftDeleteSession(ctx, usagePriceTierID))
		deleted, err := local.DeleteSessionIfTrashed(ctx, usagePriceTierID)
		require.NoError(t, err)
		require.EqualValues(t, 1, deleted)
		push()
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

func TestUsageRowsVersionNotKeptAcrossWrite(t *testing.T) {
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := NewStoreFromDB(syncer.conn)
	_, err := store.DB().ExecContext(ctx, "SYSTEM STOP VIEW prepare_usage")
	require.NoError(t, err)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	for _, stmt := range []string{"SYSTEM START VIEW prepare_usage", "SYSTEM REFRESH VIEW prepare_usage",
		"SYSTEM WAIT VIEW prepare_usage", "SYSTEM STOP VIEW prepare_usage", "SYSTEM STOP MERGES sessions"} {
		_, err := store.DB().ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	f := db.UsageFilter{Timezone: "UTC"}
	_, err = store.GetDailyUsage(ctx, f)
	require.NoError(t, err)
	kept := maps.Clone(store.dailyUsageRows.entries)
	versions := maps.Clone(store.usageVersions.tables)
	state, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	for _, stmt := range []string{"INSERT INTO sessions SELECT * FROM sessions", "SYSTEM START MERGES sessions",
		"OPTIMIZE TABLE sessions FINAL", "SYSTEM STOP MERGES sessions"} {
		_, err := store.DB().ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	oldCtx, err := store.withPartsSnapshot(ctx)
	require.NoError(t, err)
	appendMessage(t, local, usagePriceRoundID, "more usage", "2026-01-12T04:00:00.000Z")
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	slot, version, err := store.usageRowMemoKey(oldCtx, state, "daily", f, "", "", nil)
	require.NoError(t, err)
	require.Empty(t, slot)
	require.Empty(t, version)
	require.Equal(t, versions, store.usageVersions.tables)
	store.dailyUsageRows.put(slot, version, nil)
	require.Equal(t, kept, store.dailyUsageRows.entries)
	probes := store.usageRowIdentityQueries.Load()
	fresh, ok, err := store.usageRowsVersion(ctx, chUsageReadTables)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEmpty(t, fresh)
	require.Equal(t, probes+1, store.usageRowIdentityQueries.Load())
}
