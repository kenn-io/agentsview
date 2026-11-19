//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/pgvector"
)

func TestVectorCatalogMigrationKeepsExistingSearch(t *testing.T) {
	t.Parallel()
	pg := newVectorSearchTestPG(t)
	ctx := t.Context()
	id, err := ensureVectorGeneration(ctx, pg, "legacy", "fixture", 4)
	require.NoError(t, err)
	require.NoError(t, ensureVectorChunkTable(ctx, pg, id, 4))
	ext, err := vectorExtensionSchema(ctx, pg)
	require.NoError(t, err)
	insertSearchDoc(t, pg, "first", "session", 1, 1, "[]", "first")
	insertSearchDoc(t, pg, "second", "session", 2, 2, "[]", "second")
	insertSearchDoc(t, pg, "empty", "session", 3, 3, "[]", "empty")
	insertSearchChunk(t, pg, vectorChunkTable(id), ext+".halfvec", "first", 0, []float32{1, 0, 0, 0})
	insertSearchChunk(t, pg, vectorChunkTable(id), ext+".halfvec", "second", 0, []float32{1, 1, 0, 0})
	_, err = pg.ExecContext(ctx, `DROP TABLE vector_stamps; ALTER TABLE vector_generations DROP COLUMN gen_key, DROP COLUMN state`)
	require.NoError(t, err)
	readStore := &Store{pg: pg, ownedDB: pg}
	require.NoError(t, prepareServeStore(ctx, readStore, vectorSearchTestSchema))
	searcher, reason, err := (Backend{}).OpenVectorSearcher(ctx, readStore, storage.VectorGenerationInfo{Fingerprint: "legacy"}, 128, fixedEncoder([]float32{1, 0, 0, 0}))
	require.NoError(t, err)
	assert.Nil(t, searcher)
	assert.Contains(t, reason, "run pg push or hosted-provision")
	rows, err := pg.QueryContext(ctx, `SELECT doc_key FROM `+vectorChunkTable(id)+` ORDER BY embedding OPERATOR(`+ext+`.<=>) '[1,0,0,0]'::`+ext+`.halfvec`)
	require.NoError(t, err)
	var before []string
	for rows.Next() {
		var key string
		require.NoError(t, rows.Scan(&key))
		before = append(before, key)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	for range 2 {
		reason, err := ensureVectorBaseSchemaPG(ctx, pg)
		require.NoError(t, err)
		require.Empty(t, reason)
	}
	_, err = pg.ExecContext(ctx, `INSERT INTO vector_generations (fingerprint, model, dimension) VALUES ('old-writer', 'fixture', 4)`)
	require.Error(t, err)
	store, err := openVectorStore(ctx, pg)
	require.NoError(t, err)
	hits, err := store.QueryGeneration(ctx, "legacy", kitvec.Vector{1, 0, 0, 0}, 10)
	require.NoError(t, err)
	var after []string
	for _, hit := range hits {
		after = append(after, hit.Doc)
	}
	assert.Equal(t, []string{"first", "second"}, before)
	assert.Equal(t, before, after)
	calls := 0
	searcher = newVectorSearcherForTest(t, pg, id, 4, 128, func(context.Context, string) ([]float32, error) {
		calls++
		return []float32{1, 0, 0, 0}, nil
	})
	results, err := dbtest.SemanticSearch(ctx, searcher, "query", 10)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "first", results[0].Snippet)
	assert.Equal(t, "second", results[1].Snippet)
	assert.Equal(t, 1, calls)
	var stamps int
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT count(*) FROM vector_stamps`).Scan(&stamps))
	assert.Equal(t, 2, stamps)
}

func TestHostedVectorCatalogMigrationKeepsExistingSearch(t *testing.T) {
	t.Parallel()
	f := newHostedFixture(t, "vector-migration")
	ctx := t.Context()
	_, err := f.admin.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "legacy", Model: "fixture", Dimension: 4}
	require.NoError(t, ProvisionHostedVector(ctx, f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(ctx, f.admin, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	ext, err := vectorExtensionSchema(ctx, f.admin)
	require.NoError(t, err)
	_, err = f.admin.ExecContext(ctx, `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	insertSearchDoc(t, f.runtime, "first", "session", 1, 1, "[]", "first")
	insertSearchDoc(t, f.runtime, "second", "session", 2, 2, "[]", "second")
	insertSearchChunk(t, f.runtime, vectorChunkTable(id), ext+".halfvec", "first", 0, []float32{1, 0, 0, 0})
	insertSearchChunk(t, f.runtime, vectorChunkTable(id), ext+".halfvec", "second", 0, []float32{1, 1, 0, 0})
	_, err = f.admin.ExecContext(ctx, `DROP TABLE vector_stamps; ALTER TABLE vector_generations DROP COLUMN gen_key, DROP COLUMN state`)
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, EnsureHostedTenant(ctx, f.admin, f.schema, f.tenant))
	}
	_, err = f.admin.ExecContext(ctx, `GRANT SELECT ON vector_stamps, `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	store, err := openVectorStore(ctx, f.runtime)
	require.NoError(t, err)
	hits, err := store.QueryGeneration(ctx, gen.Fingerprint, kitvec.Vector{1, 0, 0, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, "first", hits[0].Doc)
	assert.Equal(t, "second", hits[1].Doc)
}

func TestVectorSearchDropsChangedDocument(t *testing.T) {
	t.Parallel()
	pg := newVectorSearchTestPG(t)
	id, _ := seedVectorSearchFixture(t, pg)
	searcher := newVectorSearcherForTest(t, pg, id, 4, 128, fixedEncoder([]float32{1, 0, 0, 0})).(*vectorSearcher)
	searcher.afterCandidates = func() {
		_, err := pg.ExecContext(t.Context(), `UPDATE vector_documents SET content='new content', content_hash='new-hash' WHERE doc_key='u1'`)
		require.NoError(t, err)
	}
	results, err := dbtest.SemanticSearch(t.Context(), searcher, "query", 10)
	require.NoError(t, err)
	for _, result := range results {
		assert.NotEqual(t, "new content", result.Snippet)
	}
}

func TestVectorSearcherRequiresPGVector08(t *testing.T) {
	t.Parallel()
	pg := newVectorSearchTestPG(t)
	ctx := t.Context()
	id, err := ensureVectorGeneration(ctx, pg, "old-version", "fixture", 4)
	require.NoError(t, err)
	require.NoError(t, ensureVectorChunkTable(ctx, pg, id, 4))
	_, err = pg.ExecContext(ctx, `UPDATE pg_extension SET extversion='0.7.4' WHERE extname='vector'`)
	require.NoError(t, err)
	searcher, reason, err := (Backend{}).OpenVectorSearcher(ctx, &Store{pg: pg, ownedDB: pg}, storage.VectorGenerationInfo{Fingerprint: "old-version"}, 128, fixedEncoder([]float32{1, 0, 0, 0}))
	require.NoError(t, err)
	assert.Nil(t, searcher)
	assert.Contains(t, reason, "pgvector 0.8.0 or later required")
}

func TestVectorSaveRejectsChangedDocument(t *testing.T) {
	t.Parallel()
	pg := newVectorSearchTestPG(t)
	ctx := t.Context()
	id, err := ensureVectorGeneration(ctx, pg, "stale", "fixture", 4)
	require.NoError(t, err)
	require.NoError(t, ensureVectorChunkTable(ctx, pg, id, 4))
	insertSearchDoc(t, pg, "changed", "session", 1, 1, "[]", "new content")
	_, err = pg.ExecContext(ctx, `UPDATE vector_documents SET content_hash='new-hash' WHERE doc_key='changed'`)
	require.NoError(t, err)
	store, err := openVectorStore(ctx, pg)
	require.NoError(t, err)
	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	scope := vectorPushScope{store: store, gen: vectorGeneration{fingerprint: "stale"}}
	err = saveVectorDocuments(ctx, tx, scope, []pgvector.Document[string]{{Doc: "changed", Revision: "old-hash", Vectors: []kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0, 0}}}}})
	require.ErrorIs(t, err, kitvec.ErrStale)
	var count int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM vector_stamps`).Scan(&count))
	assert.Zero(t, count)
}
