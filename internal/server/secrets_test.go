package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
)

func TestHumaScanSecretsReadOnly(t *testing.T) {
	t.Parallel()
	srv := &Server{cfg: config.Config{Host: "127.0.0.1"}} // nil engine
	srv.mux = http.NewServeMux()
	srv.routes()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/secrets/scan", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotImplemented, w.Code, "body: %s", w.Body.String())
}

// TestHandleListSecretsRevealGate verifies the endpoint rejects reveal from a
// non-localhost or proxied request before consulting the backend (so a nil
// SessionService is fine), and validates numeric params.
func TestHandleListSecretsRevealGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		query      string
		remoteAddr string
		xff        string
		wantStatus int
	}{
		{
			"reveal from remote", "reveal=true", "203.0.113.5:1234", "",
			http.StatusForbidden,
		},
		// A reverse proxy reaches the loopback backend, so RemoteAddr is
		// loopback; the forwarding header marks it proxied, so reveal must
		// still be rejected.
		{
			"reveal via proxied loopback", "reveal=true", "127.0.0.1:1234",
			"203.0.113.5", http.StatusForbidden,
		},
		{
			"invalid limit", "limit=abc", "127.0.0.1:1234", "",
			http.StatusBadRequest,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := &Server{
				cfg: config.Config{Host: "127.0.0.1"},
				mux: http.NewServeMux(),
			}
			srv.routes()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
				"/api/v1/secrets?"+tt.query, nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)
			assert.Equal(t, tt.wantStatus, w.Code, "body: %s", w.Body.String())
		})
	}
}
