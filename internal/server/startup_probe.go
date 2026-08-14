package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
)

const (
	startupProbePath            = "/_agentsview/startup"
	startupProbeChallengeHeader = "X-AgentsView-Startup-Challenge"
	startupProbeProofHeader     = "X-AgentsView-Startup-Proof"
)

func (s *Server) registerStartupProbeRoute() {
	s.mux.HandleFunc("GET "+startupProbePath, s.handleStartupProbe)
}

// EnableStartupProbe creates a process-local secret for proving that startup
// readiness reached this Server instance without sending the persistent bearer
// token over a socket whose owner is not yet known.
func (s *Server) EnableStartupProbe() error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("generate startup probe key: %w", err)
	}
	s.mu.Lock()
	clear(s.startupProbeKey)
	s.startupProbeKey = key
	s.mu.Unlock()
	return nil
}

// DisableStartupProbe removes the temporary startup proof endpoint secret.
func (s *Server) DisableStartupProbe() {
	s.mu.Lock()
	clear(s.startupProbeKey)
	s.startupProbeKey = nil
	s.mu.Unlock()
}

// StartupProbePath returns the mounted path of the temporary startup proof
// endpoint.
func (s *Server) StartupProbePath() string {
	return s.basePath + startupProbePath
}

// StartupProbeChallenge creates a fresh challenge and the proof that only this
// Server instance can return for it.
func (s *Server) StartupProbeChallenge() (string, string, error) {
	challengeBytes := make([]byte, 32)
	if _, err := rand.Read(challengeBytes); err != nil {
		return "", "", fmt.Errorf("generate startup probe challenge: %w", err)
	}
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes)
	proof := s.startupProbeProof(challenge)
	if proof == "" {
		return "", "", errors.New("startup probe is not enabled")
	}
	return challenge, proof, nil
}

func (s *Server) handleStartupProbe(w http.ResponseWriter, r *http.Request) {
	proof := s.startupProbeProof(r.Header.Get(startupProbeChallengeHeader))
	if proof == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set(startupProbeProofHeader, proof)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startupProbeProof(challenge string) string {
	challengeBytes, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil || len(challengeBytes) != 32 {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.startupProbeKey) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, s.startupProbeKey)
	_, _ = mac.Write(challengeBytes)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ValidStartupProbeResponse reports whether resp contains the proof derived
// from the temporary server-held startup key.
func ValidStartupProbeResponse(resp *http.Response, expected string) bool {
	return hmac.Equal(
		[]byte(resp.Header.Get(startupProbeProofHeader)),
		[]byte(expected),
	)
}

// SetStartupProbeChallenge adds a startup challenge to req.
func SetStartupProbeChallenge(req *http.Request, challenge string) {
	req.Header.Set(startupProbeChallengeHeader, challenge)
}
