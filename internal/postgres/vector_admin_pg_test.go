//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pushAdminGeneration pushes one generation's docs through the fake source and
// returns its resolved PG generation id, reusing the vector push test harness.
func pushAdminGeneration(
	t *testing.T, sync *Sync, fake *fakeVectorSource, fingerprint string,
) int64 {
	t.Helper()
	ctx := context.Background()
	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "push "+fingerprint)
	id, _, ok, err := LookupVectorGeneration(ctx, sync.pg, fingerprint)
	require.NoError(t, err)
	require.True(t, ok, "generation "+fingerprint+" registered")
	return id
}

// TestListAndDropVectorGenerations seeds two generations that share one doc
// (X#0) while genA alone embeds Y#0. It verifies ListVectorGenerations returns
// both with correct doc/chunk/machine counts in id order, that
// DropVectorGeneration(genA) removes genA's chunk table, state, machine, and
// generation rows, keeps the shared doc (genB still references it), prunes the
// genA-only doc, and that dropping a nonexistent id errors clearly.
func TestListAndDropVectorGenerations(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_admin_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "X")
	seedVectorSession(t, localDB, "Y")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-a", Model: "model-a", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"X": "hx", "Y": "hy"},
		docs: map[string][]VectorPushDoc{
			"X": {vdoc("X", "X#0", 0, "cx", "hx1", []float32{1, 0, 0, 0})},
			"Y": {vdoc("Y", "Y#0", 0, "cy", "hy1", []float32{0, 1, 0, 0})},
		},
	}
	sync.vectorSource = fake
	genA := pushAdminGeneration(t, sync, fake, "fp-a")

	// Gen B (new fingerprint/model) embeds only the shared X#0.
	fake.gen.Fingerprint = "fp-b"
	fake.gen.Model = "model-b"
	fake.hashes = map[string]string{"X": "hx"}
	fake.docs = map[string][]VectorPushDoc{
		"X": {vdoc("X", "X#0", 0, "cx", "hx1", []float32{1, 0, 0, 0})},
	}
	genB := pushAdminGeneration(t, sync, fake, "fp-b")

	gens, err := ListVectorGenerations(ctx, pg)
	require.NoError(t, err)
	require.Len(t, gens, 2)
	assert.Equal(t, genA, gens[0].ID, "generations listed in id order")
	assert.Equal(t, genB, gens[1].ID)

	a, b := gens[0], gens[1]
	assert.Equal(t, "model-a", a.Model)
	assert.Equal(t, 4, a.Dimension)
	assert.Equal(t, int64(2), a.Docs, "genA embeds X#0 and Y#0")
	assert.Equal(t, int64(2), a.Chunks)
	assert.Equal(t, []string{"test-machine"}, a.Machines)
	assert.False(t, a.CreatedAt.IsZero(), "created_at populated")
	assert.Equal(t, "fp-a", a.Fingerprint)

	assert.Equal(t, "model-b", b.Model)
	assert.Equal(t, int64(1), b.Docs, "genB embeds only the shared X#0")
	assert.Equal(t, int64(1), b.Chunks)
	assert.Equal(t, []string{"test-machine"}, b.Machines)

	require.NoError(t, DropVectorGeneration(ctx, pg, genA))

	var reg sql.NullString
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT to_regclass($1)`, vectorChunkTable(genA)).Scan(&reg))
	assert.False(t, reg.Valid, "genA chunk table dropped")

	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_generations WHERE id = $1`, genA))
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_push_state WHERE generation_id = $1`, genA))
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_generation_machines WHERE generation_id = $1`, genA))

	assert.Equal(t, 1, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE doc_key = $1`, "X#0"),
		"shared doc survives, genB still references it")
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE doc_key = $1`, "Y#0"),
		"genA-only doc pruned")

	assert.Equal(t, 1, countRows(t, pg,
		`SELECT COUNT(*) FROM `+vectorChunkTable(genB)), "genB chunk table intact")

	gens, err = ListVectorGenerations(ctx, pg)
	require.NoError(t, err)
	require.Len(t, gens, 1)
	assert.Equal(t, genB, gens[0].ID)

	err = DropVectorGeneration(ctx, pg, 99999)
	require.Error(t, err, "dropping a nonexistent generation errors")
	assert.Contains(t, err.Error(), "99999")

	// A subsequent push against genB must not abort after genA was dropped.
	fake.hashes["X"] = "hx2"
	fake.docs["X"] = []VectorPushDoc{
		vdoc("X", "X#0", 0, "cx-updated", "hx2", []float32{0, 0, 1, 0}),
	}
	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "push after drop must not abort")
	assert.Equal(t, 1, res.Vectors.SessionsPushed)
}

// TestListVectorGenerationsMissingTable verifies the read tolerates a database
// where the vector_generations table never existed (pgvector never installed,
// SQLSTATE 42P01): the list is empty and no error surfaces.
func TestListVectorGenerationsMissingTable(t *testing.T) {
	pgURL := testPGURL(t)
	schema := "agentsview_vector_admin_missing_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = pg.Close() })

	ctx := context.Background()
	_, err = pg.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	require.NoError(t, err, "drop schema")
	_, err = pg.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err, "create schema")
	t.Cleanup(func() {
		_, _ = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	})

	gens, err := ListVectorGenerations(ctx, pg)
	require.NoError(t, err, "missing table yields empty list, not error")
	assert.Empty(t, gens)
}

// TestVectorPushToleratesMissingChunkTable pins existingChunkGenerations'
// to_regclass-NULL branch: a generation row whose chunk table was removed (a
// partial reset, or a concurrent drop) must not abort a later push against a
// different generation. deleteOrphanVectorDocs must reference only chunk tables
// that still exist.
func TestVectorPushToleratesMissingChunkTable(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_admin_dangling_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "S")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-x", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"S": "h1"},
		docs: map[string][]VectorPushDoc{
			"S": {
				vdoc("S", "S#0", 0, "c0", "hc0", []float32{1, 0, 0, 0}),
				vdoc("S", "S#1", 1, "c1", "hc1", []float32{0, 1, 0, 0}),
			},
		},
	}
	sync.vectorSource = fake
	genX := pushAdminGeneration(t, sync, fake, "fp-x")

	// Second generation embeds the same docs so vector_documents rows are shared.
	fake.gen.Fingerprint = "fp-y"
	_ = pushAdminGeneration(t, sync, fake, "fp-y")

	// Remove genX's chunk table while leaving its generation row: the partial
	// state existingChunkGenerations is built to tolerate.
	_, err := pg.ExecContext(ctx,
		`DROP TABLE IF EXISTS `+vectorChunkTable(genX))
	require.NoError(t, err, "drop genX chunk table")

	// Shrink genY's session so the prune path runs; it must reference only the
	// surviving chunk table and not abort on genX's missing one.
	fake.hashes["S"] = "h2"
	fake.docs["S"] = []VectorPushDoc{
		vdoc("S", "S#0", 0, "c0", "hc0", []float32{1, 0, 0, 0}),
	}
	res, err := sync.pushVectors(ctx, false, nil, "", nil, nil)
	require.NoError(t, err, "push must not abort on a missing chunk table")
	assert.Equal(t, 1, res.SessionsPushed)
}
