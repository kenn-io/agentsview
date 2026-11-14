//go:build pgtest

package postgres

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPGFTSBooleanSemantics(t *testing.T) {
	t.Parallel()
	store := setupContentSearch(t)
	for _, row := range []struct{ id, body string }{
		{"a", "reader 压缩 archive"},
		{"b", "reader only"},
		{"c", "库 压缩"},
		{"d", "unrelated"},
		{"e", `reader OR literal NOT "reader"`},
		{"f", "fix bug login"},
		{"g", "fix bug logout"},
		{"h", "a foo"},
		{"i", "b c"},
		{"j", "b only"},
		{"k", "read*"},
		{"l", "read plain *"},
		{"n", "a or b"},
		{"o", "a NEAR b"},
	} {
		insertCSSession(t, store, row.id, "fixture", "codex", "2024-06-15T10:00:00Z", "2024-06-15T11:00:00Z")
		insertCSMessage(t, store, row.id, 0, "user", row.body, "2024-06-15T10:00:00Z", false)
	}
	for _, tc := range []struct {
		query string
		ids   []string
	}{
		{`"reader" OR "压缩"`, []string{"a", "b", "c", "e"}},
		{`"reader"OR"压缩"`, []string{"a", "b", "c", "e"}},
		{`"reader"AND"archive"`, []string{"a"}},
		{`"reader" OR "definitely_absent"`, []string{"a", "b", "e"}},
		{`"reader" NOT "压缩"`, []string{"b", "e"}},
		{`"fix bug" login`, []string{"f"}},
		{`"reader" ""`, []string{"a", "b", "e"}},
		{`""`, nil},
		{`NOT "reader"`, []string{"e"}},
		{`"a" OR "b" "c"`, []string{"a", "b", "d", "e", "h", "i", "k", "l", "n", "o"}},
		{`"b" AND ("only" OR "c")`, []string{"i", "j"}},
		{`"a" or "b"`, []string{"n"}},
		{`"a" NEAR "b"`, []string{"o"}},
		{`"read"*`, []string{"k"}},
		{`read*`, []string{"k"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: tc.query, Mode: "fts", Limit: 50})
			require.NoError(t, err)
			var ids []string
			for _, m := range page.Matches {
				ids = append(ids, m.SessionID)
			}
			assert.Equal(t, tc.ids, ids)
		})
	}
	t.Run(`"a" OR`, func(t *testing.T) {
		_, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: `"a" OR`, Mode: "fts"})
		var inputErr *db.SearchInputError
		require.ErrorAs(t, err, &inputErr)
	})
}

// TestPGFTSBooleanSnippetCentersOnMatchedOperand checks the fts snippet and the
// hybrid keyword window. Each matching operand sits past the opening window,
// after text that holds a non-matching operand, a negated operand, or an
// operator word.
func TestPGFTSBooleanSnippetCentersOnMatchedOperand(t *testing.T) {
	t.Parallel()
	filler := strings.Repeat("filler ", 40)
	for _, tc := range []struct {
		name, query, body, want string
	}{
		{"later OR operand", `"alpha" OR "beta"`, "Order notes " + filler + "beta closes", "beta closes"},
		{"OR phrase", `"absent" OR "fix bug"`, filler + "fix bug closes", "fix bug closes"},
		{"negated operand", `"alpha" NOT "beta" OR "gamma"`, "beta opens " + filler + "gamma closes", "gamma closes"},
		{"unsatisfied AND", `"alpha" AND "beta" OR "gamma"`, "alpha opens " + filler + "gamma closes", "gamma closes"},
	} {
		for _, mode := range []string{"fts", "hybrid"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				store := wireHybrid(t, &hybridFakeSearcher{})
				insertCSSession(t, store, "s1", "proj", "claude", hybridStart, hybridEnd)
				insertCSUnitMessage(t, store, "s1", 0, "user", tc.body, false, false)
				page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: tc.query, Mode: mode, Limit: 50})
				require.NoError(t, err)
				require.Len(t, page.Matches, 1)
				assert.Contains(t, page.Matches[0].Snippet, tc.want)
			})
		}
	}
}
