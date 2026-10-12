package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCodexCloudServeStatusLines(t *testing.T) {
	t.Run("one line per row", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/codex-cloud/status", r.URL.Path)
			assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"key":"k","homes":["/homes/a","/homes/b"],"state":"importing","found":3},` +
				`{"key":"/homes/c","homes":["/homes/c"],"state":"error","message":"Signed out."}]`))
		}))
		defer srv.Close()
		assert.Equal(t, []string{
			"  codex-cloud /homes/a, /homes/b: Importing, found 3 threads so far",
			"  codex-cloud /homes/c: Signed out.",
		}, codexCloudStatusLines(t.Context(), srv.URL, "secret"))
	})
	t.Run("an older daemon adds nothing", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		assert.Empty(t, codexCloudStatusLines(t.Context(), srv.URL, ""))
	})
	t.Run("a failure says so", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		assert.Equal(t, []string{"  codex-cloud status unavailable: HTTP 500"},
			codexCloudStatusLines(t.Context(), srv.URL, ""))
	})
}
