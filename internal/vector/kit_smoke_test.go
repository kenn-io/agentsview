package vector

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

func TestKitSqlitevecRoundTrip(t *testing.T) {
	require := require.New(t)

	sqlitevec.Register()
	db, err := sql.Open(vectorDriverName, vectorDSN(filepath.Join(t.TempDir(), "v.db"), false))
	require.NoError(err)
	defer db.Close()
	ctx := t.Context()
	_, err = db.ExecContext(ctx, `CREATE TABLE docs (
        doc_key TEXT PRIMARY KEY, content TEXT NOT NULL,
        content_hash TEXT NOT NULL, embed_gen TEXT)`)
	require.NoError(err)
	store, err := sqlitevec.New[string, string](ctx, db, sqlitevec.Schema{
		DocsTable: "docs", IDColumn: "doc_key", ContentColumn: "content",
		EmbedGenColumn: "embed_gen", RevisionColumn: "content_hash",
		VectorsPrefix: "docs_vectors",
	})
	require.NoError(err)
	gen := kitvec.Generation{Model: "fake", Dimensions: 3}
	fp := gen.Fingerprint()
	require.NoError(store.EnsureGeneration(ctx, fp, gen, sqlitevec.StateActive))
	_, err = db.ExecContext(ctx,
		`INSERT INTO docs VALUES ('d1', 'hello world', 'h1', NULL)`)
	require.NoError(err)
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}
	stats, err := kitvec.Fill[string, string](ctx, store, fp, enc)
	require.NoError(err)
	require.Equal(1, stats.Documents)
	hits, err := store.QueryGeneration(ctx, fp, kitvec.Vector{1, 0, 0}, 5)
	require.NoError(err)
	require.Len(hits, 1)
	require.Equal("d1", hits[0].Doc)
}
