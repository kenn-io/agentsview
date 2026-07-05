package vector

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

func TestOpenCreatesSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	var name string
	require.NoError(t, ix.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='vector_messages'`).Scan(&name))
	require.Equal(t, "vector_messages", name)

	require.NoError(t, ix.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='message_vectors_generations'`).Scan(&name))
	require.Equal(t, "message_vectors_generations", name)
}

func TestOpenReadOnlyOnMissingFileFails(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "missing-vectors.db")

	_, err := Open(ctx, path, true, 4000)
	require.Error(t, err)
}

func TestEnsureGenerationLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateBuilding)
	require.NoError(t, err)
	require.NotEmpty(t, fingerprint)

	infos, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.Equal(t, "building", infos[0].State)
	require.Equal(t, "fake-model", infos[0].Model)
	require.Equal(t, 3, infos[0].Dimension)
	require.Equal(t, fingerprint, infos[0].Fingerprint)
	require.NotZero(t, infos[0].ID)

	building, ok, err := ix.BuildingFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, fingerprint, building)

	_, ok, err = ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, ix.SetStateByID(ctx, infos[0].ID, sqlitevec.StateActive))

	active, ok, err := ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, fingerprint, active)

	_, ok, err = ix.BuildingFingerprint(ctx)
	require.NoError(t, err)
	require.False(t, ok)

	info, err := ix.GenerationByID(ctx, infos[0].ID)
	require.NoError(t, err)
	require.Equal(t, "active", info.State)
}

// TestGenerationByIDUnknownIDReturnsSentinel guards the HTTP layer's 404
// mapping: an id with no matching row must return an error matching
// ErrGenerationNotFound via errors.Is, not just any error, so callers can
// distinguish "not found" from other failures.
func TestGenerationByIDUnknownIDReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	_, err = ix.GenerationByID(ctx, 999)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrGenerationNotFound)
	require.Contains(t, err.Error(), "999")
}

func TestGenerationCoverageCounts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateBuilding)
	require.NoError(t, err)

	_, err = ix.db.ExecContext(ctx,
		`INSERT INTO vector_messages (doc_key, session_id, ordinal, content, content_hash)
		 VALUES (?, ?, ?, ?, ?)`,
		"d1", "s1", 0, "hello world", "h1")
	require.NoError(t, err)
	_, err = ix.db.ExecContext(ctx,
		`INSERT INTO vector_messages (doc_key, session_id, ordinal, content, content_hash)
		 VALUES (?, ?, ?, ?, ?)`,
		"d2", "s1", 1, "goodbye world", "h2")
	require.NoError(t, err)

	err = ix.store.SaveVectors(ctx, fingerprint, "d1", "h1", []kitvec.ChunkVector{
		{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0}},
	})
	require.NoError(t, err)

	infos, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.EqualValues(t, 1, infos[0].Embedded)
	require.EqualValues(t, 1, infos[0].Missing)
}

// TestGenerationCoverageStaleRevisionCountsAsMissing asserts that a stamp
// whose revision no longer matches the mirror row's current content_hash
// (the content changed since it was embedded) counts as Missing, not
// Embedded, since kit's store treats such a document as pending re-embed.
func TestGenerationCoverageStaleRevisionCountsAsMissing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateBuilding)
	require.NoError(t, err)

	_, err = ix.db.ExecContext(ctx,
		`INSERT INTO vector_messages (doc_key, session_id, ordinal, content, content_hash)
		 VALUES (?, ?, ?, ?, ?)`,
		"d1", "s1", 0, "hello world", "h1")
	require.NoError(t, err)

	err = ix.store.SaveVectors(ctx, fingerprint, "d1", "h1", []kitvec.ChunkVector{
		{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0}},
	})
	require.NoError(t, err)

	infos, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.EqualValues(t, 1, infos[0].Embedded)
	require.EqualValues(t, 0, infos[0].Missing)

	_, err = ix.db.ExecContext(ctx,
		`UPDATE vector_messages SET content_hash = 'changed' WHERE doc_key = ?`, "d1")
	require.NoError(t, err)

	infos, err = ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.EqualValues(t, 0, infos[0].Embedded, "stale stamp revision no longer counts as embedded")
	require.EqualValues(t, 1, infos[0].Missing, "stale stamp revision counts as missing")
}
