package server

import "go.kenn.io/agentsview/internal/db"

type worktreeMappingsResponse struct {
	Machine      string                      `json:"machine"`
	LocalMachine string                      `json:"local_machine"`
	Machines     []string                    `json:"machines"`
	Mappings     []db.WorktreeProjectMapping `json:"mappings"`
}

type worktreeMappingRequest struct {
	PathPrefix      *string `json:"path_prefix,omitempty"`
	Layout          *string `json:"layout,omitempty"`
	Project         *string `json:"project,omitempty"`
	OriginalProject *string `json:"original_project,omitempty"`
	Enabled         *bool   `json:"enabled,omitempty"`
	Machine         *string `json:"machine,omitempty"`
}

type applyWorktreeMappingsRequest struct {
	Machine *string `json:"machine,omitempty"`
}

type applyWorktreeMappingsResponse struct {
	Machine string `json:"machine"`
	db.ApplyWorktreeProjectMappingsResult
}

type worktreeReclassificationRequest struct {
	Machine         string `json:"machine"`
	PathPrefix      string `json:"path_prefix"`
	Layout          string `json:"layout,omitempty"`
	Project         string `json:"project"`
	OriginalProject string `json:"original_project,omitempty"`
	Enabled         *bool  `json:"enabled,omitempty"`
}

func (r worktreeReclassificationRequest) draft() db.WorktreeReclassificationDraft {
	enabled := true
	if r.Enabled != nil {
		enabled = *r.Enabled
	}
	layout := r.Layout
	if layout == "" {
		layout = db.WorktreeMappingLayoutExplicit
	}
	return db.WorktreeReclassificationDraft{
		Machine: r.Machine, PathPrefix: r.PathPrefix, Layout: layout,
		Project: r.Project, OriginalProject: r.OriginalProject, Enabled: enabled,
	}
}

type worktreeReclassificationApplyRequest struct {
	Machine         string `json:"machine"`
	PathPrefix      string `json:"path_prefix"`
	Layout          string `json:"layout,omitempty"`
	Project         string `json:"project"`
	OriginalProject string `json:"original_project,omitempty"`
	Enabled         *bool  `json:"enabled,omitempty"`
	MappingToken    string `json:"mapping_token"`
}

func (r worktreeReclassificationApplyRequest) draft() db.WorktreeReclassificationDraft {
	return worktreeReclassificationRequest{
		Machine: r.Machine, PathPrefix: r.PathPrefix, Layout: r.Layout,
		Project: r.Project, OriginalProject: r.OriginalProject, Enabled: r.Enabled,
	}.draft()
}

type worktreeReclassificationApplyResponse struct {
	Mapping db.WorktreeProjectMapping          `json:"mapping"`
	Result  db.WorktreeReclassificationPreview `json:"result"`
}
