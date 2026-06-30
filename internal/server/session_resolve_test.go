package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveSessionIDsEndpoint(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "older-partial-match", "alpha", 2)
	te.seedSession(t, "newer-other-session", "alpha", 2)

	w := te.get(t, "/api/v1/session-ids/resolve?partial=partial&limit=10")

	assertStatus(t, w, http.StatusOK)
	resp := decode[struct {
		IDs []string `json:"ids"`
	}](t, w)
	assert.Equal(t, []string{"older-partial-match"}, resp.IDs)
}
