package clickhouse

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"
)

// compactionShare is the share of the mirror's live messages a push must
// rewrite before it compacts. A full push from either of two archives that
// share a mirror passes it; a routine incremental push stays far below.
const compactionShare = 0.25

// compactionTables are the ReplacingMergeTree tables a session push rewrites:
// its dependents, the tables their materialized views fill, the usage
// snapshots, and the session rows.
var compactionTables = slices.Concat(
	dependentTables, derivedSessionTables, []string{usageSessionSnapshotTable, "sessions"},
)

// compactAfterRewrite merges away the superseded row versions a large push
// left behind. A rewritten session keeps its older rows until a merge puts
// both versions into one part, and the merge selector rarely merges a large
// part with smaller ones, so after full pushes the mirror holds several
// copies of every row and each final=1 read pays to collapse them.
//
// When the push wrote at least compactionShare of the mirror's live
// messages, it runs OPTIMIZE TABLE ... FINAL on each rewritten table, largest
// first, and waits for each merge. Reads keep using the old parts until the
// merged part replaces them. Compaction only speeds up reads, so a failure,
// such as a pushing role without the OPTIMIZE grant, is logged and skips the
// remaining tables without failing the push. Only cancellation is returned.
func (s *Sync) compactAfterRewrite(ctx context.Context, messagesPushed int) error {
	if messagesPushed == 0 {
		return nil
	}
	err := s.compactRewrittenTables(ctx, messagesPushed)
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	log.Printf("clickhouse push: skipped compacting the rewritten tables; reads stay correct but slower until merges collapse old row versions: %v", err)
	return nil
}

func (s *Sync) compactRewrittenTables(ctx context.Context, messagesPushed int) error {
	var live int64
	if err := s.conn.QueryRowContext(ctx,
		"SELECT sum(coalesce(stored_message_count, message_count)) FROM sessions",
	).Scan(&live); err != nil {
		return fmt.Errorf("counting live messages: %w", err)
	}
	if !compactionDue(messagesPushed, live) {
		return nil
	}
	tables, err := s.tablesBySize(ctx, compactionTables)
	if err != nil {
		return err
	}
	conn, err := openCompaction(ctx, s.target)
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Printf("clickhouse push: compacting %d tables after rewriting %d of %d live messages",
		len(tables), messagesPushed, live)
	for _, table := range tables {
		start := time.Now()
		// Without optimize_throw_if_noop the server returns success when it
		// declines the merge, such as when the disk lacks twice the table's
		// size free, and only its own log says so.
		if _, err := conn.ExecContext(ctx,
			"OPTIMIZE TABLE "+table+" FINAL SETTINGS optimize_throw_if_noop = 1"); err != nil {
			return fmt.Errorf("compacting %s: %w", table, err)
		}
		log.Printf("clickhouse push: compacted %s in %s", table, time.Since(start).Round(time.Second))
	}
	return nil
}

// compactionDue reports whether a push that wrote pushed messages rewrote
// enough of the live messages to compact.
func compactionDue(pushed int, live int64) bool {
	return live > 0 && float64(pushed) >= compactionShare*float64(live)
}

// tablesBySize returns the tables among names that hold data, largest on
// disk first, so the merge that saves reads the most runs first.
func (s *Sync) tablesBySize(ctx context.Context, names []string) ([]string, error) {
	placeholders, args := inArgs(names)
	rows, err := s.conn.QueryContext(ctx,
		"SELECT table FROM system.parts WHERE database = currentDatabase() AND active AND table IN ("+
			placeholders+") GROUP BY table ORDER BY sum(bytes_on_disk) DESC", args...)
	if err != nil {
		return nil, fmt.Errorf("listing table sizes: %w", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("scanning table size: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing table sizes: %w", err)
	}
	return tables, nil
}
