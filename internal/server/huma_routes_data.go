package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func (s *Server) registerDataRoutes() {
	group := newRouteGroup(s.api, "/api/v1/data", "Data")
	s.get(group, "/projects", "Get project inventory", s.humaDataProjects)
	s.get(group, "/projects/{project_key}/sessions",
		"List sessions for an opaque project identity", s.humaDataProjectSessions)
	s.get(group, "/project-rules", "List project rules", s.humaDataProjectRules)
	s.get(group, "/project-reclassification/candidates",
		"List archive-wide reclassification candidates",
		s.humaDataCandidates)
	s.postLong(group, "/compact", "Compact local archive", s.humaDataCompact)
}

type dataProjectRulesInput struct {
	Machine string `query:"machine" doc:"Machine to list rules for"`
}

type dataProjectRulesResponse struct {
	db.ProjectRules
	LocalMachine string `json:"local_machine"`
}

type dataCandidatesInput struct {
	ProjectLabel string `query:"project_label" doc:"Project display label"`
	ProjectKey   string `query:"project_key" required:"true" doc:"Opaque project identity key"`
}

type dataCandidatesResponse struct {
	Candidates []db.WorktreeReclassificationCandidate `json:"candidates"`
}

type dataCompactInput struct {
	Body dataCompactRequest
}

type dataCompactRequest struct {
	// The daemon chooses its own staging directory. Accepting a client path
	// here would let any authenticated remote caller make the daemon copy a
	// complete archive and backup into an arbitrary filesystem location.
	KeepBackup *bool `json:"keep_backup,omitempty" doc:"Keep the original archive backup"`
}

type dataProjectSessionsInput struct {
	ProjectKey string `path:"project_key" required:"true" doc:"Opaque project identity key"`
}

func (s *Server) humaDataProjects(
	ctx context.Context, _ *emptyInput,
) (*jsonOutput[db.ProjectInventory], error) {
	inv, err := s.db.GetProjectInventory(ctx)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("get project inventory error", err)
	}
	return &jsonOutput[db.ProjectInventory]{Body: inv}, nil
}

func (s *Server) humaDataProjectSessions(
	ctx context.Context, in *dataProjectSessionsInput,
) (*jsonOutput[db.SessionPage], error) {
	projectKey := strings.TrimSpace(in.ProjectKey)
	if projectKey == "" {
		return nil, apiError(http.StatusBadRequest, "project_key is required")
	}
	labels, err := s.db.GetActiveProjectLabels(ctx)
	if err != nil {
		return nil, internalError("list project labels", err)
	}
	catalog, err := s.db.BuildProjectIdentityMap(ctx, labels)
	if err != nil {
		return nil, internalError("resolve project identities", err)
	}
	resolved := make([]string, 0, 1)
	for label, entry := range catalog {
		if entry.ProjectKey == projectKey {
			resolved = append(resolved, label)
		}
	}
	if len(resolved) == 0 {
		return nil, apiError(http.StatusNotFound, "project not found")
	}
	page, err := s.db.ListSessions(ctx, db.SessionFilter{
		ProjectLabels:   resolved,
		IncludeChildren: true,
		Limit:           20,
		OrderBy:         "recent",
	})
	if err != nil {
		return nil, internalError("list project sessions", err)
	}
	return &jsonOutput[db.SessionPage]{Body: page}, nil
}

func (s *Server) localMachineName() string {
	if s.engine != nil {
		if machine := strings.TrimSpace(s.engine.Machine()); machine != "" {
			return machine
		}
	}
	return s.cfg.LocalMachineName
}

func (s *Server) humaDataProjectRules(
	ctx context.Context, in *dataProjectRulesInput,
) (*jsonOutput[dataProjectRulesResponse], error) {
	localMachine := s.localMachineName()
	machine := strings.TrimSpace(in.Machine)
	if machine == "" {
		machine = localMachine
	}
	rules, err := s.db.ListProjectRules(ctx, machine)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("list project rules error", err)
	}
	return &jsonOutput[dataProjectRulesResponse]{
		Body: dataProjectRulesResponse{ProjectRules: rules, LocalMachine: localMachine},
	}, nil
}

func (s *Server) humaDataCandidates(
	ctx context.Context, in *dataCandidatesInput,
) (*jsonOutput[dataCandidatesResponse], error) {
	if strings.TrimSpace(in.ProjectKey) == "" {
		return nil, apiError(http.StatusBadRequest, "project_key is required")
	}
	candidates, err := s.db.ListArchiveWorktreeCandidates(ctx,
		db.ArchiveWorktreeCandidateRequest{
			ProjectLabel: in.ProjectLabel,
			ProjectKey:   in.ProjectKey,
		})
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("list archive-wide reclassification candidates error", err)
	}
	return &jsonOutput[dataCandidatesResponse]{
		Body: dataCandidatesResponse{Candidates: candidates},
	}, nil
}

func (s *Server) humaDataCompact(
	ctx context.Context, in *dataCompactInput,
) (*jsonOutput[db.CompactResult], error) {
	if !isLocalhostContext(ctx) {
		return nil, apiError(
			http.StatusForbidden,
			"archive compaction is only permitted from localhost",
		)
	}
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	options := db.CompactOptions{}
	if in != nil {
		if in.Body.KeepBackup != nil {
			options.KeepBackup = *in.Body.KeepBackup
		}
	}
	var (
		result db.CompactResult
		err    error
	)
	if s.localCompactRunner != nil {
		result, err = s.localCompactRunner(ctx, options)
	} else {
		err = s.serializeArchiveWrite(func() error {
			result, err = local.Compact(ctx, options)
			return err
		})
	}
	if errors.Is(err, syncpkg.ErrSyncInProgress) ||
		errors.Is(err, db.ErrCompactInProgress) {
		return nil, apiError(
			http.StatusConflict,
			"another archive maintenance operation is already running",
		)
	}
	if handled := handleHumaContextError(err); handled != nil {
		return nil, handled
	}
	if handled := handleHumaReadOnly(err); handled != nil {
		return nil, handled
	}
	if err != nil {
		return nil, internalError("compact local archive error", err)
	}
	return &jsonOutput[db.CompactResult]{Body: result}, nil
}
