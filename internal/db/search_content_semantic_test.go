package db

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVectorSearcher is a canned VectorSearcher for db-layer semantic search
// tests: it returns a fixed, rank-ordered slice of hits (optionally trimmed
// to limit) or a fixed error, so tests can pin the db layer's handling of
// searcher output without a real embedding index.
type fakeVectorSearcher struct {
	hits  []VectorHit
	err   error
	calls int
}

func (f *fakeVectorSearcher) SemanticSearch(
	_ context.Context, _ string, limit int,
) ([]VectorHit, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	hits := f.hits
	if limit > 0 && limit < len(hits) {
		hits = hits[:limit]
	}
	return hits, nil
}

func TestSearchContentSemanticNoSearcherUnavailable(t *testing.T) {
	d := testDB(t)
	assert.False(t, d.HasSemantic(), "HasSemantic before wiring a searcher")

	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "hello", Mode: "semantic",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSemanticUnavailable),
		"expected ErrSemanticUnavailable, got %v", err)
}

func TestHasSemanticFlipsWithSetVectorSearcher(t *testing.T) {
	d := testDB(t)
	require.False(t, d.HasSemantic(), "HasSemantic before SetVectorSearcher")

	d.SetVectorSearcher(&fakeVectorSearcher{})
	assert.True(t, d.HasSemantic(), "HasSemantic after SetVectorSearcher")

	d.SetVectorSearcher(nil)
	assert.False(t, d.HasSemantic(), "HasSemantic after clearing the searcher")
}

func TestSearchContentSemanticRoutesAndPreservesRank(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "s1", "alpha", [][2]string{
		{"user", "hello world foo"},
	})
	seedSearchSession(t, d, "s2", "beta", [][2]string{
		{"user", "another message"},
	})
	// s2 ranks first despite sorting after "s1" alphabetically, so preserved
	// order can only come from the searcher, not a re-sort by session id.
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "s2", Ordinal: 0, Score: 0.9, Snippet: "another message"},
		{SessionID: "s1", Ordinal: 0, Score: 0.5, Snippet: "hello world foo"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "hello", Mode: "semantic", Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, page.Matches, 2, "matches")

	assert.Equal(t, "s2", page.Matches[0].SessionID, "rank order: s2 first")
	assert.Equal(t, "s1", page.Matches[1].SessionID, "rank order: s1 second")

	m0 := page.Matches[0]
	assert.Equal(t, "beta", m0.Project, "Project")
	assert.Equal(t, "message", m0.Location, "Location")
	require.NotNil(t, m0.Score, "Score")
	assert.InDelta(t, 0.9, *m0.Score, 0.0001, "Score value")
	assert.Equal(t, "another message", m0.Snippet, "Snippet")
}

func TestSearchContentSemanticProjectFilterDropsNonMatching(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "s1", "alpha", [][2]string{
		{"user", "hello world foo"},
	})
	seedSearchSession(t, d, "s2", "beta", [][2]string{
		{"user", "another message"},
	})
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "s1", Ordinal: 0, Score: 0.9, Snippet: "hello world foo"},
		{SessionID: "s2", Ordinal: 0, Score: 0.5, Snippet: "another message"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "hello", Mode: "semantic", Limit: 50, Project: "alpha",
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, page.Matches, 1, "matches after project filter")
	assert.Equal(t, "s1", page.Matches[0].SessionID, "surviving session")
}

func TestSearchContentSemanticLimitTrims(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "s1", "alpha", [][2]string{{"user", "a"}})
	seedSearchSession(t, d, "s2", "alpha", [][2]string{{"user", "b"}})
	seedSearchSession(t, d, "s3", "alpha", [][2]string{{"user", "c"}})
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "s1", Ordinal: 0, Score: 0.9, Snippet: "a"},
		{SessionID: "s2", Ordinal: 0, Score: 0.8, Snippet: "b"},
		{SessionID: "s3", Ordinal: 0, Score: 0.7, Snippet: "c"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "x", Mode: "semantic", Limit: 2,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, page.Matches, 2, "matches trimmed to limit")
	assert.Equal(t, "s1", page.Matches[0].SessionID)
	assert.Equal(t, "s2", page.Matches[1].SessionID)
}

func TestSearchContentSemanticCursorRejected(t *testing.T) {
	d := testDB(t)
	d.SetVectorSearcher(&fakeVectorSearcher{})

	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "hello", Mode: "semantic", Cursor: 1,
	})
	require.Error(t, err)
	var inputErr *SearchInputError
	assert.True(t, errors.As(err, &inputErr),
		"expected *SearchInputError, got %T: %v", err, err)
}

func TestSearchContentSemanticToolInputSourceRejected(t *testing.T) {
	d := testDB(t)
	d.SetVectorSearcher(&fakeVectorSearcher{})

	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "hello", Mode: "semantic", Sources: []string{"tool_input"},
	})
	require.Error(t, err)
	var inputErr *SearchInputError
	assert.True(t, errors.As(err, &inputErr),
		"expected *SearchInputError, got %T: %v", err, err)
}

func TestSearchContentSemanticMessagesSourceAllowed(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "s1", "alpha", [][2]string{{"user", "hello"}})
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "s1", Ordinal: 0, Score: 0.9, Snippet: "hello"},
	}})

	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "hello", Mode: "semantic", Sources: []string{"messages"},
	})
	require.NoError(t, err, "SearchContent with explicit messages source")
	require.Len(t, page.Matches, 1)
}
