//go:build chtest

package clickhouse

import (
	"context"
	"maps"
	"sync/atomic"
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

// QueryStart also records the identity probe while its progress callback runs.
func usageQueryCounts(t *testing.T, store *Store) (probes, results uint64) {
	t.Helper()
	execPreparedUsage(t, store, "SYSTEM FLUSH LOGS")
	require.NoError(t, store.DB().QueryRowContext(t.Context(), `
		SELECT countIf(startsWith(query, 'SELECT [assumeNotNull(')),
			countIf(startsWith(trimLeft(query, ' \n\t\r'), 'WITH usage_raw AS') AND
				(position(query, 'FROM usage_classified') > 0 OR position(query, 'FROM usage_localized') > 0))
		FROM system.query_log
		WHERE current_database = currentDatabase() AND type = 'QueryStart' AND is_initial_query = 1
	`).Scan(&probes, &results))
	return probes, results
}

func TestUsageRowsVersionProbeFailure(t *testing.T) {
	store, _, _ := newPushedStore(t)
	_, ok, err := store.usageRowsVersion(t.Context(), chUsageReadTables)
	require.NoError(t, err)
	require.True(t, ok)
	kept := maps.Clone(store.usageVersions.tables)
	// Dropping a table makes the identity query fail without a costly scan.
	execPreparedUsage(t, store, "DROP TABLE sessions")
	slot, version, err := store.usageRowMemoKey(t.Context(), preparedUsageState{ready: true}, "daily", db.UsageFilter{}, "", "", nil)
	require.NoError(t, err)
	require.Empty(t, slot)
	require.Empty(t, version)
	require.Equal(t, kept, store.usageVersions.tables)
}

func TestUsageMemosSurviveMerges(t *testing.T) {
	ctx := t.Context()
	local, syncer, store := newPreparedUsageStore(t)
	require.NoError(t, syncer.syncModelPricing(ctx))
	runPreparedUsageRefresh(t, store)
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
	memos := func() []any {
		return []any{maps.Clone(store.dailyUsageRows.entries), maps.Clone(store.topSessionTotals.entries),
			maps.Clone(store.usageSessionRows.entries)}
	}
	before := read()
	for _, memo := range memos() {
		require.NotEmpty(t, memo)
	}
	probes, results := usageQueryCounts(t, store)
	require.EqualValues(t, 3, results, "cold read must run all three result queries")
	require.Equal(t, before, read())
	warmProbes, warmResults := usageQueryCounts(t, store)
	require.Equal(t, probes, warmProbes, "unchanged parts need no probes")
	require.Equal(t, results, warmResults, "unchanged parts need no result queries")
	for _, table := range chUsageReadTables {
		t.Run(table, func(t *testing.T) {
			kept := memos()
			parts, err := store.readPartsSnapshot(ctx)
			require.NoError(t, err)
			execPreparedUsage(t, store, "INSERT INTO "+table+" SELECT * FROM "+table,
				"SYSTEM START MERGES "+table, "OPTIMIZE TABLE "+table+" FINAL", "SYSTEM STOP MERGES "+table)
			merged, err := store.readPartsSnapshot(ctx)
			require.NoError(t, err)
			require.NotEqual(t, parts[table], merged[table])
			probes, results := usageQueryCounts(t, store)
			require.Equal(t, before, read())
			mergedProbes, mergedResults := usageQueryCounts(t, store)
			require.Equal(t, probes+1, mergedProbes)
			require.Equal(t, results, mergedResults, "merge must reuse daily usage, Top sessions, and session count results")
			require.Equal(t, kept, memos(), "reads must be answered from all three memos")
		})
	}
	for _, table := range chUsageReadTables {
		execPreparedUsage(t, store, "SYSTEM START MERGES "+table)
	}
	// Each real change must update every memo, even when a
	// particular result, such as session counts after repricing, stays equal.
	requireMiss := func(change func()) preparedUsageRead {
		t.Helper()
		kept := memos()
		change()
		after := read()
		for i, memo := range memos() {
			require.NotEqual(t, kept[i], memo)
		}
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
	afterRefresh := requireMiss(func() { runPreparedUsageRefresh(t, store) })
	refreshed, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.NotEqual(t, state.stamp, refreshed.stamp)
	require.Equal(t, afterPricing, afterRefresh)
}

// A push after coverage selection must not cache the old coverage under new rows.
func TestUsageReadsStayFreshAfterPushDuringIdentityProbe(t *testing.T) {
	f := db.UsageFilter{Timezone: "UTC"}
	for kind, read := range map[string]func(context.Context, *Store) (any, error){
		"counts": func(ctx context.Context, s *Store) (any, error) { return s.GetUsageSessionCounts(ctx, f) },
		"daily":  func(ctx context.Context, s *Store) (any, error) { return s.GetDailyUsage(ctx, f) },
	} {
		t.Run(kind, func(t *testing.T) {
			local, syncer, store := newPreparedUsageStore(t)
			execPreparedUsage(t, store, "SYSTEM STOP MERGES model_pricing")
			before, err := read(t.Context(), store)
			require.NoError(t, err)
			// Pricing is a range dependency outside the already warm coverage key.
			execPreparedUsage(t, store, "INSERT INTO model_pricing SELECT * FROM model_pricing")
			stamp := "2026-01-12T05:00:00.000Z"
			_, err = local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
				Session:     fixtureSession("new-usage", "gamma", "new usage", stamp, 1),
				Messages:    []db.Message{usagePriceMessage("new-usage", 0, stamp, "round-test", `{"input_tokens":1}`)},
				DataVersion: 1, ReplaceMessages: true,
			}})
			require.NoError(t, err)
			probes, _ := usageQueryCounts(t, store)
			var pushed atomic.Bool
			ctx := chdriver.Context(t.Context(), chdriver.WithProgress(func(*chdriver.Progress) {
				if pushed.Load() {
					return
				}
				current, _ := usageQueryCounts(t, store)
				if current > probes && pushed.CompareAndSwap(false, true) {
					pushPreparedUsage(t, syncer)
				}
			}))
			_, err = read(ctx, store)
			require.NoError(t, err)
			require.True(t, pushed.Load(), "push must complete during the range identity probe")
			want, err := read(t.Context(), NewStoreFromDB(syncer.conn))
			require.NoError(t, err)
			require.NotEqual(t, before, want)
			got, err := read(t.Context(), store)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}
