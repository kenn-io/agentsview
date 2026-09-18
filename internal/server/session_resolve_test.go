package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestResolveSessionIDsEndpointRawSuffix(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "host~uuid", "host", 1)
	te.seedSession(t, "host~uuid-fork", "fork", 1)

	w := te.get(t, "/api/v1/session-ids/resolve?partial=uuid&raw_suffix=true&limit=2")
	assertStatus(t, w, http.StatusOK)
	raw := decode[struct {
		IDs       []string `json:"ids"`
		RawSuffix bool     `json:"raw_suffix"`
	}](t, w)
	require.Equal(t, []string{"host~uuid"}, raw.IDs)
	assert.True(t, raw.RawSuffix)

	w = te.get(t, "/api/v1/session-ids/resolve?partial=uuid&limit=2")
	assertStatus(t, w, http.StatusOK)
	partial := decode[struct {
		IDs       []string `json:"ids"`
		RawSuffix bool     `json:"raw_suffix"`
	}](t, w)
	assert.ElementsMatch(t, []string{"host~uuid", "host~uuid-fork"}, partial.IDs)
	assert.False(t, partial.RawSuffix)
	t.Logf("head: raw_response=%+v partial_response=%+v", raw, partial)
}
