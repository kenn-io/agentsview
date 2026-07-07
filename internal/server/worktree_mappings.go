package server

import "go.kenn.io/agentsview/internal/db"

type worktreeMappingsResponse struct {
	Machine  string                      `json:"machine"`
	Mappings []db.WorktreeProjectMapping `json:"mappings"`
}

type worktreeMappingRequest struct {
	PathPrefix *string `json:"path_prefix,omitempty"`
	Layout     *string `json:"layout,omitempty"`
	Project    *string `json:"project,omitempty"`
	Enabled    *bool   `json:"enabled,omitempty"`
	Machine    *string `json:"machine,omitempty"`
}

type applyWorktreeMappingsResponse struct {
	Machine string `json:"machine"`
	db.ApplyWorktreeProjectMappingsResult
}
