package server

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/artifact"
)

const artifactExchangeMaxBodyBytes = 1 << 20

// ArtifactExchangeRequest describes one bounded exchange with a folder target.
// Target is deliberately accepted only by the authenticated loopback-only
// endpoint because it names a path on the daemon's machine.
type ArtifactExchangeRequest struct {
	Target string `json:"target"`
	Full   bool   `json:"full,omitempty"`
}

// ArtifactExchangeRunner performs one folder exchange using daemon-owned
// database and sync-engine authority.
type ArtifactExchangeRunner func(
	context.Context,
	ArtifactExchangeRequest,
) (artifact.SyncResult, error)

// WithArtifactExchangeRunner enables the loopback-only artifact exchange API.
// A nil runner leaves the route unregistered.
func WithArtifactExchangeRunner(runner ArtifactExchangeRunner) Option {
	return func(s *Server) {
		s.artifactExchangeRunner = runner
	}
}

func (s *Server) handleArtifactExchange(
	w http.ResponseWriter,
	r *http.Request,
) {
	if !isLocalhostRequest(r) || !isLoopbackHTTPHost(r.Host) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, artifactExchangeMaxBodyBytes)
	var request ArtifactExchangeRequest
	if err := json.UnmarshalRead(
		r.Body, &request, json.RejectUnknownMembers(true),
	); err != nil {
		writeArtifactExchangeDecodeError(w, err)
		return
	}
	if strings.TrimSpace(request.Target) == "" ||
		!filepath.IsAbs(request.Target) {
		http.Error(w, "invalid artifact exchange request", http.StatusBadRequest)
		return
	}

	done, ok := s.idle.BeginWork()
	if !ok {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	defer done()

	result, err := s.artifactExchangeRunner(r.Context(), request)
	if err != nil {
		http.Error(w, "artifact exchange failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalEncode(jsontext.NewEncoder(w), result); err != nil {
		return
	}
}

func writeArtifactExchangeDecodeError(w http.ResponseWriter, err error) {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		http.Error(
			w,
			"artifact exchange request too large",
			http.StatusRequestEntityTooLarge,
		)
		return
	}
	http.Error(w, "invalid artifact exchange request", http.StatusBadRequest)
}

func isLoopbackHTTPHost(hostport string) bool {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
