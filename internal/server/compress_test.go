package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGzipMiddlewareCompressesAPIResponse(t *testing.T) {
	body := strings.Repeat(`{"sessions":[{"id":"s"}]}`, 80)
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(body))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sidebar-index", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	assert.Contains(t, resp.Header.Values("Vary"), "Accept-Encoding")

	gr, err := gzip.NewReader(resp.Body)
	require.NoError(t, err)
	defer gr.Close()
	got, err := io.ReadAll(gr)
	require.NoError(t, err)
	require.Equal(t, body, string(got))
}

func TestGzipMiddlewareSkipsEventStreams(t *testing.T) {
	body := strings.Repeat("event: message\ndata: {}\n\n", 80)
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()
	require.Empty(t, resp.Header.Get("Content-Encoding"))
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(got))
}

func TestGzipMiddlewareLeavesSmallAPIResponsePlain(t *testing.T) {
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Empty(t, resp.Header.Get("Content-Encoding"))
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "ok", string(got))
}
