package vector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	kitvec "go.kenn.io/kit/vector"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"hegel.dev/go/hegel"
)

type lookupFunc func(context.Context, HubLookupRequest) (HubLookupReply, error)

func (f lookupFunc) LookupHubVectors(ctx context.Context, r HubLookupRequest) (HubLookupReply, error) {
	return f(ctx, r)
}

func TestPullVectorsRoundTrip(t *testing.T) {
	ix := openTestIndex(t)
	src := &fakeUnitSource{}
	gen := fakeGeneration("hub")
	hegel.Test(t, func(ht *hegel.T) {
		bits := hegel.Draw(ht, hegel.Integers[uint32](0, math.MaxUint32))
		value := math.Float32frombits(bits)
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return
		}
		text := fmt.Sprintf("content %g", value)
		src.rows = []fakeUnit{{unit: userDoc("local", "duplicate:#%", 0, text), endedAt: "2025-01-01"}}
		expected := []float32{value, 1, -1}
		client := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
			require.Equal(ht, gen.Fingerprint(), r.Fingerprint)
			require.Equal(ht, []HubIdentity{{SessionID: "air~local", UnitKind: "user", SourceUUID: "duplicate:#%", Occurrence: 1, ContentHash: contentHash(text)}}, r.Identities)
			encoded, err := EncodeHubVector(expected)
			require.NoError(ht, err)
			return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: []HubChunk{{Index: 0, ChunkCount: 1, Vector: encoded}}}, nil
		})
		result, err := ix.Pull(t.Context(), src, client, gen, PullOptions{MapSession: func(id string) string { return "air~" + id }})
		require.NoError(ht, err)
		require.Equal(ht, gen.Fingerprint(), result.Fingerprint)
		missing, err := ix.MissingEmbeddedDocs(t.Context(), gen.Fingerprint(), nil)
		require.NoError(ht, err)
		require.Zero(ht, missing)
		exp, ok, err := ix.BeginExport(t.Context(), nil)
		require.NoError(ht, err)
		require.True(ht, ok)
		docs, _, err := exp.SessionDocs(t.Context(), "local")
		require.NoError(ht, err)
		require.Len(ht, docs, 1)
		require.Len(ht, docs[0].Chunks, 1)
		vectors := docs[0].Chunks[0].Embedding
		require.NoError(ht, exp.Close())
		require.Equal(ht, expected, vectors)
	}, hegel.WithTestCases(1000))
}

func TestPullDiscardsChangedOrDeletedDocuments(t *testing.T) {
	for _, change := range []string{"rewrite", "delete"} {
		t.Run(change, func(t *testing.T) {
			ix := openTestIndex(t)
			src := twoDocSource()
			gen := fakeGeneration("hub")
			client := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
				if change == "delete" {
					src.rows = nil
				} else {
					for i := range src.rows {
						src.rows[i].unit.Content = "rewritten"
					}
				}
				return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: []HubChunk{{Index: 0, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}, {Index: 1, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}}}, nil
			})
			_, err := ix.Pull(t.Context(), src, client, gen, PullOptions{})
			require.NoError(t, err)
			infos, err := ix.Generations(t.Context())
			require.NoError(t, err)
			require.Len(t, infos, 1)
			assert.Zero(t, infos[0].Embedded)
			if change == "rewrite" {
				assert.EqualValues(t, 2, infos[0].Missing)
				assert.Equal(t, "building", infos[0].State)
			}
		})
	}
}

func TestPullKeepsOldGenerationAndResumesAfterFailure(t *testing.T) {
	ix := openTestIndex(t)
	src := twoDocSource()
	for i := 2; i < 257; i++ {
		src.rows = append(src.rows, fakeUnit{unit: userDoc("s1", strconv.Itoa(i), i, "content"), endedAt: "2025-01-01"})
	}
	ctx := t.Context()
	old := fakeGeneration("old")
	_, err := ix.Build(ctx, src, fakeBuildEncoder(), old, BuildOptions{})
	require.NoError(t, err)
	next := fakeGeneration("new")
	interrupted := errors.New("interrupted")
	calls := 0
	client := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		calls++
		if calls == 2 {
			return HubLookupReply{}, interrupted
		}
		chunks := make([]HubChunk, len(r.Identities))
		for i := range chunks {
			chunks[i] = HubChunk{Index: i, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}
		}
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: chunks}, nil
	})
	_, err = ix.Pull(ctx, src, client, next, PullOptions{})
	require.ErrorIs(t, err, interrupted)
	require.Equal(t, 2, calls)
	missing, err := ix.MissingEmbeddedDocs(ctx, next.Fingerprint(), nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, missing)
	active, ok, err := ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, old.Fingerprint(), active)
	client = func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		require.Len(t, r.Identities, 1, "retry should request only the uncommitted batch")
		chunks := make([]HubChunk, len(r.Identities))
		for i := range chunks {
			chunks[i] = HubChunk{Index: i, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}
		}
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: chunks}, nil
	}
	result, err := ix.Pull(ctx, src, client, next, PullOptions{})
	require.NoError(t, err)
	assert.True(t, result.Activated)
	active, ok, err = ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, next.Fingerprint(), active)
}

func TestPullRejectsIncompleteOrChangedGeneration(t *testing.T) {
	for _, mode := range []string{"partial", "409", "cancel", "wrong fingerprint"} {
		t.Run(mode, func(t *testing.T) {
			ix := openTestIndex(t)
			src := twoDocSource()
			gen := fakeGeneration("hub")
			gen.Params = map[string]string{"max_input_chars": "4"}
			for i := range src.rows {
				src.rows[i].unit.Content = strings.Repeat("a", 8)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			client := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
				calls++
				if mode == "409" {
					return HubLookupReply{}, ErrHubGenerationChanged
				}
				if mode == "cancel" {
					cancel()
					return HubLookupReply{}, ctx.Err()
				}
				if calls > 1 {
					return HubLookupReply{}, errors.New("page failed")
				}
				reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: []HubChunk{{Index: 0, ChunkCount: 2, Vector: "AACAPwAAgD8AAIA/"}}, NextCursor: "next"}
				if mode == "wrong fingerprint" {
					reply.Fingerprint = "other"
				}
				return reply, nil
			})
			_, err := ix.Pull(ctx, src, client, gen, PullOptions{})
			require.Error(t, err)
			if mode == "wrong fingerprint" {
				require.ErrorIs(t, err, ErrHubGenerationChanged)
			}
			if mode == "partial" {
				require.Equal(t, 2, calls)
			}
			infos, err := ix.Generations(t.Context())
			require.NoError(t, err)
			require.Len(t, infos, 1)
			assert.Zero(t, infos[0].Embedded)
			assert.Equal(t, "building", infos[0].State)
		})
	}
}

// The in-memory source mutates only between calls; its callback is a reservation.
func (f *fakeUnitSource) WithEmbeddableSnapshot(_ context.Context, fn func(db.EmbeddableSnapshot) error) error {
	return fn(f)
}

func (f *fakeUnitSource) ScanEmbeddableUnitsForSession(ctx context.Context, id string, scope bool, limit int, fn func(db.EmbeddableUnit) error) error {
	_, err := f.ScanEmbeddableUnits(ctx, "", scope, limit, func(u db.EmbeddableUnit) error {
		if u.SessionID == id {
			return fn(u)
		}
		return nil
	})
	return err
}

type countedPullSource struct {
	*fakeUnitSource
	scans int
}

func (s *countedPullSource) ScanEmbeddableUnits(ctx context.Context, since string, scope bool, limit int, fn func(db.EmbeddableUnit) error) (string, error) {
	s.scans++
	return s.fakeUnitSource.ScanEmbeddableUnits(ctx, since, scope, limit, fn)
}

func (s *countedPullSource) WithEmbeddableSnapshot(_ context.Context, fn func(db.EmbeddableSnapshot) error) error {
	return fn(s)
}

func TestPullReconcilesEarlierBatchesOnce(t *testing.T) {
	ix := openTestIndex(t)
	src := &countedPullSource{fakeUnitSource: &fakeUnitSource{}}
	for i := range 257 {
		src.rows = append(src.rows, fakeUnit{unit: userDoc("s", strconv.Itoa(i), i, "original"), endedAt: "2025-01-01"})
	}
	calls := 0
	var first string
	hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		calls++
		if calls == 1 {
			first = r.Identities[0].SourceUUID
		} else {
			for i := range src.rows {
				if src.rows[i].unit.SourceUUID == first {
					src.rows[i].unit.Content = "changed"
				}
			}
		}
		chunks := make([]HubChunk, len(r.Identities))
		for i := range chunks {
			chunks[i] = HubChunk{Index: i, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}
		}
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: chunks}, nil
	})
	result, err := ix.Pull(t.Context(), src, hub, fakeGeneration("hub"), PullOptions{})
	require.NoError(t, err)
	require.False(t, result.Activated)
	require.Equal(t, 2, src.scans)
	infos, err := ix.Generations(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, infos[0].Missing)
}

func TestPullContinuationContracts(t *testing.T) {
	for _, mode := range []string{"valid", "empty", "late absent", "partial", "nonfinite", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ix := openTestIndex(t)
			ix.split = kitvec.SplitOptions{MaxRunes: 4}
			docs := []kitvec.Pending[string]{{Content: strings.Repeat("a", 8)}}
			ids := []HubIdentity{{SessionID: "s", ContentHash: "hash"}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
				calls++
				reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: "fp", Chunks: []HubChunk{{ChunkIndex: calls - 1, ChunkCount: 2, Vector: "AACAPwAAgD8AAIA/"}}}
				if calls == 2 {
					reply.Chunks[0].Vector = "AAAAQAAAAEAAAABA"
				}
				if calls == 1 {
					reply.NextCursor = "next"
				}
				switch mode {
				case "empty":
					reply.Chunks = nil
					reply.NextCursor = strconv.Itoa(calls)
				case "late absent":
					if calls == 2 {
						reply.Chunks = nil
						reply.Absent = []int{0}
					}
				case "partial":
					reply.NextCursor = ""
				case "nonfinite":
					reply.Chunks[0].Vector = "AACAfwAAgD8AAIA/"
				case "cancel":
					cancel()
				}
				if calls > 3 {
					return HubLookupReply{}, errors.New("unbounded pagination")
				}
				return reply, nil
			})
			vectors, _, err := ix.pullBatch(ctx, hub, "fp", 3, docs, ids)
			if mode == "valid" {
				require.NoError(t, err)
				require.Equal(t, []kitvec.ChunkVector{{ChunkIndex: 0, Vector: []float32{1, 1, 1}}, {ChunkIndex: 1, Vector: []float32{2, 2, 2}}}, vectors[0])
			} else {
				require.Error(t, err)
			}
			if mode == "empty" || mode == "cancel" {
				require.Equal(t, 1, calls)
			}
		})
	}
}

// Returning to the original build recipe must restore its document boundaries,
// even when the intervening import is interrupted during its initial reshape.
func TestPullRunCapBuildPullBuild(t *testing.T) {
	// Each case builds isolated SQLite archives; host load can exceed the
	// generator's timing budget without violating the lifecycle property.
	hegel.Test(t, func(ht *hegel.T) {
		hubCap := hegel.Draw(ht, hegel.Integers[int](1000, 5000))
		interrupted := hegel.Draw(ht, hegel.Integers[int](0, 1)) == 1
		testPullRunCapRestore(t, ht, hubCap, interrupted)
	}, hegel.WithTestCases(10), hegel.SuppressHealthCheck(hegel.TooSlow))
}

func TestPullRunCapRestore2000(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(strconv.FormatBool(interrupted), func(t *testing.T) {
			testPullRunCapRestore(t, t, 2000, interrupted)
		})
	}
}

func testPullRunCapRestore(t *testing.T, ht require.TestingT, hubCap int, interrupted bool) {
	t.Helper()
	archive := seedRunCapArchive(t, false)
	dbtest.SeedSessionWithMessages(t, archive, "newer", "project", []db.Message{dbtest.UserMsg("newer", 0, "new work")}, func(s *db.Session) { s.EndedAt = new("2024-02-01T00:00:00Z") })
	ix, err := Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"), false, 4000)
	require.NoError(ht, err)
	defer ix.Close()
	gen := fakeGeneration("local")
	gen.Params = map[string]string{"max_input_chars": "4000"}
	_, err = ix.Build(t.Context(), archive, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(ht, err)
	hubGen := fakeGeneration("hub")
	hubGen.Params = map[string]string{"max_input_chars": strconv.Itoa(hubCap)}
	var source UnitSource = archive
	if interrupted {
		source = interruptedSource{archive}
	}
	hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		absent := make([]int, len(r.Identities))
		for i := range absent {
			absent[i] = i
		}
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Absent: absent}, nil
	})
	_, err = ix.Pull(t.Context(), source, hub, hubGen, PullOptions{})
	if interrupted {
		require.Error(ht, err)
	} else {
		require.NoError(ht, err)
	}
	_, err = ix.Build(t.Context(), archive, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(ht, err)
	var documents int
	require.NoError(ht, ix.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM vector_messages WHERE session_id='session'").Scan(&documents))
	require.Equal(ht, 6, documents)
}

func TestPullClearsCompletedActiveRebuild(t *testing.T) {
	ix := openTestIndex(t)
	src, gen := twoDocSource(), fakeGeneration("hub")
	_, err := ix.Build(t.Context(), src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	stopped := errors.New("stopped rebuild")
	_, err = ix.Build(t.Context(), src, func(context.Context, []string) ([][]float32, error) { return nil, stopped }, gen, BuildOptions{FullRebuild: true})
	require.ErrorIs(t, err, stopped)
	_, err = ix.Pull(t.Context(), src, lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Absent: []int{0, 1}}, nil
	}), gen, PullOptions{})
	require.NoError(t, err)
	pending, err := ix.ActiveFullRebuildPending(t.Context(), gen.Fingerprint())
	require.NoError(t, err)
	require.True(t, pending, "an incomplete pull must preserve the rebuild marker")
	hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		chunks := make([]HubChunk, len(r.Identities))
		for i := range chunks {
			chunks[i] = HubChunk{Index: i, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}
		}
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: chunks}, nil
	})
	_, err = ix.Pull(t.Context(), src, hub, gen, PullOptions{})
	require.NoError(t, err)
	exp, ok, err := ix.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, exp.Close())
}

func TestPullStampsWhitespaceDocument(t *testing.T) {
	ix := openTestIndex(t)
	src := &fakeUnitSource{rows: []fakeUnit{{unit: userDoc("s", "blank", 0, " \t\n "), endedAt: "2024-01-01"}}}
	gen := fakeGeneration("hub")
	hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Absent: []int{0}}, nil
	})
	result, err := ix.Pull(t.Context(), src, hub, gen, PullOptions{})
	require.NoError(t, err)
	require.True(t, result.Activated)
	missing, err := ix.MissingEmbeddedDocs(t.Context(), gen.Fingerprint(), nil)
	require.NoError(t, err)
	require.Zero(t, missing)
}

func TestPullPreservesSparseChunkIndices(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		gap := hegel.Draw(ht, hegel.Integers[int](1, 20))
		testPullSparseChunks(t, ht, gap)
	}, hegel.WithTestCases(10))
}

func TestRepairAfterFailedPullUsesLocalRecipe(t *testing.T) {
	ix := openTestIndex(t)
	src := &fakeUnitSource{rows: []fakeUnit{{unit: userDoc("s", "u", 0, "abcdefgh"), endedAt: "2024-01-01"}}}
	gen := fakeGeneration("local")
	gen.Params = map[string]string{"max_input_chars": "4"}
	_, err := ix.Build(t.Context(), src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	hubGen := fakeGeneration("hub")
	hubGen.Params = map[string]string{"max_input_chars": "2"}
	stopped := errors.New("pull failed")
	_, err = ix.Pull(t.Context(), src, lookupFunc(func(context.Context, HubLookupRequest) (HubLookupReply, error) { return HubLookupReply{}, stopped }), hubGen, PullOptions{})
	require.ErrorIs(t, err, stopped)
	var seen []string
	_, err = ix.Build(t.Context(), src, recordingEncoder(&seen), gen, BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	require.Empty(t, seen, "healthy local vectors must not be rewritten using the failed hub recipe")
}

func TestPullAndBuildKeepSearchRecipeWithActiveGeneration(t *testing.T) {
	for _, mode := range []string{"pull failed", "pull incomplete", "pull cleanup failed", "build failed", "build cleanup failed", "repair retired"} {
		t.Run(mode, func(t *testing.T) {
			ix := openTestIndex(t)
			src := &fakeUnitSource{rows: []fakeUnit{{unit: userDoc("s", "u", 0, "abcdwxyz"), endedAt: "2024-01-01"}}}
			old, next := fakeGeneration("old"), fakeGeneration("next")
			old.Params = map[string]string{"max_input_chars": "4"}
			next.Params = map[string]string{"max_input_chars": "2"}
			enc := func(_ context.Context, texts []string) ([][]float32, error) {
				vectors := make([][]float32, len(texts))
				for i, text := range texts {
					vectors[i] = []float32{0, 1, 0}
					if text == "wxyz" || text == "wx" {
						vectors[i] = []float32{1, 0, 0}
					}
				}
				return vectors, nil
			}
			_, err := ix.Build(t.Context(), src, enc, old, BuildOptions{})
			require.NoError(t, err)
			stopped := errors.New("stopped")
			cleanupFailed := strings.Contains(mode, "cleanup")
			if cleanupFailed {
				require.NoError(t, ix.metaSet(t.Context(), activeFullRebuildKey, old.Fingerprint()))
				_, err = ix.db.ExecContext(t.Context(), `CREATE TRIGGER fail_activation_cleanup
BEFORE DELETE ON `+ix.spec.MetaTable+`
WHEN OLD.key = 'active_full_rebuild'
BEGIN
    SELECT RAISE(ABORT, 'injected activation cleanup failure');
END`)
				require.NoError(t, err)
			}
			var result BuildResult
			if strings.HasPrefix(mode, "build") || mode == "repair retired" {
				buildEncoder := enc
				if mode == "build failed" {
					buildEncoder = func(context.Context, []string) ([][]float32, error) { return nil, stopped }
				}
				result, err = ix.Build(t.Context(), src, buildEncoder, next, BuildOptions{})
				if mode == "repair retired" {
					require.NoError(t, err)
					_, err = ix.Build(t.Context(), src, enc, old, BuildOptions{RepairInvalid: true})
				}
			} else {
				result, err = ix.Pull(t.Context(), src, lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
					if mode == "pull failed" {
						return HubLookupReply{}, stopped
					}
					reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Absent: []int{0}}
					if cleanupFailed {
						reply.Absent = nil
						for i := range 4 {
							encoded := "AAAAAAAAgD8AAAAA"
							if i == 2 {
								encoded = "AACAPwAAAAAAAAAA"
							}
							reply.Chunks = append(reply.Chunks, HubChunk{ChunkIndex: i, ChunkCount: 4, Vector: encoded})
						}
					}
					return reply, nil
				}), next, PullOptions{})
			}
			if cleanupFailed {
				require.ErrorContains(t, err, "injected activation cleanup failure")
				require.True(t, result.Activated)
			} else if strings.HasSuffix(mode, "failed") {
				require.ErrorIs(t, err, stopped)
			} else {
				require.NoError(t, err)
			}
			want := "wxyz"
			if cleanupFailed || mode == "repair retired" {
				want = "wx"
			}
			hits, err := ix.Search(t.Context(), enc, "wxyz", 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, want, hits[0].Snippet)
		})
	}
}

func TestPullSparseChunkGapOne(t *testing.T) {
	testPullSparseChunks(t, t, 1)
}

func testPullSparseChunks(t *testing.T, ht require.TestingT, gap int) {
	t.Helper()
	ix := openTestIndex(t)
	src := &fakeUnitSource{rows: []fakeUnit{{unit: userDoc("s", "sparse", 0, "a"+strings.Repeat(" ", 4*gap+3)+"b"), endedAt: "2024-01-01"}}}
	gen := fakeGeneration("hub")
	gen.Params = map[string]string{"max_input_chars": "4"}
	calls := 0
	hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		calls++
		reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint}
		if calls == 1 {
			reply.Chunks = []HubChunk{{ChunkIndex: 0, ChunkCount: 2, Vector: "AACAPwAAgD8AAIA/"}}
			reply.NextCursor = "next"
		} else {
			reply.Chunks = []HubChunk{{ChunkIndex: gap + 1, ChunkCount: 2, Vector: "AAAAQAAAAEAAAABA"}}
		}
		return reply, nil
	})
	_, err := ix.Pull(t.Context(), src, hub, gen, PullOptions{})
	require.NoError(ht, err)
	exp, ok, err := ix.BeginExport(t.Context(), nil)
	require.NoError(ht, err)
	require.True(ht, ok)
	docs, _, err := exp.SessionDocs(t.Context(), "s")
	require.NoError(ht, err)
	require.Len(ht, docs, 1)
	require.Equal(ht, []ExportChunk{{ChunkIndex: 0, Embedding: []float32{1, 1, 1}}, {ChunkIndex: gap + 1, Embedding: []float32{2, 2, 2}}}, docs[0].Chunks)
	require.NoError(ht, exp.Close())
}
