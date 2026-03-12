package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/wesm/agentsview/internal/parser"
)

// settingsResponse is the JSON shape returned by GET /api/v1/settings.
type settingsResponse struct {
	AgentDirs        map[string][]string `json:"agent_dirs"`
	Terminal         terminalResponse    `json:"terminal"`
	GithubConfigured bool                `json:"github_configured"`
	Host             string              `json:"host"`
	Port             int                 `json:"port"`
	AuthToken        string              `json:"auth_token,omitempty"`
	RemoteAccess     bool                `json:"remote_access"`
}

// terminalResponse mirrors config.TerminalConfig for JSON output.
type terminalResponse struct {
	Mode       string `json:"mode"`
	CustomBin  string `json:"custom_bin,omitempty"`
	CustomArgs string `json:"custom_args,omitempty"`
}

func (s *Server) handleGetSettings(
	w http.ResponseWriter, r *http.Request,
) {
	// Hold the read lock for the entire duration of building the
	// response to prevent a data race with concurrent writes.
	s.mu.RLock()
	dirs := make(map[string][]string)
	for _, def := range parser.Registry {
		d := s.cfg.AgentDirs[def.Type]
		if d == nil {
			d = []string{}
		}
		dirs[string(def.Type)] = d
	}

	tc := s.cfg.Terminal
	if tc.Mode == "" {
		tc.Mode = "auto"
	}

	resp := settingsResponse{
		AgentDirs: dirs,
		Terminal: terminalResponse{
			Mode:       tc.Mode,
			CustomBin:  tc.CustomBin,
			CustomArgs: tc.CustomArgs,
		},
		GithubConfigured: s.cfg.GithubToken != "",
		Host:             s.cfg.Host,
		Port:             s.cfg.Port,
		RemoteAccess:     s.cfg.RemoteAccess,
	}

	// Only expose auth_token to localhost requests, never to remote clients.
	if isLocalhostRequest(r) {
		resp.AuthToken = s.cfg.AuthToken
	}
	s.mu.RUnlock()

	writeJSON(w, http.StatusOK, resp)
}

// settingsUpdateRequest is the JSON body for PUT /api/v1/settings.
// All fields are optional; only non-nil fields are applied.
type settingsUpdateRequest struct {
	Terminal     *terminalResponse `json:"terminal,omitempty"`
	AuthToken    *string           `json:"auth_token,omitempty"`
	RemoteAccess *bool             `json:"remote_access,omitempty"`
}

func (s *Server) handleUpdateSettings(
	w http.ResponseWriter, r *http.Request,
) {
	var req settingsUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	patch := make(map[string]any)

	// Terminal config is deliberately excluded from the generic
	// settings endpoint. It must be updated through the dedicated
	// POST /api/v1/config/terminal endpoint which validates
	// custom_bin and custom_args to prevent command injection.
	if req.Terminal != nil {
		writeError(w, http.StatusBadRequest,
			"terminal config must be updated via POST /api/v1/config/terminal")
		return
	}

	if req.AuthToken != nil {
		patch["auth_token"] = *req.AuthToken
	}

	if req.RemoteAccess != nil {
		patch["remote_access"] = *req.RemoteAccess
	}

	if len(patch) == 0 {
		// Nothing to update; return current settings.
		s.handleGetSettings(w, r)
		return
	}

	s.mu.Lock()
	err := s.cfg.SaveSettings(patch)
	// Auto-generate auth token when remote_access is enabled.
	if err == nil && s.cfg.RemoteAccess {
		err = s.cfg.EnsureAuthToken()
	}
	s.mu.Unlock()
	if err != nil {
		log.Printf("save settings: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Return the full updated settings.
	s.handleGetSettings(w, r)
}
