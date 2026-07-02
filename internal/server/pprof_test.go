package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
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

func TestPprofRequiresBearerAuthWhenAuthEnabled(t *testing.T) {
	te := setupWithServerOpts(t,
		[]server.Option{server.WithPprof(true)},
		func(cfg *config.Config) {
			cfg.RequireAuth = true
			cfg.AuthToken = "pprof-secret"
		},
	)

	w := te.get(t, "/debug/pprof/cmdline")
	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"pprof must be gated like /api/ when require_auth is on")

	req := httptest.NewRequest(
		http.MethodGet, "/debug/pprof/cmdline", nil,
	)
	req.Header.Set("Authorization", "Bearer pprof-secret")
	rec := httptest.NewRecorder()
	te.handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/plain; charset=utf-8",
		rec.Header().Get("Content-Type"),
		"a valid bearer token must reach the pprof handler")
}

func TestPprofRejectsUnexpectedHost(t *testing.T) {
	te := setupWithServerOpts(
		t, []server.Option{server.WithPprof(true)},
	)

	req := httptest.NewRequest(
		http.MethodGet, "/debug/pprof/cmdline", nil,
	)
	req.Host = "attacker.example.net"
	rec := httptest.NewRecorder()
	te.handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"pprof must enforce the same Host allowlist as API routes")
}

func TestPprofPathStaysUngatedWhenDisabled(t *testing.T) {
	te := setup(t, func(cfg *config.Config) {
		cfg.RequireAuth = true
		cfg.AuthToken = "pprof-secret"
	})

	w := te.get(t, "/debug/pprof/cmdline")
	assert.Equal(t, http.StatusOK, w.Code,
		"with pprof disabled the path is SPA fallback and must not be auth-gated")
}
