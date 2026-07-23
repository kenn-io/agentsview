package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// VectorGenerationRow is one row of `pg vectors list`: a registered PG vector
// generation with its embedding identity and the live doc/chunk/machine counts
// derived from its per-generation chunk table and the machine registry.
type VectorGenerationRow struct {
	ID          int64
	Fingerprint string
	Model       string
	Dimension   int
	Docs        int64
	Chunks      int64
	Machines    []string
	CreatedAt   time.Time
}

// ListVectorGenerations returns every registered generation with its doc,
// chunk, and machine counts, ordered by id (oldest first). Docs and chunks come
// from the generation's chunk table (docs = distinct doc_keys, chunks = rows);
// a generation whose chunk table does not yet exist reports zero for both. A
// missing vector_generations table (pgvector never installed, SQLSTATE 42P01)
// yields an empty list rather than an error, matching the read-side gate's
// tolerance elsewhere in this package.
func ListVectorGenerations(
	ctx context.Context, pg *sql.DB,
) ([]VectorGenerationRow, error) {
	rows, err := pg.QueryContext(ctx, `
SELECT id, fingerprint, model, dimension, created_at
  FROM vector_generations ORDER BY id`)
	if isUndefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing vector generations: %w", err)
	}
	var gens []VectorGenerationRow
	for rows.Next() {
		var g VectorGenerationRow
		if err := rows.Scan(
			&g.ID, &g.Fingerprint, &g.Model, &g.Dimension, &g.CreatedAt,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scanning vector generation: %w", err)
		}
		gens = append(gens, g)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterating vector generations: %w", err)
	}
	_ = rows.Close()

	for i := range gens {
		if err := fillVectorGenerationCounts(ctx, pg, &gens[i]); err != nil {
			return nil, err
		}
	}
	return gens, nil
}

// fillVectorGenerationCounts populates a row's Docs, Chunks, and Machines from
// the generation's chunk table and the machine registry. The counts must be
// read after the outer generation query is drained (they issue their own
// queries), so ListVectorGenerations collects rows first, then fills each.
func fillVectorGenerationCounts(
	ctx context.Context, pg *sql.DB, g *VectorGenerationRow,
) error {
	docs, chunks, err := vectorChunkCounts(ctx, pg, g.ID)
	if err != nil {
		return err
	}
	g.Docs, g.Chunks = docs, chunks
	machines, err := vectorGenerationMachines(ctx, pg, g.ID)
	if err != nil {
		return err
	}
	g.Machines = machines
	return nil
}

// vectorChunkCounts returns the distinct doc_key count and total chunk-row
// count in the generation's chunk table. A generation row can exist before its
// chunk table is created (or after a partial reset); a missing table is not an
// error and reports zero for both, guarded by to_regclass.
func vectorChunkCounts(
	ctx context.Context, pg *sql.DB, genID int64,
) (docs, chunks int64, err error) {
	table := vectorChunkTable(genID)
	var present bool
	if err := pg.QueryRowContext(ctx,
		`SELECT to_regclass($1) IS NOT NULL`, table).Scan(&present); err != nil {
		return 0, 0, fmt.Errorf("probing chunk table for generation %d: %w", genID, err)
	}
	if !present {
		return 0, 0, nil
	}
	if err := pg.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT count(DISTINCT doc_key), count(*) FROM %s`, table,
	)).Scan(&docs, &chunks); err != nil {
		return 0, 0, fmt.Errorf("counting chunks for generation %d: %w", genID, err)
	}
	return docs, chunks, nil
}

// vectorGenerationMachines lists the machines that have pushed the generation,
// sorted by name, from vector_generation_machines.
func vectorGenerationMachines(
	ctx context.Context, pg *sql.DB, genID int64,
) ([]string, error) {
	rows, err := pg.QueryContext(ctx,
		`SELECT machine FROM vector_generation_machines
		  WHERE generation_id = $1 ORDER BY machine`, genID)
	if err != nil {
		return nil, fmt.Errorf("listing machines for generation %d: %w", genID, err)
	}
	defer func() { _ = rows.Close() }()
	var machines []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scanning generation machine: %w", err)
		}
		display := vectorGenerationMachineDisplayName(m)
		if len(machines) > 0 && machines[len(machines)-1] == display {
			continue
		}
		machines = append(machines, display)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating generation machines: %w", err)
	}
	return machines, nil
}

func vectorGenerationMachineDisplayName(raw string) string {
	if head, _, ok := strings.Cut(raw, "|"+pushMarkerKeyPrefix); ok {
		return head
	}
	return raw
}

// DropVectorGeneration removes a generation and all of its data: its chunk
// table, its vector_push_state and vector_generation_machines rows, and its
// vector_generations row, then prunes vector_documents rows referenced by no
// remaining generation's chunk table (shared docs another generation still
// embeds survive). It runs in one transaction so a failure leaves the
// generation intact. Dropping an id that does not exist, or dropping against a
// database without the vector tables (SQLSTATE 42P01), returns a clear error.
func DropVectorGeneration(ctx context.Context, pg *sql.DB, id int64) error {
	var one int
	err := pg.QueryRowContext(ctx,
		`SELECT 1 FROM vector_generations WHERE id = $1`, id).Scan(&one)
	if isUndefinedTable(err) {
		return fmt.Errorf(
			"no vector generations exist (pgvector not initialized for this target)")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("vector generation %d does not exist", id)
	}
	if err != nil {
		return fmt.Errorf("looking up vector generation %d: %w", id, err)
	}

	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin drop generation tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := dropVectorGenerationRows(ctx, tx, id); err != nil {
		return err
	}
	remaining, err := existingChunkGenerationsTx(ctx, tx)
	if err != nil {
		return err
	}
	if err := pruneUnreferencedVectorDocs(ctx, tx, remaining); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit drop generation tx: %w", err)
	}
	return nil
}

// dropVectorGenerationRows drops the generation's chunk table and deletes its
// state, machine, and generation rows within the transaction. PG DDL is
// transactional, so a later step's failure rolls the DROP back with the rest.
func dropVectorGenerationRows(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DROP TABLE IF EXISTS %s`, vectorChunkTable(id))); err != nil {
		return fmt.Errorf("dropping chunk table for generation %d: %w", id, err)
	}
	stmts := []struct {
		what string
		sql  string
	}{
		{"push state", `DELETE FROM vector_push_state WHERE generation_id = $1`},
		{"machine rows", `DELETE FROM vector_generation_machines WHERE generation_id = $1`},
		{"generation row", `DELETE FROM vector_generations WHERE id = $1`},
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s.sql, id); err != nil {
			return fmt.Errorf("deleting %s for generation %d: %w", s.what, id, err)
		}
	}
	return nil
}

// existingChunkGenerationsTx returns the ids of the still-registered
// generations whose chunk table currently exists, probed with to_regclass. It
// mirrors Sync.existingChunkGenerations but runs inside the drop transaction so
// the orphan-doc prune never references a missing chunk table (which would
// abort the transaction). The outer generation query is drained before the
// per-generation probes, since a transaction serves one query at a time.
func existingChunkGenerationsTx(ctx context.Context, tx *sql.Tx) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM vector_generations`)
	if err != nil {
		return nil, fmt.Errorf("listing remaining generations: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scanning remaining generation id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterating remaining generations: %w", err)
	}
	_ = rows.Close()

	var existing []int64
	for _, id := range ids {
		var present bool
		if err := tx.QueryRowContext(ctx,
			`SELECT to_regclass($1) IS NOT NULL`,
			vectorChunkTable(id)).Scan(&present); err != nil {
			return nil, fmt.Errorf("probing chunk table for generation %d: %w", id, err)
		}
		if present {
			existing = append(existing, id)
		}
	}
	return existing, nil
}

// pruneUnreferencedVectorDocs deletes vector_documents rows that no remaining
// generation's chunk table references. When no generation with a chunk table
// remains, every doc is orphaned and removed. genIDs must be pre-filtered to
// existing chunk tables (existingChunkGenerationsTx) so the NOT EXISTS probes
// never reference a missing table.
func pruneUnreferencedVectorDocs(
	ctx context.Context, tx *sql.Tx, genIDs []int64,
) error {
	var conds strings.Builder
	for _, id := range genIDs {
		fmt.Fprintf(&conds,
			" AND NOT EXISTS (SELECT 1 FROM %s c WHERE c.doc_key = d.doc_key)",
			vectorChunkTable(id))
	}
	stmt := fmt.Sprintf(
		`DELETE FROM vector_documents d WHERE true%s`, conds.String())
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("pruning unreferenced vector docs: %w", err)
	}
	return nil
}
