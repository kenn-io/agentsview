package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/shlex"
	"go.kenn.io/agentsview/internal/config"
)

func (s *Server) registerConfigRoutes() {
	group := newRouteGroup(s.api, "/api/v1/config", "Config")

	get(s, group, "/github", "Get GitHub config", s.humaGetGithubConfig)
	post(s, group, "/github", "Set GitHub config", s.humaSetGithubConfig)
	get(s, group, "/terminal", "Get terminal config", s.humaGetTerminalConfig)
	post(s, group, "/terminal", "Set terminal config", s.humaSetTerminalConfig)
}

type terminalMode string

const (
	terminalModeAuto      terminalMode = "auto"
	terminalModeCustom    terminalMode = "custom"
	terminalModeClipboard terminalMode = "clipboard"
)

type githubConfigResponse struct {
	Configured bool `json:"configured"`
}

type setGithubConfigInput struct {
	Body struct {
		Token string `json:"token" required:"true" minLength:"1" doc:"GitHub token"`
	}
}

type setGithubConfigResponse struct {
	Success  bool   `json:"success"`
	Username string `json:"username"`
}

type terminalConfigInput struct {
	Body terminalConfigBody
}

type terminalConfigBody struct {
	Mode       terminalMode `json:"mode" enum:"auto,custom,clipboard" doc:"Terminal launch mode"`
	CustomBin  string       `json:"custom_bin,omitempty" doc:"Terminal binary path when mode is custom"`
	CustomArgs string       `json:"custom_args,omitempty" doc:"Argument template containing {cmd} when mode is custom"`
}

func terminalConfigBodyFromConfig(tc config.TerminalConfig) terminalConfigBody {
	mode := terminalMode(tc.Mode)
	if mode == "" {
		mode = terminalModeAuto
	}
	return terminalConfigBody{
		Mode:       mode,
		CustomBin:  tc.CustomBin,
		CustomArgs: tc.CustomArgs,
	}
}

func (b terminalConfigBody) config() config.TerminalConfig {
	return config.TerminalConfig{
		Mode:       string(b.Mode),
		CustomBin:  b.CustomBin,
		CustomArgs: b.CustomArgs,
	}
}

func (s *Server) humaGetGithubConfig(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[githubConfigResponse], error) {
	return &jsonOutput[githubConfigResponse]{
		Body: githubConfigResponse{Configured: s.githubToken(ctx) != ""},
	}, nil
}

func (s *Server) humaSetGithubConfig(
	ctx context.Context,
	in *setGithubConfigInput,
) (*jsonOutput[setGithubConfigResponse], error) {
	token := strings.TrimSpace(in.Body.Token)
	if token == "" {
		return nil, apiError(http.StatusBadRequest, "token required")
	}
	username, err := validateGithubToken(ctx, token)
	if err != nil {
		return nil, apiError(http.StatusUnauthorized, err.Error())
	}
	s.mu.Lock()
	err = s.cfg.SaveGithubToken(token)
	s.mu.Unlock()
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, "failed to save token")
	}
	return &jsonOutput[setGithubConfigResponse]{
		Body: setGithubConfigResponse{Success: true, Username: username},
	}, nil
}

func (s *Server) humaGetTerminalConfig(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[terminalConfigBody], error) {
	s.mu.RLock()
	tc := s.cfg.Terminal
	s.mu.RUnlock()
	return &jsonOutput[terminalConfigBody]{
		Body: terminalConfigBodyFromConfig(tc),
	}, nil
}

func (s *Server) humaSetTerminalConfig(
	_ context.Context,
	in *terminalConfigInput,
) (*jsonOutput[terminalConfigBody], error) {
	body := in.Body
	tc := body.config()
	switch terminalMode(tc.Mode) {
	case terminalModeAuto, terminalModeCustom, terminalModeClipboard:
	default:
		return nil, apiError(http.StatusBadRequest,
			`mode must be "auto", "custom", or "clipboard"`)
	}
	if tc.Mode == string(terminalModeCustom) && tc.CustomBin == "" {
		return nil, apiError(http.StatusBadRequest,
			`custom_bin is required when mode is "custom"`)
	}
	if tc.Mode == string(terminalModeCustom) {
		if tc.CustomArgs != "" && !strings.Contains(tc.CustomArgs, "{cmd}") {
			return nil, apiError(http.StatusBadRequest,
				`custom_args must contain the {cmd} placeholder so the resume command is passed to the terminal`)
		}
		if tc.CustomArgs != "" {
			if _, splitErr := shlex.Split(tc.CustomArgs); splitErr != nil {
				return nil, apiError(http.StatusBadRequest,
					fmt.Sprintf("custom_args has invalid shell syntax: %v", splitErr))
			}
		}
	}
	s.mu.Lock()
	err := s.cfg.SaveTerminalConfig(tc)
	s.mu.Unlock()
	if err != nil {
		return nil, internalError("save terminal config", err)
	}
	return &jsonOutput[terminalConfigBody]{
		Body: terminalConfigBodyFromConfig(tc),
	}, nil
}
