//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestFleetBatchAndSearchMetadata(t *testing.T) {
	store := setupContentSearch(t)
	for _, id := range []string{"codex:shared", "node-a~codex:shared", "unrelated"} {
		insertCSSession(t, store, id, "app", "codex", "2026-09-01T12:00:00Z", "2026-09-01T12:01:00Z")
		insertCSMessage(t, store, id, 0, "user", "fleetneedle", "2026-09-01T12:00:00Z", false)
	}
	_, err := store.DB().Exec(`UPDATE sessions SET session_name='Provider title'`)
	require.NoError(t, err)
	page, err := store.ListSessions(t.Context(), db.SessionFilter{IDs: []string{"codex:shared"}})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 2)
	empty, err := store.ListSessions(t.Context(), db.SessionFilter{IDs: []string{}})
	require.NoError(t, err)
	assert.Empty(t, empty.Sessions)
	store.SetVectorSearcher(&hybridFakeSearcher{hits: []db.VectorHit{
		{SessionID: "codex:shared", Ordinal: 0, Score: 0.9},
		{SessionID: "node-a~codex:shared", Ordinal: 0, Score: 0.8},
		{SessionID: "unrelated", Ordinal: 0, Score: 0.7},
	}})
	for _, mode := range []string{"substring", "regex", "fts", "terms", "semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "fleetneedle", Mode: mode, Sources: []string{"messages"}, Limit: 20})
			require.NoError(t, err)
			require.Len(t, page.Matches, 3)
			for _, match := range page.Matches {
				assert.Equal(t, "test-machine", match.Machine)
				require.NotNil(t, match.DisplayName)
				assert.Equal(t, "Provider title", *match.DisplayName)
			}
		})
	}
	search, err := store.Search(t.Context(), db.SearchFilter{Query: "fleetneedle", Limit: 20})
	require.NoError(t, err)
	require.Len(t, search.Results, 3)
	for _, result := range search.Results {
		assert.Equal(t, "test-machine", result.Machine)
	}
}
