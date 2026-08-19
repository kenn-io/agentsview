package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/rawsync"
)

// contextKey is an unexported type for context keys in this package.
type contextKey int

const (
	// ctxKeyRemoteAuth indicates the request passed token auth.
	// When set to true, host-check and CORS middleware skip
	// their restrictions.
	ctxKeyRemoteAuth contextKey = iota
	ctxKeyRawSyncIdentity
)

// isRemoteAuth returns true if the request was authenticated as a
// remote client by the auth middleware.
func isRemoteAuth(r *http.Request) bool {
	v, _ := r.Context().Value(ctxKeyRemoteAuth).(bool)
	return v
}

// isLocalhostRequest returns true only for a request that arrived as a
// direct loopback connection (127.0.0.0/8, ::1) and was not relayed by a
// reverse proxy. Callers use it to gate exposure of secrets (the auth token,
// unredacted search snippets) to "local only" clients.
//
// RemoteAddr alone is insufficient: a managed Caddy proxy reverse-proxies to
// the loopback backend, so every proxied request — including ones from remote
// LAN clients allowed through the proxy — would carry a loopback RemoteAddr.
// Proxies add X-Forwarded-For/X-Real-IP/Forwarded, so any of those headers
// means the connection is not a direct local one and must not be trusted. A
// genuinely local attacker spoofing these headers only denies themselves, so
// the check fails closed.
func isLocalhostRequest(r *http.Request) bool {
	if hasForwardingHeader(r) {
		return false
	}
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

// hasForwardingHeader reports whether the request carries any header a reverse
// proxy adds when relaying a client, which indicates the connection reached
// the server through a proxy rather than directly.
func hasForwardingHeader(r *http.Request) bool {
	return r.Header.Get("X-Forwarded-For") != "" ||
		r.Header.Get("X-Real-IP") != "" ||
		r.Header.Get("Forwarded") != ""
}

// protectedPath reports whether a path is subject to bearer auth and
// Host validation: every /api/ route, plus /debug/pprof/ when the
// profiling handlers are mounted. With pprof disabled the path is
// SPA fallback and stays ungated like other static assets.
func (s *Server) protectedPath(path string) bool {
	return strings.HasPrefix(path, "/api/") ||
		(s.pprofEnabled && strings.HasPrefix(path, "/debug/pprof/"))
}

// authMiddleware enforces Bearer token authentication for protected
// routes (/api/ including /api/ping, and /debug/pprof/ when enabled)
// when require_auth is on. Non-protected routes such as static
// assets are never gated.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static assets are always served.
		if !s.protectedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// Read config once for all checks below.
		s.mu.RLock()
		token := s.cfg.AuthToken
		authRequired := s.cfg.RequireAuth
		writeTimeout := s.cfg.WriteTimeout
		s.mu.RUnlock()

		// CORS preflight requests (OPTIONS) never include credentials.
		// Let them through so the browser can negotiate CORS before
		// sending the authenticated request. When auth is required,
		// mark OPTIONS as authenticated so the CORS middleware
		// allows the preflight for cross-origin clients.
		if r.Method == http.MethodOptions {
			if s.isRawSyncOwnAuthPath(r.URL.Path) || authRequired && token != "" {
				ctx := context.WithValue(r.Context(), ctxKeyRemoteAuth, true)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Raw-sync machine routes authenticate their own device credentials
		// and scoped tokens before Host and CORS middleware relax remote
		// restrictions. They never compare against the legacy shared bearer.
		if s.isRawSyncOwnAuthPath(r.URL.Path) {
			baseCtx := r.Context()
			authCtx := baseCtx
			cancel := func() {}
			if writeTimeout > 0 {
				authCtx, cancel = context.WithTimeout(authCtx, writeTimeout)
			}
			defer cancel()
			identity, err := s.authenticateRawSyncRequest(r.WithContext(authCtx))
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					http.Error(w, "raw sync authentication timed out", http.StatusGatewayTimeout)
					return
				}
				if errors.Is(err, rawsync.ErrUnauthorized) ||
					errors.Is(err, rawsync.ErrInvalid) {
					setCORSOnAuthError(w, r)
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				http.Error(w, "raw sync authentication failed", http.StatusInternalServerError)
				return
			}
			handlerCtx := authCtx
			if r.Method == http.MethodPatch && isRawSyncUploadPath(r.URL.Path) {
				// Upload authentication stays bounded by WriteTimeout, but body
				// transfer and checksum finalization use the client's request
				// lifetime plus the route's extended transport read deadline.
				cancel()
				handlerCtx = baseCtx
			}
			ctx := context.WithValue(handlerCtx, ctxKeyRawSyncIdentity, identity)
			ctx = context.WithValue(ctx, ctxKeyRemoteAuth, true)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		machineAuth := isRemoteSyncPath(r.URL.Path) ||
			(s.artifactExchangeRunner != nil &&
				isArtifactExchangePath(r.URL.Path))

		// When auth is not required, skip token checks entirely
		// except for machine-to-machine remote sync archive APIs,
		// which must still set ctxKeyRemoteAuth before host/CORS
		// middleware runs.
		if !authRequired && !machineAuth {
			next.ServeHTTP(w, r)
			return
		}
		// Auth required but no token configured — fail closed.
		if token == "" {
			if machineAuth {
				http.Error(w,
					"server misconfiguration: auth token required for machine API",
					http.StatusInternalServerError)
				return
			}
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

func isRemoteSyncPath(path string) bool {
	return strings.HasPrefix(path, "/api/v1/remote-sync/")
}

func isArtifactExchangePath(path string) bool {
	return path == "/api/v1/artifacts/exchange"
}

func (s *Server) isRawSyncOwnAuthPath(path string) bool {
	if s.rawSyncDeviceAuth == nil {
		return false
	}
	if path == "/api/v1/raw-sync/tokens" {
		return true
	}
	if s.rawSyncCustody == nil {
		return false
	}
	if path == "/api/v1/raw-sync/objects/missing" ||
		path == "/api/v1/raw-sync/manifests" {
		return true
	}
	return s.rawSyncUploads != nil && isRawSyncUploadPath(path)
}

func (s *Server) authenticateRawSyncRequest(
	r *http.Request,
) (rawsync.AuthIdentity, error) {
	secret, err := rawSyncBearer(r.Header.Get("Authorization"))
	if err != nil {
		return rawsync.AuthIdentity{}, err
	}
	switch r.URL.Path {
	case "/api/v1/raw-sync/tokens":
		return s.rawSyncDeviceAuth.AuthenticateCredential(
			r.Context(), r.Header.Get(rawSyncDeviceIDHeader), secret,
		)
	case "/api/v1/raw-sync/objects/missing":
		return s.rawSyncDeviceAuth.AuthenticateToken(
			r.Context(), secret, rawsync.ScopeNegotiate,
		)
	case "/api/v1/raw-sync/manifests":
		return s.rawSyncDeviceAuth.AuthenticateToken(
			r.Context(), secret, rawsync.ScopeCommit,
		)
	default:
		if s.rawSyncUploads != nil && isRawSyncUploadPath(r.URL.Path) {
			return s.rawSyncDeviceAuth.AuthenticateToken(
				r.Context(), secret, rawsync.ScopeUpload,
			)
		}
		return rawsync.AuthIdentity{}, rawsync.ErrUnauthorized
	}
}

func isRawSyncUploadPath(path string) bool {
	if path == "/api/v1/raw-sync/uploads" {
		return true
	}
	remainder, ok := strings.CutPrefix(path, "/api/v1/raw-sync/uploads/")
	return ok && remainder != "" && !strings.Contains(remainder, "/")
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
