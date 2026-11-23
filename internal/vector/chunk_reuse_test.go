package vector

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
)

func TestBuildGrowingMessageReusesChunksAndRetry(t *testing.T) {
	ix := openTestIndex(t)
	ix.split = kitvec.SplitOptions{MaxRunes: 10, Overlap: 2}
	src := twoDocSource()
	src.rows = src.rows[:1]
	src.rows[0].unit.Content = "abcdefghijklmnopqrstuv"
	gen := fakeGeneration("reuse")
	var inputs []string
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		inputs = append(inputs, texts...)
		return fakeBuildEncoder()(ctx, texts)
	}
	_, err := ix.Build(t.Context(), src, enc, gen, BuildOptions{})
	require.NoError(t, err)
	old := kitvec.Split(src.rows[0].unit.Content, ix.split)
	require.Len(t, old, 3)
	src.rows[0].unit.Content += "wxyzABCDEFGH"
	src.rows[0].endedAt = "2025-01-01T00:00:00Z"
	inputs = nil
	fail := func(context.Context, []string) ([][]float32, error) { return nil, errors.New("offline") }
	_, err = ix.Build(t.Context(), src, fail, gen, BuildOptions{})
	require.Error(t, err)
	var progress []BuildProgress
	result, err := ix.Build(t.Context(), src, enc, gen, BuildOptions{Progress: func(p BuildProgress) { progress = append(progress, p) }})
	require.NoError(t, err)
	require.NotEmpty(t, progress)
	assert.EqualValues(t, result.Fill.Chunks, progress[len(progress)-1].Done)
	assert.EqualValues(t, result.Fill.Chunks, progress[len(progress)-1].Total)
	chunks := kitvec.Split(src.rows[0].unit.Content, ix.split)
	var expected []string
	for _, c := range chunks[2:] {
		expected = append(expected, c.Text)
	}
	assert.Equal(t, expected, inputs)
	var count int
	require.NoError(t, ix.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM message_vectors_reuse`).Scan(&count))
	assert.Zero(t, count)
}

func TestReuseEncoderMapsInvalidChunk(t *testing.T) {
	ix := openTestIndex(t)
	gen := reuseGeneration{ordinal: 1, dimension: 3}
	_, err := ix.db.ExecContext(t.Context(), `INSERT INTO message_vectors_reuse VALUES (?, ?, ?)`, gen.ordinal, contentHash("hit"), []byte{0, 0, 128, 63, 0, 0, 0, 0, 0, 0, 0, 0})
	require.NoError(t, err)
	enc := ix.reuseEncoder(func(_ context.Context, texts []string) ([][]float32, error) {
		assert.Equal(t, []string{"miss", "bad"}, texts)
		return nil, &kitvec.InvalidVectorError{Chunk: 1, Component: -1, Reason: "zero"}
	}, gen)
	_, err = enc(t.Context(), []string{"hit", "miss", "bad"})
	var invalid *kitvec.InvalidVectorError
	require.ErrorAs(t, err, &invalid)
	assert.Equal(t, 2, invalid.Chunk)
}

func TestFullRebuildClearsReuseBeforeFailure(t *testing.T) {
	ix := openTestIndex(t)
	ix.split = kitvec.SplitOptions{MaxRunes: 10, Overlap: 2}
	src := twoDocSource()
	src.rows = src.rows[:1]
	src.rows[0].unit.Content = strings.Repeat("a", 22)
	gen := fakeGeneration("reuse-full")
	_, err := ix.Build(t.Context(), src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	src.rows[0].unit.Content += "bcdefghijk"
	src.rows[0].endedAt = "2025-01-01T00:00:00Z"
	fail := func(context.Context, []string) ([][]float32, error) { return nil, errors.New("offline") }
	_, err = ix.Build(t.Context(), src, fail, gen, BuildOptions{})
	require.Error(t, err)
	_, err = ix.Build(t.Context(), src, fail, gen, BuildOptions{FullRebuild: true})
	require.Error(t, err)
	var inputs []string
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		inputs = append(inputs, texts...)
		return fakeBuildEncoder()(ctx, texts)
	}
	_, err = ix.Build(t.Context(), src, enc, gen, BuildOptions{})
	require.NoError(t, err)
	assert.Len(t, inputs, len(kitvec.Split(src.rows[0].unit.Content, ix.split)))
}

func TestReuseEncoderHitsAndWrongCount(t *testing.T) {
	ix := openTestIndex(t)
	gen := reuseGeneration{ordinal: 1, dimension: 3}
	_, err := ix.db.ExecContext(t.Context(), `INSERT INTO message_vectors_reuse VALUES (?, ?, ?)`, gen.ordinal, contentHash("hit"), []byte{0, 0, 128, 63, 0, 0, 0, 0, 0, 0, 0, 0})
	require.NoError(t, err)
	enc := ix.reuseEncoder(func(_ context.Context, texts []string) ([][]float32, error) {
		assert.Equal(t, []string{"miss"}, texts)
		return nil, nil
	}, gen)
	vectors, err := enc(t.Context(), []string{"hit", "hit"})
	require.NoError(t, err)
	assert.Equal(t, [][]float32{{1, 0, 0}, {1, 0, 0}}, vectors)
	_, err = enc(t.Context(), []string{"hit", "miss"})
	require.ErrorContains(t, err, "encoder returned 0 vectors for 1 inputs")
}

func TestBuildGrowingTwiceBetweenBuilds(t *testing.T) {
	ix := openTestIndex(t)
	ix.split = kitvec.SplitOptions{MaxRunes: 10, Overlap: 2}
	src := twoDocSource()
	src.rows = src.rows[:1]
	src.rows[0].unit.Content = "abcdefghijklmnopqrstuv"
	gen := fakeGeneration("reuse-twice")
	_, err := ix.Build(t.Context(), src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	src.rows[0].unit.Content += "wxyz"
	src.rows[0].endedAt = "2025-01-01T00:00:00Z"
	fail := func(context.Context, []string) ([][]float32, error) { return nil, errors.New("offline") }
	_, err = ix.Build(t.Context(), src, fail, gen, BuildOptions{})
	require.Error(t, err)
	src.rows[0].unit.Content += "ABCDEFGH"
	src.rows[0].endedAt = "2026-01-01T00:00:00Z"
	var inputs []string
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		inputs = append(inputs, texts...)
		return fakeBuildEncoder()(ctx, texts)
	}
	_, err = ix.Build(t.Context(), src, enc, gen, BuildOptions{})
	require.NoError(t, err)
	chunks := kitvec.Split(src.rows[0].unit.Content, ix.split)
	var expected []string
	for _, c := range chunks[2:] {
		expected = append(expected, c.Text)
	}
	assert.Equal(t, expected, inputs)
}

func TestBuildDoesNotReuseMismatchedChunkMap(t *testing.T) {
	ix := openTestIndex(t)
	ix.split = kitvec.SplitOptions{MaxRunes: 10, Overlap: 2}
	src := twoDocSource()
	src.rows = src.rows[:1]
	src.rows[0].unit.Content = "abcdefghijklmnopqrstuv"
	gen := fakeGeneration("reuse-map")
	_, err := ix.Build(t.Context(), src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	_, err = ix.db.ExecContext(t.Context(), `DELETE FROM message_vectors_chunks WHERE chunk_index = 1`)
	require.NoError(t, err)
	src.rows[0].unit.Content += "wxyzABCDEFGH"
	src.rows[0].endedAt = "2025-01-01T00:00:00Z"
	var inputs []string
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		inputs = append(inputs, texts...)
		return fakeBuildEncoder()(ctx, texts)
	}
	_, err = ix.Build(t.Context(), src, enc, gen, BuildOptions{})
	require.NoError(t, err)
	assert.Len(t, inputs, len(kitvec.Split(src.rows[0].unit.Content, ix.split)))
}

func TestBuildReusesChunksEmbeddedBeforeUpgrade(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := Open(ctx, path, false, 10)
	require.NoError(t, err)
	src := twoDocSource()
	src.rows = src.rows[:1]
	src.rows[0].unit.Content = "abcdefghijklmnopqrstuv"
	gen := fakeGeneration("old-index")
	_, err = ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	_, err = ix.db.ExecContext(ctx, `DROP TABLE message_vectors_reuse`)
	require.NoError(t, err)
	require.NoError(t, ix.Close())
	ix, err = Open(ctx, path, false, 10)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	old := kitvec.Split(src.rows[0].unit.Content, ix.split)
	src.rows[0].unit.Content += "wxyzABCDEFGH"
	src.rows[0].endedAt = "2025-01-01T00:00:00Z"
	var inputs []string
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		inputs = append(inputs, texts...)
		return fakeBuildEncoder()(ctx, texts)
	}
	_, err = ix.Build(ctx, src, enc, gen, BuildOptions{})
	require.NoError(t, err)
	chunks := kitvec.Split(src.rows[0].unit.Content, ix.split)
	var expected []string
	for _, c := range chunks[len(old)-1:] {
		expected = append(expected, c.Text)
	}
	assert.Equal(t, expected, inputs)
}

func TestBuildReuseMatchesFreshVectors(t *testing.T) {
	ctx := t.Context()
	ix := openTestIndex(t)
	ix.split = kitvec.SplitOptions{MaxRunes: 10, Overlap: 2}
	src := twoDocSource()
	src.rows = src.rows[:1]
	src.rows[0].unit.Content = "abcdefghijklmnopqrstuv"
	gen := fakeGeneration("compare")
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		vectors := make([][]float32, len(texts))
		for i, text := range texts {
			vectors[i] = []float32{float32(len(text)), float32(text[0]), 1}
		}
		return vectors, nil
	}
	_, err := ix.Build(ctx, src, enc, gen, BuildOptions{})
	require.NoError(t, err)
	src.rows[0].unit.Content += "wxyzABCDEFGH"
	src.rows[0].endedAt = "2025-01-01T00:00:00Z"
	_, err = ix.Build(ctx, src, enc, gen, BuildOptions{})
	require.NoError(t, err)
	fresh := openTestIndex(t)
	fresh.split = ix.split
	_, err = fresh.Build(ctx, src, enc, gen, BuildOptions{})
	require.NoError(t, err)
	read := func(index *Index) map[int][]byte {
		rows, err := index.db.QueryContext(ctx, `SELECT c.chunk_index, v.embedding FROM message_vectors_chunks c JOIN message_vectors_v1 v ON v.rowid = c.vec_rowid ORDER BY c.chunk_index`)
		require.NoError(t, err)
		defer rows.Close()
		result := make(map[int][]byte)
		for rows.Next() {
			var n int
			var blob []byte
			require.NoError(t, rows.Scan(&n, &blob))
			result[n] = blob
		}
		require.NoError(t, rows.Err())
		return result
	}
	assert.Equal(t, read(fresh), read(ix))
}

func TestBuildDoesNotReuseInvalidStoredVector(t *testing.T) {
	ix := openTestIndex(t)
	ix.split = kitvec.SplitOptions{MaxRunes: 10, Overlap: 2}
	src := twoDocSource()
	src.rows = src.rows[:1]
	src.rows[0].unit.Content = "abcdefghijklmnopqrstuv"
	gen := fakeGeneration("invalid")
	_, err := ix.Build(t.Context(), src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	_, err = ix.db.ExecContext(t.Context(), `UPDATE message_vectors_v1 SET embedding = zeroblob(12) WHERE rowid = (SELECT vec_rowid FROM message_vectors_chunks WHERE chunk_index = 0)`)
	require.NoError(t, err)
	src.rows[0].unit.Content += "wxyzABCDEFGH"
	src.rows[0].endedAt = "2025-01-01T00:00:00Z"
	var inputs []string
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		inputs = append(inputs, texts...)
		return fakeBuildEncoder()(ctx, texts)
	}
	_, err = ix.Build(t.Context(), src, enc, gen, BuildOptions{})
	require.NoError(t, err)
	chunks := kitvec.Split(src.rows[0].unit.Content, ix.split)
	var expected []string
	for i, chunk := range chunks {
		if i != 1 {
			expected = append(expected, chunk.Text)
		}
	}
	assert.Equal(t, expected, inputs)
}
