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
	post(s, group, "/worktree-mappings/preview",
		"Preview worktree project reclassification", s.humaPreviewWorktreeReclassification)
	post(s, group, "/worktree-mappings/reclassify",
		"Apply worktree project reclassification", s.humaReclassifyWorktreeProject)
}

type settingsInput struct {
	Body settingsUpdateRequest
}

type worktreeMappingCreateInput struct {
	Body worktreeMappingRequest
}

type worktreeMappingListInput struct {
	Machine string `query:"machine" doc:"Machine whose mappings should be listed"`
}

type worktreeMappingUpdateInput struct {
	ID   string `path:"id" required:"true" doc:"Mapping ID"`
	Body worktreeMappingRequest
}

type worktreeMappingPathInput struct {
	ID string `path:"id" required:"true" doc:"Mapping ID"`
}

type worktreeMappingApplyInput struct {
	Body applyWorktreeMappingsRequest
}

type worktreeReclassificationPreviewInput struct {
	Body worktreeReclassificationRequest
}

type worktreeReclassificationApplyInput struct {
	Body worktreeReclassificationApplyRequest
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
		machine = s.cfg.LocalMachineName
	}
	return localDB, machine, nil
}

func (s *Server) humaListWorktreeMappings(
	ctx context.Context,
	in *worktreeMappingListInput,
) (*jsonOutput[worktreeMappingsResponse], error) {
	localDB, localMachine, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	machine := strings.TrimSpace(in.Machine)
	if machine == "" {
		machine = localMachine
	}
	mappings, err := localDB.ListWorktreeProjectMappings(ctx, machine)
	if err != nil {
		return nil, internalError("list worktree mappings", err)
	}
	machines, err := localDB.ListWorktreeProjectMappingMachines(ctx)
	if err != nil {
		return nil, internalError("list worktree mapping machines", err)
	}
	return &jsonOutput[worktreeMappingsResponse]{
		Body: worktreeMappingsResponse{
			Machine: machine, LocalMachine: localMachine,
			Machines: machines, Mappings: mappings,
		},
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
	if in.Body.PathPrefix == nil {
		return nil, apiError(http.StatusBadRequest, "path_prefix is required")
	}
	if in.Body.Machine != nil && strings.TrimSpace(*in.Body.Machine) != "" {
		machine = strings.TrimSpace(*in.Body.Machine)
	}
	enabled := true
	if in.Body.Enabled != nil {
		enabled = *in.Body.Enabled
	}
	mapping, err := localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine:         machine,
		PathPrefix:      *in.Body.PathPrefix,
		Layout:          valueOrDefault(in.Body.Layout, db.WorktreeMappingLayoutExplicit),
		Project:         stringValueOrEmpty(in.Body.Project),
		OriginalProject: stringValueOrEmpty(in.Body.OriginalProject),
		Enabled:         enabled,
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
	localDB, _, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	id, err := parseWorktreeMappingHumaID(in.ID)
	if err != nil {
		return nil, err
	}
	existing, err := localDB.GetWorktreeProjectMapping(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apiError(http.StatusNotFound, "mapping not found")
	}
	if err != nil {
		return nil, internalError("get worktree mapping", err)
	}
	if in.Body.PathPrefix == nil || in.Body.Enabled == nil {
		return nil, apiError(http.StatusBadRequest,
			"path_prefix and enabled are required")
	}
	mapping, err := localDB.UpdateWorktreeProjectMapping(ctx, existing.Machine, id, db.WorktreeProjectMapping{
		PathPrefix:      *in.Body.PathPrefix,
		Layout:          valueOrDefault(in.Body.Layout, db.WorktreeMappingLayoutExplicit),
		Project:         stringValueOrEmpty(in.Body.Project),
		OriginalProject: stringValueOrEmpty(in.Body.OriginalProject),
		Enabled:         *in.Body.Enabled,
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
	localDB, _, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	id, err := parseWorktreeMappingHumaID(in.ID)
	if err != nil {
		return nil, err
	}
	existing, err := localDB.GetWorktreeProjectMapping(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apiError(http.StatusNotFound, "mapping not found")
	}
	if err != nil {
		return nil, internalError("get worktree mapping", err)
	}
	err = localDB.DeleteWorktreeProjectMapping(ctx, existing.Machine, id)
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
	in *worktreeMappingApplyInput,
) (*jsonOutput[applyWorktreeMappingsResponse], error) {
	localDB, machine, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	if in.Body.Machine != nil && strings.TrimSpace(*in.Body.Machine) != "" {
		machine = strings.TrimSpace(*in.Body.Machine)
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

func (s *Server) humaPreviewWorktreeReclassification(
	ctx context.Context,
	in *worktreeReclassificationPreviewInput,
) (*jsonOutput[db.WorktreeReclassificationPreview], error) {
	localDB, _, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	preview, err := localDB.PreviewWorktreeReclassification(ctx, in.Body.draft())
	if err != nil {
		return nil, humaWorktreeReclassificationError(err)
	}
	return &jsonOutput[db.WorktreeReclassificationPreview]{Body: preview}, nil
}

func (s *Server) humaReclassifyWorktreeProject(
	ctx context.Context,
	in *worktreeReclassificationApplyInput,
) (*jsonOutput[worktreeReclassificationApplyResponse], error) {
	localDB, _, err := s.localWorktreeMappingHumaDB()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Body.MappingToken) == "" {
		return nil, apiError(http.StatusBadRequest, "mapping_token is required")
	}
	draft := in.Body.draft()
	// The server, not the client, resolves the exact (machine, prefix)
	// collision. Apply rechecks both this identity and the accepted mapping-set
	// token under the engine's exclusive write boundary.
	current, err := localDB.PreviewWorktreeReclassification(ctx, draft)
	if err != nil {
		return nil, humaWorktreeReclassificationError(err)
	}
	mapping, result, err := s.engine.ApplyWorktreeReclassification(
		ctx, draft, in.Body.MappingToken, current.ExistingMappingID,
	)
	if err != nil {
		return nil, humaWorktreeReclassificationError(err)
	}
	return &jsonOutput[worktreeReclassificationApplyResponse]{
		Body: worktreeReclassificationApplyResponse{Mapping: mapping, Result: result},
	}, nil
}

func humaWorktreeReclassificationError(err error) error {
	switch {
	case errors.Is(err, db.ErrWriterClosed):
		return writerClosedError()
	case errors.Is(err, db.ErrWorktreeMappingSetChanged):
		return apiError(http.StatusConflict, err.Error())
	case errors.Is(err, db.ErrWorktreeMappingInvalid):
		return apiError(http.StatusBadRequest, err.Error())
	case strings.Contains(err.Error(), "required"):
		return apiError(http.StatusBadRequest, err.Error())
	default:
		return internalError("worktree reclassification", err)
	}
}

func humaWorktreeMappingError(err error) error {
	switch {
	case errors.Is(err, db.ErrWorktreeMappingInvalid):
		return apiError(http.StatusBadRequest, err.Error())
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

func valueOrDefault(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

func stringValueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
