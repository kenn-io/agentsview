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
	// ctxKeyRemoteAuth indicates the request is from an authenticated
	// remote client. When set to true, host-check and CORS middleware
	// skip their restrictions.
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

// authMiddleware enforces Bearer token authentication for remote
// API requests. Localhost connections always bypass auth for backward
// compatibility. Non-API routes (static assets) are never gated.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only gate /api/ routes — static assets are always served.
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// Localhost connections always bypass auth.
		if isLocalhostRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		// Remote request: require valid Bearer token.
		s.mu.RLock()
		token := s.cfg.AuthToken
		remoteEnabled := s.cfg.RemoteAccess
		s.mu.RUnlock()

		// When remote access is not explicitly enabled, let the
		// request fall through to the host-check middleware which
		// already allows local-interface IPs (including Tailscale).
		if !remoteEnabled {
			next.ServeHTTP(w, r)
			return
		}
		// Remote access enabled but no token configured yet — reject.
		if token == "" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		// Check Bearer token in Authorization header first, then
		// fall back to ?token= query param (needed for EventSource
		// and export URLs which cannot set custom headers).
		var provided string
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			provided = strings.TrimPrefix(auth, "Bearer ")
		} else if qt := r.URL.Query().Get("token"); qt != "" {
			provided = qt
		} else {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if provided != token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Mark this request as authenticated remote so downstream
		// middleware (host-check, CORS) can relax restrictions.
		ctx := context.WithValue(r.Context(), ctxKeyRemoteAuth, true)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
