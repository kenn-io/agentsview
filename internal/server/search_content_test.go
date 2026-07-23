package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
)

// TestHandleSearchContentInvalidParams covers the handler's request-validation
// rejections, which all fire before the SessionService is consulted (so a nil
// backend is fine).
func TestHandleSearchContentInvalidParams(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		query      string
		remoteAddr string
		xff        string
		wantStatus int
	}{
		{"empty pattern", "", "127.0.0.1:1234", "", http.StatusBadRequest},
		{"invalid mode", "pattern=x&mode=bad", "127.0.0.1:1234", "", http.StatusBadRequest},
		{"invalid limit", "pattern=x&limit=abc", "127.0.0.1:1234", "", http.StatusBadRequest},
		{"invalid cursor", "pattern=x&cursor=abc", "127.0.0.1:1234", "", http.StatusBadRequest},
		{"invalid timezone", "pattern=x&timezone=Fake%2FZone", "127.0.0.1:1234", "", http.StatusBadRequest},
		{"reveal from remote", "pattern=x&reveal=true", "203.0.113.5:1234", "", http.StatusForbidden},
		// A reverse proxy reaches the loopback backend, so RemoteAddr is
		// loopback; the forwarding header marks it as proxied, so reveal
		// must still be rejected.
		{"reveal via proxied loopback", "pattern=x&reveal=true", "127.0.0.1:1234", "203.0.113.5", http.StatusForbidden},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := &Server{
				cfg: config.Config{Host: "127.0.0.1"},
				mux: http.NewServeMux(),
			}
			srv.routes()
			req := httptest.NewRequest(http.MethodGet,
				"/api/v1/search/content?"+tt.query, nil)
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

// TestIsLocalhostRequest documents the security contract of the helper that
// gates secret exposure (auth token, unredacted snippets): a request is local
// only when it arrived directly on a loopback address with no reverse-proxy
// forwarding headers.
func TestIsLocalhostRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		remoteAddr string
		header     string
		value      string
		want       bool
	}{
		{"direct loopback", "127.0.0.1:1234", "", "", true},
		{"direct ipv6 loopback", "[::1]:1234", "", "", true},
		{"non-loopback", "203.0.113.5:1234", "", "", false},
		{"proxied loopback x-forwarded-for", "127.0.0.1:1234", "X-Forwarded-For", "203.0.113.5", false},
		{"proxied loopback x-real-ip", "127.0.0.1:1234", "X-Real-IP", "203.0.113.5", false},
		{"proxied loopback forwarded", "127.0.0.1:1234", "Forwarded", "for=203.0.113.5", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.header != "" {
				req.Header.Set(tt.header, tt.value)
			}
			assert.Equal(t, tt.want, isLocalhostRequest(req))
		})
	}
}
