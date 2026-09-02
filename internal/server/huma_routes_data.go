package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

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
	s.postLong(group, "/strip-images/preview",
		"Preview inline tool-result image removal", s.humaDataStripImagesPreview)
	s.postLong(group, "/strip-images",
		"Remove retained inline tool-result images", s.humaDataStripImages)
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

type dataStripImagesPreviewInput struct {
	Body dataStripImagesRequest
}

type dataStripImagesInput struct {
	Body dataStripImagesApplyRequest
}

// The request carries the whole selection. The server re-evaluates it under
// the archive write boundary instead of accepting a client-held preview,
// because ingestion between preview and apply can add matching rows and
// those rows are inside the selection the caller asked for. Values are used
// as sent: cmd/agentsview/db_strip.go builds the same filter from raw flag
// bytes, so trimming here would make the two surfaces select differently.
type dataStripImagesRequest struct {
	Project string `json:"project,omitempty" doc:"Sessions whose project contains this substring; empty matches all projects"`
	Before  string `json:"before,omitempty" doc:"Sessions that ended before this date (YYYY-MM-DD); empty applies no date bound"`
}

// Confirmed is required and must be true. An empty body would otherwise select
// every stored image in the archive, so acceptance of the submitted filter is
// explicit. It is not authentication and not evidence that a preview ran.
type dataStripImagesApplyRequest struct {
	Project   string `json:"project,omitempty" doc:"Sessions whose project contains this substring; empty matches all projects"`
	Before    string `json:"before,omitempty" doc:"Sessions that ended before this date (YYYY-MM-DD); empty applies no date bound"`
	Confirmed *bool  `json:"confirmed,omitempty" doc:"Must be true; confirms cleanup of every session matching the filter when the run starts"`
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
	return s.cfg.InstallationID
}

func (s *Server) humaDataProjectRules(
	ctx context.Context, in *dataProjectRulesInput,
) (*jsonOutput[dataProjectRulesResponse], error) {
	localMachine := s.localMachineName()
	machine := strings.TrimSpace(in.Machine)
	if machine == "" {
		machine = localMachine
	}
	machine, err := db.ResolveMachineFilter(ctx, s.db, machine)
	if err != nil {
		return nil, serverError(err)
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
		err = s.tryArchiveWrite(func() error {
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

// stripImagesTarget applies the same access gates as archive compaction and
// builds the selection. Localhost is checked before the backend type so a
// remote caller cannot probe which backend is serving.
func (s *Server) stripImagesTarget(
	ctx context.Context, project, before string,
) (*db.DB, db.StripImagesFilter, error) {
	if !isLocalhostContext(ctx) {
		return nil, db.StripImagesFilter{}, apiError(
			http.StatusForbidden,
			"tool-result image removal is only permitted from localhost",
		)
	}
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil, db.StripImagesFilter{}, apiError(
			http.StatusNotImplemented, "not available in remote mode",
		)
	}
	if before != "" {
		if _, err := time.Parse("2006-01-02", before); err != nil {
			return nil, db.StripImagesFilter{}, apiError(
				http.StatusBadRequest, "before must be a YYYY-MM-DD date",
			)
		}
	}
	return local, db.StripImagesFilter{Project: project, Before: before}, nil
}

func (s *Server) humaDataStripImagesPreview(
	ctx context.Context, in *dataStripImagesPreviewInput,
) (*jsonOutput[db.StripImagesReport], error) {
	var project, before string
	if in != nil {
		project, before = in.Body.Project, in.Body.Before
	}
	local, filter, err := s.stripImagesTarget(ctx, project, before)
	if err != nil {
		return nil, err
	}
	report, err := local.PreviewStripToolImages(ctx, filter)
	if handled := handleHumaContextError(err); handled != nil {
		return nil, handled
	}
	if handled := handleHumaReadOnly(err); handled != nil {
		return nil, handled
	}
	if err != nil {
		return nil, internalError("preview tool result image removal error", err)
	}
	return &jsonOutput[db.StripImagesReport]{Body: report}, nil
}

func (s *Server) humaDataStripImages(
	ctx context.Context, in *dataStripImagesInput,
) (*jsonOutput[db.StripImagesReport], error) {
	if in == nil || in.Body.Confirmed == nil || !*in.Body.Confirmed {
		return nil, apiError(http.StatusBadRequest,
			"confirmed must be true to remove stored image payloads")
	}
	local, filter, err := s.stripImagesTarget(ctx, in.Body.Project, in.Body.Before)
	if err != nil {
		return nil, err
	}
	var report db.StripImagesReport
	// StripToolImages documents that the caller owns the archive write lock;
	// the daemon's foreground exclusive boundary is that ownership, not the
	// CLI flock, and it refuses rather than queues behind a worker pass.
	err = s.tryArchiveWrite(func() error {
		var stripErr error
		report, stripErr = local.StripToolImages(ctx, filter)
		// An error can follow per-session commits, even with an empty report.
		if stripErr != nil || report.Changed > 0 {
			s.notifySessionMutation()
		}
		if report.Changed > 0 && s.broadcaster != nil {
			s.broadcaster.Emit("sessions")
		}
		return stripErr
	})
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
		return nil, internalError("strip tool result images error", err)
	}
	return &jsonOutput[db.StripImagesReport]{Body: report}, nil
}
