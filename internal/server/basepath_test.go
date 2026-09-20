package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBasePath_StripsPrefixForAPI(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/app"))

	req := httptest.NewRequestWithContext(t.Context(), "GET", "/app/api/v1/sessions", nil)
	req.Host = "127.0.0.1:0"
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	// 200 or 503 (timeout) both confirm the route was matched
	// and prefix was stripped. 404 or 403 would indicate a
	// base-path routing failure.
	require.NotEqual(t, http.StatusNotFound, w.Code,
		"GET /app/api/v1/sessions = %d, want route match; body: %s",
		w.Code, w.Body.String())
	require.NotEqual(t, http.StatusForbidden, w.Code,
		"GET /app/api/v1/sessions = %d, want route match; body: %s",
		w.Code, w.Body.String())
}

func TestBasePath_RedirectsBarePrefix(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/app"))

	req := httptest.NewRequestWithContext(t.Context(), "GET", "/app", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusMovedPermanently, w.Code)
	require.Equal(t, "/app/", w.Header().Get("Location"))
}

func TestBasePath_InjectsBaseHrefIntoHTML(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/viewer"))

	req := httptest.NewRequestWithContext(t.Context(), "GET", "/viewer/", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `<base href="/viewer/">`,
		"missing <base href> tag in response")
}

func TestBasePath_RewritesAssetPaths(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/viewer"))

	req := httptest.NewRequestWithContext(t.Context(), "GET", "/viewer/", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	body := w.Body.String()

	// Asset paths should be prefixed.
	assert.NotContains(t, body, `src="/assets/`,
		"found unprefixed src=\"/assets/ in HTML")
	assert.NotContains(t, body, `href="/assets/`,
		"found unprefixed href=\"/assets/ in HTML")
	assert.NotContains(t, body, `href="/favicon`,
		"found unprefixed href=\"/favicon in HTML")

	// External URLs must NOT be prefixed.
	assert.NotContains(t, body, `href="/viewer/https://`,
		"external URL was incorrectly prefixed")
}

func TestBasePath_SPAFallbackServesIndex(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/app"))

	// A non-existent path should fall back to index.html
	// with the base tag injected.
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/app/some/route", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `<base href="/app/">`,
		"SPA fallback missing <base href> tag")
}

func TestBasePath_RejectsSiblingPath(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/app"))

	// /appfoo should NOT be handled — only /app or /app/...
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/appfoo/bar", nil)
	req.Host = "127.0.0.1:0"
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestBasePath_TrailingSlashNormalized(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/app/"))

	// WithBasePath trims trailing slash.
	require.Equal(t, "/app", s.basePath)
}
