package postgres

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"log"

	"go.kenn.io/agentsview/internal/storage"
)

// vectorBaseDDL creates the backend-agnostic vector tables. Chunk tables
// are per-generation (fixed halfvec dimension). vector_documents mirrors
// the local vectors.db
// mirror table (internal/vector/index.go messageMirrorDDL); vector_push_state
// is the pusher-side delta state, written transactionally with doc/chunk
// upserts so a PG reset wipes state and data together (self-healing).
const vectorBaseDDL = `
CREATE TABLE IF NOT EXISTS vector_generations (
    id          BIGSERIAL PRIMARY KEY,
    fingerprint TEXT NOT NULL UNIQUE,
    gen_key     TEXT NOT NULL UNIQUE,
    state       TEXT NOT NULL,
    model       TEXT NOT NULL,
    dimension   INTEGER NOT NULL,
    params      JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS vector_generation_machines (
    generation_id BIGINT NOT NULL,
    machine       TEXT NOT NULL,
    last_push_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (generation_id, machine)
);
CREATE TABLE IF NOT EXISTS vector_documents (
    doc_key      TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL,
    source_uuid  TEXT NOT NULL DEFAULT '',
    ordinal      INTEGER NOT NULL,
    ordinal_end  INTEGER NOT NULL,
    subordinate  BOOLEAN NOT NULL DEFAULT FALSE,
    offsets      TEXT NOT NULL DEFAULT '[]',
    content      TEXT NOT NULL,
    content_hash TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_vector_documents_session_ordinal
    ON vector_documents (session_id, ordinal);
` + vectorStampsDDL + `
CREATE TABLE IF NOT EXISTS vector_push_state (
    generation_id BIGINT NOT NULL,
    session_id    TEXT NOT NULL,
    doc_agg_hash  TEXT NOT NULL,
    PRIMARY KEY (generation_id, session_id)
);
`

const vectorStampsDDL = `
CREATE TABLE IF NOT EXISTS vector_stamps (
    generation_id BIGINT NOT NULL,
    doc_key TEXT NOT NULL REFERENCES vector_documents(doc_key) ON DELETE CASCADE,
    revision TEXT,
    PRIMARY KEY (generation_id, doc_key)
);
CREATE INDEX IF NOT EXISTS vector_stamps_by_doc ON vector_stamps (doc_key);
`

// VectorExtensionAvailable reports whether the pgvector extension is
// installed in this database (probe only; no DDL).
func VectorExtensionAvailable(ctx context.Context, pg *sql.DB) (bool, error) {
	var one int
	err := pg.QueryRowContext(ctx,
		`SELECT 1 FROM pg_extension WHERE extname = 'vector'`).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probing pgvector extension: %w", err)
	}
	return true, nil
}

// vectorHalfvecAvailable reports whether pgvector provides halfvec and supports
// iterative scans (0.8.0 or later), plus the installed version for diagnostics.
// The type probe is scoped to the extension's own namespace.
func vectorHalfvecAvailable(ctx context.Context, pg *sql.DB) (bool, string, error) {
	var hasHalfvec bool
	var version string
	err := pg.QueryRowContext(ctx, `
SELECT EXISTS (
           SELECT 1 FROM pg_type t
           WHERE t.typname = 'halfvec' AND t.typnamespace = e.extnamespace
       ), e.extversion
  FROM pg_extension e WHERE e.extname = 'vector'`).Scan(&hasHalfvec, &version)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("probing pgvector halfvec support: %w", err)
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return false, version, nil
	}
	return hasHalfvec && (major > 0 || minor >= 8), version, nil
}

// ensureVectorBaseSchemaPG best-effort-installs pgvector and creates the
// base vector tables. A non-empty reason (with nil error) means vectors are
// unavailable on this server: like createContentSearchIndexesPG it never
// fails schema setup — CockroachDB, a missing package, a role without CREATE
// privilege, or a pgvector too old for iterative scans leave semantic search
// unavailable with a one-line notice.
func ensureVectorBaseSchemaPG(ctx context.Context, pg *sql.DB) (string, error) {
	if _, err := pg.ExecContext(ctx,
		`CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		log.Printf("pg schema: pgvector unavailable, semantic search disabled: %v", err)
	}
	available, err := VectorExtensionAvailable(ctx, pg)
	if err != nil {
		return "", err
	}
	if !available {
		return "pgvector extension unavailable", nil
	}
	hasHalfvec, version, err := vectorHalfvecAvailable(ctx, pg)
	if err != nil {
		return "", err
	}
	if !hasHalfvec {
		// CREATE EXTENSION IF NOT EXISTS never upgrades an existing
		// extension object, so a server whose pgvector package was upgraded
		// in place can still run an old extension version. ALTER EXTENSION
		// heals that case; a server with no newer version fails it and
		// vectors degrade below.
		if _, err := pg.ExecContext(ctx, `ALTER EXTENSION vector UPDATE`); err != nil {
			log.Printf("pg schema: pgvector %s upgrade attempt failed: %v", version, err)
		} else if hasHalfvec, version, err = vectorHalfvecAvailable(ctx, pg); err != nil {
			return "", err
		}
	}
	if !hasHalfvec {
		reason := fmt.Sprintf(
			"pgvector 0.8.0 or later required (found %s)",
			version)
		log.Printf("pg schema: %s; semantic search disabled", reason)
		return reason, nil
	}
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':vector-catalog', 0))`); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, vectorBaseDDL); err != nil {
		return "", fmt.Errorf("creating vector base schema: %w", err)
	}
	if err := migrateVectorCatalogPG(ctx, tx); err != nil {
		return "", err
	}
	return "", tx.Commit()
}

// migrateVectorCatalogPG backfills stamps once for indexes created by older writers.
func migrateVectorCatalogPG(ctx context.Context, tx *sql.Tx) error {
	var migrated bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='vector_generations' AND column_name='gen_key')`).Scan(&migrated); err != nil {
		return err
	}
	if migrated {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE vector_generations ADD COLUMN gen_key TEXT, ADD COLUMN state TEXT;
UPDATE vector_generations SET gen_key=fingerprint, state='active';
ALTER TABLE vector_generations ALTER COLUMN gen_key SET NOT NULL, ALTER COLUMN state SET NOT NULL, ADD UNIQUE (gen_key)`); err != nil {
		return fmt.Errorf("upgrading vector catalog: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM vector_generations ORDER BY id`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		table := vectorChunkTable(id)
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO vector_stamps(generation_id,doc_key,revision) SELECT $1,d.doc_key,d.content_hash FROM vector_documents d WHERE EXISTS (SELECT 1 FROM `+table+` c WHERE c.doc_key=d.doc_key) ON CONFLICT (generation_id,doc_key) DO NOTHING`, id); err != nil {
			return fmt.Errorf("backfilling vector stamps: %w", err)
		}
	}
	return nil
}

// vectorChunkTable names generation genID's chunk table. Per-generation
// tables are required: pgvector columns have a fixed typed dimension and
// HNSW requires it (mirrors sqlite-vec's per-generation vec0 tables).
func vectorChunkTable(genID int64) string {
	return fmt.Sprintf("vector_chunks_g%d", genID)
}

// vectorExtensionSchema locates the namespace pgvector's types and operator
// classes were installed into. CREATE EXTENSION installs into whichever
// schema was first on search_path the first time it ran anywhere in the
// database, which is not necessarily the connection's current search_path
// (Open sets search_path to the target schema only, mirroring
// createContentSearchIndexesPG's pg_trgm lookup). halfvec and
// halfvec_cosine_ops must be schema-qualified with it, or every schema
// besides the one that happened to install the extension fails to resolve
// them.
func vectorExtensionSchema(ctx context.Context, pg pgProjectIdentityExecer) (string, error) {
	var extSchema string
	if err := pg.QueryRowContext(ctx,
		`SELECT n.nspname FROM pg_extension e
		 JOIN pg_namespace n ON n.oid = e.extnamespace
		 WHERE e.extname = 'vector'`,
	).Scan(&extSchema); err != nil {
		return "", fmt.Errorf("locating pgvector schema: %w", err)
	}
	quoted, err := quoteIdentifier(extSchema)
	if err != nil {
		return "", fmt.Errorf("invalid pgvector schema %q: %w", extSchema, err)
	}
	return quoted, nil
}

// ensureVectorChunkTable creates genID's halfvec chunk table and its HNSW
// cosine index. halfvec stays under HNSW's dimension limits (covers
// 2560-dim models plain vector cannot index) and halves storage.
func ensureVectorChunkTable(
	ctx context.Context, pg pgProjectIdentityExecer, genID int64, dimension int,
) error {
	extSchema, err := vectorExtensionSchema(ctx, pg)
	if err != nil {
		return err
	}
	table := vectorChunkTable(genID)
	if _, err := pg.ExecContext(ctx, fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    doc_key     TEXT NOT NULL,
    chunk_index INTEGER NOT NULL,
    embedding   %s.halfvec(%d) NOT NULL,
    PRIMARY KEY (doc_key, chunk_index)
)`, table, extSchema, dimension)); err != nil {
		return fmt.Errorf("creating %s: %w", table, err)
	}
	if _, err := pg.ExecContext(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_%s_hnsw ON %s
		 USING hnsw (embedding %s.halfvec_cosine_ops)`, table, table, extSchema)); err != nil {
		return fmt.Errorf("creating %s hnsw index: %w", table, err)
	}
	return nil
}

// ensureVectorRecipeColumn adds vector_generations.params for writers whose
// tables predate recipe publication. It runs only on the push path, never in
// serve-side schema preparation.
func ensureVectorRecipeColumn(ctx context.Context, pg *sql.DB) error {
	existing, err := loadExistingColumns(ctx, pg, nil, "vector_generations")
	if err != nil {
		return fmt.Errorf("checking vector recipe column: %w", err)
	}
	if _, err := ensureColumns(ctx, pg, existing, []columnMigration{{
		table: "vector_generations", column: "params", def: "params JSONB",
	}}); err != nil {
		return fmt.Errorf("adding vector recipe column: %w", err)
	}
	return nil
}

// publishVectorRecipe records params for fingerprint's generation when the
// row has none. Stored params are never overwritten: a fingerprint names
// one immutable recipe.
func publishVectorRecipe(
	ctx context.Context, pg pgProjectIdentityExecer, fingerprint string, params map[string]string,
) error {
	encoded, err := json.Marshal(params, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encoding vector recipe: %w", err)
	}
	if _, err := pg.ExecContext(ctx, `
UPDATE vector_generations SET params = $2::jsonb
WHERE fingerprint = $1 AND params IS NULL`,
		fingerprint, string(encoded)); err != nil {
		return fmt.Errorf("publishing vector recipe: %w", err)
	}
	return nil
}

// ensureVectorGeneration registers fingerprint's generation, returning its
// id. Machines with matching embedding configs share one generation.
func ensureVectorGeneration(
	ctx context.Context, pg pgProjectIdentityExecer, fingerprint, model string, dimension int,
) (int64, error) {
	if _, err := pg.ExecContext(ctx, `
INSERT INTO vector_generations (fingerprint, gen_key, state, model, dimension)
VALUES ($1, $1, 'active', $2, $3) ON CONFLICT (fingerprint) DO NOTHING`,
		fingerprint, model, dimension); err != nil {
		return 0, fmt.Errorf("registering vector generation: %w", err)
	}
	id, _, ok, err := LookupVectorGeneration(ctx, pg, fingerprint)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("vector generation %q vanished after insert", fingerprint)
	}
	return id, nil
}

// LookupVectorGeneration resolves a config fingerprint to its PG
// generation, the read-side gate pg serve checks at startup. A missing
// vector_generations table (pgvector never installed, SQLSTATE 42P01) is
// treated as a clean not-found, not an error, so pg serve degrades to a
// plain "semantic search unavailable" notice instead of failing startup.
func LookupVectorGeneration(
	ctx context.Context, pg pgProjectIdentityExecer, fingerprint string,
) (int64, int, bool, error) {
	var id int64
	var dim int
	err := pg.QueryRowContext(ctx,
		`SELECT id, dimension FROM vector_generations WHERE fingerprint = $1`,
		fingerprint).Scan(&id, &dim)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if isUndefinedTable(err) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("looking up vector generation: %w", err)
	}
	return id, dim, true, nil
}

// VectorChunkTableExists reports whether genID's chunk table exists — the
// second half of the read-side startup gate. A generation row can exist
// without its chunk table: registering the generation and creating the table
// are separate statements on the push side, so a push interrupted between
// them (or a manual table drop) leaves the row behind. Wiring a searcher
// against such a generation would fail every query with a missing-relation
// error instead of degrading to db.ErrSemanticUnavailable. The unqualified
// name resolves through the connection's search_path, like every other
// read-side query.
func VectorChunkTableExists(
	ctx context.Context, pg *sql.DB, genID int64,
) (bool, error) {
	var present bool
	if err := pg.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`,
		vectorChunkTable(genID)).Scan(&present); err != nil {
		return false, fmt.Errorf(
			"probing chunk table for generation %d: %w", genID, err)
	}
	return present, nil
}

// ListVectorGenerationInfo returns every registered generation's identity
// and published recipe ordered by id (oldest first), for recipe adoption and
// the serve startup notice when no generation matches the local config. A
// missing vector_generations table (SQLSTATE 42P01) yields an empty slice,
// not an error, matching LookupVectorGeneration's tolerance for a database
// without pgvector. Reading params through to_jsonb tolerates tables that
// predate the params column, so read-only servers need no migration.
func ListVectorGenerationInfo(
	ctx context.Context, pg *sql.DB,
) ([]storage.VectorGenerationInfo, error) {
	rows, err := pg.QueryContext(ctx,
		`SELECT fingerprint, model, dimension, to_jsonb(g)->>'params'
		 FROM vector_generations g ORDER BY id`)
	if isUndefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing vector generations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var gens []storage.VectorGenerationInfo
	for rows.Next() {
		var g storage.VectorGenerationInfo
		var params sql.NullString
		if err := rows.Scan(&g.Fingerprint, &g.Model, &g.Dimension, &params); err != nil {
			return nil, fmt.Errorf("scanning vector generation: %w", err)
		}
		// A recipe this build cannot decode is unsupported, not a read
		// failure: the generation is listed without params.
		if params.Valid &&
			json.Unmarshal([]byte(params.String), &g.Params) != nil {
			g.Params = nil
		}
		gens = append(gens, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating vector generations: %w", err)
	}
	return gens, nil
}
