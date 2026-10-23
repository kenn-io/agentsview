package vector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"go.kenn.io/agentsview/internal/db"
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
		src.rows = append(src.rows, fakeUnit{unit: userDoc("s1", fmt.Sprint(i), i, "content"), endedAt: "2025-01-01"})
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
	for _, mode := range []string{"partial", "409", "cancel", "wrong fingerprint", "nonfinite"} {
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
				if mode == "nonfinite" {
					reply.Chunks[0].Vector = "AACAfwAAgD8AAIA/"
				}
				return reply, nil
			})
			_, err := ix.Pull(ctx, src, client, gen, PullOptions{})
			require.Error(t, err)
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

func (f *fakeUnitSource) ScanEmbeddableUnitsForSession(ctx context.Context, id string, scope bool, cap int, fn func(db.EmbeddableUnit) error) error {
	_, err := f.ScanEmbeddableUnits(ctx, "", scope, cap, func(u db.EmbeddableUnit) error {
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

func (s *countedPullSource) ScanEmbeddableUnits(ctx context.Context, since string, scope bool, cap int, fn func(db.EmbeddableUnit) error) (string, error) {
	s.scans++
	return s.fakeUnitSource.ScanEmbeddableUnits(ctx, since, scope, cap, fn)
}

func (s *countedPullSource) WithEmbeddableSnapshot(_ context.Context, fn func(db.EmbeddableSnapshot) error) error {
	return fn(s)
}

func TestPullReconcilesEarlierBatchesOnce(t *testing.T) {
	ix := openTestIndex(t)
	src := &countedPullSource{fakeUnitSource: &fakeUnitSource{}}
	for i := 0; i < 257; i++ {
		src.rows = append(src.rows, fakeUnit{unit: userDoc("s", fmt.Sprint(i), i, "original"), endedAt: "2025-01-01"})
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
	for _, mode := range []string{"valid", "empty", "late absent", "partial", "nonfinite", "cancel", "page failure"} {
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
				if calls == 1 {
					reply.NextCursor = "next"
				}
				if calls == 2 && mode == "page failure" {
					return HubLookupReply{}, errors.New("page failed")
				}
				switch mode {
				case "empty":
					reply.Chunks = nil
					reply.NextCursor = fmt.Sprint(calls)
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
				require.Len(t, vectors[0], 2)
			} else {
				require.Error(t, err)
			}
			if mode == "empty" || mode == "cancel" {
				require.Equal(t, 1, calls)
			}
		})
	}
}
