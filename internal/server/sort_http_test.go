package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/db"
)

func orderedSessionIDs(sessions []db.Session) []string {
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	return ids
}

// TestListSessions_OrderBy exercises the ?order_by / ?descending query params
// and rejection of an out-of-enum value.
func TestListSessions_OrderBy(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s-lo", "p", 2)
	te.seedSession(t, "s-hi", "p", 9)

	// order_by=messages defaults to ascending.
	w := te.get(t, "/api/v1/sessions?order_by=messages")
	assertStatus(t, w, http.StatusOK)
	resp := decode[sessionListResponse](t, w)
	assert.Equal(t, []string{"s-lo", "s-hi"}, orderedSessionIDs(resp.Sessions))

	// descending=true flips the order.
	w = te.get(t, "/api/v1/sessions?order_by=messages&descending=true")
	assertStatus(t, w, http.StatusOK)
	resp = decode[sessionListResponse](t, w)
	assert.Equal(t, []string{"s-hi", "s-lo"}, orderedSessionIDs(resp.Sessions))

	// An out-of-enum order_by is rejected (Huma 422 remapped to 400).
	w = te.get(t, "/api/v1/sessions?order_by=bogus")
	assertStatus(t, w, http.StatusBadRequest)
}
