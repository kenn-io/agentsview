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
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestSearchContent_ActiveFilterHTTPCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name              string
		empty, retryError bool
	}{
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
					if searches == 1 {
						assert.Equal(t, "2024-06-15T11:50:00Z", q.Get("exclude_active_since"))
					} else {
						assert.Empty(t, q.Get("exclude_active_since"))
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
			if tc.empty {
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
