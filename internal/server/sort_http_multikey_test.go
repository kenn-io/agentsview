package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/db"
)

// sessionListPage decodes the list response including the pagination cursor,
// which sessionListResponse omits.
type sessionListPage struct {
	Sessions   []db.Session `json:"sessions"`
	NextCursor string       `json:"next_cursor"`
	Total      int          `json:"total"`
}

// seedForMultiKey inserts three sessions whose (messages, started) values make
// a two-key sort with mixed directions observable.
func seedForMultiKey(t *testing.T, te *testEnv) {
	t.Helper()
	te.seedSession(t, "mk-a", "p", 1, func(s *db.Session) {
		s.StartedAt = new("2024-03-01T00:00:00Z")
	})
	te.seedSession(t, "mk-b", "p", 1, func(s *db.Session) {
		s.StartedAt = new("2024-01-01T00:00:00Z")
	})
	te.seedSession(t, "mk-c", "p", 2, func(s *db.Session) {
		s.StartedAt = new("2024-02-01T00:00:00Z")
	})
}

// TestListSessions_OrderBy_MultiKey exercises the comma-separated key:dir spec
// and the descending fallback for unsuffixed keys over HTTP.
func TestListSessions_OrderBy_MultiKey(t *testing.T) {
	t.Run("explicit per-key directions", func(t *testing.T) {
		te := setup(t)
		seedForMultiKey(t, te)
		// messages asc, then started desc.
		w := te.get(t, "/api/v1/sessions?order_by=messages:asc,started:desc")
		assertStatus(t, w, http.StatusOK)
		resp := decode[sessionListResponse](t, w)
		assert.Equal(t, []string{"mk-a", "mk-b", "mk-c"}, orderedSessionIDs(resp.Sessions))
	})

	t.Run("descending fallback fills unsuffixed key", func(t *testing.T) {
		te := setup(t)
		seedForMultiKey(t, te)
		// messages (bare -> descending via param), then started:asc explicit.
		w := te.get(t, "/api/v1/sessions?order_by=messages,started:asc&descending=true")
		assertStatus(t, w, http.StatusOK)
		resp := decode[sessionListResponse](t, w)
		assert.Equal(t, []string{"mk-c", "mk-b", "mk-a"}, orderedSessionIDs(resp.Sessions))
	})
}

// TestListSessions_OrderBy_MultiKeyPagination walks a multi-key sort one page at
// a time and asserts the keyset cursor reproduces the full-listing order.
func TestListSessions_OrderBy_MultiKeyPagination(t *testing.T) {
	te := setup(t)
	seedForMultiKey(t, te)

	full := te.get(t, "/api/v1/sessions?order_by=messages:asc,started:desc")
	assertStatus(t, full, http.StatusOK)
	want := orderedSessionIDs(decode[sessionListPage](t, full).Sessions)

	var got []string
	cursor := ""
	for i := 0; i < len(want)+1; i++ {
		url := "/api/v1/sessions?order_by=messages:asc,started:desc&limit=1"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		w := te.get(t, url)
		assertStatus(t, w, http.StatusOK)
		resp := decode[sessionListPage](t, w)
		got = append(got, orderedSessionIDs(resp.Sessions)...)
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	assert.Equal(t, want, got)
}

// TestListSessions_OrderBy_Invalid rejects malformed order_by specs with 400.
func TestListSessions_OrderBy_Invalid(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "p", 3)

	for _, spec := range []string{
		"bogus",             // unknown key
		"messages,bogus",    // unknown key in list
		"messages:up",       // bad direction token
		"messages,messages", // duplicate key
		"messages,,started", // empty term
	} {
		t.Run(spec, func(t *testing.T) {
			w := te.get(t, "/api/v1/sessions?order_by="+spec)
			assertStatus(t, w, http.StatusBadRequest)
		})
	}
}

// TestSidebarIndex_OrderByInvalid confirms the sidebar-index route, which shares
// the session-filter input struct, also rejects a malformed order_by rather than
// silently accepting and ignoring it (the dropped enum used to guard both routes).
func TestSidebarIndex_OrderByInvalid(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "p", 3)

	w := te.get(t, "/api/v1/sessions/sidebar-index?order_by=bogus")
	assertStatus(t, w, http.StatusBadRequest)

	// A valid spec is still accepted even though the sidebar applies its own order.
	w = te.get(t, "/api/v1/sessions/sidebar-index?order_by=messages:desc")
	assertStatus(t, w, http.StatusOK)
}
