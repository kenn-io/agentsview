package vector

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

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
