package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func (s *Server) registerSettingsRoutes() {
	group := newRouteGroup(s.api, "/api/v1/settings", "Settings")

	get(s, group, "", "Get settings", s.humaGetSettings)
	put(s, group, "", "Update settings", s.humaUpdateSettings)
	get(s, group, "/worktree-mappings", "List worktree mappings", s.humaListWorktreeMappings)
	post(s, group, "/worktree-mappings", "Create worktree mapping", s.humaCreateWorktreeMapping)
	put(s, group, "/worktree-mappings/{id}", "Update worktree mapping", s.humaUpdateWorktreeMapping)
	deleteRoute(s, group, "/worktree-mappings/{id}", "Delete worktree mapping", s.humaDeleteWorktreeMapping)
	post(s, group, "/worktree-mappings/apply", "Apply worktree mappings", s.humaApplyWorktreeMappings)
}

type settingsInput struct {
	Body settingsUpdateRequest
}

type worktreeMappingCreateInput struct {
	Body worktreeMappingRequest
}

type worktreeMappingUpdateInput struct {
	ID   string `path:"id" required:"true" doc:"Mapping ID"`
	Body worktreeMappingRequest
}

type worktreeMappingPathInput struct {
	ID string `path:"id" required:"true" doc:"Mapping ID"`
}

func (s *Server) humaGetSettings(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[settingsResponse], error) {
	s.mu.RLock()
	dirs := make(map[string][]string)
	for _, def := range parser.Registry {
		if !def.FileBased && def.EnvVar == "" {
			continue
		}
		d := s.cfg.AgentDirs[def.Type]
		if d == nil {
			d = []string{}
		}
		dirs[string(def.Type)] = d
	}
	tc := s.cfg.Terminal
	if tc.Mode == "" {
		tc.Mode = string(terminalModeAuto)
	}
	githubToken := s.cfg.GithubToken
	resp := settingsResponse{
		AgentDirs: dirs,
		Terminal: terminalResponse{
			Mode:       tc.Mode,
			CustomBin:  tc.CustomBin,
			CustomArgs: tc.CustomArgs,
		},
		Host:        s.cfg.Host,
		Port:        s.cfg.Port,
		RequireAuth: s.cfg.RequireAuth,
		ReadOnly:    s.db.ReadOnly(),
	}
	if isLocalhostContext(ctx) {
		resp.AuthToken = s.cfg.AuthToken
	}
	s.mu.RUnlock()
	resp.GithubConfigured = resolveGitHubToken(ctx, githubToken) != ""
	return &jsonOutput[settingsResponse]{Body: resp}, nil
}

func (s *Server) humaUpdateSettings(
	ctx context.Context,
	in *settingsInput,
) (*jsonOutput[settingsResponse], error) {
	if s.db.ReadOnly() {
		return nil, apiError(http.StatusNotImplemented,
			"settings cannot be modified in read-only mode")
	}
	if in.Body.Terminal != nil {
		return nil, apiError(http.StatusBadRequest,
			"terminal config must be updated via POST /api/v1/config/terminal")
	}
	patch := make(map[string]any)
	if in.Body.AuthToken != nil {
		patch["auth_token"] = *in.Body.AuthToken
	}
	if in.Body.RequireAuth != nil {
		patch["require_auth"] = *in.Body.RequireAuth
	}
	if len(patch) > 0 {
		s.mu.Lock()
		err := s.cfg.SaveSettings(patch)
		if err == nil && s.cfg.RequireAuth {
			err = s.cfg.EnsureAuthToken()
		}
		s.mu.Unlock()
		if err != nil {
			return nil, internalError("save settings", err)
		}
	}
	return s.humaGetSettings(ctx, &emptyInput{})
}

func (s *Server) localWorktreeMappingHumaDB() (*db.DB, string, error) {
	localDB, ok := s.db.(*db.DB)
	if !ok || localDB == nil || localDB.ReadOnly() || s.engine == nil {
		return nil, "", apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	machine := strings.TrimSpace(s.engine.Machine())
	if machine == "" {
		machine = "local"
	}
	return localDB, machine, nil
}

func (s *Server) humaListWorktreeMappings(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[worktreeMappingsResponse], error) {
	localDB, machine, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	mappings, err := localDB.ListWorktreeProjectMappings(ctx, machine)
	if err != nil {
		return nil, internalError("list worktree mappings", err)
	}
	return &jsonOutput[worktreeMappingsResponse]{
		Body: worktreeMappingsResponse{Machine: machine, Mappings: mappings},
	}, nil
}

func (s *Server) humaCreateWorktreeMapping(
	ctx context.Context,
	in *worktreeMappingCreateInput,
) (*createdOutput[db.WorktreeProjectMapping], error) {
	localDB, machine, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	if in.Body.PathPrefix == nil || in.Body.Project == nil {
		return nil, apiError(http.StatusBadRequest, "path_prefix and project are required")
	}
	enabled := true
	if in.Body.Enabled != nil {
		enabled = *in.Body.Enabled
	}
	mapping, err := localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine:    machine,
		PathPrefix: *in.Body.PathPrefix,
		Project:    *in.Body.Project,
		Enabled:    enabled,
	})
	if err != nil {
		return nil, humaWorktreeMappingError(err)
	}
	return &createdOutput[db.WorktreeProjectMapping]{Status: http.StatusCreated, Body: mapping}, nil
}

func (s *Server) humaUpdateWorktreeMapping(
	ctx context.Context,
	in *worktreeMappingUpdateInput,
) (*jsonOutput[db.WorktreeProjectMapping], error) {
	localDB, machine, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	id, err := parseWorktreeMappingHumaID(in.ID)
	if err != nil {
		return nil, err
	}
	if in.Body.PathPrefix == nil || in.Body.Project == nil || in.Body.Enabled == nil {
		return nil, apiError(http.StatusBadRequest,
			"path_prefix, project, and enabled are required")
	}
	mapping, err := localDB.UpdateWorktreeProjectMapping(ctx, machine, id, db.WorktreeProjectMapping{
		PathPrefix: *in.Body.PathPrefix,
		Project:    *in.Body.Project,
		Enabled:    *in.Body.Enabled,
	})
	if err != nil {
		return nil, humaWorktreeMappingError(err)
	}
	return &jsonOutput[db.WorktreeProjectMapping]{Body: mapping}, nil
}

func (s *Server) humaDeleteWorktreeMapping(
	ctx context.Context,
	in *worktreeMappingPathInput,
) (*noContentOutput, error) {
	localDB, machine, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	id, err := parseWorktreeMappingHumaID(in.ID)
	if err != nil {
		return nil, err
	}
	err = localDB.DeleteWorktreeProjectMapping(ctx, machine, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apiError(http.StatusNotFound, "mapping not found")
	}
	if err != nil {
		return nil, internalError("delete worktree mapping", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func parseWorktreeMappingHumaID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, apiError(http.StatusNotFound, "mapping not found")
	}
	return id, nil
}

func (s *Server) humaApplyWorktreeMappings(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[applyWorktreeMappingsResponse], error) {
	localDB, machine, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	result, err := localDB.ApplyWorktreeProjectMappings(ctx, machine)
	if err != nil {
		return nil, internalError("apply worktree mappings", err)
	}
	return &jsonOutput[applyWorktreeMappingsResponse]{
		Body: applyWorktreeMappingsResponse{
			Machine:                            machine,
			ApplyWorktreeProjectMappingsResult: result,
		},
	}, nil
}

func humaWorktreeMappingError(err error) error {
	switch {
	case strings.Contains(err.Error(), "required"):
		return apiError(http.StatusBadRequest, err.Error())
	case errors.Is(err, db.ErrWorktreeMappingDuplicate):
		return apiError(http.StatusConflict, "worktree mapping already exists")
	case errors.Is(err, sql.ErrNoRows):
		return apiError(http.StatusNotFound, "mapping not found")
	default:
		return internalError("worktree mapping write", err)
	}
}
