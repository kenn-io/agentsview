//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const vectorSchemaTestSchema = "agentsview_vector_schema_test"

func cleanVectorSchemaTestPG(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()
	_, _ = pg.Exec(
		"DROP SCHEMA IF EXISTS " + vectorSchemaTestSchema + " CASCADE",
	)
}

// TestEnsureVectorBaseSchema verifies that ensureVectorBaseSchemaPG installs
// pgvector and the base vector tables idempotently, and that generation
// registration and chunk table creation round-trip through
// ensureVectorGeneration, ensureVectorChunkTable, and
// LookupVectorGeneration.
func TestEnsureVectorBaseSchema(t *testing.T) {
	pgURL := testPGURL(t)
	cleanVectorSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanVectorSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, vectorSchemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()

	// EnsureSchema creates the schema itself (Open only sets search_path;
	// it does not create the target schema), and also exercises the
	// ensureVectorBaseSchemaPG wiring inside EnsureSchema.
	require.NoError(t, EnsureSchema(ctx, pg, vectorSchemaTestSchema))

	unavailable, err := ensureVectorBaseSchemaPG(ctx, pg)
	require.NoError(t, err)
	if unavailable != "" {
		t.Skip(unavailable)
	}

	// Idempotent.
	_, err = ensureVectorBaseSchemaPG(ctx, pg)
	require.NoError(t, err)

	id, err := ensureVectorGeneration(ctx, pg, "fp-1", "test-model", 4)
	require.NoError(t, err)
	id2, err := ensureVectorGeneration(ctx, pg, "fp-1", "test-model", 4)
	require.NoError(t, err)
	assert.Equal(t, id, id2)

	require.NoError(t, ensureVectorChunkTable(ctx, pg, id, 4))
	require.NoError(t, ensureVectorChunkTable(ctx, pg, id, 4)) // idempotent

	gotID, dim, ok, err := LookupVectorGeneration(ctx, pg, "fp-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, id, gotID)
	assert.Equal(t, 4, dim)

	_, _, ok, err = LookupVectorGeneration(ctx, pg, "fp-missing")
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestVectorChunkTableExists pins the second half of the read-side startup
// gate: a generation row registered without its chunk table (a push
// interrupted between ensureVectorGeneration and ensureVectorChunkTable)
// reports the table absent, so serve degrades to unavailable instead of
// wiring a searcher that fails every query with a missing-relation error.
func TestVectorChunkTableExists(t *testing.T) {
	pgURL := testPGURL(t)
	cleanVectorSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanVectorSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, vectorSchemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, vectorSchemaTestSchema))
	unavailable, err := ensureVectorBaseSchemaPG(ctx, pg)
	require.NoError(t, err)
	if unavailable != "" {
		t.Skip(unavailable)
	}

	// Generation registered, chunk table not yet created.
	id, err := ensureVectorGeneration(ctx, pg, "fp-partial", "test-model", 4)
	require.NoError(t, err)
	exists, err := VectorChunkTableExists(ctx, pg, id)
	require.NoError(t, err)
	assert.False(t, exists, "generation row without chunk table")

	require.NoError(t, ensureVectorChunkTable(ctx, pg, id, 4))
	exists, err = VectorChunkTableExists(ctx, pg, id)
	require.NoError(t, err)
	assert.True(t, exists, "chunk table created")
}

// TestVectorGenerationLookupTolerateMissingTable verifies that the read-side
// gate degrades cleanly when the vector_generations table never existed
// (pgvector never installed): LookupVectorGeneration reports not-found and
// ListVectorGenerationFingerprints returns empty, both without error, so pg
// serve records a "semantic unavailable" reason instead of aborting startup.
func TestVectorGenerationLookupTolerateMissingTable(t *testing.T) {
	pgURL := testPGURL(t)
	cleanVectorSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanVectorSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, vectorSchemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()
	// Create the schema but deliberately do not install the vector tables,
	// so vector_generations does not exist (SQLSTATE 42P01 on query).
	_, err = pg.ExecContext(ctx,
		"CREATE SCHEMA IF NOT EXISTS "+vectorSchemaTestSchema)
	require.NoError(t, err)
	require.False(t, tableExistsPG(t, pg, "vector_generations"),
		"vector_generations must be absent for the 42P01 tolerance check")

	_, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-any")
	require.NoError(t, err, "missing table must be tolerated as not-found")
	assert.False(t, ok)

	fingerprints, err := ListVectorGenerationFingerprints(ctx, pg)
	require.NoError(t, err, "missing table must yield an empty list, not an error")
	assert.Empty(t, fingerprints)
}

// TestListVectorGenerationFingerprints verifies fingerprints come back in id
// order (oldest generation first), the order pg serve's startup notice lists
// the generations PG already has.
func TestListVectorGenerationFingerprints(t *testing.T) {
	pgURL := testPGURL(t)
	cleanVectorSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanVectorSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, vectorSchemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, vectorSchemaTestSchema))
	unavailable, err := ensureVectorBaseSchemaPG(ctx, pg)
	require.NoError(t, err)
	if unavailable != "" {
		t.Skip(unavailable)
	}

	_, err = ensureVectorGeneration(ctx, pg, "fp-alpha", "model-a", 4)
	require.NoError(t, err)
	_, err = ensureVectorGeneration(ctx, pg, "fp-beta", "model-b", 8)
	require.NoError(t, err)

	fingerprints, err := ListVectorGenerationFingerprints(ctx, pg)
	require.NoError(t, err)
	assert.Equal(t, []string{"fp-alpha", "fp-beta"}, fingerprints)
}

// tableExistsPG reports whether a base table exists in the connection's
// current schema.
func tableExistsPG(t *testing.T, pg *sql.DB, table string) bool {
	t.Helper()
	var one int
	err := pg.QueryRow(
		`SELECT 1 FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_name = $1`, table,
	).Scan(&one)
	if err == sql.ErrNoRows {
		return false
	}
	require.NoError(t, err, "checking table "+table)
	return true
}

// TestEnsureSchemaFastPathCreatesVectorTables simulates upgrading an existing
// deployment: a full schema is already current (so EnsureSchema takes the
// pushSchemaCurrent fast path), but predates the vector tables. A plain
// post-upgrade pg push must still create them via the fast path's best-effort
// vector setup, otherwise semantic search never becomes available.
func TestEnsureSchemaFastPathCreatesVectorTables(t *testing.T) {
	pgURL := testPGURL(t)
	cleanVectorSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanVectorSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, vectorSchemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, vectorSchemaTestSchema))

	if available, err := VectorExtensionAvailable(ctx, pg); err != nil {
		require.NoError(t, err)
	} else if !available {
		t.Skip("pgvector extension unavailable")
	}
	require.True(t, pushSchemaCurrent(ctx, pg),
		"a freshly ensured schema must read as current so the fast path is exercised")

	// Simulate a pre-vector deployment: drop the four vector tables while the
	// rest of the schema stays current.
	for _, table := range []string{
		"vector_push_state", "vector_documents",
		"vector_generation_machines", "vector_generations",
	} {
		_, err := pg.ExecContext(ctx, `DROP TABLE IF EXISTS `+table+` CASCADE`)
		require.NoError(t, err, "dropping "+table)
	}
	require.False(t, tableExistsPG(t, pg, "vector_generations"),
		"vector_generations must be gone before the fast-path re-ensure")

	sync := &Sync{
		pg:      pg,
		machine: "test-machine",
		schema:  vectorSchemaTestSchema,
	}
	require.NoError(t, sync.EnsureSchema(ctx))

	assert.True(t, tableExistsPG(t, pg, "vector_generations"),
		"the fast path must recreate the vector tables on an up-to-date schema")
	assert.True(t, tableExistsPG(t, pg, "vector_documents"))
	assert.True(t, tableExistsPG(t, pg, "vector_push_state"))
}

// TestVectorHalfvecGate pins the availability gate's two observable shapes:
// on a server with pgvector >= 0.7 the gate reports available (empty reason)
// and the halfvec probe returns the type plus a version; on a server without
// pgvector the gate degrades to a reason instead of an error, which is what
// keeps an old or missing extension from hard-failing `pg push`.
func TestVectorHalfvecGate(t *testing.T) {
	pgURL := testPGURL(t)
	cleanVectorSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanVectorSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, vectorSchemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, vectorSchemaTestSchema))

	unavailable, err := ensureVectorBaseSchemaPG(ctx, pg)
	require.NoError(t, err, "gate must degrade, never error")
	hasHalfvec, version, err := vectorHalfvecAvailable(ctx, pg)
	require.NoError(t, err)

	if unavailable == "" {
		assert.True(t, hasHalfvec, "available gate implies halfvec exists")
		assert.NotEmpty(t, version, "installed extension reports a version")
		return
	}
	assert.False(t, hasHalfvec,
		"unavailable reason %q must mean halfvec is missing", unavailable)
}
