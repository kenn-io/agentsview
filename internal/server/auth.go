package server

import (
	"context"
	"net"
	"net/http"
	"strings"
)

// contextKey is an unexported type for context keys in this package.
type contextKey int

const (
	// ctxKeyRemoteAuth indicates the request passed token auth.
	// When set to true, host-check and CORS middleware skip
	// their restrictions.
	ctxKeyRemoteAuth contextKey = iota
)

// isRemoteAuth returns true if the request was authenticated as a
// remote client by the auth middleware.
func isRemoteAuth(r *http.Request) bool {
	v, _ := r.Context().Value(ctxKeyRemoteAuth).(bool)
	return v
}

// isLocalhostRequest returns true when the request originates from
// a loopback address (127.0.0.0/8, ::1). It checks RemoteAddr,
// which is set by net/http to the client's IP.
func isLocalhostRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// authMiddleware enforces Bearer token authentication when
// require_auth is enabled. Non-API routes (static assets) are
// never gated.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only gate /api/ routes — static assets are always served.
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// Read config once for all checks below.
		s.mu.RLock()
		token := s.cfg.AuthToken
		authRequired := s.cfg.RequireAuth
		s.mu.RUnlock()

		// CORS preflight requests (OPTIONS) never include credentials.
		// Let them through so the browser can negotiate CORS before
		// sending the authenticated request. When auth is required,
		// mark OPTIONS as authenticated so the CORS middleware
		// allows the preflight for cross-origin clients.
		if r.Method == http.MethodOptions {
			if authRequired && token != "" {
				ctx := context.WithValue(r.Context(), ctxKeyRemoteAuth, true)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// When auth is not required, skip token checks entirely.
		if !authRequired {
			next.ServeHTTP(w, r)
			return
		}
		// Auth required but no token configured — fail closed.
		if token == "" {
			http.Error(w,
				"server misconfiguration: auth required but no token set",
				http.StatusInternalServerError)
			return
		}

		// Check Bearer token in Authorization header. The ?token=
		// query param fallback is restricted to SSE endpoints
		// (see isSSEPath) because EventSource cannot set custom
		// headers. All other endpoints must use the Authorization
		// header.
		var provided string
		auth := r.Header.Get("Authorization")
		if t, ok := strings.CutPrefix(auth, "Bearer "); ok {
			provided = t
		} else if qt := r.URL.Query().Get("token"); qt != "" && isSSEPath(r.URL.Path) {
			provided = qt
		} else {
			setCORSOnAuthError(w, r)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if provided != token {
			setCORSOnAuthError(w, r)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Mark this request as authenticated remote so downstream
		// middleware (host-check, CORS) can relax restrictions.
		ctx := context.WithValue(r.Context(), ctxKeyRemoteAuth, true)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isSSEPath reports whether the given path is a server-sent events
// endpoint that accepts a ?token= query parameter in place of the
// Authorization header. The query-param fallback exists because
// browser EventSource cannot set headers.
func isSSEPath(path string) bool {
	return strings.HasSuffix(path, "/watch") || path == "/api/v1/events"
}

// setCORSOnAuthError adds CORS headers to 401 responses so
// cross-origin browsers can read the auth failure status. Without
// these headers, 401s from authMiddleware (which runs before
// corsMiddleware) become opaque network errors, preventing the
// frontend from detecting auth failures and prompting for a token.
//
// Only used for token-related 401s in remote mode, where the token
// is the access boundary and cross-origin requests are expected.
// Not used for 403s (remote access disabled / no token configured)
// which are not auth challenges the client can resolve.
func setCORSOnAuthError(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	ensureVaryHeader(w.Header(), "Origin")
}
