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
	for _, scenario := range []string{"healthy", "unpublished", "partial deletion", "full deletion", "changed after read", "parts move"} {
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
			case "partial deletion", "full deletion":
				condition := ""
				if scenario == "partial deletion" {
					condition = " AND ordinal = 0"
				}
				execPreparedUsage(t, store, fmt.Sprintf("ALTER TABLE usage_messages DELETE WHERE session_id = '%s'%s SETTINGS mutations_sync = 2", preparedUsageLongID, condition))
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
			_, results := usageQueryCounts(t, store)
			for i, f := range filters {
				got := readPreparedUsage(t, store, f)
				if scenario == "parts move" && i == 0 {
					_, afterMovedRead := usageQueryCounts(t, store)
					require.Greater(t, afterMovedRead-results, uint64(1))
				}
				fresh := readPreparedUsage(t, NewStoreFromDB(store.DB(), store.bulk), f)
				require.Equal(t, fresh, got)
				if scenario == "healthy" {
					require.Equal(t, before[i], got)
				} else if scenario == "unpublished" || scenario == "partial deletion" || scenario == "full deletion" || scenario == "changed after read" {
					require.NotEqual(t, before[i].daily, got.daily)
				}
			}
			_, afterResults := usageQueryCounts(t, store)
			if scenario == "healthy" {
				require.Equal(t, 1, proofs)
				// Fresh stores issue three queries each; counts read once per filter.
				require.EqualValues(t, 8, afterResults-results)
				for i, f := range filters {
					require.Equal(t, before[i], readPreparedUsage(t, store, f))
				}
				_, finalResults := usageQueryCounts(t, store)
				require.Equal(t, 1, proofs)
				require.Equal(t, afterResults, finalResults)
			} else if scenario != "changed after read" && scenario != "parts move" {
				require.Equal(t, 1, proofs)
				require.Greater(t, afterResults-results, uint64(8))
			} else if scenario == "changed after read" {
				require.Zero(t, proofs)
				require.Greater(t, afterResults-results, uint64(8))
			}
		})
	}
}

func TestUsageCarryDiskFirst(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	_, syncer, prepared := newPreparedUsageStore(t)
	require.NoError(t, prepared.openActivityReportDisk(syncer.target))
	f := db.UsageFilter{From: "2026-01-01", To: "2026-01-30", Timezone: "UTC", Breakdowns: true}
	want := readPreparedUsage(t, prepared, f)
	execPreparedUsage(t, prepared, "TRUNCATE TABLE prepared_usage")
	store := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, store.openActivityReportDisk(syncer.target))
	require.Equal(t, want, readPreparedUsage(t, store, f))
	runPreparedUsageRefresh(t, store)
	proofs := 0
	store.beforeUsageCarryCheck = func() { proofs++ }
	_, results := usageQueryCounts(t, store)
	require.Equal(t, want, readPreparedUsage(t, store, f))
	require.Equal(t, want, readPreparedUsage(t, store, f))
	_, afterResults := usageQueryCounts(t, store)
	require.Zero(t, proofs)
	// Counts have no disk entry, so the first ready visit reads them once.
	require.EqualValues(t, 1, afterResults-results)
}

func TestUsageCarryInterruptedSoftDelete(t *testing.T) {
	t.Parallel()
	local, syncer, store := newPreparedUsageStore(t)
	ctx := t.Context()
	require.NoError(t, local.SoftDeleteSession(ctx, usagePriceRoundID))
	syncer.hooks = &pushHooks{afterUsageSnapshots: func([]db.Session) error { return errors.New("injected session publication failure") }}
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.NotZero(t, result.Errors)
	syncer.hooks = nil
	var snapshotDeleted, sessionDeleted, prepares bool
	require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT s.deleted_at IS NOT NULL, u.deleted_at IS NOT NULL, u.prepares_usage FROM sessions s INNER JOIN usage_session_snapshots u ON s.id = u.id WHERE s.id = ?`, usagePriceRoundID).Scan(&sessionDeleted, &snapshotDeleted, &prepares))
	require.False(t, sessionDeleted)
	require.True(t, snapshotDeleted)
	require.False(t, prepares)
	execPreparedUsage(t, store, "TRUNCATE TABLE prepared_usage")
	f := db.UsageFilter{Timezone: "UTC", Breakdowns: true}
	before := readPreparedUsage(t, store, f)
	runPreparedUsageRefresh(t, store)
	fresh := readPreparedUsage(t, NewStoreFromDB(store.DB(), store.bulk), f)
	require.NotEqual(t, before.daily, fresh.daily)
	require.Equal(t, fresh, readPreparedUsage(t, store, f))
}

func TestUsageCarryInterruptedStartTime(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, syncer, store := newPreparedUsageStore(t)
	const id = "carry-start"
	session := fixtureSession(id, "carry-start", "start", "2026-01-12T03:00:00.000Z", 1)
	message := usagePriceMessage(id, 0, "", "round-test", `{"input_tokens":123}`)
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{Session: session, Messages: []db.Message{message}, DataVersion: 1, ReplaceMessages: true}})
	require.NoError(t, err)
	pushPreparedUsage(t, syncer)
	session.StartedAt = new("2026-01-13T03:00:00.000Z")
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{Session: session, DataVersion: 1, ReplaceMessages: false}})
	require.NoError(t, err)
	syncer.hooks = &pushHooks{afterUsageSnapshots: func([]db.Session) error { return errors.New("injected session publication failure") }}
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.NotZero(t, result.Errors)
	syncer.hooks = nil
	var oldStart, newStart string
	var newer uint64
	require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT toString(s.started_at), toString(u.started_at), countIf(m.push_version > s.content_version) FROM sessions s INNER JOIN usage_session_snapshots u ON u.id = s.id INNER JOIN usage_messages m ON m.session_id = s.id WHERE s.id = ? GROUP BY s.started_at, u.started_at`, id).Scan(&oldStart, &newStart, &newer))
	require.Contains(t, oldStart, "2026-01-12")
	require.Contains(t, newStart, "2026-01-13")
	require.Zero(t, newer)
	execPreparedUsage(t, store, "TRUNCATE TABLE prepared_usage")
	all := db.UsageFilter{Project: "carry-start", Timezone: "UTC"}
	rangeFilter := db.UsageFilter{Project: "carry-start", From: "2026-01-12", To: "2026-01-12", Timezone: "UTC"}
	before := readPreparedUsage(t, store, all)
	require.Len(t, before.daily.Daily, 1)
	require.Equal(t, "2026-01-12", before.daily.Daily[0].Date)
	beforeRange := readPreparedUsage(t, store, rangeFilter)
	require.Len(t, beforeRange.top, 1)
	runPreparedUsageRefresh(t, store)
	fresh := NewStoreFromDB(store.DB(), store.bulk)
	expected := readPreparedUsage(t, fresh, all)
	require.Len(t, expected.daily.Daily, 1)
	require.Equal(t, "2026-01-13", expected.daily.Daily[0].Date)
	expectedRange := readPreparedUsage(t, fresh, rangeFilter)
	require.Empty(t, expectedRange.top)
	require.Equal(t, expected, readPreparedUsage(t, store, all))
	require.Equal(t, expectedRange, readPreparedUsage(t, store, rangeFilter))
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
	for _, table := range chUsageCarryTables {
		before := version()
		insert(table)
		require.NotEqual(t, before, version(), table)
	}
}
