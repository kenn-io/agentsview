package vector

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// fakeSearchEncoder maps distinct known texts to orthogonal 3-dimensional
// vectors so a query for one topic scores a perfect match against its own
// document and a clear non-match against the others.
func fakeSearchEncoder() func(_ context.Context, texts []string) ([][]float32, error) {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			switch {
			case strings.Contains(text, "alpha"):
				out[i] = []float32{1, 0, 0}
			case strings.Contains(text, "beta"):
				out[i] = []float32{0, 1, 0}
			default:
				out[i] = []float32{0, 0, 1}
			}
		}
		return out, nil
	}
}

// threeDocSearchSource returns three single-topic documents in one session,
// one each for "alpha", "beta", and a third ("gamma") topic.
func threeDocSearchSource() *fakeMessageSource {
	return &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u1", Ordinal: 0, Content: "this message mentions alpha topic"},
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u2", Ordinal: 1, Content: "this message mentions beta topic"},
			endedAt: "2024-01-01T00:00:01Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u3", Ordinal: 2, Content: "this message mentions gamma topic"},
			endedAt: "2024-01-01T00:00:02Z",
		},
	}}
}

func TestSearchReturnsBestMatchFirstWithSnippet(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := threeDocSearchSource()
	gen := fakeGeneration("fake-model")

	_, err := ix.Build(ctx, src, fakeSearchEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	hits, err := ix.Search(ctx, fakeSearchEncoder(), "alpha", 10)
	require.NoError(t, err)
	require.NotEmpty(t, hits)

	best := hits[0]
	assert.Equal(t, "s1", best.SessionID)
	assert.Equal(t, 0, best.Ordinal)
	assert.InDelta(t, 1.0, best.Score, 0.01)
	assert.Contains(t, best.Snippet, "alpha")
}

func TestSearchLimitCapsResults(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := threeDocSearchSource()
	gen := fakeGeneration("fake-model")

	_, err := ix.Build(ctx, src, fakeSearchEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	hits, err := ix.Search(ctx, fakeSearchEncoder(), "alpha", 1)
	require.NoError(t, err)
	assert.Len(t, hits, 1)
}

func TestSearchNoGenerationsReturnsErrNoActiveGeneration(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	_, err := ix.Search(ctx, fakeSearchEncoder(), "alpha", 10)
	assert.ErrorIs(t, err, ErrNoActiveGeneration)
}

func TestSearchBuildingOnlyReturnsBuildingError(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := threeDocSearchSource()

	_, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)

	gen := fakeGeneration("fake-model")
	_, err = ix.EnsureGeneration(ctx, gen, sqlitevec.StateBuilding)
	require.NoError(t, err)

	_, err = ix.Search(ctx, fakeSearchEncoder(), "alpha", 10)
	require.Error(t, err)
	var buildingErr *BuildingError
	require.ErrorAs(t, err, &buildingErr)
	assert.Equal(t, 0, buildingErr.Percent, "nothing has been embedded yet")
}

// TestSearchIgnoresBuildingGenerationOfDifferentDimension covers Search's
// active-generation-only query path: while a model/dimension change is in
// progress, a building generation of a different dimension coexists with
// the active one. kitvec.Search's default (query every live generation with
// one caller-supplied encoder) would try to run the active encoder's
// 3-dimensional query vector against the building generation's 5-dimensional
// vec0 table and fail. Search must encode once for, and query only, the
// active generation, so the building generation's differing dimension never
// matters and only active-generation hits come back.
func TestSearchIgnoresBuildingGenerationOfDifferentDimension(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := threeDocSearchSource()
	activeGen := fakeGeneration("active-model")

	_, err := ix.Build(ctx, src, fakeSearchEncoder(), activeGen, BuildOptions{})
	require.NoError(t, err)

	buildingGen := kitvec.Generation{Model: "building-model", Dimensions: 5}
	_, err = ix.EnsureGeneration(ctx, buildingGen, sqlitevec.StateBuilding)
	require.NoError(t, err)

	hits, err := ix.Search(ctx, fakeSearchEncoder(), "alpha", 10)
	require.NoError(t, err, "a building generation of a different dimension must not break search")
	require.NotEmpty(t, hits)
	assert.Equal(t, "s1", hits[0].SessionID)
	assert.Equal(t, 0, hits[0].Ordinal)
	assert.InDelta(t, 1.0, hits[0].Score, 0.01)
}

func TestStaleActiveTrueWhenFingerprintsDiffer(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := threeDocSearchSource()
	gen := fakeGeneration("fake-model")

	_, err := ix.Build(ctx, src, fakeSearchEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	stale, err := ix.StaleActive(ctx, "some-other-fingerprint")
	require.NoError(t, err)
	assert.True(t, stale)

	stale, err = ix.StaleActive(ctx, gen.Fingerprint())
	require.NoError(t, err)
	assert.False(t, stale, "matching fingerprint is not stale")
}

func TestStaleActiveFalseWhenNoActiveGeneration(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	stale, err := ix.StaleActive(ctx, "anything")
	require.NoError(t, err)
	assert.False(t, stale, "no active generation means nothing to compare")
}
