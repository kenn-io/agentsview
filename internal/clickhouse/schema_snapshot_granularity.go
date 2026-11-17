package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
)

// A rebuild of usage_session_snapshots works in two numbered tables: the
// rebuild table it copies the snapshots into, which after the swap holds the
// old table, and the baseline table, the (id, revision) pairs the copy held
// just before the swap. Each rebuild takes the next number, and CREATE TABLE
// fails when the name exists, so a number belongs to one rebuild. Numbers
// keep a process that saw an abandoned rebuild from dropping the tables of
// the rebuild that replaced it: it drops only the numbers it saw.
const (
	usageSnapshotRebuildPrefix  = usageSessionSnapshotTable + "_rebuild_"
	usageSnapshotBaselinePrefix = usageSessionSnapshotTable + "_baseline_"
	usageSnapshotRebuildPattern = "^" + usageSessionSnapshotTable + "_(rebuild|baseline)_[0-9]+$"
)

// usageSnapshotRebuildStale is how old the tables of a rebuild must be
// before a schema step treats its process as stopped. A rebuild takes
// seconds, so hour-old tables belong to a process that was killed during
// one.
const usageSnapshotRebuildStale = time.Hour

// usageSnapshotNoExchangeLog logs once per process that the snapshot table
// stays slow to merge, since every schema step would otherwise repeat it.
var usageSnapshotNoExchangeLog sync.Once

func usageSnapshotRebuildName(number uint64) string {
	return fmt.Sprintf("%s%d", usageSnapshotRebuildPrefix, number)
}

func usageSnapshotBaselineName(number uint64) string {
	return fmt.Sprintf("%s%d", usageSnapshotBaselinePrefix, number)
}

// usageSnapshotRebuild is what a schema step sees of one numbered rebuild.
type usageSnapshotRebuild struct {
	number   uint64
	rebuild  bool
	baseline bool
	// rowGranules reports that the rebuild table has index_granularity = 1,
	// which makes it the old snapshot table the rebuild swapped out.
	rowGranules bool
	// stale reports that every table of the rebuild is older than the
	// schema step's stale age.
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
//     it gained. Record the copy's (id, revision) pairs in the baseline table.
//  4. Swap the two tables with EXCHANGE TABLES, which is atomic, so reads and
//     pushes always find a complete table under the snapshot name. Just
//     before, touchUsageSnapshots marks the old table as new.
//  5. Measured against the baseline, apply what pushes changed in the old
//     table between step 3 and the swap: copy the rows it gained and delete
//     the rows it lost. Then drop the baseline and the old table.
//
// A process that stops after the swap leaves the baseline, and a later
// schema step finishes step 5 once the tables are stale. Only a push still
// writing at the swap, whose insert or delete lands in the old table after
// step 5 reads it, is missed; the next full push repairs it.
//
// A database that cannot swap tables keeps its table: two renames would
// leave a moment with no snapshot table, and pushes and reads would fail.
func rebuildUsageSnapshots(ctx context.Context, conn *sql.DB) error {
	return rebuildUsageSnapshotsAfter(ctx, conn, usageSnapshotRebuildStale)
}

// rebuildUsageSnapshotsAfter is rebuildUsageSnapshots with the age at which
// another process's rebuild counts as stopped.
func rebuildUsageSnapshotsAfter(ctx context.Context, conn *sql.DB, staleAfter time.Duration) (err error) {
	rowGranules, rebuilds, err := readUsageSnapshotRebuilds(ctx, conn, staleAfter)
	if err != nil {
		return err
	}
	if !rowGranules {
		return finishUsageSnapshotRebuilds(ctx, conn, rebuilds)
	}
	var newest uint64
	if len(rebuilds) > 0 {
		last := rebuilds[len(rebuilds)-1]
		if !last.stale {
			log.Printf("ClickHouse: another process is rebuilding %s; skipping the rebuild here", usageSessionSnapshotTable)
			return nil
		}
		newest = last.number
	}
	exchange, err := chExchangeSupportedSinceStart(ctx, conn)
	if err != nil {
		return err
	}
	if !exchange {
		usageSnapshotNoExchangeLog.Do(func() {
			log.Printf("ClickHouse: %s keeps one row per granule, so merging it after large pushes stays slow: "+
				"this database cannot swap tables (EXCHANGE TABLES), usually because its data directory is on NFS. "+
				"Moving the data directory to a local disk lets the next start rebuild it.", usageSessionSnapshotTable)
		})
		return nil
	}
	number := newest + 1
	claimed, err := claimUsageSnapshotRebuild(ctx, conn, number)
	if err != nil || !claimed {
		return err
	}
	swapped := false
	defer func() {
		if swapped || err == nil {
			return
		}
		// The tables are this process's, so a failed rebuild drops them and
		// the next schema step can start over without waiting for them to go
		// stale. Dropping them is quick; it runs even when ctx is done.
		if dropErr := dropUsageSnapshotRebuild(context.WithoutCancel(ctx), conn, number); dropErr != nil {
			err = errors.Join(err, dropErr)
		}
	}()
	// Every rebuild seen was stale, and a newer one would have made the claim
	// fail, so these belong to processes that stopped.
	for _, stopped := range rebuilds {
		if err := dropUsageSnapshotRebuild(ctx, conn, stopped.number); err != nil {
			return err
		}
	}
	start := time.Now()
	var rows uint64
	if err := conn.QueryRowContext(ctx, "SELECT count() FROM "+usageSessionSnapshotTable+
		" SETTINGS final = 0").Scan(&rows); err != nil {
		return fmt.Errorf("counting %s rows: %w", usageSessionSnapshotTable, err)
	}
	log.Printf("ClickHouse: rebuilding %s (%d rows) with larger granules so merges after large pushes run faster",
		usageSessionSnapshotTable, rows)
	rebuild := usageSnapshotRebuildName(number)
	columns, err := storedColumns(ctx, conn, usageSessionSnapshotTable)
	if err != nil {
		return err
	}
	if err := copySnapshots(ctx, conn, usageSessionSnapshotTable, rebuild, columns); err != nil {
		return err
	}
	if err := catchUpUsageSnapshotRebuild(ctx, conn, number, columns); err != nil {
		return err
	}
	if err := touchUsageSnapshots(ctx, conn); err != nil {
		return err
	}
	// A process that found this rebuild stale took a newer number, or
	// another rebuild finished; swapping now would replace its result.
	superseded, err := usageSnapshotRebuildSuperseded(ctx, conn, number)
	if err != nil {
		return err
	}
	if superseded {
		return dropUsageSnapshotRebuild(ctx, conn, number)
	}
	if _, err := conn.ExecContext(ctx,
		"EXCHANGE TABLES "+usageSessionSnapshotTable+" AND "+rebuild); err != nil {
		return fmt.Errorf("swapping in the rebuilt %s: %w", usageSessionSnapshotTable, err)
	}
	swapped = true
	if err := finishUsageSnapshotRebuild(ctx, conn, number); err != nil {
		return err
	}
	log.Printf("ClickHouse: rebuilt %s in %s", usageSessionSnapshotTable, time.Since(start).Round(time.Millisecond))
	return nil
}

// readUsageSnapshotRebuilds reports whether usage_session_snapshots has
// one-row granules and lists the numbered rebuilds in number order.
func readUsageSnapshotRebuilds(ctx context.Context, conn *sql.DB, staleAfter time.Duration) (
	rowGranules bool, rebuilds []usageSnapshotRebuild, err error,
) {
	rows, err := conn.QueryContext(ctx, `SELECT name, `+chTableNumberSQL+` AS number,
			extract(engine_full, 'index_granularity = ([0-9]+)') = '1',
			metadata_modification_time <= now() - toIntervalSecond(?)
		FROM system.tables WHERE database = currentDatabase() AND (name = ? OR match(name, ?))
		ORDER BY number`, int64(staleAfter/time.Second), usageSessionSnapshotTable, usageSnapshotRebuildPattern)
	if err != nil {
		return false, nil, fmt.Errorf("reading %s granularity: %w", usageSessionSnapshotTable, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var number uint64
		var granules, stale uint8
		if err := rows.Scan(&name, &number, &granules, &stale); err != nil {
			return false, nil, fmt.Errorf("scanning %s granularity: %w", usageSessionSnapshotTable, err)
		}
		if name == usageSessionSnapshotTable {
			rowGranules = granules != 0
			continue
		}
		if len(rebuilds) == 0 || rebuilds[len(rebuilds)-1].number != number {
			rebuilds = append(rebuilds, usageSnapshotRebuild{number: number, stale: true})
		}
		r := &rebuilds[len(rebuilds)-1]
		r.stale = r.stale && stale != 0
		if strings.HasPrefix(name, usageSnapshotRebuildPrefix) {
			r.rebuild, r.rowGranules = true, granules != 0
		} else {
			r.baseline = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("reading %s granularity: %w", usageSessionSnapshotTable, err)
	}
	return rowGranules, rebuilds, nil
}

// catchUpUsageSnapshotRebuild runs step 3 for the rebuild of number: it
// applies to the copy what pushes changed in usage_session_snapshots while
// the copy ran, then records the copy's rows in the baseline table.
func catchUpUsageSnapshotRebuild(ctx context.Context, conn *sql.DB, number uint64, columns []string) error {
	rebuild, baseline := usageSnapshotRebuildName(number), usageSnapshotBaselineName(number)
	if err := deleteSnapshots(ctx, conn, rebuild, "(id, revision) NOT IN (SELECT id, revision FROM "+
		usageSessionSnapshotTable+" SETTINGS final = 0)"); err != nil {
		return err
	}
	if err := copySnapshots(ctx, conn, usageSessionSnapshotTable, rebuild, columns, rebuild); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE "+baseline+" ENGINE = MergeTree ORDER BY (id, revision) "+
		"AS SELECT id, revision FROM "+rebuild+" SETTINGS final = 0"); err != nil {
		return fmt.Errorf("recording the %s rebuild baseline: %w", usageSessionSnapshotTable, err)
	}
	return nil
}

// claimUsageSnapshotRebuild creates the rebuild table of number. Of the
// processes that read the same rebuilds, only one creates it. A process
// that read them before a long pause can create a number whose rebuild
// already finished, or that a newer rebuild passed; it drops the table
// again and leaves the rebuild to the other.
func claimUsageSnapshotRebuild(ctx context.Context, conn *sql.DB, number uint64) (bool, error) {
	_, err := conn.ExecContext(ctx, "CREATE TABLE "+usageSnapshotRebuildName(number)+" AS "+
		usageSessionSnapshotTable+" ENGINE = ReplacingMergeTree(revision) ORDER BY id")
	if ex, ok := errors.AsType[*chdriver.Exception](err); ok && ex.Code == codeTableAlreadyExists {
		log.Printf("ClickHouse: another process is rebuilding %s; skipping the rebuild here", usageSessionSnapshotTable)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("creating %s rebuild: %w", usageSessionSnapshotTable, err)
	}
	superseded, err := usageSnapshotRebuildSuperseded(ctx, conn, number)
	if err == nil && !superseded {
		return true, nil
	}
	if dropErr := dropUsageSnapshotRebuild(context.WithoutCancel(ctx), conn, number); dropErr != nil {
		err = errors.Join(err, dropErr)
	}
	return false, err
}

// usageSnapshotRebuildSuperseded reports whether the rebuild of number
// must stop: usage_session_snapshots no longer has one-row granules, or a
// process took a newer number.
func usageSnapshotRebuildSuperseded(ctx context.Context, conn *sql.DB, number uint64) (bool, error) {
	var superseded uint8
	if err := conn.QueryRowContext(ctx, `SELECT countIf(match(name, ?) AND `+chTableNumberSQL+` > ?) > 0
			OR countIf(name = ? AND extract(engine_full, 'index_granularity = ([0-9]+)') != '1') > 0
		FROM system.tables WHERE database = currentDatabase()`,
		usageSnapshotRebuildPattern, number, usageSessionSnapshotTable).Scan(&superseded); err != nil {
		return false, fmt.Errorf("checking the %s rebuild: %w", usageSessionSnapshotTable, err)
	}
	if superseded != 0 {
		log.Printf("ClickHouse: another process rebuilt %s or took over its rebuild; stopping the rebuild here",
			usageSessionSnapshotTable)
	}
	return superseded != 0, nil
}

// finishUsageSnapshotRebuilds cleans up after the rebuilds of processes
// that stopped, once usage_session_snapshots has default granularity. A
// rebuild that holds the old table and its baseline stopped during step 5,
// so it finishes step 5. Two processes can finish the same rebuild at once;
// their copies of a row are identical, and ReplacingMergeTree keeps one.
// Any other stale rebuild is dropped as it is: a copy made before the swap
// is not needed, and an old table without a baseline already went through
// step 5, so copying its rows again could bring back sessions deleted since.
func finishUsageSnapshotRebuilds(ctx context.Context, conn *sql.DB, rebuilds []usageSnapshotRebuild) error {
	for _, r := range rebuilds {
		if !r.stale {
			continue
		}
		if r.rebuild && r.rowGranules && r.baseline {
			log.Printf("ClickHouse: finishing a %s rebuild that stopped after its swap", usageSessionSnapshotTable)
			if err := finishUsageSnapshotRebuild(ctx, conn, r.number); err != nil {
				return err
			}
			continue
		}
		if err := dropUsageSnapshotRebuild(ctx, conn, r.number); err != nil {
			return err
		}
	}
	return nil
}

// finishUsageSnapshotRebuild runs step 5 for the rebuild of number, whose
// rebuild table holds the old snapshot table. Rows the old table holds and
// the baseline lacks were pushed between step 3 and the swap; rows the
// baseline holds and the old table lacks were deleted then. A session
// deleted after the swap is in both, so it stays deleted.
func finishUsageSnapshotRebuild(ctx context.Context, conn *sql.DB, number uint64) error {
	old, baseline := usageSnapshotRebuildName(number), usageSnapshotBaselineName(number)
	columns, err := storedColumns(ctx, conn, old)
	if err != nil {
		return err
	}
	if err := copySnapshots(ctx, conn, old, usageSessionSnapshotTable, columns, baseline, usageSessionSnapshotTable); err != nil {
		return err
	}
	if err := deleteSnapshots(ctx, conn, usageSessionSnapshotTable,
		"(id, revision) IN (SELECT id, revision FROM "+baseline+" SETTINGS final = 0) "+
			"AND (id, revision) NOT IN (SELECT id, revision FROM "+old+" SETTINGS final = 0)"); err != nil {
		return err
	}
	return dropUsageSnapshotRebuild(ctx, conn, number)
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

// copySnapshots copies the rows of from into to, every version and the
// stored values of MATERIALIZED columns, so the copy recomputes nothing. It
// skips the (id, revision) pairs any table in skip holds.
func copySnapshots(ctx context.Context, conn *sql.DB, from, to string, columns []string, skip ...string) error {
	list := strings.Join(columns, ", ")
	conditions := make([]string, len(skip))
	for i, table := range skip {
		conditions[i] = "(id, revision) NOT IN (SELECT id, revision FROM " + table + " SETTINGS final = 0)"
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO "+to+" ("+list+") SELECT "+list+" FROM "+from+where+
		" SETTINGS final = 0, insert_allow_materialized_columns = 1"); err != nil {
		return fmt.Errorf("copying %s rows into %s: %w", from, to, err)
	}
	return nil
}

// deleteSnapshots deletes the rows of table that match condition. Most
// rebuilds find none, so it counts before starting a delete.
func deleteSnapshots(ctx context.Context, conn *sql.DB, table, condition string) error {
	var count uint64
	if err := conn.QueryRowContext(ctx, "SELECT count() FROM "+table+" WHERE "+condition+
		" SETTINGS final = 0").Scan(&count); err != nil {
		return fmt.Errorf("counting %s rows a push removed during the rebuild: %w", table, err)
	}
	if count == 0 {
		return nil
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+condition); err != nil {
		return fmt.Errorf("deleting %s rows a push removed during the rebuild: %w", table, err)
	}
	return nil
}

// touchUsageSnapshots rewrites the snapshot table's metadata, unchanged,
// so the table the swap moves to the rebuild name looks as new as the swap.
// EXCHANGE TABLES keeps each table's metadata time, and without this a
// second process would find the swapped-out table stale at once and run
// step 5 while its owner still runs it.
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

// dropUsageSnapshotRebuild drops the tables of the rebuild of number, the
// baseline first: an old table left without its baseline has finished step
// 5. It does not wait for queries still reading the tables, such as a
// prepared usage refresh; the server removes the data once they finish.
func dropUsageSnapshotRebuild(ctx context.Context, conn *sql.DB, number uint64) error {
	for _, table := range []string{usageSnapshotBaselineName(number), usageSnapshotRebuildName(number)} {
		if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			return fmt.Errorf("dropping %s: %w", table, err)
		}
	}
	return nil
}
