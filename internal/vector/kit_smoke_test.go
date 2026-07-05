package vector

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

func TestKitSqlitevecRoundTrip(t *testing.T) {
	sqlitevec.Register()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "v.db"))
	require.NoError(t, err)
	defer db.Close()
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `CREATE TABLE docs (
        doc_key TEXT PRIMARY KEY, content TEXT NOT NULL,
        content_hash TEXT NOT NULL, embed_gen TEXT)`)
	require.NoError(t, err)
	store, err := sqlitevec.New[string, string](ctx, db, sqlitevec.Schema{
		DocsTable: "docs", IDColumn: "doc_key", ContentColumn: "content",
		EmbedGenColumn: "embed_gen", RevisionColumn: "content_hash",
		VectorsPrefix: "docs_vectors",
	})
	require.NoError(t, err)
	gen := kitvec.Generation{Model: "fake", Dimensions: 3}
	fp := gen.Fingerprint()
	require.NoError(t, store.EnsureGeneration(ctx, fp, gen, sqlitevec.StateActive))
	_, err = db.ExecContext(ctx,
		`INSERT INTO docs VALUES ('d1', 'hello world', 'h1', NULL)`)
	require.NoError(t, err)
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}
	stats, err := kitvec.Fill[string, string](ctx, store, fp, enc,
		kitvec.FillOptions[string]{})
	require.NoError(t, err)
	require.Equal(t, 1, stats.Documents)
	hits, err := store.QueryGeneration(ctx, fp, kitvec.Vector{1, 0, 0}, 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "d1", hits[0].Doc)
}
