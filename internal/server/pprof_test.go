package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/server"
)

func TestPprofDisabledByDefault(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/debug/pprof/cmdline")

	// Without the option the path falls through to the SPA
	// handler, which never serves the pprof text/plain payload.
	assert.NotEqual(t, "text/plain; charset=utf-8",
		w.Header().Get("Content-Type"),
		"pprof must not be reachable unless enabled")
}

func TestPprofEnabledServesProfiles(t *testing.T) {
	te := setupWithServerOpts(
		t, []server.Option{server.WithPprof(true)},
	)

	w := te.get(t, "/debug/pprof/cmdline")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/plain; charset=utf-8",
		w.Header().Get("Content-Type"))

	w = te.get(t, "/debug/pprof/heap?debug=1")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "heap profile:",
		"named profiles should be served via the pprof index")
}
