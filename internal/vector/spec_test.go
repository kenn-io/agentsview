package vector

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// auxSpec is a second, minimal store used to prove two IndexSpecs can share
// one vectors.db without touching each other's state.
func auxSpec() IndexSpec {
	return IndexSpec{
		Name:          "aux",
		DocsTable:     "vector_aux_docs",
		MetaTable:     "vector_aux_meta",
		VectorsPrefix: "aux_vectors",
		MirrorDDL: `
CREATE TABLE IF NOT EXISTS vector_aux_docs (
    doc_key      TEXT PRIMARY KEY,
    content      TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    embed_gen    TEXT
);
CREATE TABLE IF NOT EXISTS vector_aux_meta (
    key TEXT PRIMARY KEY, value TEXT NOT NULL
);
`,
		MirrorSchemaVersion: "1",
	}
}

func openSpecT(t *testing.T, path string, spec IndexSpec, readOnly bool) *Index {
	t.Helper()
	ix, err := OpenSpec(context.Background(), path, spec, readOnly, 8192)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

func testGen(model string) kitvec.Generation {
	return kitvec.Generation{Model: model, Dimensions: 4, Params: map[string]string{"p": "1"}}
}

// Two stores in one vectors.db: generations and metadata stay disjoint.
func TestSpecCoexistenceGenerationsAndMetaAreDisjoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.db")
	ctx := context.Background()

	msg := openSpecT(t, path, MessageIndexSpec(), false)
	aux := openSpecT(t, path, auxSpec(), false)

	_, err := msg.EnsureGeneration(ctx, testGen("model-msg"), sqlitevec.StateActive)
	require.NoError(t, err)
	_, err = aux.EnsureGeneration(ctx, testGen("model-aux"), sqlitevec.StateActive)
	require.NoError(t, err)

	msgGens, err := msg.Generations(ctx)
	require.NoError(t, err)
	auxGens, err := aux.Generations(ctx)
	require.NoError(t, err)

	require.Len(t, msgGens, 1)
	require.Len(t, auxGens, 1)
	assert.Equal(t, "model-msg", msgGens[0].Model)
	assert.Equal(t, "model-aux", auxGens[0].Model)

	// gen_model metadata landed in each store's own meta table.
	_, ok, err := aux.metaGet(ctx, "gen_model:"+msgGens[0].Fingerprint)
	require.NoError(t, err)
	assert.False(t, ok, "message-store gen_model key leaked into aux meta table")
}

// A schema-version reset of one store must leave the other store's tables,
// metadata, and generations intact.
func TestSpecResetLeavesOtherStoreIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.db")
	ctx := context.Background()

	msg := openSpecT(t, path, MessageIndexSpec(), false)
	aux := openSpecT(t, path, auxSpec(), false)
	_, err := msg.EnsureGeneration(ctx, testGen("model-msg"), sqlitevec.StateActive)
	require.NoError(t, err)
	_, err = aux.EnsureGeneration(ctx, testGen("model-aux"), sqlitevec.StateActive)
	require.NoError(t, err)
	require.NoError(t, msg.Close())

	// Re-open the message store as if built by an older schema version:
	// the write path must drop and recreate ONLY message-store tables.
	stale := MessageIndexSpec()
	stale.MirrorSchemaVersion = "999"
	msg = openSpecT(t, path, stale, false)

	msgGens, err := msg.Generations(ctx)
	require.NoError(t, err)
	assert.Empty(t, msgGens, "message store should have been reset")

	auxGens, err := aux.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, auxGens, 1, "aux store must survive the message-store reset")
	assert.Equal(t, "model-aux", auxGens[0].Model)
}

// The reset's prefix match must be literal, not LIKE: "_" in a prefix such
// as "message_vectors" is a single-character LIKE wildcard, so a wildcard
// match would drop a table like "messageXvectors_generations" (X at the
// wildcard position) that belongs to no store owned by the spec.
func TestSpecResetPrefixMatchIsLiteral(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.db")
	ctx := context.Background()

	msg := openSpecT(t, path, MessageIndexSpec(), false)
	_, err := msg.db.ExecContext(ctx,
		`CREATE TABLE "messageXvectors_generations" (id INTEGER PRIMARY KEY)`)
	require.NoError(t, err)
	require.NoError(t, msg.Close())

	stale := MessageIndexSpec()
	stale.MirrorSchemaVersion = "999"
	msg = openSpecT(t, path, stale, false)

	var n int
	require.NoError(t, msg.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master
 WHERE type = 'table' AND name = 'messageXvectors_generations'`).Scan(&n))
	assert.Equal(t, 1, n, "wildcard-position decoy table must survive a message-store reset")
}

// The read path fails closed per store: a version mismatch on one store
// must not affect reads on the other.
func TestSpecReadPathMismatchIsPerStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.db")
	ctx := context.Background()

	msg := openSpecT(t, path, MessageIndexSpec(), false)
	aux := openSpecT(t, path, auxSpec(), false)
	_, err := aux.EnsureGeneration(ctx, testGen("model-aux"), sqlitevec.StateActive)
	require.NoError(t, err)
	require.NoError(t, msg.Close())

	stale := MessageIndexSpec()
	stale.MirrorSchemaVersion = "999"
	msgRO := openSpecT(t, path, stale, true)

	_, err = msgRO.Generations(ctx)
	assert.ErrorIs(t, err, ErrMirrorVersionMismatch)

	auxGens, err := aux.Generations(ctx)
	require.NoError(t, err)
	assert.Len(t, auxGens, 1)
}
