package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

// TestSearchContentExcludeSessionDropsEveryRepeatedID pins that every
// repeated exclude_session value is honoured, not just the first. Huma
// disables query-param explode by default and then reads a single value and
// splits it on commas, while the HTTP backend sends one exclude_session
// param per ID — so without the explode tag each ID after the first is
// silently ignored on remote-daemon searches.
func TestSearchContentExcludeSessionDropsEveryRepeatedID(t *testing.T) {
	te := setup(t)
	for _, id := range []string{"live-a", "live-b", "history"} {
		te.seedSession(t, id, "proj", 1)
		te.seedMessages(t, id, 1, func(_ int, m *db.Message) {
			m.Content = "zebra sighting in " + id
		})
	}

	search := func(t *testing.T, query string) []string {
		t.Helper()
		w := te.get(t, "/api/v1/search/content?pattern=zebra"+query)
		assertStatus(t, w, http.StatusOK)
		res := decode[service.ContentSearchResult](t, w)
		ids := make([]string, 0, len(res.Matches))
		for _, m := range res.Matches {
			ids = append(ids, m.SessionID)
		}
		return ids
	}

	require.ElementsMatch(t, []string{"live-a", "live-b", "history"},
		search(t, ""),
		"fixture must match every seeded session before exclusion")
	assert.ElementsMatch(t, []string{"history", "live-b"},
		search(t, "&exclude_session=live-a"),
		"a single exclude_session must drop only that session")
	assert.ElementsMatch(t, []string{"history"},
		search(t, "&exclude_session=live-a&exclude_session=live-b"),
		"every repeated exclude_session must be applied")
}
