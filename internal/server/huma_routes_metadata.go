package server

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/update"
)

func (s *Server) registerMetadataRoutes() {
	group := newRouteGroup(s.api, "/api/v1", "Metadata")

	get(s, group, "/projects", "List projects", s.humaListProjects)
	get(s, group, "/machines", "List machines", s.humaListMachines)
	get(s, group, "/branches", "List branches", s.humaListBranches)
	get(s, group, "/agents", "List agents", s.humaListAgents)
	get(s, group, "/stats", "Get stats", s.humaGetStats)
	get(s, group, "/session-stats", "Get session stats", s.humaGetSessionStats)
	get(s, group, "/version", "Get server version", s.humaGetVersion)
	get(s, group, "/update/check", "Check for updates", s.humaCheckUpdate)
}

type statsInput struct {
	BoolIncludeInput
}

type sessionStatsInput struct {
	BoolIncludeInput
	Since                 string   `query:"since" doc:"Start of window"`
	Until                 string   `query:"until" doc:"End of window"`
	Agent                 string   `query:"agent" doc:"Filter by agent"`
	IncludeProjects       []string `query:"include_project" doc:"Restrict to these projects"`
	ExcludeProjects       []string `query:"exclude_project" doc:"Exclude these projects"`
	Timezone              string   `query:"timezone" doc:"IANA timezone name"`
	IncludeGitOutcomes    bool     `query:"include_git_outcomes" doc:"Include git-derived outcome stats"`
	IncludeGitHubOutcomes bool     `query:"include_github_outcomes" doc:"Include GitHub PR outcome stats"`
}

type projectsResponse struct {
	Projects []db.ProjectInfo `json:"projects"`
}

type machinesResponse struct {
	Machines []string `json:"machines"`
}

type branchesResponse struct {
	Branches []db.BranchInfo `json:"branches"`
}

type agentsResponse struct {
	Agents []db.AgentInfo `json:"agents"`
}

func (s *Server) humaGetStats(
	ctx context.Context,
	in *statsInput,
) (*jsonOutput[db.Stats], error) {
	stats, err := s.db.GetStats(ctx, !in.IncludeOneShot, !in.IncludeAutomated)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[db.Stats]{Body: stats}, nil
}

func (s *Server) humaGetSessionStats(
	ctx context.Context,
	in *sessionStatsInput,
) (*jsonOutput[*service.SessionStats], error) {
	githubToken := ""
	if in.IncludeGitHubOutcomes {
		githubToken = s.githubToken(ctx)
	}
	stats, err := s.sessions.Stats(ctx, service.StatsFilter{
		ApplyDefaultVisibility: true,
		Since:                  in.Since,
		Until:                  in.Until,
		Agent:                  in.Agent,
		IncludeOneShot:         in.IncludeOneShot,
		IncludeAutomated:       in.IncludeAutomated,
		IncludeProjects:        in.IncludeProjects,
		ExcludeProjects:        in.ExcludeProjects,
		Timezone:               in.Timezone,
		IncludeGitOutcomes:     in.IncludeGitOutcomes,
		IncludeGitHubOutcomes:  in.IncludeGitHubOutcomes,
		GHToken:                githubToken,
	})
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		var inputErr *db.StatsInputError
		if errors.As(err, &inputErr) {
			return nil, apiError(http.StatusBadRequest, inputErr.Msg)
		}
		return nil, internalError("session stats error", err)
	}
	return &jsonOutput[*service.SessionStats]{Body: stats}, nil
}

func (s *Server) humaListProjects(
	ctx context.Context,
	in *statsInput,
) (*jsonOutput[projectsResponse], error) {
	projects, err := s.db.GetProjects(ctx, !in.IncludeOneShot, !in.IncludeAutomated)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[projectsResponse]{Body: projectsResponse{Projects: projects}}, nil
}

func (s *Server) humaListMachines(
	ctx context.Context,
	in *statsInput,
) (*jsonOutput[machinesResponse], error) {
	machines, err := s.db.GetMachines(ctx, !in.IncludeOneShot, !in.IncludeAutomated)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[machinesResponse]{Body: machinesResponse{Machines: machines}}, nil
}

type branchScopeParam string

type branchesInput struct {
	BoolIncludeInput
	Scope branchScopeParam `query:"scope" enum:"roots,all" doc:"Session scope: roots (default) counts only root sessions; all also counts subagent and fork sessions, matching the activity and usage rollups"`
}

func (s *Server) humaListBranches(
	ctx context.Context,
	in *branchesInput,
) (*jsonOutput[branchesResponse], error) {
	scope := db.BranchScopeRoots
	if in.Scope == "all" {
		scope = db.BranchScopeAll
	}
	branches, err := s.db.GetBranches(
		ctx, scope, !in.IncludeOneShot, !in.IncludeAutomated)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[branchesResponse]{Body: branchesResponse{Branches: branches}}, nil
}

func (s *Server) humaListAgents(
	ctx context.Context,
	in *statsInput,
) (*jsonOutput[agentsResponse], error) {
	agents, err := s.db.GetAgents(ctx, !in.IncludeOneShot, !in.IncludeAutomated)
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[agentsResponse]{Body: agentsResponse{Agents: agents}}, nil
}

func (s *Server) humaGetVersion(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[VersionInfo], error) {
	return &jsonOutput[VersionInfo]{Body: s.version}, nil
}

func (s *Server) humaCheckUpdate(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[updateCheckResponse], error) {
	if s.cfg.DisableUpdateCheck {
		return &jsonOutput[updateCheckResponse]{
			Body: updateCheckResponse{CurrentVersion: s.version.Version},
		}, nil
	}
	checkFn := s.updateCheckFn
	if checkFn == nil {
		checkFn = update.CheckForUpdate
	}
	info, err := checkFn(s.version.Version, false, s.dataDir)
	if err != nil || info == nil {
		return &jsonOutput[updateCheckResponse]{
			Body: updateCheckResponse{CurrentVersion: s.version.Version},
		}, nil
	}
	return &jsonOutput[updateCheckResponse]{
		Body: updateCheckResponse{
			UpdateAvailable: !info.IsDevBuild,
			CurrentVersion:  info.CurrentVersion,
			LatestVersion:   info.LatestVersion,
			IsDevBuild:      info.IsDevBuild,
		},
	}, nil
}
