package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSPAIndex = "<!doctype html><html><head></head><body>app</body></html>"

func newSPATestServer(t *testing.T, opts ...Option) *Server {
	t.Helper()

	s := testServer(t, 0, opts...)
	assets := fstest.MapFS{
		"index.html": {
			Data: []byte(testSPAIndex),
		},
		"assets/index-abc123.js": {
			Data: []byte("console.log('app');"),
		},
	}
	s.spaFS = assets
	s.spaHandler = http.FileServerFS(assets)
	return s
}

func TestSPAIndexRequiresRevalidation(t *testing.T) {
	s := newSPATestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "no-cache", w.Header().Get("Cache-Control"))
	assert.Equal(t, testSPAIndex, w.Body.String())
}

func TestSPAFingerprintedAssetIsImmutable(t *testing.T) {
	s := newSPATestServer(t)

	req := httptest.NewRequest(
		http.MethodGet, "/assets/index-abc123.js", nil,
	)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "public, max-age=31536000, immutable",
		w.Header().Get("Cache-Control"))
	assert.Equal(t, "text/javascript; charset=utf-8",
		w.Header().Get("Content-Type"))
	assert.Equal(t, "console.log('app');", w.Body.String())
}

func TestSPAMissingAssetReturnsNotFound(t *testing.T) {
	s := newSPATestServer(t)

	req := httptest.NewRequest(
		http.MethodGet, "/assets/obsolete.js", nil,
	)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.NotContains(t, w.Body.String(), testSPAIndex)
}

func TestSPAClientRouteFallsBackToRevalidatedIndex(t *testing.T) {
	s := newSPATestServer(t, WithBasePath("/viewer"))

	req := httptest.NewRequest(
		http.MethodGet, "/viewer/sessions/example", nil,
	)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "no-cache", w.Header().Get("Cache-Control"))
	assert.Contains(t, w.Body.String(), `<base href="/viewer/">`)
}
