package vector

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPullAndBuildPinSearchRecipeDuringAndAfterActivation(t *testing.T) {
	for _, method := range []string{"pull", "build"} {
		t.Run(method, func(t *testing.T) {
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
			prepared, err := ix.PrepareSearch(t.Context(), enc, "wxyz")
			require.NoError(t, err)
			checkDuring := func(ctx context.Context) error {
				hits, err := ix.Search(ctx, enc, "wxyz", 1)
				if err != nil {
					return err
				}
				if len(hits) != 1 || hits[0].Snippet != "wxyz" {
					return errors.New("active search recipe changed during operation")
				}
				return nil
			}
			if strings.HasPrefix(method, "build") {
				_, err = ix.Build(t.Context(), src, func(ctx context.Context, texts []string) ([][]float32, error) {
					if err := checkDuring(ctx); err != nil {
						return nil, err
					}
					return enc(ctx, texts)
				}, next, BuildOptions{})
			} else {
				_, err = ix.Pull(t.Context(), src, lookupFunc(func(ctx context.Context, r HubLookupRequest) (HubLookupReply, error) {
					if err := checkDuring(ctx); err != nil {
						return HubLookupReply{}, err
					}
					reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint}
					for i := range 4 {
						encoded := "AAAAAAAAgD8AAAAA"
						if i == 2 {
							encoded = "AACAPwAAAAAAAAAA"
						}
						reply.Chunks = append(reply.Chunks, HubChunk{ChunkIndex: i, ChunkCount: 4, Vector: encoded})
					}
					return reply, nil
				}), next, PullOptions{})
			}
			require.NoError(t, err)
			hits, _, err := prepared(t.Context(), 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.Equal(t, "wxyz", hits[0].Snippet, "a prepared search retains its captured generation recipe")
			hits, err = ix.Search(t.Context(), enc, "wxyz", 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.Equal(t, "wx", hits[0].Snippet, "a new search uses the activated recipe")
		})
	}
}

type pullFailingRefreshSource struct{ failingRefreshSource }

func (pullFailingRefreshSource) WithEmbeddableSnapshot(context.Context, func(db.EmbeddableSnapshot) error) error {
	return errors.New("unexpected archive snapshot after failed refresh")
}

func TestPullCanceledScopeChangeDoesNotCertifyOldScope(t *testing.T) {
	ix, gen := newBuiltTestIndex(t)
	_, err := ix.Build(t.Context(), exportTestSource(), fakeExportEncoder(), gen, BuildOptions{IncludeAutomated: true})
	require.NoError(t, err)
	_, err = ix.Pull(t.Context(), pullFailingRefreshSource{failingRefreshSource{
		unit: db.EmbeddableUnit{Kind: "user", SessionID: "session-2", SourceUUID: "u2", Deleted: true},
		err:  context.Canceled,
	}}, lookupFunc(func(context.Context, HubLookupRequest) (HubLookupReply, error) {
		return HubLookupReply{}, errors.New("unexpected lookup after failed refresh")
	}), gen, PullOptions{})
	require.ErrorIs(t, err, context.Canceled)
	exp, ok, err := ix.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, exp.Generation().IncludeAutomated, "partially reconciled scope must remain uncertified")
	require.NoError(t, exp.Close())
	_, err = ix.Build(t.Context(), exportTestSource(), fakeExportEncoder(), gen, BuildOptions{IncludeAutomated: true})
	require.NoError(t, err)
	exp, ok, err = ix.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, exp.Generation().IncludeAutomated)
	assert.True(t, *exp.Generation().IncludeAutomated)
	docs, _, err := exp.SessionDocs(t.Context(), "session-2")
	require.NoError(t, err)
	require.Len(t, docs, 1)
	assert.Equal(t, "world", docs[0].Content)
	require.NoError(t, exp.Close())
}

func TestBuildUnboundedGenerationRecipe(t *testing.T) {
	for _, maxChars := range []int{0, -1} {
		t.Run(strconv.Itoa(maxChars), func(t *testing.T) {
			ix, err := Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"), false, maxChars)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, ix.Close()) })
			src := &fakeUnitSource{rows: []fakeUnit{{unit: userDoc("s", "u", 0, "abcdwxyz"), endedAt: "2024-01-01"}}}
			_, err = ix.Build(t.Context(), src, fakeBuildEncoder(), fakeGeneration("unbounded"), BuildOptions{})
			require.NoError(t, err)
			hits, err := ix.Search(t.Context(), fakeBuildEncoder(), "query", 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.Equal(t, "abcdwxyz", hits[0].Snippet)
		})
	}
}
