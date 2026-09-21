//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A chunk table created before the content-hash stamp (legacy DDL, no
// column) is migrated by EnsureVectorChunkContentHashColumn during read-side
// wiring: the column appears with an empty default, so pre-stamp rows read
// as generation-unverifiable instead of breaking every query. The migration
// is idempotent.
func TestEnsureVectorChunkContentHashColumnMigratesLegacyTable(t *testing.T) {
	pg := newVectorSearchTestPG(t, testPGURL(t), "agentsview_vector_schema_test")
	extSchema, err := vectorExtensionSchema(t.Context(), pg)
	require.NoError(t, err, "vectorExtensionSchema")

	_, err = pg.Exec(`CREATE TABLE vector_chunks_g999999 (
    doc_key      TEXT NOT NULL,
    chunk_index  INTEGER NOT NULL,
    embedding    ` + extSchema + `.vector(4) NOT NULL,
    PRIMARY KEY (doc_key, chunk_index)
)`)
	require.NoError(t, err, "create legacy chunk table")
	t.Cleanup(func() { _, _ = pg.Exec(`DROP TABLE vector_chunks_g999999`) })

	_, err = pg.Exec(`INSERT INTO vector_chunks_g999999 (doc_key, chunk_index, embedding)
		VALUES ('legacy', 0, '[1,0,0,0]')`)
	require.NoError(t, err, "insert legacy row")

	require.NoError(t, EnsureVectorChunkContentHashColumn(
		t.Context(), pg, 999999), "migrate legacy chunk table")

	var hash string
	require.NoError(t, pg.QueryRow(
		`SELECT content_hash FROM vector_chunks_g999999 WHERE doc_key = 'legacy'`,
	).Scan(&hash))
	assert.Empty(t, hash, "pre-stamp rows read as generation-unverifiable")

	require.NoError(t, EnsureVectorChunkContentHashColumn(
		t.Context(), pg, 999999), "migration is idempotent")
}
