package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/vector"
)

// enabledVectorConfig returns a minimal Config with [vector] enabled and a
// fresh DataDir, so the push source resolves a vectors.db path under a temp
// dir that no other test touches.
func enabledVectorConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		DataDir: t.TempDir(),
		Vector: config.VectorConfig{
			Enabled: true,
			Embeddings: config.VectorEmbeddingsConfig{
				Model:         "fake-model",
				Dimension:     4,
				MaxInputChars: 8192,
			},
		},
	}
}

// fakePushUnitSource is a vector.UnitSource that replays a fixed slice of
// units, enough to build a tiny active generation for the round-trip test.
type fakePushUnitSource struct {
	units []db.EmbeddableUnit
}

func (f fakePushUnitSource) ScanEmbeddableUnits(
	_ context.Context, _ string, _ bool, fn func(db.EmbeddableUnit) error,
) (string, error) {
	for _, u := range f.units {
		if err := fn(u); err != nil {
			return "", err
		}
	}
	return "2024-01-01T00:00:03Z", nil
}

// fakePushEncoder returns a deterministic 4-dimensional encoder matching the
// gen dimension the round-trip test builds with.
func fakePushEncoder() kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0, 0}
		}
		return out, nil
	}
}

// testPushUnitSource returns the fixed unit set the push-source tests build
// their vectors.db from: two sessions, three documents.
func testPushUnitSource() fakePushUnitSource {
	return fakePushUnitSource{units: []db.EmbeddableUnit{
		{SessionID: "session-1", Kind: "user", SourceUUID: "u1", Content: "hello"},
		{
			SessionID: "session-1", Kind: "run", SourceUUID: "a1",
			Ordinal: 1, OrdinalEnd: 2, Content: "first\n\nsecond",
			Offsets: []db.UnitOffset{{Ordinal: 1}, {Ordinal: 2, RuneStart: 7, ByteStart: 7}},
		},
		{SessionID: "session-2", Kind: "user", SourceUUID: "u2", Content: "world"},
	}}
}

// buildTestVectorsDB opens a read-write index at cfg's resolved vectors.db
// path, builds and activates one generation over two sessions, then closes
// it so the push source can reopen the file read-only.
func buildTestVectorsDB(t *testing.T, cfg config.Config) {
	t.Helper()
	ctx := context.Background()
	ix, err := vector.Open(
		ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), false,
		cfg.Vector.Embeddings.MaxInputChars,
	)
	require.NoError(t, err)
	defer ix.Close()

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 4}
	result, err := ix.Build(
		ctx, testPushUnitSource(), fakePushEncoder(), gen, vector.BuildOptions{},
	)
	require.NoError(t, err)
	require.True(t, result.Activated)
}

// closePushSource registers a cleanup that closes the adapter's vectors.db
// handle. Required on Windows, where TempDir removal fails while the sqlite
// file is still open.
func closePushSource(t *testing.T, src postgres.VectorPushSource) {
	t.Helper()
	t.Cleanup(func() {
		require.NoError(t, src.(*vectorPushSource).Close())
	})
}

func TestNewVectorPushSourceDisabled(t *testing.T) {
	cfg := config.Config{} // [vector] disabled
	assert.Nil(t, newVectorPushSource(cfg))
}

func TestVectorPushSourceMissingFile(t *testing.T) {
	cfg := enabledVectorConfig(t)
	src := newVectorPushSource(cfg)
	require.NotNil(t, src)
	closePushSource(t, src)

	_, ok, err := src.Generation(context.Background())
	require.NoError(t, err)
	assert.False(t, ok) // no vectors.db yet -> nothing to push, not an error
}

// TestVectorPushSourceMissingFileThenBuilt pins that a missing vectors.db is
// not memoized: the SAME adapter reports "nothing to push" before any build and
// then picks up a generation built at the same path afterward, as a daemon push
// that starts before embeddings exist must.
func TestVectorPushSourceMissingFileThenBuilt(t *testing.T) {
	ctx := context.Background()
	cfg := enabledVectorConfig(t)
	src := newVectorPushSource(cfg)
	require.NotNil(t, src)
	closePushSource(t, src)

	_, ok, err := src.Generation(ctx)
	require.NoError(t, err)
	require.False(t, ok, "no vectors.db yet -> nothing to push")

	buildTestVectorsDB(t, cfg)

	gen, ok, err := src.Generation(ctx)
	require.NoError(t, err)
	require.True(t, ok, "same adapter must pick up a later build")
	assert.Equal(t, "fake-model", gen.Model)

	hashes, err := src.SessionDocHashes(ctx, nil)
	require.NoError(t, err)
	assert.Len(t, hashes, 2)
}

// TestVectorPushSourceNotReadyDuringRebuild pins the partial-coverage gate: a
// full rebuild clears the active generation's stamps in place before
// re-embedding, so an aborted (or still running) rebuild leaves the active
// generation with missing embeddings. Exporting that view would evict valid
// PG vectors, so Generation must refuse with ErrVectorSourceNotReady until a
// build completes.
func TestVectorPushSourceNotReadyDuringRebuild(t *testing.T) {
	ctx := context.Background()
	cfg := enabledVectorConfig(t)
	buildTestVectorsDB(t, cfg)

	// Start a full rebuild whose encoder fails: resetGeneration has already
	// cleared the active generation's stamps, so coverage is now partial —
	// exactly the state a concurrent push would observe mid-rebuild.
	ix, err := vector.Open(
		ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), false,
		cfg.Vector.Embeddings.MaxInputChars,
	)
	require.NoError(t, err)
	failingEncoder := func(_ context.Context, _ []string) ([][]float32, error) {
		return nil, errors.New("embeddings endpoint down")
	}
	_, err = ix.Build(ctx, testPushUnitSource(), failingEncoder,
		kitvec.Generation{Model: "fake-model", Dimensions: 4},
		vector.BuildOptions{FullRebuild: true},
	)
	require.Error(t, err, "rebuild must abort on encoder failure")
	require.NoError(t, ix.Close())

	src := newVectorPushSource(cfg)
	require.NotNil(t, src)
	closePushSource(t, src)

	_, ok, err := src.Generation(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, postgres.ErrVectorSourceNotReady)
	assert.False(t, ok)
}

func TestVectorPushSourceRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg := enabledVectorConfig(t)
	buildTestVectorsDB(t, cfg)

	src := newVectorPushSource(cfg)
	require.NotNil(t, src)
	closePushSource(t, src)

	gen, ok, err := src.Generation(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.NotEmpty(t, gen.Fingerprint)
	assert.Equal(t, "fake-model", gen.Model)
	assert.Equal(t, 4, gen.Dimension)

	hashes, err := src.SessionDocHashes(ctx, nil)
	require.NoError(t, err)
	require.Len(t, hashes, 2)
	assert.Contains(t, hashes, "session-1")
	assert.Contains(t, hashes, "session-2")

	// Cross-check the adapter's mapping against the raw export API: every
	// VectorPushDoc field must mirror its ExportDoc source exactly.
	ix, err := vector.Open(
		ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), true,
		cfg.Vector.Embeddings.MaxInputChars,
	)
	require.NoError(t, err)
	defer ix.Close()
	exp, ok, err := ix.ActiveExport(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	want, wantHash, err := ix.ExportSessionDocs(ctx, exp.Ordinal, "session-1")
	require.NoError(t, err)
	require.NotEmpty(t, want)
	assert.Equal(t, hashes["session-1"], wantHash,
		"export hash must match the delta-scan aggregate for an unchanged index")

	docs, gotHash, err := src.SessionDocs(ctx, "session-1")
	require.NoError(t, err)
	assert.Equal(t, wantHash, gotHash)
	require.Len(t, docs, len(want))
	for i, got := range docs {
		w := want[i]
		assert.Equal(t, w.DocKey, got.DocKey)
		assert.Equal(t, w.SessionID, got.SessionID)
		assert.Equal(t, w.SourceUUID, got.SourceUUID)
		assert.Equal(t, w.Ordinal, got.Ordinal)
		assert.Equal(t, w.OrdinalEnd, got.OrdinalEnd)
		assert.Equal(t, w.Subordinate, got.Subordinate)
		assert.Equal(t, w.OffsetsJSON, got.OffsetsJSON)
		assert.Equal(t, w.Content, got.Content)
		assert.Equal(t, w.ContentHash, got.ContentHash)
		require.Len(t, got.Chunks, len(w.Chunks))
		for j, gc := range got.Chunks {
			assert.Equal(t, w.Chunks[j].ChunkIndex, gc.ChunkIndex)
			assert.Equal(t, w.Chunks[j].Embedding, gc.Embedding)
		}
	}
}

// TestCloseVectorPushSource pins the creator-owned handle lifecycle: closing
// releases the memoized read-only vectors.db handle (so per-push and
// per-watch-loop sources do not leak SQLite handles), a nil source is a
// no-op, and a closed adapter transparently reopens on the next call.
func TestCloseVectorPushSource(t *testing.T) {
	closeVectorPushSource(nil) // must not panic

	cfg := enabledVectorConfig(t)
	buildTestVectorsDB(t, cfg)
	src := newVectorPushSource(cfg)
	require.NotNil(t, src)

	ctx := context.Background()
	_, ok, err := src.Generation(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	adapter := src.(*vectorPushSource)
	require.NotNil(t, adapter.ix, "Generation memoizes the open handle")

	closeVectorPushSource(src)
	assert.Nil(t, adapter.ix, "close releases the memoized handle")
	closeVectorPushSource(src) // idempotent

	_, ok, err = src.Generation(ctx)
	require.NoError(t, err)
	assert.True(t, ok, "a closed adapter reopens lazily")
	closePushSource(t, src)
}
