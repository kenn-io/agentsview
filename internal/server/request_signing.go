package server

import (
	"context"
	"crypto/hmac"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/requestsign"
	"go.kenn.io/agentsview/internal/service"
)

const (
	ctxKeyRestrictedIngress       contextKey = 101
	ctxKeyRestrictedIngressPrefix contextKey = 102
)

func restrictedIngress(ctx context.Context) bool {
	value, _ := ctx.Value(ctxKeyRestrictedIngress).(bool)
	return value
}

func restrictedIngressPrefix(ctx context.Context) string {
	prefix, _ := ctx.Value(ctxKeyRestrictedIngressPrefix).(string)
	return prefix
}

func (s *Server) validateRestrictedPolicyAuth(policy requestsign.Policy) error {
	for _, key := range policy.Keys {
		if key.Grant != "reader" {
			continue
		}
		s.mu.RLock()
		token := s.cfg.AuthToken
		s.mu.RUnlock()
		if token == "" {
			return errors.New("machine signing reader grant requires a configured auth_token")
		}
		break
	}
	return nil
}

func machineRequestLogMiddleware(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, prefix)
		if strings.HasPrefix(path, "/api/") {
			log.Printf("%s %s", r.Method, path)
		}
		next.ServeHTTP(w, r)
	})
}

// RestrictedHandler mounts only signed machine operations. Its fixed external
// URI configuration is trusted operator input, never taken from proxy headers.
// Main Handler and browser authentication retain their existing behavior.
func (s *Server) RestrictedHandler(policyFile string) (http.Handler, func() error, error) {
	policy, _, err := requestsign.ReadPolicy(policyFile)
	if err != nil {
		return nil, nil, err
	}
	if err := s.validateRestrictedPolicyAuth(policy); err != nil {
		return nil, nil, err
	}
	replay, err := requestsign.OpenReplay(policy.ReplayDB)
	if err != nil {
		return nil, nil, err
	}
	verifier, err := requestsign.NewVerifier(requestsign.VerifyConfig{
		ExternalURL: policy.ExternalURL, StripPrefix: policy.StripPrefix, Replay: replay,
		Keys: func() (map[string]requestsign.Key, error) {
			updated, keys, err := requestsign.ReadPolicy(policyFile)
			if err != nil {
				return nil, err
			}
			if updated.ExternalURL != policy.ExternalURL || updated.StripPrefix != policy.StripPrefix || updated.ReplayDB != policy.ReplayDB || updated.Listen != policy.Listen {
				return nil, errors.New("listener signing policy changed; restart required")
			}
			if err := s.validateRestrictedPolicyAuth(updated); err != nil {
				return nil, err
			}
			return keys, nil
		},
	})
	if err != nil {
		_ = replay.Close()
		return nil, nil, err
	}
	externalPrefix := externalSigningPrefix(policy.ExternalURL)
	// Signing validates the original target before any mux can normalize paths.
	// The authentication stage then strips a preserved external prefix exactly once.
	authenticate := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !policy.StripPrefix {
				r = r.Clone(r.Context())
				r.URL.Path = strings.TrimPrefix(r.URL.Path, externalPrefix)
				r.URL.RawPath = ""
			}
			ctx := context.WithValue(r.Context(), ctxKeyRestrictedIngress, true)
			ctx = context.WithValue(ctx, ctxKeyRestrictedIngressPrefix, externalPrefix)
			s.authMiddlewareRequired(next, true).ServeHTTP(w, r.WithContext(ctx))
		})
	}
	authorize := func(key requestsign.Key, r *http.Request) bool {
		if !restrictedOperation(key.Grant, r.Method, r.URL.Path) {
			return false
		}
		if key.Grant == "contributor" {
			identity, err := rawSyncIdentityFromContext(r.Context())
			if err != nil || identity.DeviceID != key.DeviceID {
				return false
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			current, err := s.authenticateRawSyncRequest(r.WithContext(ctx))
			return err == nil && current.DeviceID == identity.DeviceID && current.TenantID == identity.TenantID
		}
		s.mu.RLock()
		token := s.cfg.AuthToken
		s.mu.RUnlock()
		provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		return ok && token != "" && hmac.Equal([]byte(provided), []byte(token))
	}
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/version" && service.SupportsRecallQueries(s.sessions) {
			w.Header().Set("X-Agentsview-Recall-Queries", "non-recording")
		}
		gzipMiddleware(s.mux).ServeHTTP(w, r)
	})
	h := machineRequestLogMiddleware(externalPrefix, verifier.Wrap(dispatch, authenticate, authorize))
	return s.idle.Wrap(h), replay.Close, nil
}

func externalSigningPrefix(base string) string {
	// base is a validated HTTPS URL; keep every segment in its configured path.
	externalURL, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return strings.TrimRight(externalURL.Path, "/")
}

func restrictedOperation(grant, method, path string) bool {
	if grant == "contributor" {
		switch path {
		case "/api/v1/raw-sync/tokens", "/api/v1/raw-sync/objects/missing", "/api/v1/raw-sync/manifests", "/api/v1/raw-sync/uploads":
			return method == http.MethodPost
		case "/api/v1/raw-sync/status", "/api/v1/raw-sync/health":
			return method == http.MethodGet
		}
		return isRawSyncUploadPath(path) && path != "/api/v1/raw-sync/uploads" && (method == http.MethodPatch || method == http.MethodHead)
	}
	if grant != "reader" {
		return false
	}
	if method == http.MethodPost {
		return path == "/api/v1/recall/query"
	}
	if method != http.MethodGet {
		return false
	}
	switch path {
	case "/api/ping", "/api/v1/version", "/api/v1/sessions", "/api/v1/sessions/sidebar-index", "/api/v1/session-ids/resolve", "/api/v1/projects", "/api/v1/machines", "/api/v1/agents", "/api/v1/branches", "/api/v1/search", "/api/v1/search/content", "/api/v1/events", "/api/v1/recall/entries", "/api/v1/memory/status", "/api/v1/usage/summary":
		return true
	}
	if rest, ok := strings.CutPrefix(path, "/api/v1/sessions/"); ok {
		parts := strings.Split(rest, "/")
		if parts[0] == "" {
			return false
		}
		if len(parts) == 1 {
			return true
		}
		if len(parts) != 2 {
			return false
		}
		switch parts[1] {
		case "messages", "tool-calls", "children", "activity", "timing", "usage", "watch", "md", "search":
			return true
		}
	}
	if rest, ok := strings.CutPrefix(path, "/api/v1/recall/entries/"); ok {
		return rest != "" && !strings.Contains(rest, "/")
	}
	return false
}
