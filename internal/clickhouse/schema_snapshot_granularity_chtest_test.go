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

// snapshotKeys returns the (id, revision) pairs of table in a comparable
// form.
func snapshotKeys(t *testing.T, conn *sql.DB, table string) string {
	t.Helper()
	var keys string
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT toString(arraySort(groupArray(
			tuple(id, toString(revision))))) FROM `+table+` SETTINGS final = 0`).Scan(&keys))
	return keys
}

// revisions returns the stored revisions of one snapshot, oldest first.
func revisions(t *testing.T, conn *sql.DB, id string) []string {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(), `SELECT toString(revision) FROM usage_session_snapshots
		WHERE id = ? ORDER BY revision SETTINGS final = 0`, id)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var revision string
		require.NoError(t, rows.Scan(&revision))
		out = append(out, revision)
	}
	require.NoError(t, rows.Err())
	return out
}

func rowGranules(t *testing.T, conn *sql.DB, table string) bool {
	t.Helper()
	var engine string
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT engine_full FROM system.tables
		WHERE database = currentDatabase() AND name = ?`, table).Scan(&engine))
	return strings.Contains(engine, "index_granularity = 1,") || strings.HasSuffix(engine, "index_granularity = 1")
}

// rebuildTables lists the numbered rebuild and baseline tables.
func rebuildTables(t *testing.T, conn *sql.DB) []string {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(), `SELECT name FROM system.tables
		WHERE database = currentDatabase() AND name LIKE 'usage\_session\_snapshots\_%' ORDER BY name`)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

func execAll(t *testing.T, conn *sql.DB, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		_, err := conn.ExecContext(t.Context(), stmt)
		require.NoError(t, err, stmt)
	}
}

// The schema step moves a snapshot table with one row per granule to the
// default granularity and keeps every row, older versions and stored
// MATERIALIZED values included.
func TestSchemaRebuildsRowGranuleSnapshots(t *testing.T) {
	t.Parallel()
	store, _, _ := newPushedStore(t)
	conn := store.DB()
	makeRowGranuleSnapshots(t, conn)
	require.True(t, rowGranules(t, conn, usageSessionSnapshotTable))
	before := snapshotRows(t, conn, usageSessionSnapshotTable)

	require.NoError(t, EnsureSchemaOn(t.Context(), conn))

	assert.False(t, rowGranules(t, conn, usageSessionSnapshotTable))
	assert.Equal(t, before, snapshotRows(t, conn, usageSessionSnapshotTable))
	assert.Empty(t, rebuildTables(t, conn))
}

// Rebuild tables under an hour old belong to another process's rebuild,
// before or after its swap, so the schema step leaves every table alone.
func TestSchemaLeavesAnotherProcessRebuild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		swapped bool
		tables  []string
	}{
		{"copying", false, []string{"CREATE TABLE usage_session_snapshots_rebuild_1 AS usage_session_snapshots " +
			"ENGINE = ReplacingMergeTree(revision) ORDER BY id"}},
		{"swapped", true, []string{"CREATE TABLE usage_session_snapshots_rebuild_1 AS usage_session_snapshots " +
			"ENGINE = ReplacingMergeTree(revision) ORDER BY id SETTINGS index_granularity = 1",
			"CREATE TABLE usage_session_snapshots_baseline_1 ENGINE = MergeTree ORDER BY (id, revision) " +
				"AS SELECT id, revision FROM usage_session_snapshots SETTINGS final = 0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, _, _ := newPushedStore(t)
			conn := store.DB()
			if !tc.swapped {
				makeRowGranuleSnapshots(t, conn)
			}
			execAll(t, conn, tc.tables...)
			tables := rebuildTables(t, conn)

			require.NoError(t, rebuildUsageSnapshots(t.Context(), conn))

			assert.Equal(t, !tc.swapped, rowGranules(t, conn, usageSessionSnapshotTable))
			assert.Equal(t, tables, rebuildTables(t, conn))
		})
	}
}

// A schema step that finds a stale rebuild from before its swap drops it
// and rebuilds the table itself.
func TestSchemaReplacesStoppedRebuild(t *testing.T) {
	t.Parallel()
	store, _, _ := newPushedStore(t)
	conn := store.DB()
	makeRowGranuleSnapshots(t, conn)
	before := snapshotRows(t, conn, usageSessionSnapshotTable)
	execAll(t, conn,
		"CREATE TABLE usage_session_snapshots_rebuild_1 AS usage_session_snapshots "+
			"ENGINE = ReplacingMergeTree(revision) ORDER BY id",
		"CREATE TABLE usage_session_snapshots_baseline_1 ENGINE = MergeTree ORDER BY (id, revision) "+
			"AS SELECT id, revision FROM usage_session_snapshots SETTINGS final = 0")

	require.NoError(t, rebuildUsageSnapshotsAfter(t.Context(), conn, 0))

	assert.False(t, rowGranules(t, conn, usageSessionSnapshotTable))
	assert.Equal(t, before, snapshotRows(t, conn, usageSessionSnapshotTable))
	assert.Empty(t, rebuildTables(t, conn))
}

// A process that saw an older rebuild and claims the next number after
// another process already took a later one gives its number up and leaves
// the later rebuild's table.
func TestRebuildClaimYieldsToNewerRebuild(t *testing.T) {
	t.Parallel()
	store, _, _ := newPushedStore(t)
	conn := store.DB()
	makeRowGranuleSnapshots(t, conn)
	execAll(t, conn, "CREATE TABLE usage_session_snapshots_rebuild_3 AS usage_session_snapshots "+
		"ENGINE = ReplacingMergeTree(revision) ORDER BY id")

	claimed, err := claimUsageSnapshotRebuild(t.Context(), conn, 2)

	require.NoError(t, err)
	assert.False(t, claimed)
	assert.Equal(t, []string{"usage_session_snapshots_rebuild_3"}, rebuildTables(t, conn))
}

// Before the swap, the rebuild table takes the deletes and new versions a
// push wrote to the snapshot table during the copy, and the baseline
// records the result.
func TestRebuildCatchesUpWithPushesDuringCopy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, _, _ := newPushedStore(t)
	conn := store.DB()
	columns, err := storedColumns(ctx, conn, usageSessionSnapshotTable)
	require.NoError(t, err)
	list := strings.Join(columns, ", ")
	execAll(t, conn, "CREATE TABLE usage_session_snapshots_rebuild_1 AS usage_session_snapshots "+
		"ENGINE = ReplacingMergeTree(revision) ORDER BY id")
	require.NoError(t, copySnapshots(ctx, conn, usageSessionSnapshotTable, usageSnapshotRebuildName(1), columns))

	var deleted, republished string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT min(id), max(id) FROM usage_session_snapshots`).
		Scan(&deleted, &republished))
	execAll(t, conn,
		"DELETE FROM usage_session_snapshots WHERE id = '"+deleted+"'",
		"INSERT INTO usage_session_snapshots ("+list+") SELECT "+olderVersion(columns, 5)+
			" FROM usage_session_snapshots WHERE id = '"+republished+"' "+
			"SETTINGS final = 1, insert_allow_materialized_columns = 1")
	live := snapshotRows(t, conn, usageSessionSnapshotTable)
	require.NotEqual(t, live, snapshotRows(t, conn, usageSnapshotRebuildName(1)))

	require.NoError(t, catchUpUsageSnapshotRebuild(ctx, conn, 1, columns))

	assert.Equal(t, live, snapshotRows(t, conn, usageSnapshotRebuildName(1)))
	assert.Equal(t, snapshotKeys(t, conn, usageSessionSnapshotTable), snapshotKeys(t, conn, usageSnapshotBaselineName(1)))
}

// A schema step that finds a rebuild stopped after its swap finishes it when
// the baseline remains: the snapshot table gains what a push wrote to the
// old table before the swap, and loses what a push deleted from it then,
// while a session deleted after the swap stays deleted. An old table
// without a baseline already finished, so its rows are not copied again.
func TestSchemaFinishesRebuildStoppedAfterSwap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		baseline bool
	}{{"with baseline", true}, {"without baseline", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store, _, _ := newPushedStore(t)
			conn := store.DB()
			columns, err := storedColumns(ctx, conn, usageSessionSnapshotTable)
			require.NoError(t, err)
			list := strings.Join(columns, ", ")
			var ids []string
			require.NoError(t, conn.QueryRowContext(ctx, `SELECT arraySort(groupUniqArray(id))
				FROM usage_session_snapshots`).Scan(&ids))
			require.GreaterOrEqual(t, len(ids), 3)
			deletedBefore, pushedBefore, deletedAfter := ids[0], ids[1], ids[2]
			original := revisions(t, conn, pushedBefore)
			require.Len(t, original, 1)

			// The old table as the swap left it.
			old := usageSnapshotRebuildName(1)
			execAll(t, conn,
				"CREATE TABLE "+old+" AS usage_session_snapshots ENGINE = ReplacingMergeTree(revision) "+
					"ORDER BY id SETTINGS index_granularity = 1",
				"INSERT INTO "+old+" ("+list+") SELECT "+list+" FROM usage_session_snapshots "+
					"SETTINGS final = 0, insert_allow_materialized_columns = 1")
			if tc.baseline {
				execAll(t, conn, "CREATE TABLE "+usageSnapshotBaselineName(1)+
					" ENGINE = MergeTree ORDER BY (id, revision) AS SELECT id, revision FROM "+old+
					" SETTINGS final = 0")
			}
			// Pushes to the old table between step 3 and the swap.
			execAll(t, conn,
				"DELETE FROM "+old+" WHERE id = '"+deletedBefore+"'",
				"INSERT INTO "+old+" ("+list+") SELECT "+olderVersion(columns, 5)+" FROM "+old+
					" WHERE id = '"+pushedBefore+"' SETTINGS final = 1, insert_allow_materialized_columns = 1",
				// A push to the snapshot table after the swap.
				"DELETE FROM usage_session_snapshots WHERE id = '"+deletedAfter+"'")
			var pushed string
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT toString(max(revision)) FROM "+old+
				" WHERE id = ? SETTINGS final = 0", pushedBefore).Scan(&pushed))

			require.NoError(t, rebuildUsageSnapshotsAfter(ctx, conn, 0))

			if tc.baseline {
				assert.Empty(t, revisions(t, conn, deletedBefore))
				assert.Equal(t, append(original, pushed), revisions(t, conn, pushedBefore))
			} else {
				assert.Len(t, revisions(t, conn, deletedBefore), 1)
				assert.Equal(t, original, revisions(t, conn, pushedBefore))
			}
			assert.Empty(t, revisions(t, conn, deletedAfter))
			assert.Empty(t, rebuildTables(t, conn))
		})
	}
}
