package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
)

// usageSnapshotRebuildTable is where rebuildUsageSnapshots copies
// usage_session_snapshots before swapping the two. Its fixed name is also
// the rebuild's lock: CREATE TABLE fails when the name exists, so only one
// schema step copies at a time.
const usageSnapshotRebuildTable = usageSessionSnapshotTable + "_rebuild"

// usageSnapshotRebuildStaleSQL is how old a leftover rebuild table must be
// before a schema step treats its owner as stopped. A copy takes seconds,
// so an hour-old table belongs to a process that was killed during one.
const usageSnapshotRebuildStaleSQL = "INTERVAL 1 HOUR"

// usageSnapshotTableState is what rebuildUsageSnapshots needs to know about
// the snapshot table or its rebuild table.
type usageSnapshotTableState struct {
	exists bool
	// rowGranules reports index_granularity = 1, the setting releases
	// before this one created usage_session_snapshots with.
	rowGranules bool
	// stale reports that the table's metadata is older than
	// usageSnapshotRebuildStaleSQL.
	stale bool
}

// rebuildUsageSnapshots moves a usage_session_snapshots table created with
// one row per granule to a table with ClickHouse's default granularity.
//
// One-row granules made every merge of the table read and write one row
// per block across each column and array substream. A production
// OPTIMIZE ... FINAL of about 500,000 rows took 437 s, almost all of it
// CPU; on a copy of those rows the same merge took 167 s at one row per
// granule and 2.6 s at the default. ClickHouse cannot change
// index_granularity on an existing table, so the rows move to a new one:
//
//  1. Create the rebuild table with the snapshot table's columns, including
//     the MATERIALIZED expressions that table was created with, and the
//     default granularity.
//  2. Copy every row, older versions included, with the stored values of the
//     MATERIALIZED columns.
//  3. Apply what pushes from other processes changed during the copy: delete
//     the copied rows the snapshot table no longer holds, then copy the rows
//     it gained.
//  4. Swap the two tables with EXCHANGE TABLES, which is atomic, so reads and
//     pushes always find a complete table under the snapshot name. Just
//     before, touchUsageSnapshots marks the old table as new.
//  5. Copy the rows a push wrote to the old table between step 3 and the
//     swap, then drop the old table.
//
// A push that writes in the instant between step 5's copy and the swap
// finishing can still lose its rows, and a delete in the instant between
// step 3 and the swap can come back. The next full push repairs either.
//
// A database that cannot swap tables keeps its table: two renames would
// leave a moment with no snapshot table, and pushes and reads would fail.
func rebuildUsageSnapshots(ctx context.Context, conn *sql.DB) (err error) {
	live, build, err := readUsageSnapshotTableStates(ctx, conn)
	if err != nil {
		return err
	}
	if !live.rowGranules {
		// A rebuild table with one-row granules is the old snapshot table a
		// rebuild swapped out. Its owner may still be copying its last rows,
		// so only a stale one is dropped, without copying: rows copied that
		// late could bring back sessions deleted since the swap.
		if build.exists && build.rowGranules && build.stale {
			return dropUsageSnapshotRebuild(ctx, conn)
		}
		return nil
	}
	if build.exists {
		if !build.stale {
			log.Printf("ClickHouse: another process is rebuilding %s; this one leaves it alone", usageSessionSnapshotTable)
			return nil
		}
		log.Printf("ClickHouse: dropping %s, left by a %s rebuild that stopped", usageSnapshotRebuildTable, usageSessionSnapshotTable)
		if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+usageSnapshotRebuildTable+" SYNC"); err != nil {
			return fmt.Errorf("dropping stopped %s rebuild: %w", usageSessionSnapshotTable, err)
		}
	}
	exchange, err := chExchangeSupportedSinceStart(ctx, conn)
	if err != nil {
		return err
	}
	if !exchange {
		log.Printf("ClickHouse: this database cannot swap tables (EXCHANGE TABLES), so %s keeps one row per "+
			"granule and merging it after large pushes stays slow", usageSessionSnapshotTable)
		return nil
	}
	_, err = conn.ExecContext(ctx, "CREATE TABLE "+usageSnapshotRebuildTable+" AS "+usageSessionSnapshotTable+
		" ENGINE = ReplacingMergeTree(revision) ORDER BY id")
	if ex, ok := errors.AsType[*chdriver.Exception](err); ok && ex.Code == codeTableAlreadyExists {
		log.Printf("ClickHouse: another process is rebuilding %s; this one leaves it alone", usageSessionSnapshotTable)
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating %s rebuild: %w", usageSessionSnapshotTable, err)
	}
	swapped := false
	defer func() {
		if swapped || err == nil {
			return
		}
		// The table is this process's copy, so a failed copy drops it and the
		// next schema step can start over without waiting for it to go stale.
		// Dropping an unshared table is quick; it runs even when ctx is done.
		if _, dropErr := conn.ExecContext(context.WithoutCancel(ctx),
			"DROP TABLE IF EXISTS "+usageSnapshotRebuildTable+" SYNC"); dropErr != nil {
			err = errors.Join(err, fmt.Errorf("dropping the failed %s rebuild: %w", usageSessionSnapshotTable, dropErr))
		}
	}()
	start := time.Now()
	log.Printf("ClickHouse: rebuilding %s with larger granules so merges after large pushes run faster",
		usageSessionSnapshotTable)
	columns, err := storedColumns(ctx, conn, usageSessionSnapshotTable)
	if err != nil {
		return err
	}
	if err := copyMissingSnapshots(ctx, conn, usageSessionSnapshotTable, usageSnapshotRebuildTable, columns, false); err != nil {
		return err
	}
	if err := dropRemovedSnapshots(ctx, conn); err != nil {
		return err
	}
	if err := copyMissingSnapshots(ctx, conn, usageSessionSnapshotTable, usageSnapshotRebuildTable, columns, true); err != nil {
		return err
	}
	if err := touchUsageSnapshots(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx,
		"EXCHANGE TABLES "+usageSessionSnapshotTable+" AND "+usageSnapshotRebuildTable); err != nil {
		return fmt.Errorf("swapping in the rebuilt %s: %w", usageSessionSnapshotTable, err)
	}
	swapped = true
	if err := copyMissingSnapshots(ctx, conn, usageSnapshotRebuildTable, usageSessionSnapshotTable, columns, true); err != nil {
		return err
	}
	if err := dropUsageSnapshotRebuild(ctx, conn); err != nil {
		return err
	}
	log.Printf("ClickHouse: rebuilt %s in %s", usageSessionSnapshotTable, time.Since(start).Round(time.Millisecond))
	return nil
}

// readUsageSnapshotTableStates reads the state of usage_session_snapshots
// and its rebuild table.
func readUsageSnapshotTableStates(ctx context.Context, conn *sql.DB) (live, build usageSnapshotTableState, err error) {
	rows, err := conn.QueryContext(ctx, `SELECT name,
			extract(engine_full, 'index_granularity = ([0-9]+)') = '1',
			metadata_modification_time <= now() - `+usageSnapshotRebuildStaleSQL+`
		FROM system.tables WHERE database = currentDatabase() AND name IN (?, ?)`,
		usageSessionSnapshotTable, usageSnapshotRebuildTable)
	if err != nil {
		return live, build, fmt.Errorf("reading %s granularity: %w", usageSessionSnapshotTable, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var rowGranules, stale uint8
		if err := rows.Scan(&name, &rowGranules, &stale); err != nil {
			return live, build, fmt.Errorf("scanning %s granularity: %w", usageSessionSnapshotTable, err)
		}
		state := usageSnapshotTableState{exists: true, rowGranules: rowGranules != 0, stale: stale != 0}
		if name == usageSessionSnapshotTable {
			live = state
		} else {
			build = state
		}
	}
	if err := rows.Err(); err != nil {
		return live, build, fmt.Errorf("reading %s granularity: %w", usageSessionSnapshotTable, err)
	}
	return live, build, nil
}

// storedColumns lists the columns of table that hold data, in table order:
// ordinary, DEFAULT, and MATERIALIZED columns.
func storedColumns(ctx context.Context, conn *sql.DB, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM system.columns
		WHERE database = currentDatabase() AND table = ? AND default_kind NOT IN ('ALIAS', 'EPHEMERAL')
		ORDER BY position`, table)
	if err != nil {
		return nil, fmt.Errorf("listing %s columns: %w", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scanning %s column: %w", table, err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing %s columns: %w", table, err)
	}
	return columns, nil
}

// copyMissingSnapshots copies the rows of from into to, every version and
// the stored values of MATERIALIZED columns, so the copy recomputes
// nothing. With onlyMissing it copies only the (id, revision) pairs to
// lacks, which a push wrote to from after an earlier copy.
func copyMissingSnapshots(ctx context.Context, conn *sql.DB, from, to string, columns []string, onlyMissing bool) error {
	list := strings.Join(columns, ", ")
	where := ""
	if onlyMissing {
		where = " WHERE (id, revision) NOT IN (SELECT id, revision FROM " + to + " SETTINGS final = 0)"
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO "+to+" ("+list+") SELECT "+list+" FROM "+from+where+
		" SETTINGS final = 0, insert_allow_materialized_columns = 1"); err != nil {
		return fmt.Errorf("copying %s rows into %s: %w", from, to, err)
	}
	return nil
}

// dropRemovedSnapshots deletes from the rebuild table the copied rows that
// usage_session_snapshots no longer holds, because a push deleted their
// session or a merge replaced them with a newer version, which the rebuild
// table also gets. Most rebuilds find none, so it counts before starting a
// delete.
func dropRemovedSnapshots(ctx context.Context, conn *sql.DB) error {
	const removed = "(id, revision) NOT IN (SELECT id, revision FROM " + usageSessionSnapshotTable + " SETTINGS final = 0)"
	var count uint64
	if err := conn.QueryRowContext(ctx, "SELECT count() FROM "+usageSnapshotRebuildTable+" WHERE "+removed+
		" SETTINGS final = 0").Scan(&count); err != nil {
		return fmt.Errorf("counting %s rows removed during the rebuild: %w", usageSessionSnapshotTable, err)
	}
	if count == 0 {
		return nil
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM "+usageSnapshotRebuildTable+" WHERE "+removed); err != nil {
		return fmt.Errorf("deleting %s rows removed during the rebuild: %w", usageSessionSnapshotTable, err)
	}
	return nil
}

// touchUsageSnapshots rewrites the snapshot table's metadata, unchanged,
// so the table the swap moves to the rebuild name looks as new as the swap.
// EXCHANGE TABLES keeps each table's metadata time, and without this a
// second process would find the swapped-out table stale at once and drop it
// while its owner still copies the last rows from it.
func touchUsageSnapshots(ctx context.Context, conn *sql.DB) error {
	var comment string
	if err := conn.QueryRowContext(ctx, `SELECT comment FROM system.tables
		WHERE database = currentDatabase() AND name = ?`, usageSessionSnapshotTable).Scan(&comment); err != nil {
		return fmt.Errorf("reading the %s comment: %w", usageSessionSnapshotTable, err)
	}
	if _, err := conn.ExecContext(ctx, "ALTER TABLE "+usageSessionSnapshotTable+
		" MODIFY COMMENT "+chSQLString(comment)); err != nil {
		return fmt.Errorf("marking %s before the swap: %w", usageSessionSnapshotTable, err)
	}
	return nil
}

// dropUsageSnapshotRebuild drops the old snapshot table a rebuild swapped
// out. It does not wait for queries still reading the table, such as a
// prepared usage refresh; the server removes the data once they finish.
func dropUsageSnapshotRebuild(ctx context.Context, conn *sql.DB) error {
	if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+usageSnapshotRebuildTable); err != nil {
		return fmt.Errorf("dropping the old %s: %w", usageSessionSnapshotTable, err)
	}
	return nil
}
