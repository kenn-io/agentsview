package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestContentTypeWrapper verifies that Content-Type is only set if missing
// when the status code matches the trigger status.
func TestContentTypeWrapper(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		handler         http.HandlerFunc
		triggerStatus   int
		wantStatus      int
		wantContentType string
		wantBody        string
	}{
		{
			name: "SetsContentTypeOnTriggerStatusMissingHeader",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(`{"error":"timeout"}`))
			},
			triggerStatus:   http.StatusServiceUnavailable,
			wantStatus:      http.StatusServiceUnavailable,
			wantContentType: "application/json",
			wantBody:        `{"error":"timeout"}`,
		},
		{
			name: "RespectsExistingContentTypeOnTriggerStatus",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte("timeout error"))
			},
			triggerStatus:   http.StatusServiceUnavailable,
			wantStatus:      http.StatusServiceUnavailable,
			wantContentType: "text/plain",
			wantBody:        "timeout error",
		},
		{
			name: "IgnoresNonTriggerStatus",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("ok"))
			},
			triggerStatus:   http.StatusServiceUnavailable,
			wantStatus:      http.StatusOK,
			wantContentType: "", // Not set by wrapper
			wantBody:        "ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			wrapper := &contentTypeWrapper{
				ResponseWriter: w,
				contentType:    "application/json",
				triggerStatus:  tt.triggerStatus,
			}

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			tt.handler(wrapper, req)

			assertRecorderStatus(t, w, tt.wantStatus)

			resp := w.Result()
			defer resp.Body.Close()

			gotCT := resp.Header.Get("Content-Type")
			if tt.wantContentType != "" {
				if gotCT != tt.wantContentType {
					t.Errorf("Content-Type = %q, want %q", gotCT, tt.wantContentType)
				}
			} else if gotCT == "application/json" {
				// Wrapper shouldn't improperly force its Content-Type
				t.Errorf("Content-Type = %q, unexpectedly forced by wrapper", gotCT)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("failed to read body: %v", err)
			}
			if string(body) != tt.wantBody {
				t.Errorf("body = %q, want %q", string(body), tt.wantBody)
			}
		})
	}
}

// TestMiddlewareTimeout verifies that API routes are wrapped with timeout
// middleware (which produces a 503 JSON timeout response when the handler
// exceeds the configured duration) and that export/SPA routes are NOT wrapped.
func TestMiddlewareTimeout(t *testing.T) {
	t.Parallel()

	srv := testServer(
		t, 10*time.Millisecond,
		withHandlerDelay(100*time.Millisecond),
	)
	// Use a real listener to discover the bound port, then
	// rebuild Handler() with the correct port in the Host
	// allowlist.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv.SetPort(port)
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)

	tests := []struct {
		name        string
		path        string
		wantTimeout bool
		wantStatus  int // Only checked if wantTimeout is false
	}{
		{"Wrapped_ListSessions", "/api/v1/sessions", true, 0},
		{"Wrapped_GetStats", "/api/v1/stats", true, 0},
		{"Unwrapped_ExportSession", "/api/v1/sessions/invalid-id/export", false, http.StatusNotFound},
		{"Unwrapped_SPA", "/", false, http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resp, err := ts.Client().Get(ts.URL + tt.path)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if tt.wantTimeout {
				assertTimeoutResponse(t, resp)
			} else {
				if isTimeoutResponse(t, resp) {
					t.Errorf("%s: unexpected timeout for unwrapped route", tt.path)
				}
				if resp.StatusCode != tt.wantStatus {
					t.Errorf("%s: status = %d, want %d", tt.path, resp.StatusCode, tt.wantStatus)
				}
			}
		})
	}
}

// parseCSP splits a Content-Security-Policy string into a map of
// directive name -> source list.
func parseCSP(csp string) map[string]string {
	out := map[string]string{}
	for part := range strings.SplitSeq(csp, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, sources, _ := strings.Cut(part, " ")
		out[name] = strings.TrimSpace(sources)
	}
	return out
}

func TestCSPMiddlewareSetsHeaderOnNonAPIRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		host     string
		port     int
		basePath string
		wantCSP  bool
		// wantDirectives maps a directive name to its exact expected
		// source list. Resource directives stay pinned to the server
		// origin; connect-src is widened to any http/https/ws/wss.
		wantDirectives map[string]string
	}{
		{
			name:    "SPA root pins origin and widens connect-src",
			path:    "/",
			host:    "127.0.0.1",
			port:    8081,
			wantCSP: true,
			wantDirectives: map[string]string{
				"default-src":     "'self' http://127.0.0.1:8081",
				"script-src":      "'self' http://127.0.0.1:8081",
				"connect-src":     "'self' http: https: ws: wss:",
				"img-src":         "'self' http://127.0.0.1:8081 data:",
				"style-src":       "'self' http://127.0.0.1:8081 'unsafe-inline' https://fonts.googleapis.com",
				"font-src":        "'self' http://127.0.0.1:8081 data: https://fonts.gstatic.com",
				"object-src":      "'none'",
				"base-uri":        "'none'",
				"frame-ancestors": "'none'",
			},
		},
		{
			name:    "IPv6 loopback brackets pinned origin",
			path:    "/sessions/abc",
			host:    "::1",
			port:    9090,
			wantCSP: true,
			wantDirectives: map[string]string{
				"script-src":  "'self' http://[::1]:9090",
				"connect-src": "'self' http: https: ws: wss:",
			},
		},
		{
			name:     "base path relaxes base-uri to self",
			path:     "/app/",
			host:     "127.0.0.1",
			port:     8081,
			basePath: "/app",
			wantCSP:  true,
			wantDirectives: map[string]string{
				"connect-src": "'self' http: https: ws: wss:",
				"base-uri":    "'self'",
			},
		},
		{
			name:    "API route gets no CSP",
			path:    "/api/v1/sessions",
			host:    "127.0.0.1",
			port:    8081,
			wantCSP: false,
		},
		{
			name:    "API subpath gets no CSP",
			path:    "/api/v1/stats",
			host:    "127.0.0.1",
			port:    8081,
			wantCSP: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			handler := cspMiddleware(tt.host, tt.port, tt.basePath, inner)

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			csp := w.Header().Get("Content-Security-Policy")
			if !tt.wantCSP {
				if csp != "" {
					t.Errorf("expected no CSP header on API route, got %q", csp)
				}
				return
			}
			if csp == "" {
				t.Fatal("expected CSP header, got empty")
			}
			got := parseCSP(csp)
			for name, want := range tt.wantDirectives {
				if got[name] != want {
					t.Errorf("directive %s = %q, want %q", name, got[name], want)
				}
			}
			if xfo := w.Header().Get("X-Frame-Options"); xfo != "DENY" {
				t.Errorf("X-Frame-Options = %q, want DENY", xfo)
			}
		})
	}
}

// TestBuildCSPPolicyWidensConnectSrcOnly is a regression test for the
// CSP that blocked the "Connect to Remote Server" feature. The SPA
// points fetch/SSE/WebSocket at an arbitrary remote origin stored
// client-side (frontend/src/lib/api/client.ts), so connect-src must
// permit any http/https/ws/wss origin — while every directive that
// gates code or resource loading stays pinned to the server origin.
func TestBuildCSPPolicyWidensConnectSrcOnly(t *testing.T) {
	t.Parallel()

	directives := parseCSP(buildCSPPolicy("127.0.0.1", 8081, ""))

	if got := directives["connect-src"]; got != "'self' http: https: ws: wss:" {
		t.Errorf("connect-src = %q, want it widened to any http/https/ws/wss origin", got)
	}

	// Scheme-source wildcards must not leak into the directives that
	// gate code/resource loading, or the widening would defeat the
	// remaining CSP protection.
	locked := []string{"default-src", "script-src", "img-src", "style-src", "font-src"}
	schemeWildcards := map[string]bool{
		"http:": true, "https:": true, "ws:": true, "wss:": true,
	}
	for _, name := range locked {
		for field := range strings.FieldsSeq(directives[name]) {
			if schemeWildcards[field] {
				t.Errorf("directive %s must stay pinned but allows %q (full: %q)",
					name, field, directives[name])
			}
		}
	}
}

func TestCORSMiddlewareMergesVaryHeader(t *testing.T) {
	t.Parallel()

	allowedOrigins := map[string]bool{
		"http://127.0.0.1:8080": true,
	}
	cors := corsMiddleware(
		allowedOrigins, false, 8080, nil,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "Accept-Encoding")
		cors.ServeHTTP(w, r)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assertRecorderStatus(t, w, http.StatusOK)
	vary := w.Header().Get("Vary")
	if !strings.Contains(vary, "Accept-Encoding") {
		t.Fatalf("expected Vary to include Accept-Encoding, got %q", vary)
	}
	if !strings.Contains(vary, "Origin") {
		t.Fatalf("expected Vary to include Origin, got %q", vary)
	}
}
