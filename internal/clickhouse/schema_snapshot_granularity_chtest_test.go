//go:build chtest

package clickhouse

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeRowGranuleSnapshots replaces usage_session_snapshots with a copy
// created as releases before the rebuild created it, with one row per
// granule, holding the same rows and stored values plus an older version of
// one snapshot.
func makeRowGranuleSnapshots(t *testing.T, conn *sql.DB) {
	t.Helper()
	ctx := t.Context()
	columns, err := storedColumns(ctx, conn, usageSessionSnapshotTable)
	require.NoError(t, err)
	list := strings.Join(columns, ", ")
	for _, stmt := range []string{
		"CREATE TABLE snapshots_row_granules AS usage_session_snapshots ENGINE = ReplacingMergeTree(revision) " +
			"ORDER BY id SETTINGS index_granularity = 1",
		"INSERT INTO snapshots_row_granules (" + list + ") SELECT " + list + " FROM usage_session_snapshots " +
			"SETTINGS final = 0, insert_allow_materialized_columns = 1",
		"INSERT INTO snapshots_row_granules (" + list + ") SELECT " + olderVersion(columns, -1) +
			" FROM usage_session_snapshots ORDER BY id LIMIT 1 " +
			"SETTINGS final = 0, insert_allow_materialized_columns = 1",
		"EXCHANGE TABLES usage_session_snapshots AND snapshots_row_granules",
		"DROP TABLE snapshots_row_granules SYNC",
	} {
		_, err := conn.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
}

// olderVersion selects columns as another version of the same snapshot:
// push_version moved by delta, with the revision that push version gets.
func olderVersion(columns []string, delta int) string {
	selected := make([]string, len(columns))
	for i, column := range columns {
		switch column {
		case "push_version":
			selected[i] = fmt.Sprintf("toUInt64(push_version + %d) AS push_version", delta)
		case "revision":
			selected[i] = fmt.Sprintf("toUInt128(revision + %d) AS revision", 2*delta)
		default:
			selected[i] = column
		}
	}
	return strings.Join(selected, ", ")
}

// snapshotRows returns every stored row of table, older versions included,
// with its stored MATERIALIZED values, in a comparable form.
func snapshotRows(t *testing.T, conn *sql.DB, table string) string {
	t.Helper()
	var rows string
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT toString(arraySort(groupArray(tuple(
			id, toString(revision), push_version, prepares_usage, sipHash64(usage_messages),
			sipHash64(usage_facts), sipHash64(usage_recorded_costs), sipHash64(usage_events)))))
		FROM `+table+` SETTINGS final = 0`).Scan(&rows))
	return rows
}

func rowGranules(t *testing.T, conn *sql.DB, table string) bool {
	t.Helper()
	var engine string
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT engine_full FROM system.tables
		WHERE database = currentDatabase() AND name = ?`, table).Scan(&engine))
	return strings.Contains(engine, "index_granularity = 1,") || strings.HasSuffix(engine, "index_granularity = 1")
}

func tableExists(t *testing.T, conn *sql.DB, table string) bool {
	t.Helper()
	var n uint64
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT count() FROM system.tables
		WHERE database = currentDatabase() AND name = ?`, table).Scan(&n))
	return n > 0
}

// The schema step moves a snapshot table with one row per granule to the
// default granularity and keeps every row, older versions and stored
// MATERIALIZED values included.
func TestSchemaRebuildsRowGranuleSnapshots(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, _, _ := newPushedStore(t)
	conn := store.DB()
	makeRowGranuleSnapshots(t, conn)
	require.True(t, rowGranules(t, conn, usageSessionSnapshotTable))
	before := snapshotRows(t, conn, usageSessionSnapshotTable)

	require.NoError(t, EnsureSchemaOn(ctx, conn))

	assert.False(t, rowGranules(t, conn, usageSessionSnapshotTable))
	assert.Equal(t, before, snapshotRows(t, conn, usageSessionSnapshotTable))
	assert.False(t, tableExists(t, conn, usageSnapshotRebuildTable))

	// A rebuilt table needs nothing more.
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	assert.Equal(t, before, snapshotRows(t, conn, usageSessionSnapshotTable))
}

// A rebuild table under an hour old belongs to another process's rebuild,
// before or after its swap, so the schema step leaves both tables alone.
func TestSchemaLeavesAnotherProcessRebuild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rebuild string
	}{
		{"copying", "CREATE TABLE " + usageSnapshotRebuildTable + " AS usage_session_snapshots " +
			"ENGINE = ReplacingMergeTree(revision) ORDER BY id"},
		{"swapped", "CREATE TABLE " + usageSnapshotRebuildTable + " AS usage_session_snapshots " +
			"ENGINE = ReplacingMergeTree(revision) ORDER BY id SETTINGS index_granularity = 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store, _, _ := newPushedStore(t)
			conn := store.DB()
			if tc.name == "copying" {
				makeRowGranuleSnapshots(t, conn)
			}
			liveGranules := rowGranules(t, conn, usageSessionSnapshotTable)
			_, err := conn.ExecContext(ctx, tc.rebuild)
			require.NoError(t, err)

			require.NoError(t, rebuildUsageSnapshots(ctx, conn))

			assert.Equal(t, liveGranules, rowGranules(t, conn, usageSessionSnapshotTable))
			assert.True(t, tableExists(t, conn, usageSnapshotRebuildTable))
		})
	}
}

// Before the swap, the rebuild table takes the deletes and new versions a
// push wrote to the snapshot table during the copy.
func TestRebuildCatchesUpWithPushesDuringCopy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, _, _ := newPushedStore(t)
	conn := store.DB()
	columns, err := storedColumns(ctx, conn, usageSessionSnapshotTable)
	require.NoError(t, err)
	list := strings.Join(columns, ", ")
	_, err = conn.ExecContext(ctx, "CREATE TABLE "+usageSnapshotRebuildTable+" AS usage_session_snapshots "+
		"ENGINE = ReplacingMergeTree(revision) ORDER BY id")
	require.NoError(t, err)
	require.NoError(t, copyMissingSnapshots(ctx, conn, usageSessionSnapshotTable, usageSnapshotRebuildTable, columns, false))

	var deleted, republished string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT min(id), max(id) FROM usage_session_snapshots`).
		Scan(&deleted, &republished))
	for _, stmt := range []string{
		"DELETE FROM usage_session_snapshots WHERE id = '" + deleted + "'",
		"INSERT INTO usage_session_snapshots (" + list + ") SELECT " + olderVersion(columns, 5) +
			" FROM usage_session_snapshots WHERE id = '" + republished + "' " +
			"SETTINGS final = 1, insert_allow_materialized_columns = 1",
	} {
		_, err := conn.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	live := snapshotRows(t, conn, usageSessionSnapshotTable)
	require.NotEqual(t, live, snapshotRows(t, conn, usageSnapshotRebuildTable))

	require.NoError(t, dropRemovedSnapshots(ctx, conn))
	require.NoError(t, copyMissingSnapshots(ctx, conn, usageSessionSnapshotTable, usageSnapshotRebuildTable, columns, true))

	assert.Equal(t, live, snapshotRows(t, conn, usageSnapshotRebuildTable))
}
