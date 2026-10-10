package mcp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestSearchContent_ActiveBeforeLimit(t *testing.T) {
	ts, d := newTestToolset(t)
	for i := range 7 {
		id := fmt.Sprintf("session-%d", i)
		ended := "2024-06-15T10:00:00Z"
		if i < 3 {
			ended = "2024-06-15T11:59:00Z"
		}
		dbtest.SeedSession(t, d, id, "project-a", func(s *db.Session) {
			s.UserMessageCount = 2
			s.MessageCount = 3
			s.EndedAt = &ended
		})
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
			dbtest.UserMsg(id, 0, "docker-compose.test.yml"),
		}))
	}
	_, out, err := ts.searchContent(t.Context(), nil, searchContentIn{
		Pattern: "docker-compose.test.yml", Limit: 3,
	})
	require.NoError(t, err)
	require.Len(t, out.Matches, 3)
	assert.True(t, out.Exclusions.RecentActive)
	require.NotNil(t, out.NextCursor)
	_, page, err := ts.searchContent(t.Context(), nil, searchContentIn{
		Pattern: "docker-compose.test.yml", Limit: 3, Cursor: *out.NextCursor,
	})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Nil(t, page.NextCursor)
	ids := []string{page.Matches[0].SessionID}
	for _, match := range out.Matches {
		ids = append(ids, match.SessionID)
	}
	assert.ElementsMatch(t, []string{"session-3", "session-4", "session-5", "session-6"}, ids)
}

func TestSearchContent_ActiveFilterHTTPCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		supported, empty, retryError bool
	}{
		{name: "updated server", supported: true},
		{name: "older server"},
		{name: "older server empty page", empty: true},
		{name: "older server retry fails", retryError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			searches := 0
			lookups := map[string]int{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/search/content" {
					searches++
					q := r.URL.Query()
					assert.Equal(t, "needle", q.Get("pattern"))
					assert.Equal(t, "9", q.Get("limit"))
					assert.Equal(t, "4", q.Get("cursor"))
					if searches == 1 {
						assert.Equal(t, "2024-06-15T11:50:00Z", q.Get("exclude_active_since"))
					} else {
						assert.Empty(t, q.Get("exclude_active_since"))
					}
					if tc.supported {
						w.Header().Set("X-AgentsView-Active-Filter", "true")
						_, err := w.Write([]byte(`{"matches":[{"session_id":"idle"}],"next_cursor":13}`))
						assert.NoError(t, err)
						return
					}
					if tc.retryError && searches == 2 {
						w.WriteHeader(http.StatusInternalServerError)
						_, err := w.Write([]byte(`{"message":"search failed"}`))
						assert.NoError(t, err)
						return
					}
					if tc.empty {
						_, err := w.Write([]byte(`{"matches":[{"session_id":"active"}],"next_cursor":13}`))
						assert.NoError(t, err)
						return
					}
					_, err := w.Write([]byte(`{"matches":[{"session_id":"active"},{"session_id":"active"},{"session_id":"idle","timestamp":"2024-06-15T11:59:00Z"},{"session_id":"malformed","timestamp":"2024-06-15T11:59:00Z"},{"session_id":"unknown","timestamp":"2024-06-15T11:59:00Z"},{"session_id":"missing-active","timestamp":"2024-06-15T11:59:00Z"},{"session_id":"missing-idle","timestamp":"2024-06-15T11:50:00Z"},{"session_id":"boundary"},{"session_id":"missing-unknown","timestamp":"invalid"}],"next_cursor":13}`))
					assert.NoError(t, err)
					return
				}
				id := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
				lookups[id]++
				activity, ok := map[string]string{
					"active": "2024-06-15T11:59:00Z", "idle": "2024-06-15T10:00:00Z",
					"malformed": "invalid", "unknown": "", "boundary": "2024-06-15T11:50:00Z",
				}[id]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					_, err := w.Write([]byte(`{"message":"missing"}`))
					assert.NoError(t, err)
					return
				}
				started := "2024-06-15T11:59:00Z"
				if id == "unknown" {
					started = ""
				}
				_, err := fmt.Fprintf(w, `{"id":%q,"ended_at":%q,"started_at":%q,"created_at":""}`, id, activity, started)
				assert.NoError(t, err)
			}))
			t.Cleanup(srv.Close)
			ts := &toolset{svc: servicehttp.NewHTTPBackend(srv.URL, "", false, ""), now: func() time.Time { return fixedNow }}
			_, out, err := ts.searchContent(t.Context(), nil, searchContentIn{Pattern: "needle", Limit: 9, Cursor: 4})
			if tc.retryError {
				require.ErrorContains(t, err, "search failed")
				assert.Equal(t, 2, searches)
				assert.Empty(t, lookups)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, out.NextCursor)
			assert.Equal(t, 13, *out.NextCursor)
			assert.True(t, out.Exclusions.RecentActive)
			if tc.supported {
				assert.Equal(t, 1, searches)
				assert.Empty(t, lookups)
				assert.Zero(t, out.ExcludedActive)
				require.Len(t, out.Matches, 1)
				assert.Equal(t, "idle", out.Matches[0].SessionID)
			} else if tc.empty {
				assert.Equal(t, 2, searches)
				assert.Equal(t, 1, out.ExcludedActive)
				assert.Empty(t, out.Matches)
				assert.Equal(t, map[string]int{"active": 1}, lookups)
			} else {
				assert.Equal(t, 2, searches)
				assert.Equal(t, 3, out.ExcludedActive)
				ids := make([]string, 0, len(out.Matches))
				for _, match := range out.Matches {
					ids = append(ids, match.SessionID)
				}
				assert.ElementsMatch(t, []string{"idle", "malformed", "unknown", "missing-idle", "boundary", "missing-unknown"}, ids)
				assert.Equal(t, map[string]int{"active": 1, "idle": 1, "malformed": 1, "unknown": 1, "missing-active": 1, "missing-idle": 1, "boundary": 1, "missing-unknown": 1}, lookups)
			}
		})
	}
}
