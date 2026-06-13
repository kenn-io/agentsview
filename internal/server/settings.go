package server

// settingsResponse is the JSON shape returned by GET /api/v1/settings.
type settingsResponse struct {
	AgentDirs        map[string][]string `json:"agent_dirs"`
	Terminal         terminalResponse    `json:"terminal"`
	GithubConfigured bool                `json:"github_configured"`
	Host             string              `json:"host"`
	Port             int                 `json:"port"`
	AuthToken        string              `json:"auth_token,omitempty"`
	RequireAuth      bool                `json:"require_auth"`
	ReadOnly         bool                `json:"read_only"`
}

// terminalResponse mirrors config.TerminalConfig for JSON output.
type terminalResponse struct {
	Mode       string `json:"mode"`
	CustomBin  string `json:"custom_bin,omitempty"`
	CustomArgs string `json:"custom_args,omitempty"`
}

// settingsUpdateRequest is the JSON body for PUT /api/v1/settings.
// All fields are optional; only non-nil fields are applied.
type settingsUpdateRequest struct {
	Terminal    *terminalResponse `json:"terminal,omitempty"`
	AuthToken   *string           `json:"auth_token,omitempty"`
	RequireAuth *bool             `json:"require_auth,omitempty"`
}
