package server

import (
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

// settingsResponse is the JSON shape returned by GET /api/v1/settings.
type settingsResponse struct {
	AgentDirs        map[string][]string       `json:"agent_dirs"`
	SessionProviders []sessionProviderResponse `json:"session_providers"`
	DisabledAgents   []parser.AgentType        `json:"disabled_agents"`
	Terminal         terminalResponse          `json:"terminal"`
	GithubConfigured bool                      `json:"github_configured"`
	Host             string                    `json:"host"`
	Port             int                       `json:"port"`
	ChartPalette     config.ChartPalette       `json:"chart_palette"`
	AuthToken        string                    `json:"auth_token,omitempty"`
	RequireAuth      bool                      `json:"require_auth"`
	ReadOnly         bool                      `json:"read_only"`
}

type sessionProviderResponse struct {
	ID                 parser.AgentType `json:"id"`
	DisplayName        string           `json:"display_name"`
	Dirs               []string         `json:"dirs"`
	PostAnswerToolWork bool             `json:"post_answer_tool_work,omitzero"`
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
	Terminal       *terminalResponse `json:"terminal,omitempty"`
	AuthToken      *string           `json:"auth_token,omitempty"`
	RequireAuth    *bool             `json:"require_auth,omitempty"`
	ChartPalette   *string           `json:"chart_palette,omitempty"`
	DisabledAgents *[]string         `json:"disabled_agents,omitempty"`
}
