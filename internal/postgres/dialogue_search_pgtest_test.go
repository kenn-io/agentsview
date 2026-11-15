//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestPGDialogueSearchParity(t *testing.T) {
	t.Parallel()
	syncer, local, _, ctx := newSessionProvenancePushSync(t, "agentsview_dialogue_search_test")
	dbtest.SeedDialogueSearch(t, local)
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
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

func TestPGWordSearchParity(t *testing.T) {
	t.Parallel()
	server, _ := testPGServer(t)
	if server.cockroach {
		t.Skip("PostgreSQL only")
	}
	syncer, local, _, ctx := newSessionProvenancePushSync(t, "agentsview_word_search_test")
	dbtest.SeedWordSearch(t, local)
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertWordSearch(t, store)
	dbtest.AssertExplicitMirrorFTS(t, store)
}

func TestPGWordSearchCLocale(t *testing.T) {
	t.Parallel()
	server, _ := testPGServer(t)
	if server.cockroach {
		t.Skip("PostgreSQL only")
	}
	pgURL := testPGURL(t)
	name := pgDatabaseName(t, pgURL)
	_, err := server.admin.ExecContext(t.Context(), "DROP DATABASE "+name)
	require.NoError(t, err)
	_, err = server.admin.ExecContext(t.Context(), "CREATE DATABASE "+name+" TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'")
	require.NoError(t, err)
	store := setupContentSearch(t)
	for _, tc := range []struct {
		name, body, pattern string
		want                int
	}{
		{"cjk-after", "invoice漢", "invoice absentword", 0},
		{"cjk-before", "漢invoice", "invoice absentword", 0},
		{"cjk-phrase", "invoice漢rounding", `"invoice rounding" absentword`, 0},
		{"punctuation", "invoice—rounding", `"invoice rounding" absentword`, 1},
		{"underscore", "invoice_rounding", `"invoice rounding" absentword`, 1},
		{"accent-single", "see the café résumé", "café", 1},
		{"accent-longer", "see the cafétéria menu", "café", 0},
		{"accent-literal", "café résumé", "café absentword", 1},
		{"accent-adjacent", "caféé", "café absentword", 0},
		{"accent-distinct", "cafe résumé", "café absentword", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			insertCSSession(t, store, tc.name, tc.name, "claude", "2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
			insertCSMessage(t, store, tc.name, 0, "user", tc.body, "2026-05-01T10:00:00Z", false)
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: tc.pattern, Mode: "fts", Project: tc.name, Sources: []string{"messages"}, Limit: 50})
			require.NoError(t, err)
			require.Len(t, page.Matches, tc.want)
			if tc.want > 0 {
				assert.Equal(t, tc.name, page.Matches[0].SessionID)
			}
		})
	}
}

func TestPGCockroachNaturalFtsCompatibility(t *testing.T) {
	t.Parallel()
	store := setupContentSearch(t)
	if !store.cockroach {
		t.Skip("CockroachDB only")
	}
	status, err := service.GetMemoryStatus(t.Context(), service.NewReadOnlyBackend(store))
	require.NoError(t, err)
	assert.Equal(t, "cockroach", status.Archive.Backend)
	insertCSSession(t, store, "crdb-search-compat", "search-compat", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "crdb-search-compat", 0, "user",
		"invoice records rounding before a final decision", "2026-05-01T10:00:00Z", false)
	insertCSSession(t, store, "crdb-search-partial", "search-compat", "claude",
		"2026-05-01T11:00:00Z", "2026-05-01T11:30:00Z")
	insertCSMessage(t, store, "crdb-search-partial", 0, "user",
		"invoice and rounding", "2026-05-01T11:00:00Z", false)
	for _, filter := range []db.ContentSearchFilter{
		{Pattern: "invoice rounding decision", Mode: "fts", Sources: []string{"messages"}, Limit: 50},
		{Pattern: "invoice rounding decision", Mode: "hybrid", KeywordOnly: true, Limit: 50},
	} {
		page, err := store.SearchContent(t.Context(), filter)
		require.NoError(t, err)
		require.Len(t, page.Matches, 1, "mode %s", filter.Mode)
		assert.Equal(t, "crdb-search-compat", page.Matches[0].SessionID)
	}
}
