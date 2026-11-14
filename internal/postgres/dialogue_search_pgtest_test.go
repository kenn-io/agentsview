//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestPGDialogueSearchParity(t *testing.T) {
	t.Parallel()
	syncer, local, _, ctx := newSessionProvenancePushSync(t, "agentsview_dialogue_search_test")
	dbtest.SeedDialogueSearch(t, local)
	dbtest.SeedWordSearch(t, local)
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	defer store.Close()
	dbtest.AssertDialogueSearchParity(t, local, store, "substring", "regex", "fts")
	if !store.cockroach {
		dbtest.AssertWordSearch(t, store)
	}
	t.Run("fts ignores sources", func(t *testing.T) {
		for _, pattern := range []string{"answerneedle", "reasoningneedle"} {
			f := db.ContentSearchFilter{Pattern: pattern, Mode: "fts", Project: "dialogue-search", Sources: []string{"thinking"}, Limit: 50}
			want, err := local.SearchContent(t.Context(), f)
			require.NoError(t, err)
			got, err := store.SearchContent(t.Context(), f)
			require.NoError(t, err)
			if pattern == "answerneedle" {
				require.Len(t, want.Matches, 1)
				require.Len(t, got.Matches, 1)
				assert.Equal(t, want.Matches[0].SessionID, got.Matches[0].SessionID)
				assert.Equal(t, want.Matches[0].Ordinal, got.Matches[0].Ordinal)
				assert.Equal(t, want.Matches[0].Snippet, got.Matches[0].Snippet)
				assert.Equal(t, "message", got.Matches[0].Location)
				assert.Equal(t, "answerneedle", got.Matches[0].Snippet)
			} else {
				assert.Empty(t, want.Matches)
				assert.Empty(t, got.Matches)
			}
		}
	})
	// Terms returns an exchange rather than the helper's per-message hits.
	t.Run("terms", func(t *testing.T) {
		for _, pattern := range []string{"answerneedle", "legacyneedle", "reasoningneedle", "toolneedle", "resultneedle", "emptyneedle", "classifiedtoolneedle", "unclassifiedneedle"} {
			t.Run(pattern, func(t *testing.T) {
				f := db.ContentSearchFilter{Pattern: pattern, Mode: "terms", Project: "dialogue-search", Sources: []string{"messages"}, Limit: 50}
				want, err := local.SearchContent(t.Context(), f)
				require.NoError(t, err)
				got, err := store.SearchContent(t.Context(), f)
				require.NoError(t, err)
				if pattern != "answerneedle" && pattern != "legacyneedle" {
					assert.Empty(t, want.Matches)
					assert.Empty(t, got.Matches)
					return
				}
				require.Len(t, want.Matches, 1)
				require.Len(t, got.Matches, 1)
				assert.Equal(t, want.Matches[0].SessionID, got.Matches[0].SessionID)
				assert.Equal(t, want.Matches[0].Location, got.Matches[0].Location)
				assert.Equal(t, want.Matches[0].Ordinal, got.Matches[0].Ordinal)
				assert.Equal(t, want.Matches[0].Snippet, got.Matches[0].Snippet)
				assert.Equal(t, 0, got.Matches[0].Ordinal)
			})
		}
	})
}

func TestPGCockroachNaturalFtsCompatibility(t *testing.T) {
	t.Parallel()
	store := setupContentSearch(t)
	if !store.cockroach {
		t.Skip("CockroachDB only")
	}
	insertCSSession(t, store, "crdb-search-compat", "search-compat", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "crdb-search-compat", 0, "user",
		"invoice rounding decision", "2026-05-01T10:00:00Z", false)
	for _, filter := range []db.ContentSearchFilter{
		{Pattern: "invoice rounding decision", Mode: "fts", Sources: []string{"messages"}, Limit: 50},
		{Pattern: "invoice rounding decision", Mode: "hybrid", KeywordOnly: true, Limit: 50},
	} {
		_, err := store.SearchContent(t.Context(), filter)
		require.NoError(t, err)
	}
}
