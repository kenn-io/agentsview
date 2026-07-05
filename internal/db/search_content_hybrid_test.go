package db

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSearchContentHybridNoSearcherUnavailable pins the same capability gate
// as "semantic": hybrid needs a wired VectorSearcher regardless of FTS
// availability, and reports ErrSemanticUnavailable when none is wired.
func TestSearchContentHybridNoSearcherUnavailable(t *testing.T) {
	d := testDB(t)
	assert.False(t, d.HasSemantic(), "HasSemantic before wiring a searcher")

	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "hello", Mode: "hybrid",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSemanticUnavailable),
		"expected ErrSemanticUnavailable, got %v", err)
}

// TestSearchContentHybridCursorRejected pins the shared semantic/hybrid
// validation: cursor pagination is rejected before the capability check.
func TestSearchContentHybridCursorRejected(t *testing.T) {
	d := testDB(t)
	d.SetVectorSearcher(&fakeVectorSearcher{})

	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "hello", Mode: "hybrid", Cursor: 1,
	})
	require.Error(t, err)
	var inputErr *SearchInputError
	assert.True(t, errors.As(err, &inputErr),
		"expected *SearchInputError, got %T: %v", err, err)
}

// TestSearchContentHybridFTSUnavailable pins the FTS-missing capability gate:
// hybrid additionally requires db.HasFTS() and mirrors mode "fts"'s error
// when the messages_fts table is gone.
func TestSearchContentHybridFTSUnavailable(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	d.SetVectorSearcher(&fakeVectorSearcher{})
	_, err := d.getWriter().Exec("DROP TABLE IF EXISTS messages_fts")
	require.NoError(t, err, "drop messages_fts")

	_, err = d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "hello", Mode: "hybrid",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errFTSUnavailable),
		"expected errFTSUnavailable, got %v", err)
}

// TestSearchContentHybridBothLegsOutrankSingleLeg pins reciprocal-rank
// fusion's core guarantee: a document ranked top by both the vector and FTS
// legs must fuse to a higher score than a document appearing in only one leg,
// and must sort first.
func TestSearchContentHybridBothLegsOutrankSingleLeg(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "both", "proj", [][2]string{
		{"user", "needle in a haystack"},
	})
	seedSearchSession(t, d, "vec-only", "proj", [][2]string{
		{"user", "totally unrelated content"},
	})
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "both", Ordinal: 0, Score: 0.9, Snippet: "needle in a haystack"},
		{SessionID: "vec-only", Ordinal: 0, Score: 0.8, Snippet: "totally unrelated content"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "needle", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 2, "matches")

	require.Equal(t, "both", page.Matches[0].SessionID, "double-leg hit ranks first")
	require.Equal(t, "vec-only", page.Matches[1].SessionID, "single-leg hit ranks second")
	require.NotNil(t, page.Matches[0].Score, "Score")
	require.NotNil(t, page.Matches[1].Score, "Score")
	assert.Greater(t, *page.Matches[0].Score, *page.Matches[1].Score,
		"double-leg fused score must exceed single-leg fused score")
}

// TestSearchContentHybridVectorOnlyAndFTSOnlyBothAppear pins RRF's union
// semantics: a hit found only by the vector leg and a hit found only by the
// FTS leg must both survive the fusion, not just the leg-overlapping ones.
func TestSearchContentHybridVectorOnlyAndFTSOnlyBothAppear(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "fts-only", "proj", [][2]string{
		{"user", "needle in a haystack"},
	})
	seedSearchSession(t, d, "vec-only", "proj", [][2]string{
		{"user", "totally unrelated content"},
	})
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "vec-only", Ordinal: 0, Score: 0.9, Snippet: "totally unrelated content"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "needle", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 2, "matches")

	var ids []string
	for _, m := range page.Matches {
		ids = append(ids, m.SessionID)
	}
	assert.ElementsMatch(t, []string{"fts-only", "vec-only"}, ids)
}

// TestSearchContentHybridScoresStrictlyDescending pins that fused RRF scores
// order the page strictly descending, with no inversions or unexpected ties
// across a mix of a double-leg hit and single-leg (vector-only) hits.
func TestSearchContentHybridScoresStrictlyDescending(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "both", "proj", [][2]string{
		{"user", "needle in a haystack"},
	})
	seedSearchSession(t, d, "vec2", "proj", [][2]string{
		{"user", "other stuff entirely"},
	})
	seedSearchSession(t, d, "vec3", "proj", [][2]string{
		{"user", "yet more stuff"},
	})
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "both", Ordinal: 0, Score: 0.9, Snippet: "needle in a haystack"},
		{SessionID: "vec2", Ordinal: 0, Score: 0.8, Snippet: "other stuff entirely"},
		{SessionID: "vec3", Ordinal: 0, Score: 0.7, Snippet: "yet more stuff"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "needle", Mode: "hybrid", Limit: 50,
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 3, "matches")

	for i := 1; i < len(page.Matches); i++ {
		require.NotNil(t, page.Matches[i-1].Score, "Score at %d", i-1)
		require.NotNil(t, page.Matches[i].Score, "Score at %d", i)
		assert.Greater(t, *page.Matches[i-1].Score, *page.Matches[i].Score,
			"scores must be strictly descending at index %d", i)
	}
}

// TestSearchContentHybridProjectFilterConstrainsBothLegs pins that the
// session-scope filter narrows both legs: the FTS leg filters in SQL, the
// vector leg is filtered post-hoc before the merge, and a session outside the
// requested project must be dropped from the fused result even though it
// matches both legs' raw searches.
func TestSearchContentHybridProjectFilterConstrainsBothLegs(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "in-scope", "alpha", [][2]string{
		{"user", "needle in a haystack"},
	})
	seedSearchSession(t, d, "out-of-scope", "beta", [][2]string{
		{"user", "needle in another haystack"},
	})
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "in-scope", Ordinal: 0, Score: 0.9, Snippet: "needle in a haystack"},
		{SessionID: "out-of-scope", Ordinal: 0, Score: 0.8, Snippet: "needle in another haystack"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "needle", Mode: "hybrid", Limit: 50, Project: "alpha",
	})
	require.NoError(t, err, "SearchContent hybrid")
	require.Len(t, page.Matches, 1, "matches after project filter")
	assert.Equal(t, "in-scope", page.Matches[0].SessionID, "surviving session")
}
