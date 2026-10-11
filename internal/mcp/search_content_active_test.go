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
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestSearchContent_ActivityCutoffPagination(t *testing.T) {
	ts, d := newTestToolset(t)
	now := fixedNow
	ts.now = func() time.Time { return now }
	for _, f := range []struct{ id, end string }{
		{"X", "2024-06-15T11:50:01Z"},
		{"A", "2024-06-15T11:40:00Z"},
		{"B", "2024-06-15T11:30:00Z"},
		{"C", "2024-06-15T11:20:00Z"},
	} {
		dbtest.SeedSession(t, d, f.id, "project-a", func(s *db.Session) {
			s.MessageCount = 2
			s.UserMessageCount = 2
			s.EndedAt = new(f.end)
		})
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
			dbtest.UserMsg(f.id, 0, "needle"),
			dbtest.UserMsg(f.id, 1, "unrelated"),
		}))
	}
	_, first, err := ts.searchContent(t.Context(), nil, searchContentIn{Pattern: "needle", Limit: 2})
	require.NoError(t, err)
	require.Len(t, first.Matches, 2)
	assert.Equal(t, "A", first.Matches[0].SessionID)
	assert.Equal(t, "B", first.Matches[1].SessionID)
	assert.Equal(t, "2024-06-15T11:50:00Z", first.ActivityCutoff)
	require.NotNil(t, first.NextCursor)
	assert.Equal(t, 2, *first.NextCursor)
	now = fixedNow.Add(2 * time.Second)
	_, second, err := ts.searchContent(t.Context(), nil, searchContentIn{
		Pattern: "needle", Limit: 2, Cursor: *first.NextCursor, ActivityCutoff: first.ActivityCutoff,
	})
	require.NoError(t, err)
	require.Len(t, second.Matches, 1)
	assert.Equal(t, "C", second.Matches[0].SessionID)
	assert.Equal(t, "2024-06-15T11:50:00Z", second.ActivityCutoff)
	assert.Nil(t, second.NextCursor)
}

func TestSearchContent_ActivityCutoffContinuationInputs(t *testing.T) {
	const missing = "activity_cutoff is required with a positive cursor: restart search_content without a cursor, then pass both next_cursor as cursor and activity_cutoff"
	const malformed = "invalid activity_cutoff: use RFC3339"
	const future = "activity_cutoff must not be later than the recent-activity boundary: restart search_content without a cursor or activity_cutoff"
	for _, tc := range []struct {
		name    string
		in      searchContentIn
		wantErr string
		cutoff  string
	}{
		{"default first page", searchContentIn{}, "", "2024-06-15T11:50:00Z"},
		{"blank current session first page", searchContentIn{CurrentSessionID: "   "}, "", "2024-06-15T11:50:00Z"},
		{"include_one_shot", searchContentIn{IncludeOneShot: true}, "", "2024-06-15T11:50:00Z"},
		{"missing", searchContentIn{Cursor: 2}, missing, ""},
		{"blank current session", searchContentIn{Cursor: 2, CurrentSessionID: "   "}, missing, ""},
		{"malformed continuation", searchContentIn{Cursor: 2, ActivityCutoff: "invalid"}, malformed, ""},
		{"date only", searchContentIn{Cursor: 2, ActivityCutoff: "2024-06-15"}, malformed, ""},
		{"malformed include active", searchContentIn{Cursor: 2, IncludeActive: true, ActivityCutoff: "invalid"}, malformed, ""},
		{"malformed current session", searchContentIn{Cursor: 2, CurrentSessionID: "current", ActivityCutoff: "invalid"}, malformed, ""},
		{"future boundary", searchContentIn{Cursor: 2, ActivityCutoff: "2024-06-15T11:50:00.000000001Z"}, future, ""},
		{"exact boundary", searchContentIn{Cursor: 2, ActivityCutoff: "2024-06-15T11:50:00Z"}, "", "2024-06-15T11:50:00Z"},
		{"fractional continuation", searchContentIn{Cursor: 2, ActivityCutoff: "2024-06-15T11:49:59.123456789Z"}, "", "2024-06-15T11:49:59.123456789Z"},
		{"include active", searchContentIn{Cursor: 2, IncludeActive: true}, "", ""},
		{"current session", searchContentIn{Cursor: 2, CurrentSessionID: "current"}, "", ""},
		{"include active ignores cutoff", searchContentIn{Cursor: 2, IncludeActive: true, ActivityCutoff: "2024-06-15T11:50:00.000000001Z"}, "", ""},
		{"current session ignores cutoff", searchContentIn{Cursor: 2, CurrentSessionID: "current", ActivityCutoff: "2024-06-15T11:50:00.000000001Z"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeContentSearchService{result: &service.ContentSearchResult{ActiveFilterApplied: true}}
			ts := &toolset{svc: fake, now: func() time.Time { return fixedNow }}
			in := tc.in
			in.Pattern = "needle"
			_, out, err := ts.searchContent(t.Context(), nil, in)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				assert.Empty(t, fake.lastReq.Pattern)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, in.Cursor, fake.lastReq.Cursor)
			assert.Equal(t, tc.cutoff, fake.lastReq.ExcludeActiveSince)
			assert.Equal(t, tc.cutoff, out.ActivityCutoff)
			assert.Equal(t, tc.cutoff != "", out.Exclusions.RecentActive)
			if in.CurrentSessionID != "current" {
				assert.Empty(t, fake.lastReq.ExcludeSessionIDs)
				assert.Empty(t, out.Exclusions.CurrentSessionID)
			}
		})
	}
}

func TestSearchContent_ActiveFilterHTTPCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name      string
		supported bool
	}{
		{name: "older server"},
		{name: "server applies activity filter", supported: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			searches := 0
			lookups := map[string]int{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/search/content" {
					searches++
					q := r.URL.Query()
					assert.Equal(t, "4", q.Get("cursor"))
					assert.Equal(t, "2024-06-15T11:50:00Z", q.Get("exclude_active_since"))
					if tc.supported {
						_, err := w.Write([]byte(`{"active_filter_applied":true,"matches":[{"session_id":"idle","timestamp":"2024-06-15T11:59:00Z"}],"next_cursor":13}`))
						assert.NoError(t, err)
						return
					}
					_, err := w.Write([]byte(`{"matches":[{"session_id":"active"},{"session_id":"active"},{"session_id":"idle","timestamp":"2024-06-15T11:59:00Z"},{"session_id":"malformed","timestamp":"2024-06-15T11:59:00Z"},{"session_id":"missing-active","timestamp":"2024-06-15T11:59:00Z"},{"session_id":"missing-idle","timestamp":"2024-06-15T11:50:00Z"},{"session_id":"boundary"},{"session_id":"crossing"}],"next_cursor":13}`))
					assert.NoError(t, err)
					return
				}
				id := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
				lookups[id]++
				activity, ok := map[string]string{
					"active": "2024-06-15T11:59:00Z", "idle": "2024-06-15T10:00:00Z",
					"malformed": "invalid", "boundary": "2024-06-15T11:50:00Z",
					"crossing": "2024-06-15T11:50:01Z",
				}[id]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					_, err := w.Write([]byte(`{"message":"missing"}`))
					assert.NoError(t, err)
					return
				}
				started := "2024-06-15T11:59:00Z"
				_, err := fmt.Fprintf(w, `{"id":%q,"ended_at":%q,"started_at":%q,"created_at":""}`, id, activity, started)
				assert.NoError(t, err)
			}))
			t.Cleanup(srv.Close)
			ts := &toolset{svc: servicehttp.NewHTTPBackend(srv.URL, "", false, ""), now: func() time.Time { return fixedNow.Add(2 * time.Second) }}
			_, out, err := ts.searchContent(t.Context(), nil, searchContentIn{Pattern: "needle", Limit: 8, Cursor: 4, ActivityCutoff: "2024-06-15T11:50:00Z"})
			require.NoError(t, err)
			assert.Equal(t, "2024-06-15T11:50:00Z", out.ActivityCutoff)
			require.NotNil(t, out.NextCursor)
			assert.Equal(t, 13, *out.NextCursor)
			assert.True(t, out.Exclusions.RecentActive)
			assert.Equal(t, 1, searches)
			if tc.supported {
				require.Len(t, out.Matches, 1)
				assert.Equal(t, "idle", out.Matches[0].SessionID)
				assert.Zero(t, out.ExcludedActive)
				assert.Empty(t, lookups)
				return
			}
			assert.Equal(t, 4, out.ExcludedActive)
			ids := make([]string, 0, len(out.Matches))
			for _, match := range out.Matches {
				ids = append(ids, match.SessionID)
			}
			assert.ElementsMatch(t, []string{"idle", "malformed", "missing-idle", "boundary"}, ids)
			assert.Equal(t, map[string]int{"active": 1, "idle": 1, "malformed": 1, "missing-active": 1, "missing-idle": 1, "boundary": 1, "crossing": 1}, lookups)
		})
	}
}
