package vector

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
)

func TestRecallBuildDoesNotUseChunkReuse(t *testing.T) {
	ctx := t.Context()
	ix := openSpecT(t, filepath.Join(t.TempDir(), "vectors.db"), RecallIndexSpec(), false)
	var count int
	require.NoError(t, ix.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, ix.spec.reuseTable()).Scan(&count))
	assert.Zero(t, count)

	gen := fakeGeneration("recall")
	_, err := ix.Build(ctx, twoDocSource(), fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	_, err = ix.Build(ctx, twoDocSource(), fakeBuildEncoder(), gen, BuildOptions{FullRebuild: true})
	require.NoError(t, err)
}

func TestBuildChunkReuse(t *testing.T) {
	for _, mode := range []string{"retry", "grows twice", "full rebuild", "mismatched chunks", "legacy active"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			ix := openTestIndex(t)
			ix.split = kitvec.SplitOptions{MaxRunes: 10, Overlap: 2}
			src := twoDocSource()
			src.rows = src.rows[:1]
			src.rows[0].unit.Content = "abcdefghijklmnopqrstuv"
			gen := fakeGeneration("reuse")
			other := fakeGeneration("other")
			if mode == "legacy active" {
				_, err := ix.Build(ctx, src, fakeBuildEncoder(), other, BuildOptions{})
				require.NoError(t, err)
			}
			_, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
			require.NoError(t, err)
			if mode == "mismatched chunks" {
				_, err = ix.db.ExecContext(ctx, `DELETE FROM message_vectors_chunks WHERE chunk_index = 1`)
				require.NoError(t, err)
			}
			src.rows[0].unit.Content += "wxyz"
			src.rows[0].endedAt = "2025-01-01T00:00:00Z"
			opts := BuildOptions{}
			buildGen := gen
			if mode == "legacy active" {
				opts.Space = legacySpace(gen.Fingerprint())
				buildGen = other
			}
			fail := func(context.Context, []string) ([][]float32, error) { return nil, errors.New("offline") }
			if mode != "mismatched chunks" {
				_, err = ix.Build(ctx, src, fail, buildGen, opts)
				require.Error(t, err)
			}
			if mode == "legacy active" {
				var count int
				require.NoError(t, ix.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_vectors_reuse`).Scan(&count))
				assert.Zero(t, count)
			}
			if mode == "full rebuild" {
				_, err = ix.Build(ctx, src, fail, gen, BuildOptions{FullRebuild: true})
				require.Error(t, err)
			}
			if mode == "grows twice" || mode == "mismatched chunks" {
				src.rows[0].unit.Content += "ABCDEFGH"
				src.rows[0].endedAt = "2026-01-01T00:00:00Z"
			}
			var inputs []string
			enc := func(ctx context.Context, texts []string) ([][]float32, error) {
				inputs = append(inputs, texts...)
				return fakeBuildEncoder()(ctx, texts)
			}
			var progress []BuildProgress
			opts.Progress = func(p BuildProgress) { progress = append(progress, p) }
			result, err := ix.Build(ctx, src, enc, buildGen, opts)
			require.NoError(t, err)
			require.NotEmpty(t, progress)
			assert.EqualValues(t, result.Fill.Chunks, progress[len(progress)-1].Done)
			assert.EqualValues(t, result.Fill.Chunks, progress[len(progress)-1].Total)
			chunks := kitvec.Split(src.rows[0].unit.Content, ix.split)
			var expected []string
			for i, chunk := range chunks {
				if i >= 2 || mode == "full rebuild" || mode == "mismatched chunks" || mode == "legacy active" {
					expected = append(expected, chunk.Text)
				}
			}
			assert.Equal(t, expected, inputs)
			if mode == "legacy active" {
				assert.Equal(t, gen.Fingerprint(), result.Fingerprint)
			}
			var count int
			require.NoError(t, ix.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_vectors_reuse`).Scan(&count))
			assert.Zero(t, count)
		})
	}
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

func TestBuildReuseMixedDocuments(t *testing.T) {
	ctx := t.Context()
	ix := openTestIndex(t)
	ix.split = kitvec.SplitOptions{MaxRunes: 10, Overlap: 2}
	src := &fakeUnitSource{}
	for i, doc := range []struct{ id, content string }{
		{"grow", "abcdefghijklmnopqrstuv"},
		{"shrink", "ABCDEFGHIJKLMNOPQRSTUV"},
		{"edit", "0123456789abcdefghijkl"},
		{"unchanged", "!@#$%^&*()_+-=[]{}|;:,"},
	} {
		src.rows = append(src.rows, fakeUnit{unit: userDoc("session", doc.id, i, doc.content), endedAt: "2024-01-01T00:00:00Z"})
	}
	gen := fakeGeneration("mixed")
	var inputs []string
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		inputs = append(inputs, texts...)
		vectors := make([][]float32, len(texts))
		for i, text := range texts {
			hash := sha256.Sum256([]byte(text))
			vectors[i] = []float32{float32(binary.LittleEndian.Uint32(hash[0:4]) & 0xffffff), float32(binary.LittleEndian.Uint32(hash[4:8]) & 0xffffff), float32(binary.LittleEndian.Uint32(hash[8:12]) & 0xffffff)}
		}
		return vectors, nil
	}
	_, err := ix.Build(ctx, src, enc, gen, BuildOptions{})
	require.NoError(t, err)
	src.rows[0].unit.Content += "wxyzABCDEFGH"
	src.rows[1].unit.Content = "ABCDEFGHIJKLM"
	src.rows[2].unit.Content = "0123456789Xbcdefghijkl"
	for i := range src.rows {
		src.rows[i].endedAt = "2025-01-01T00:00:00Z"
	}
	inputs = nil
	_, err = ix.Build(ctx, src, enc, gen, BuildOptions{BatchSize: 3})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"qrstuvwxyz", "yzABCDEFGH", "IJKLM", "89Xbcdefgh"}, inputs)
	fresh := openTestIndex(t)
	fresh.split = ix.split
	_, err = fresh.Build(ctx, src, enc, gen, BuildOptions{BatchSize: 3})
	require.NoError(t, err)
	read := func(index *Index) map[string][]byte {
		rows, err := index.db.QueryContext(ctx, `SELECT c.doc_key, c.chunk_index, v.embedding FROM message_vectors_chunks c JOIN message_vectors_v1 v ON v.rowid = c.vec_rowid`)
		require.NoError(t, err)
		defer rows.Close()
		result := make(map[string][]byte)
		for rows.Next() {
			var key string
			var n int
			var blob []byte
			require.NoError(t, rows.Scan(&key, &n, &blob))
			result[fmt.Sprintf("%s/%d", key, n)] = blob
		}
		require.NoError(t, rows.Err())
		return result
	}
	assert.Equal(t, read(fresh), read(ix))
}

func TestDeleteMirrorDocumentRemovesOnlyItsReuse(t *testing.T) {
	ix := openTestIndex(t)
	ctx := t.Context()
	_, err := ix.db.ExecContext(ctx, `INSERT INTO message_vectors_reuse VALUES (1, 'a', 'hash', x'0000803f0000000000000000'), (1, 'b', 'hash', x'0000803f0000000000000000')`)
	require.NoError(t, err)
	_, err = ix.deleteMirrorDocument(ctx, "a")
	require.NoError(t, err)
	var key string
	require.NoError(t, ix.db.QueryRowContext(ctx, `SELECT doc_key FROM message_vectors_reuse`).Scan(&key))
	assert.Equal(t, "b", key)
	plan := explainVectorPlan(t, ix, `DELETE FROM message_vectors_reuse WHERE doc_key = ?`, "a")
	assert.Contains(t, strings.Join(plan, " "), "message_vectors_reuse_doc (doc_key=?)")
}

func TestSplitPersistedChunkBoundaries(t *testing.T) {
	// These boundaries match kit v0.2.0, used when message embedding shipped.
	// Version the generation recipe before changing Split's boundaries, so old
	// vectors are not reused by position.
	chunks := kitvec.Split("abc😀efghijklmnop", kitvec.SplitOptions{MaxRunes: 6, Overlap: 2})
	assert.Equal(t, []kitvec.Chunk{{Index: 0, Text: "abc😀ef"}, {Index: 1, Text: "efghij"}, {Index: 2, Text: "ijklmn"}, {Index: 3, Text: "mnop"}}, chunks)
}

func TestReuseEncoder(t *testing.T) {
	ix := openTestIndex(t)
	ctx := t.Context()
	_, err := ix.db.ExecContext(ctx, `INSERT INTO message_vectors_reuse VALUES (1, 'a', ?, zeroblob(12)), (1, 'b', ?, x'0000803f0000000000000000'), (1, 'a', ?, zeroblob(12))`, contentHash("hit"), contentHash("hit"), contentHash("miss"))
	require.NoError(t, err)
	gen := reuseGeneration{ordinal: 1, dimension: 3}
	t.Run("invalid copies and misses", func(t *testing.T) {
		enc := ix.reuseEncoder(func(_ context.Context, texts []string) ([][]float32, error) {
			assert.Equal(t, []string{"miss"}, texts)
			return [][]float32{{0, 1, 0}}, nil
		}, gen)
		got, err := enc(ctx, []string{"hit", "miss", "hit"})
		require.NoError(t, err)
		assert.Equal(t, [][]float32{{1, 0, 0}, {0, 1, 0}, {1, 0, 0}}, got)
	})
	t.Run("all hits and wrong count", func(t *testing.T) {
		enc := ix.reuseEncoder(func(_ context.Context, texts []string) ([][]float32, error) {
			assert.Equal(t, []string{"miss"}, texts)
			return nil, nil
		}, gen)
		got, err := enc(ctx, []string{"hit", "hit"})
		require.NoError(t, err)
		assert.Equal(t, [][]float32{{1, 0, 0}, {1, 0, 0}}, got)
		_, err = enc(ctx, []string{"hit", "miss"})
		require.ErrorContains(t, err, "encoder returned 0 vectors for 1 inputs")
	})
}

func BenchmarkReuseEncoderLookup(b *testing.B) {
	for _, count := range []int{1, 20000} {
		b.Run(fmt.Sprintf("rows-%d", count), func(b *testing.B) {
			ctx := b.Context()
			ix, err := Open(ctx, filepath.Join(b.TempDir(), "vectors.db"), false, 10)
			require.NoError(b, err)
			b.Cleanup(func() { assert.NoError(b, ix.Close()) })
			_, err = ix.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x < ?)
INSERT INTO message_vectors_reuse SELECT 1, printf('doc-%d', x), printf('hash-%d', x), x'0000803f0000000000000000' FROM n`, count)
			require.NoError(b, err)
			_, err = ix.db.ExecContext(ctx, `INSERT INTO message_vectors_reuse VALUES (1, 'wanted', ?, x'0000803f0000000000000000')`, contentHash("hit"))
			require.NoError(b, err)
			enc := ix.reuseEncoder(fakeBuildEncoder(), reuseGeneration{ordinal: 1, dimension: 3})
			b.ResetTimer()
			for b.Loop() {
				if _, err := enc(ctx, []string{"hit", "miss"}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
