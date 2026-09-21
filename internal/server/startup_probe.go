package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

const (
	startupProbePath            = "/_agentsview/startup"
	startupProbeChallengeHeader = "X-AgentsView-Startup-Challenge"
	startupProbeProofHeader     = "X-AgentsView-Startup-Proof"
)

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

// describeStartupProbe keeps the temporary native route in the generated client.
func (s *Server) describeStartupProbe() {
	op := &huma.Operation{
		OperationID: "get-startup-probe", Method: http.MethodGet, Path: startupProbePath,
		Summary: "Prove daemon startup readiness", Tags: []string{"Startup"},
		Parameters: []*huma.Param{{Name: startupProbeChallengeHeader, In: "header", Required: true, Schema: &huma.Schema{Type: "string"}}},
		Responses: map[string]*huma.Response{
			"204": {Description: "Startup proof", Headers: map[string]*huma.Param{startupProbeProofHeader: {Schema: &huma.Schema{Type: "string"}}}},
			"404": {Description: "Startup probe disabled or challenge invalid"},
		},
	}
	s.api.OpenAPI().AddOperation(op)
	s.handleHTTP(op, s.handleStartupProbe)
}
