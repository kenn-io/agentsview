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
	for _, failure := range []string{"identity cancellation", "parts timeout"} {
		t.Run(failure, func(t *testing.T) {
			store, _, _ := newPushedStore(t)
			_, ok, err := store.usageRowsVersion(t.Context(), chUsageReadTables, "")
			require.NoError(t, err)
			require.True(t, ok)
			kept := maps.Clone(store.usageVersions.tables)
			execPreparedUsage(t, store, "SYSTEM STOP MERGES sessions", "INSERT INTO sessions SELECT * FROM sessions")
			ctx, err := store.withPartsSnapshot(t.Context())
			require.NoError(t, err)
			failingContext := func() context.Context {
				if failure == "identity cancellation" {
					ctx, cancel := context.WithCancel(ctx)
					cancel()
					return ctx
				}
				settings := chdriver.Settings{"timeout_before_checking_execution_speed": 0}
				return chdriver.Context(ctx, chdriver.WithSettings(settings), chdriver.WithProgress(func(*chdriver.Progress) {
					// The identity query has already sent its settings. Make only
					// the following parts re-read exceed its statement budget.
					settings["max_execution_time"] = 0.000001
				}))
			}
			_, ok, err = store.usageRowsVersion(failingContext(), chUsageReadTables, "")
			require.NoError(t, err)
			require.False(t, ok)
			require.Equal(t, kept, store.usageVersions.tables)
			slot, version, err := store.usageRowMemoKey(failingContext(), preparedUsageState{ready: true}, "daily", db.UsageFilter{}, "", "", nil)
			require.NoError(t, err)
			require.Empty(t, slot)
			require.Empty(t, version)
			if failure == "parts timeout" {
				execPreparedUsage(t, store, "SYSTEM FLUSH LOGS")
				var timeouts uint64
				require.NoError(t, store.DB().QueryRowContext(t.Context(), `
			SELECT count() FROM system.query_log
			WHERE current_database = currentDatabase() AND exception_code = 159
				AND position(query, 'FROM system.parts') > 0
		`).Scan(&timeouts))
				require.EqualValues(t, 2, timeouts, "both optional parts re-reads must time out")
			}
		})
	}
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
			// Price records are probed per digest: the prepared state's stored
			// digest, the digest the daily and Top sessions reads join, and the
			// session counts' empty digest.
			wantProbes := uint64(1)
			if table == chUsagePriceTable {
				wantProbes = 3
			}
			require.Equal(t, probes+wantProbes, mergedProbes)
			require.Equal(t, results, mergedResults, "merge must reuse daily usage, Top sessions, and session count results")
		})
	}
	for _, table := range chUsageReadTables {
		execPreparedUsage(t, store, "SYSTEM START MERGES "+table)
	}
	// Each real change must update every memo, even when a
	// particular result, such as session counts after repricing, stays equal.
	requireMiss := func(change func()) {
		t.Helper()
		kept := memos()
		change()
		read()
		for i, memo := range memos() {
			require.NotEqual(t, kept[i], memo)
		}
	}
	requireMiss(func() {
		appendMessage(t, local, usagePriceRoundID, "more usage", "2026-01-12T04:00:00.000Z")
		pushPreparedUsage(t, syncer)
	})
	requireMiss(func() {
		require.NoError(t, local.SoftDeleteSession(ctx, usagePriceTierID))
		deleted, err := local.DeleteSessionIfTrashed(ctx, usagePriceTierID)
		require.NoError(t, err)
		require.EqualValues(t, 1, deleted)
		pushPreparedUsage(t, syncer)
	})
	requireMiss(func() {
		require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "round-test",
			InputPerMTok: money.MustParseDollars("2"), OutputPerMTok: money.MustParseDollars("3")}}))
		require.NoError(t, syncer.syncModelPricing(ctx))
	})
	state, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	requireMiss(func() { runPreparedUsageRefresh(t, store) })
	refreshed, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.NotEqual(t, state.stamp, refreshed.stamp)
}

// A push during either identity probe must leave subsequent usage reads fresh.
func TestUsageReadsStayFreshAfterPushDuringIdentityProbe(t *testing.T) {
	f := db.UsageFilter{Timezone: "UTC"}
	for _, table := range []string{"model_pricing", "sessions"} {
		for kind, read := range map[string]func(context.Context, *Store) (any, error){
			"counts": func(ctx context.Context, s *Store) (any, error) { return s.GetUsageSessionCounts(ctx, f) },
			"daily":  func(ctx context.Context, s *Store) (any, error) { return s.GetDailyUsage(ctx, f) },
		} {
			t.Run(table+"/"+kind, func(t *testing.T) {
				local, syncer, store := newPreparedUsageStore(t)
				execPreparedUsage(t, store, "SYSTEM STOP MERGES "+table)
				before, err := read(t.Context(), store)
				require.NoError(t, err)
				// Force the selected probe to read changed parts.
				execPreparedUsage(t, store, "INSERT INTO "+table+" SELECT * FROM "+table)
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
				require.True(t, pushed.Load(), "push must complete during the identity probe")
				if table == "sessions" {
					// Unstable coverage must not be stored under the empty version,
					// which another push during a probe would reuse.
					store.coverageCache.mu.Lock()
					fingerprint := store.coverageCache.fingerprint
					store.coverageCache.mu.Unlock()
					require.NotEmpty(t, fingerprint)
				}
				want, err := read(t.Context(), NewStoreFromDB(syncer.conn))
				require.NoError(t, err)
				require.NotEqual(t, before, want)
				got, err := read(t.Context(), store)
				require.NoError(t, err)
				require.Equal(t, want, got)
			})
		}
	}
}
