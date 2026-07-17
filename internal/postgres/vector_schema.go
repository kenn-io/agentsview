package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// vectorBaseDDL creates the backend-agnostic vector tables. Chunk tables
// are per-generation (fixed halfvec dimension) and created lazily by
// ensureVectorChunkTable. vector_documents mirrors the local vectors.db
// mirror table (internal/vector/index.go messageMirrorDDL); vector_push_state
// is the pusher-side delta state, written transactionally with doc/chunk
// upserts so a PG reset wipes state and data together (self-healing).
const vectorBaseDDL = `
CREATE TABLE IF NOT EXISTS vector_generations (
    id          BIGSERIAL PRIMARY KEY,
    fingerprint TEXT NOT NULL UNIQUE,
    model       TEXT NOT NULL,
    dimension   INTEGER NOT NULL,
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
-- The Codex stale-vector scan (staleCodexVectorSessionIDsPG) runs on every
-- schema ensure and read-side compat check, because a compatibility-marked
-- writer can push a stale ciphertext vector even after the one-time sweep.
-- Its leading-wildcard LIKE cannot use a btree lookup, so give it a partial
-- index that keeps the steady-state probe proportional to the (normally
-- empty) suspicious set instead of the whole vector corpus.
CREATE INDEX IF NOT EXISTS idx_vector_documents_codex_ciphertext
    ON vector_documents (session_id) WHERE content LIKE '%gAAAAA%';
CREATE TABLE IF NOT EXISTS vector_push_state (
    generation_id BIGINT NOT NULL,
    session_id    TEXT NOT NULL,
    doc_agg_hash  TEXT NOT NULL,
    PRIMARY KEY (generation_id, session_id)
);
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

// vectorHalfvecAvailable reports whether the installed pgvector provides the
// halfvec type (added in pgvector 0.7.0), plus the installed extension
// version for diagnostics. The type probe is scoped to the extension's own
// namespace so an unrelated type named halfvec elsewhere cannot satisfy it.
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
	return hasHalfvec, version, nil
}

// ensureVectorBaseSchemaPG best-effort-installs pgvector and creates the
// base vector tables. A non-empty reason (with nil error) means vectors are
// unavailable on this server: like createContentSearchIndexesPG it never
// fails schema setup — CockroachDB, a missing package, a role without CREATE
// privilege, or a pgvector too old for halfvec leave semantic search
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
			"pgvector %s lacks the halfvec type; upgrade the server to pgvector 0.7.0+",
			version)
		log.Printf("pg schema: %s; semantic search disabled", reason)
		return reason, nil
	}
	if _, err := pg.ExecContext(ctx, vectorBaseDDL); err != nil {
		return "", fmt.Errorf("creating vector base schema: %w", err)
	}
	return "", nil
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
func vectorExtensionSchema(ctx context.Context, pg *sql.DB) (string, error) {
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
	ctx context.Context, pg *sql.DB, genID int64, dimension int,
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

// ensureVectorGeneration registers fingerprint's generation, returning its
// id. Machines with matching embedding configs share one generation.
func ensureVectorGeneration(
	ctx context.Context, pg *sql.DB, fingerprint, model string, dimension int,
) (int64, error) {
	if _, err := pg.ExecContext(ctx, `
INSERT INTO vector_generations (fingerprint, model, dimension)
VALUES ($1, $2, $3) ON CONFLICT (fingerprint) DO NOTHING`,
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
	ctx context.Context, pg *sql.DB, fingerprint string,
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

// ListVectorGenerationFingerprints returns every registered generation's
// fingerprint ordered by id (oldest first), for pg serve's startup notice
// when no generation matches the local config. A missing vector_generations
// table (SQLSTATE 42P01) yields an empty slice, not an error, matching
// LookupVectorGeneration's tolerance for a database without pgvector.
func ListVectorGenerationFingerprints(
	ctx context.Context, pg *sql.DB,
) ([]string, error) {
	rows, err := pg.QueryContext(ctx,
		`SELECT fingerprint FROM vector_generations ORDER BY id`)
	if isUndefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing vector generations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var fingerprints []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, fmt.Errorf("scanning vector generation fingerprint: %w", err)
		}
		fingerprints = append(fingerprints, fp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating vector generations: %w", err)
	}
	return fingerprints, nil
}
