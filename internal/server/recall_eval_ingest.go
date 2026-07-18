//go:build evalingest

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// maxEvalFieldRunes caps the length of the required identifier-like eval
// ingest fields (extractor_method, source_version) at the HTTP boundary,
// mirroring the same cap enforced in the db layer.
const maxEvalFieldRunes = 200

type evalTrajectoryIngestRequest struct {
	RunID           string          `json:"run_id"`
	TrajectoryID    string          `json:"trajectory_id"`
	Trajectory      json.RawMessage `json:"trajectory"`
	ExtractorMethod string          `json:"extractor_method"`
	SourceVersion   string          `json:"source_version"`
	Project         string          `json:"project"`
	CWD             string          `json:"cwd"`
	GitBranch       string          `json:"git_branch"`
	Agent           string          `json:"agent"`
}

// registerEvalIngestRoutes wires the raw-trajectory eval ingest endpoint.
// It only exists in the evalingest build: the endpoint is a lab-only
// surface and the default binary does not serve it (see
// recall_eval_ingest_off.go).
func (s *Server) registerEvalIngestRoutes() {
	s.mux.Handle(
		"POST /api/v1/recall/eval/trajectories",
		s.withTimeout(
			"POST /api/v1/recall/eval/trajectories",
			s.handleIngestEvalTrajectory,
		),
	)
}

// handleIngestEvalTrajectory ingests one raw eval trajectory as chunked,
// FTS-indexed recall rows for lab-only keyword recall. It mirrors
// handleImportRecallEntries: the production-data-dir guard reads its override from a
// query parameter before the body is decoded, so the refusal fires before a
// potentially large trajectory is parsed. The harness runs against a throwaway
// DB, so it passes without the override.
func (s *Server) handleIngestEvalTrajectory(
	w http.ResponseWriter, r *http.Request,
) {
	allowProductionImport, ok := parseBoolParam(
		w, r, "allow_production_import",
	)
	if !ok {
		return
	}
	if !allowProductionImport &&
		(config.IsDefaultAgentsviewDataDir(s.cfg.DataDir) ||
			config.IsDefaultAgentsviewDBPath(s.cfg.DBPath)) {
		writeError(
			w,
			http.StatusForbidden,
			"eval trajectory ingest refuses to write against the default agentsview data directory; set allow_production_import=true only when intentionally targeting that archive",
		)
		return
	}
	var req evalTrajectoryIngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.RunID) == "" {
		writeError(w, http.StatusBadRequest, "run_id is required")
		return
	}
	if strings.TrimSpace(req.TrajectoryID) == "" {
		writeError(w, http.StatusBadRequest, "trajectory_id is required")
		return
	}
	if msg := evalRequiredFieldError("extractor_method", req.ExtractorMethod); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if msg := evalRequiredFieldError("source_version", req.SourceVersion); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if isEmptyTrajectory(req.Trajectory) {
		writeError(w, http.StatusBadRequest, "trajectory is required")
		return
	}
	result, err := s.db.IngestEvalTrajectory(
		r.Context(),
		db.EvalTrajectoryIngest{
			RunID:           req.RunID,
			TrajectoryID:    req.TrajectoryID,
			Trajectory:      req.Trajectory,
			ExtractorMethod: req.ExtractorMethod,
			SourceVersion:   req.SourceVersion,
			Project:         req.Project,
			CWD:             req.CWD,
			GitBranch:       req.GitBranch,
			Agent:           req.Agent,
		},
	)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		if handleReadOnly(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// isEmptyTrajectory reports whether the raw trajectory field is absent or an
// explicit JSON null, both of which fail the "trajectory is required" check. A
// present-but-empty object (e.g. {}) is not empty here: it ingests to zero
// chunks, a successful no-op.
func isEmptyTrajectory(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}

// evalRequiredFieldError returns a 400-ready message if value is empty or
// exceeds maxEvalFieldRunes once trimmed, or "" if value is valid.
func evalRequiredFieldError(field, value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return field + " is required"
	}
	if len([]rune(trimmed)) > maxEvalFieldRunes {
		return fmt.Sprintf(
			"%s exceeds maximum length of %d characters", field, maxEvalFieldRunes,
		)
	}
	return ""
}
