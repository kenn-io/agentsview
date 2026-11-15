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
	for _, scenario := range []string{"healthy", "unpublished", "partial deletion", "changed after read", "parts move", "late old writer", "snapshot ahead"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			local, syncer, store := newPreparedUsageStore(t)
			writeLongUsageSession(t, local, 100)
			if scenario == "healthy" {
				republishRoundSession(t, local, 123)
				require.NoError(t, local.SoftDeleteSession(t.Context(), usagePriceTierID))
			}
			pushPreparedUsage(t, syncer)
			execPreparedUsage(t, store, "TRUNCATE TABLE prepared_usage")
			switch scenario {
			case "unpublished":
				execPreparedUsage(t, store, fmt.Sprintf(`INSERT INTO messages SELECT * REPLACE ('{"input_tokens":999999}' AS token_usage, push_version + 1 AS push_version) FROM messages WHERE session_id = '%s' AND ordinal = 0`, preparedUsageLongID))
			case "late old writer":
				execPreparedUsage(t, store, fmt.Sprintf(`INSERT INTO usage_messages SELECT * REPLACE (toInt64(9999) AS ordinal, toInt64(1000000) AS usage_input, push_version - 1 AS push_version, toUInt128(push_version - 1) * 2 + 1 AS revision) FROM usage_messages WHERE session_id = '%s' LIMIT 1`, preparedUsageLongID))
			case "partial deletion":
				execPreparedUsage(t, store, fmt.Sprintf("ALTER TABLE usage_messages DELETE WHERE session_id = '%s' AND ordinal = 0 SETTINGS mutations_sync = 2", preparedUsageLongID))
			case "snapshot ahead":
				execPreparedUsage(t, store, fmt.Sprintf(`INSERT INTO usage_session_snapshots SELECT * REPLACE (push_version + 1 AS push_version, toUInt128(push_version + 1) * 2 + 1 AS revision) FROM usage_session_snapshots WHERE id = '%s'`, preparedUsageLongID))
			}
			filters := []db.UsageFilter{{Timezone: "UTC", Breakdowns: true}, {From: "2026-01-01", To: "2026-01-30", Timezone: "UTC", Breakdowns: true}}
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
				counts, err := store.GetUsageSessionCounts(t.Context(), f)
				require.NoError(t, err)
				got := preparedUsageRead{daily, top, counts}
				fresh := readPreparedUsage(t, NewStoreFromDB(store.DB(), store.bulk), f)
				require.Equal(t, fresh, got)
				if scenario == "late old writer" && i == 0 {
					require.NotEqual(t, before[i].top, fresh.top)
				}
				if scenario == "healthy" {
					require.Equal(t, before[i], got)
				} else if scenario == "unpublished" || scenario == "partial deletion" || scenario == "changed after read" || scenario == "late old writer" {
					require.NotEqual(t, before[i].daily, got.daily)
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
	ctx := t.Context()
	version := func() string {
		t.Helper()
		v, err := store.usageReadFingerprint(ctx, preparedUsageState{}, "")
		require.NoError(t, err)
		return v
	}
	insert := func(table string) {
		t.Helper()
		var name, typ string
		require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT name, type FROM system.columns WHERE database = currentDatabase() AND table = ? AND default_kind = '' ORDER BY position LIMIT 1`, table).Scan(&name, &typ))
		execPreparedUsage(t, store, "SYSTEM STOP MERGES "+table,
			fmt.Sprintf("INSERT INTO %s (`%s`) SELECT defaultValueOfTypeName('%s')", table, name, typ))
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

func TestUsageCarryEqualCountOlderRow(t *testing.T) {
	local, syncer, store := newPreparedUsageStore(t)
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
	execPreparedUsage(t, store, "TRUNCATE TABLE prepared_usage")
	_, err = local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, DataVersion: 1, ReplaceMessages: true}})
	require.NoError(t, err)
	injected := false
	syncer.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error {
		var remaining uint64
		if err := store.DB().QueryRowContext(t.Context(), "SELECT count() FROM usage_messages WHERE session_id = ?", id).Scan(&remaining); err != nil {
			return err
		}
		if remaining != 0 {
			return fmt.Errorf("usage row remained after deletion: %d", remaining)
		}
		if _, err := store.DB().ExecContext(t.Context(), "INSERT INTO usage_messages SELECT * FROM older_usage"); err != nil {
			return err
		}
		injected = true
		return errors.New("injected publication failure")
	}}
	result, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.Errors)
	require.True(t, injected)
	f := db.UsageFilter{Project: "older-row", Timezone: "UTC"}
	before := readPreparedUsage(t, store, f)
	runPreparedUsageRefresh(t, store)
	fresh := readPreparedUsage(t, NewStoreFromDB(store.DB(), store.bulk), f)
	got := readPreparedUsage(t, store, f)
	require.EqualValues(t, 10, before.daily.Totals.InputTokens)
	require.EqualValues(t, 100, fresh.daily.Totals.InputTokens)
	require.Len(t, before.top, 1)
	require.Len(t, fresh.top, 1)
	require.Len(t, got.top, 1)
	require.EqualValues(t, 10, before.top[0].InputTokens)
	if fresh.daily.Totals.InputTokens != got.daily.Totals.InputTokens {
		t.Errorf("Summary input tokens: want %d, got %d", fresh.daily.Totals.InputTokens, got.daily.Totals.InputTokens)
	}
	if fresh.top[0].InputTokens != got.top[0].InputTokens {
		t.Errorf("Top input tokens: want %d, got %d", fresh.top[0].InputTokens, got.top[0].InputTokens)
	}
}
