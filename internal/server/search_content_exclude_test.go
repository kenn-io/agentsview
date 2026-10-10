package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
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

func TestSearchContentActiveFilterHTTP(t *testing.T) {
	te := setup(t)
	for _, id := range []string{"active", "idle"} {
		ended := "2024-06-15T10:00:00Z"
		if id == "active" {
			ended = "2024-06-15T11:59:00Z"
		}
		te.seedSession(t, id, "project-a", 3, func(s *db.Session) { s.EndedAt = &ended })
		te.seedMessages(t, id, 3, func(i int, m *db.Message) {
			if i == 0 {
				m.Content = "docker-compose.test.yml"
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "127.0.0.1:0"
		te.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	svc := servicehttp.NewHTTPBackend(srv.URL, "", false, "")
	page, err := svc.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "docker-compose.test.yml", Limit: 2, ExcludeActiveSince: "2024-06-15T11:50:00Z",
	})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "idle", page.Matches[0].SessionID)
}
