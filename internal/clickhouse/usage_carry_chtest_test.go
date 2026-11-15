//go:build chtest

package clickhouse

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestUsageCarry(t *testing.T) {
	for _, scenario := range []string{"healthy", "unpublished", "partial deletion", "changed after read", "parts move", "snapshot ahead", "equal count older row"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			local, syncer, store := newPreparedUsageStore(t)
			if scenario == "equal count older row" {
				const id = "older-row"
				session := fixtureSession(id, "older-row", "older", "2026-01-12T03:00:00.000Z", 1)
				message := usagePriceMessage(id, 0, "2026-01-12T03:00:00.000Z", "round-test", `{"input_tokens":10}`)
				_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, Messages: []db.Message{message}, DataVersion: 1, ReplaceMessages: true}})
				require.NoError(t, err)
				pushPreparedUsage(t, syncer)
				execPreparedUsage(t, store, "CREATE TABLE older_usage AS usage_messages ENGINE = Memory",
					fmt.Sprintf("INSERT INTO older_usage SELECT * FROM usage_messages WHERE session_id = '%s'", id))
				message.TokenUsage = []byte(`{"input_tokens":100}`)
				_, err = local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, Messages: []db.Message{message}, DataVersion: 1, ReplaceMessages: true}})
				require.NoError(t, err)
				pushPreparedUsage(t, syncer)
				_, err = local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, DataVersion: 1, ReplaceMessages: true}})
				require.NoError(t, err)
				syncer.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error {
					if _, err := store.DB().ExecContext(t.Context(), "INSERT INTO usage_messages SELECT * FROM older_usage"); err != nil {
						return err
					}
					return errors.New("injected publication failure")
				}}
				_, err = syncer.Push(t.Context(), false, nil)
				require.NoError(t, err)
			} else {
				writeLongUsageSession(t, local, 100)
				if scenario == "healthy" {
					republishRoundSession(t, local, 123)
					require.NoError(t, local.SoftDeleteSession(t.Context(), usagePriceTierID))
				}
				pushPreparedUsage(t, syncer)
			}
			execPreparedUsage(t, store, "TRUNCATE TABLE prepared_usage")
			switch scenario {
			case "unpublished":
				execPreparedUsage(t, store, fmt.Sprintf(`INSERT INTO messages SELECT * REPLACE ('{"input_tokens":999999}' AS token_usage, push_version + 1 AS push_version) FROM messages WHERE session_id = '%s' AND ordinal = 0`, preparedUsageLongID))
			case "partial deletion":
				execPreparedUsage(t, store, fmt.Sprintf("ALTER TABLE usage_messages DELETE WHERE session_id = '%s' AND ordinal = 0 SETTINGS mutations_sync = 2", preparedUsageLongID))
			case "snapshot ahead":
				execPreparedUsage(t, store, fmt.Sprintf(`INSERT INTO usage_session_snapshots SELECT * REPLACE (push_version + 1 AS push_version, toUInt128(push_version + 1) * 2 + 1 AS revision) FROM usage_session_snapshots WHERE id = '%s'`, preparedUsageLongID))
			}
			filters := []db.UsageFilter{{Timezone: "UTC", Breakdowns: true}, {From: "2026-01-01", To: "2026-01-30", Timezone: "UTC", Breakdowns: true}}
			if scenario == "equal count older row" {
				filters = []db.UsageFilter{{Project: "older-row", Timezone: "UTC"}}
			}
			before := make([]preparedUsageRead, len(filters))
			for i, f := range filters {
				before[i] = readPreparedUsage(t, store, f)
			}
			if scenario == "changed after read" {
				republishRoundSession(t, local, 123)
				pushPreparedUsage(t, syncer)
			}
			runPreparedUsageRefresh(t, store)
			proofs := 0
			store.beforeUsageCarryCheck = func() {
				proofs++
				if scenario == "parts move" && proofs == 1 {
					execPreparedUsage(t, store, "INSERT INTO usage_messages SELECT * FROM usage_messages")
				}
			}
			for i, f := range filters {
				_, beforeResults := usageQueryCounts(t, store)
				daily, err := store.GetDailyUsage(t.Context(), f)
				require.NoError(t, err)
				_, afterDaily := usageQueryCounts(t, store)
				top, err := store.GetTopSessionsByCost(t.Context(), f, 10)
				require.NoError(t, err)
				_, afterTop := usageQueryCounts(t, store)
				if scenario == "healthy" {
					require.Equal(t, beforeResults, afterDaily)
					require.Equal(t, afterDaily, afterTop)
				} else {
					require.Greater(t, afterDaily, beforeResults)
					require.Greater(t, afterTop, afterDaily)
				}
				fresh := readPreparedUsage(t, NewStoreFromDB(store.DB(), store.bulk), f)
				require.Equal(t, fresh.daily, daily)
				require.Equal(t, fresh.top, top)
				if scenario == "healthy" {
					require.Equal(t, before[i].daily, daily)
					require.Equal(t, before[i].top, top)
				} else if scenario == "unpublished" || scenario == "partial deletion" || scenario == "changed after read" {
					require.NotEqual(t, before[i].daily, daily)
				} else if scenario == "equal count older row" {
					require.EqualValues(t, 10, before[i].daily.Totals.InputTokens)
					require.EqualValues(t, 100, fresh.daily.Totals.InputTokens)
				}
			}
			if scenario == "changed after read" {
				require.Zero(t, proofs)
			} else {
				require.Positive(t, proofs)
			}
		})
	}
}

func TestUsageCarryVersionTables(t *testing.T) {
	t.Parallel()
	_, _, store := newPreparedUsageStore(t)
	version := func() string {
		t.Helper()
		v, err := store.usageReadFingerprint(t.Context(), preparedUsageState{}, "")
		require.NoError(t, err)
		return v
	}
	for _, table := range chUsageCarryTables {
		execPreparedUsage(t, store, "SYSTEM STOP MERGES "+table)
	}
	insert := func(table string) {
		t.Helper()
		var name, typ string
		require.NoError(t, store.DB().QueryRowContext(t.Context(), `SELECT name, type FROM system.columns WHERE database = currentDatabase() AND table = ? AND default_kind = '' ORDER BY position LIMIT 1`, table).Scan(&name, &typ))
		execPreparedUsage(t, store, fmt.Sprintf("INSERT INTO %s (`%s`) SELECT defaultValueOfTypeName('%s')", table, name, typ))
	}
	for _, table := range []string{"prepared_usage", "sync_metadata"} {
		before := version()
		insert(table)
		require.Equal(t, before, version(), table)
	}
	expectedInputs := []string{
		"sessions", "usage_session_snapshots", "cursor_usage_events", "usage_event_prices",
		"usage_price_contexts", "model_pricing", "model_pricing_bands", "genai_pricing",
		"usage_messages", "usage_events", "usage_price_models",
	}
	for _, table := range expectedInputs {
		before := version()
		insert(table)
		require.NotEqual(t, before, version(), table)
	}
}

func TestUsageCarryDiskRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, store := newPreparedUsageStore(t)
	require.NoError(t, store.openActivityReportDisk(syncer.target))
	writeLongUsageSession(t, local, 100)
	pushPreparedUsage(t, syncer)
	runPreparedUsageRefresh(t, store)
	filters := []db.UsageFilter{{Timezone: "UTC", Breakdowns: true}, {From: "2026-01-01", To: "2026-01-30", Timezone: "UTC", Breakdowns: true}}
	old := make([]preparedUsageRead, len(filters))
	for i, f := range filters {
		old[i] = readPreparedUsage(t, store, f)
	}
	execPreparedUsage(t, store, "TRUNCATE TABLE prepared_usage")
	store = newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, store.openActivityReportDisk(syncer.target))
	candidate := readPreparedUsage(t, store, filters[0])
	runPreparedUsageRefresh(t, store)
	seed := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, seed.openActivityReportDisk(syncer.target))
	prepared := readPreparedUsage(t, seed, filters[0])
	require.Equal(t, candidate.daily, prepared.daily)
	require.Equal(t, candidate.top, prepared.top)
	proofs := 0
	store.beforeUsageCarryCheck = func() { proofs++ }
	_, before := usageQueryCounts(t, store)
	daily, err := store.GetDailyUsage(t.Context(), filters[0])
	require.NoError(t, err)
	top, err := store.GetTopSessionsByCost(t.Context(), filters[0], 10)
	require.NoError(t, err)
	_, after := usageQueryCounts(t, store)
	require.Equal(t, prepared.daily, daily)
	require.Equal(t, prepared.top, top)
	require.Equal(t, before, after)
	require.Zero(t, proofs)
	store.beforeUsageCarryCheck = nil
	republishRoundSession(t, local, 123)
	pushPreparedUsage(t, syncer)
	execPreparedUsage(t, store, "TRUNCATE TABLE prepared_usage")
	raw := make([]preparedUsageRead, len(filters))
	for i, f := range filters {
		raw[i] = readPreparedUsage(t, store, f)
		require.NotEqual(t, old[i].daily, raw[i].daily)
	}
	runPreparedUsageRefresh(t, store)
	for _, f := range filters {
		_, before := usageQueryCounts(t, store)
		_, err := store.GetDailyUsage(t.Context(), f)
		require.NoError(t, err)
		_, err = store.GetTopSessionsByCost(t.Context(), f, 10)
		require.NoError(t, err)
		_, after := usageQueryCounts(t, store)
		require.Equal(t, before, after)
	}
	reopened := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, reopened.openActivityReportDisk(syncer.target))
	for i, f := range filters {
		_, before := usageQueryCounts(t, reopened)
		daily, err := reopened.GetDailyUsage(t.Context(), f)
		require.NoError(t, err)
		top, err := reopened.GetTopSessionsByCost(t.Context(), f, 10)
		require.NoError(t, err)
		_, after := usageQueryCounts(t, reopened)
		require.Equal(t, before, after)
		require.Equal(t, raw[i].daily, daily)
		require.Equal(t, raw[i].top, top)
	}
}
