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
	syncer, local, _, ctx := newSessionProvenancePushSync(t, "agentsview_dialogue_search_test")
	dbtest.SeedDialogueSearch(t, local)
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store, err := NewStore(testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	defer store.Close()
	dbtest.AssertDialogueSearchParity(t, local, store, "substring", "regex", "fts")
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
