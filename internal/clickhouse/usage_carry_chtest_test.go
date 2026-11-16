//go:build chtest

package clickhouse

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestUsageCarry(t *testing.T) {
	for _, scenario := range []string{"healthy", "unpublished", "partial deletion", "changed after read", "parts move", "snapshot ahead", "snapshot restored", "snapshot merge", "raw merge", "equal count older row"} {
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
			var mutation uint64
			switch scenario {
			case "unpublished":
				execPreparedUsage(t, store, fmt.Sprintf(`INSERT INTO messages SELECT * REPLACE ('{"input_tokens":999999}' AS token_usage, push_version + 1 AS push_version) FROM messages WHERE session_id = '%s' AND ordinal = 0`, preparedUsageLongID))
			case "partial deletion":
				execPreparedUsage(t, store, fmt.Sprintf("ALTER TABLE usage_messages DELETE WHERE session_id = '%s' AND ordinal = 0 SETTINGS mutations_sync = 2", preparedUsageLongID))
			case "snapshot ahead":
				execPreparedUsage(t, store, fmt.Sprintf(`INSERT INTO usage_session_snapshots SELECT * REPLACE (push_version + 1 AS push_version, toUInt128(push_version + 1) * 2 + 1 AS revision) FROM usage_session_snapshots WHERE id = '%s'`, preparedUsageLongID))
			case "snapshot restored":
				execPreparedUsage(t, store, "CREATE TABLE original_snapshot AS usage_session_snapshots ENGINE = Memory", fmt.Sprintf("INSERT INTO original_snapshot SELECT * FROM usage_session_snapshots WHERE id = '%s'", preparedUsageLongID))
			case "raw merge":
				execPreparedUsage(t, store, "INSERT INTO usage_messages SELECT * FROM usage_messages", "OPTIMIZE TABLE usage_messages FINAL", "ALTER TABLE usage_messages UPDATE usage_input = usage_input WHERE 1 SETTINGS mutations_sync = 2", "SYSTEM STOP MERGES usage_messages")
				require.NoError(t, store.DB().QueryRowContext(t.Context(), `SELECT max(data_version) FROM system.parts WHERE database = currentDatabase() AND table = 'usage_messages' AND active`).Scan(&mutation))
				execPreparedUsage(t, store, "INSERT INTO usage_messages SELECT * FROM usage_messages")
			case "snapshot merge":
				execPreparedUsage(t, store, "SYSTEM STOP MERGES usage_session_snapshots")
			}
			var rawParts string
			if scenario == "raw merge" {
				var err error
				rawParts, err = store.tablePartsFingerprint(t.Context(), []string{"usage_messages"})
				require.NoError(t, err)
			}
			filters := []db.UsageFilter{{Timezone: "UTC", Breakdowns: true}, {From: "2026-01-01", To: "2026-01-30", Timezone: "UTC", Breakdowns: true}}
			if scenario == "equal count older row" {
				filters = []db.UsageFilter{{Project: "older-row", Timezone: "UTC"}}
			}
			before := make([]preparedUsageRead, len(filters))
			for i, f := range filters {
				before[i] = readPreparedUsage(t, store, f)
			}
			if scenario == "snapshot restored" {
				execPreparedUsage(t, store, fmt.Sprintf(`INSERT INTO usage_session_snapshots SELECT * REPLACE (push_version + 1 AS push_version, toUInt128(push_version + 1) * 2 + 1 AS revision) FROM usage_session_snapshots WHERE id = '%s'`, preparedUsageLongID))
			}
			if scenario == "snapshot merge" {
				execPreparedUsage(t, store, fmt.Sprintf("INSERT INTO usage_session_snapshots SELECT * FROM usage_session_snapshots WHERE id = '%s'", preparedUsageLongID))
				parts, err := store.tablePartsFingerprint(t.Context(), []string{usageSessionSnapshotTable})
				require.NoError(t, err)
				execPreparedUsage(t, store, "SYSTEM START MERGES usage_session_snapshots", "OPTIMIZE TABLE usage_session_snapshots FINAL")
				merged, err := store.tablePartsFingerprint(t.Context(), []string{usageSessionSnapshotTable})
				require.NoError(t, err)
				require.NotEqual(t, parts, merged)
			}
			if scenario == "raw merge" {
				beforeMerge, err := store.readPartsSnapshot(t.Context())
				require.NoError(t, err)
				execPreparedUsage(t, store, "SYSTEM START MERGES usage_messages", "OPTIMIZE TABLE usage_messages FINAL")
				afterMerge, err := store.readPartsSnapshot(t.Context())
				require.NoError(t, err)
				require.NotEqual(t, rawParts, afterMerge["usage_messages"].digest)
				m := int64(mutation)
				require.Positive(t, m)
				require.True(t, slices.ContainsFunc(beforeMerge["usage_messages"].parts, func(p partInfo) bool { return p.mutation == m }))
				require.False(t, slices.ContainsFunc(beforeMerge["usage_messages"].parts, func(p partInfo) bool { return p.minBlock <= m && m <= p.maxBlock }))
				require.True(t, slices.ContainsFunc(afterMerge["usage_messages"].parts, func(p partInfo) bool { return p.minBlock < m && m < p.maxBlock }))
			}
			if scenario == "changed after read" {
				republishRoundSession(t, local, 123)
				pushPreparedUsage(t, syncer)
			}
			runPreparedUsageRefresh(t, store)
			if scenario == "snapshot restored" {
				execPreparedUsage(t, store, "INSERT INTO usage_session_snapshots SELECT * FROM original_snapshot")
			}
			proofs := 0
			store.beforeUsageCarryCheck = func() {
				proofs++
				if scenario == "parts move" && proofs == 1 {
					execPreparedUsage(t, store, "INSERT INTO usage_messages SELECT * FROM usage_messages")
				}
			}
			rawScans := func() uint64 {
				t.Helper()
				execPreparedUsage(t, store, "SYSTEM FLUSH LOGS")
				var n uint64
				require.NoError(t, store.DB().QueryRowContext(t.Context(), `SELECT count() FROM system.query_log WHERE current_database = currentDatabase() AND type = 'QueryStart' AND is_initial_query = 1 AND (startsWith(query, 'SELECT [assumeNotNull(') OR startsWith(query, '/* final rows */ ')) AND position(query, 'FROM usage_messages') > 0`).Scan(&n))
				return n
			}
			for i, f := range filters {
				var scansBefore uint64
				if scenario == "raw merge" {
					scansBefore = rawScans()
				}
				_, beforeResults := usageQueryCounts(t, store)
				daily, err := store.GetDailyUsage(t.Context(), f)
				require.NoError(t, err)
				_, afterDaily := usageQueryCounts(t, store)
				top, err := store.GetTopSessionsByCost(t.Context(), f, 10)
				require.NoError(t, err)
				_, afterTop := usageQueryCounts(t, store)
				if scenario == "raw merge" {
					require.Equal(t, scansBefore, rawScans())
				}
				if scenario == "healthy" || scenario == "raw merge" || scenario == "snapshot merge" {
					require.Equal(t, beforeResults, afterDaily)
					require.Equal(t, afterDaily, afterTop)
				} else {
					require.Greater(t, afterDaily, beforeResults)
					require.Greater(t, afterTop, afterDaily)
				}
				fresh := readPreparedUsage(t, NewStoreFromDB(store.DB(), store.bulk), f)
				require.Equal(t, fresh.daily, daily)
				require.Equal(t, fresh.top, top)
				if scenario == "unpublished" || scenario == "partial deletion" || scenario == "changed after read" {
					require.NotEqual(t, before[i].daily, daily)
				} else if scenario == "equal count older row" {
					require.EqualValues(t, 10, before[i].daily.Totals.InputTokens)
					require.EqualValues(t, 100, fresh.daily.Totals.InputTokens)
				}
			}
			if scenario == "changed after read" || scenario == "snapshot restored" {
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
		if table == "genai_pricing" {
			execPreparedUsage(t, store, "INSERT INTO genai_pricing (singleton) VALUES (42)")
			return
		}
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
	priceVersion, err := store.usageReadFingerprint(t.Context(), preparedUsageState{}, "test-digest")
	require.NoError(t, err)
	execPreparedUsage(t, store, "INSERT INTO usage_event_prices (pricing_digest, price_key) VALUES ('test-digest', 'test-key')")
	updatedPriceVersion, err := store.usageReadFingerprint(t.Context(), preparedUsageState{}, "test-digest")
	require.NoError(t, err)
	require.NotEqual(t, priceVersion, updatedPriceVersion)
	before := version()
	execPreparedUsage(t, store, "SYSTEM START MERGES usage_session_snapshots", "ALTER TABLE usage_session_snapshots MATERIALIZE COLUMN usage_recorded_costs SETTINGS mutations_sync = 2")
	require.Equal(t, before, version())
	before = version()
	execPreparedUsage(t, store, "SYSTEM START MERGES usage_messages", "ALTER TABLE usage_messages DELETE WHERE ordinal = 0 SETTINGS mutations_sync = 2")
	require.NotEqual(t, before, version())
}

func TestUsageCarryRecordedCostMaterialization(t *testing.T) {
	t.Parallel()
	local, syncer, store := newPreparedUsageStore(t)
	const id = "recorded-cost"
	session := fixtureSession(id, "recorded-cost", "cost", "2026-01-13T00:00:00.000Z", 1)
	message := usagePriceMessage(id, 0, "2026-01-13T00:00:00.000Z", "unknown-recorded-model", `{"input_tokens":10,"recorded_cost_microdollars":1234}`)
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, Messages: []db.Message{message}, DataVersion: 1, ReplaceMessages: true}})
	require.NoError(t, err)
	pushPreparedUsage(t, syncer)
	runPreparedUsageRefresh(t, store)
	execPreparedUsage(t, store, "ALTER TABLE usage_session_snapshots DROP COLUMN usage_recorded_costs", "ALTER TABLE usage_session_snapshots ADD COLUMN "+chSnapshotRecordedCostsColumn, "TRUNCATE TABLE prepared_usage")
	filters := []db.UsageFilter{{Project: "recorded-cost", Timezone: "UTC", Breakdowns: true}, {Project: "recorded-cost", From: "2026-01-01", To: "2026-01-30", Timezone: "UTC", Breakdowns: true}}
	raw := make([]preparedUsageRead, len(filters))
	for i, f := range filters {
		raw[i] = readPreparedUsage(t, store, f)
		require.EqualValues(t, 1234, raw[i].daily.Totals.TotalCost.Microdollars)
	}
	parts, err := store.tablePartsFingerprint(t.Context(), []string{usageSessionSnapshotTable})
	require.NoError(t, err)
	execPreparedUsage(t, store, "ALTER TABLE usage_session_snapshots MATERIALIZE COLUMN usage_recorded_costs SETTINGS mutations_sync = 2")
	afterParts, err := store.tablePartsFingerprint(t.Context(), []string{usageSessionSnapshotTable})
	require.NoError(t, err)
	require.NotEqual(t, parts, afterParts)
	runPreparedUsageRefresh(t, store)
	proofs := 0
	store.beforeUsageCarryCheck = func() { proofs++ }
	for i, f := range filters {
		_, before := usageQueryCounts(t, store)
		daily, err := store.GetDailyUsage(t.Context(), f)
		require.NoError(t, err)
		top, err := store.GetTopSessionsByCost(t.Context(), f, 10)
		require.NoError(t, err)
		_, after := usageQueryCounts(t, store)
		require.Equal(t, before, after)
		fresh := readPreparedUsage(t, NewStoreFromDB(store.DB(), store.bulk), f)
		require.Equal(t, raw[i].daily, daily)
		require.Equal(t, raw[i].top, top)
		require.Equal(t, fresh.daily, daily)
		require.Equal(t, fresh.top, top)
	}
	require.Positive(t, proofs)
}

func TestUsageCarrySnapshotMovesDuringProbe(t *testing.T) {
	t.Parallel()
	local, syncer, store := newPreparedUsageStore(t)
	writeLongUsageSession(t, local, 100)
	pushPreparedUsage(t, syncer)
	execPreparedUsage(t, store, "TRUNCATE TABLE prepared_usage")
	store.afterTrackerReads = func() {
		execPreparedUsage(t, store, fmt.Sprintf(`INSERT INTO usage_session_snapshots SELECT * REPLACE (push_version + 1 AS push_version, toUInt128(push_version + 1) * 2 + 1 AS revision) FROM usage_session_snapshots WHERE id = '%s'`, preparedUsageLongID))
	}
	version, err := store.usageReadFingerprint(t.Context(), preparedUsageState{}, "")
	require.NoError(t, err)
	require.Empty(t, version)
	store.afterTrackerReads = nil
	_, before := usageQueryCounts(t, store)
	readPreparedUsage(t, store, db.UsageFilter{Timezone: "UTC"})
	_, after := usageQueryCounts(t, store)
	require.Greater(t, after, before)
}

func TestUsageCarryDiskRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, store := newPreparedUsageStore(t)
	require.NoError(t, store.openActivityReportDisk(syncer.target))
	writeLongUsageSession(t, local, 100)
	pushPreparedUsage(t, syncer)
	runPreparedUsageRefresh(t, store)
	filters := []db.UsageFilter{{Timezone: "UTC", Breakdowns: true}, {From: "2026-01-01", To: "2026-01-30", Timezone: "UTC", Breakdowns: true}}
	execPreparedUsage(t, store, "TRUNCATE TABLE prepared_usage")
	store = newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, store.openActivityReportDisk(syncer.target))
	readPreparedUsage(t, store, filters[0])
	runPreparedUsageRefresh(t, store)
	seed := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, seed.openActivityReportDisk(syncer.target))
	prepared := readPreparedUsage(t, seed, filters[0])
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
