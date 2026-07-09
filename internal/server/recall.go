package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func handleContextError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, "gateway timeout")
		return true
	}
	return false
}

func handleReadOnly(w http.ResponseWriter, err error) bool {
	if errors.Is(err, db.ErrReadOnly) {
		writeError(w, http.StatusNotImplemented, "not available in remote mode")
		return true
	}
	return false
}

type recallQueryRequest struct {
	Query               string `json:"query"`
	Project             string `json:"project"`
	CWD                 string `json:"cwd"`
	GitBranch           string `json:"git_branch"`
	Agent               string `json:"agent"`
	Type                string `json:"type"`
	Scope               string `json:"scope"`
	Status              string `json:"status"`
	ExtractorMethod     string `json:"extractor_method"`
	SourceSessionID     string `json:"source_session_id"`
	SourceEpisodeID     string `json:"source_episode_id"`
	SourceRunID         string `json:"source_run_id"`
	SupersedesEntryID   string `json:"supersedes_entry_id"`
	SupersededByEntryID string `json:"superseded_by_entry_id"`
	TrustedOnly         bool   `json:"trusted_only"`
	Limit               int    `json:"limit"`
	IncludeContext      bool   `json:"include_context"`
	ContextMaxBytes     int    `json:"context_max_bytes"`
}

type recallQueryResponse struct {
	RecallEntries  []db.RecallResult           `json:"entries"`
	TrustedOnly    bool                        `json:"trusted_only"`
	Summary        *service.RecallQuerySummary `json:"summary,omitempty"`
	Context        string                      `json:"context,omitempty"`
	ContextMeta    *service.RecallContextMeta  `json:"context_meta,omitempty"`
	ContextEntries []db.RecallResult           `json:"context_entries,omitempty"`
	ContextSummary *service.RecallQuerySummary `json:"context_summary,omitempty"`
}

func (s *Server) handleListRecallEntries(
	w http.ResponseWriter, r *http.Request,
) {
	q := r.URL.Query()
	limit, ok := parseIntParam(w, r, "limit")
	if !ok {
		return
	}
	if err := service.ValidateRecallEntryLimit(limit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	trustedOnly, ok := parseBoolParam(w, r, "trusted_only")
	if !ok {
		return
	}
	query := db.RecallQuery{
		Text:                q.Get("q"),
		Project:             q.Get("project"),
		CWD:                 q.Get("cwd"),
		GitBranch:           q.Get("git_branch"),
		Agent:               q.Get("agent"),
		Type:                q.Get("type"),
		Scope:               q.Get("scope"),
		Status:              q.Get("status"),
		ExtractorMethod:     q.Get("extractor_method"),
		SourceSessionID:     q.Get("source_session_id"),
		SourceEpisodeID:     q.Get("source_episode_id"),
		SourceRunID:         q.Get("source_run_id"),
		SupersedesEntryID:   q.Get("supersedes_entry_id"),
		SupersededByEntryID: q.Get("superseded_by_entry_id"),
		TrustedOnly:         trustedOnly,
		Limit:               limit,
	}
	page, err := s.db.QueryRecallEntries(r.Context(), query)
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
	if page.RecallEntries == nil {
		page.RecallEntries = []db.RecallResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":      page.RecallEntries,
		"trusted_only": query.TrustedOnly,
	})
}

func (s *Server) handleGetRecallEntry(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")
	recall, err := s.db.GetRecallEntry(r.Context(), id)
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
	if recall == nil {
		writeError(w, http.StatusNotFound, "recall entry not found")
		return
	}
	writeJSON(w, http.StatusOK, recall)
}

func (s *Server) handleQueryRecallEntries(
	w http.ResponseWriter, r *http.Request,
) {
	var req recallQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := service.ValidateRecallEntryLimit(req.Limit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := s.db.QueryRecallEntries(r.Context(), db.RecallQuery{
		Text:                req.Query,
		Project:             req.Project,
		CWD:                 req.CWD,
		GitBranch:           req.GitBranch,
		Agent:               req.Agent,
		Type:                req.Type,
		Scope:               req.Scope,
		Status:              req.Status,
		ExtractorMethod:     req.ExtractorMethod,
		SourceSessionID:     req.SourceSessionID,
		SourceEpisodeID:     req.SourceEpisodeID,
		SourceRunID:         req.SourceRunID,
		SupersedesEntryID:   req.SupersedesEntryID,
		SupersededByEntryID: req.SupersededByEntryID,
		TrustedOnly:         req.TrustedOnly,
		Limit:               req.Limit,
	})
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
	if page.RecallEntries == nil {
		page.RecallEntries = []db.RecallResult{}
	}
	resp := recallQueryResponse{
		RecallEntries: page.RecallEntries,
		TrustedOnly:   req.TrustedOnly,
		Summary:       service.BuildRecallQuerySummary(page.RecallEntries),
	}
	if req.IncludeContext {
		contextText, contextMeta, err := service.BuildRecallContext(
			page.RecallEntries, req.ContextMaxBytes, req.Query,
		)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp.Context = contextText
		resp.ContextMeta = contextMeta
		resp.ContextEntries = service.RecallContextResults(
			page.RecallEntries, contextMeta,
		)
		resp.ContextSummary = service.BuildRecallContextSummary(
			page.RecallEntries, contextMeta,
		)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleImportRecallEntries(
	w http.ResponseWriter, r *http.Request,
) {
	dryRun, ok := parseBoolParam(w, r, "dry_run")
	if !ok {
		return
	}
	allowProductionImport, ok := parseBoolParam(
		w, r, "allow_production_import",
	)
	if !ok {
		return
	}
	requireExistingSessions, ok := recallImportRequiresExistingSessions(w, r)
	if !ok {
		return
	}
	if !allowProductionImport &&
		(config.IsDefaultAgentsviewDataDir(s.cfg.DataDir) ||
			config.IsDefaultAgentsviewDBPath(s.cfg.DBPath)) {
		writeError(
			w,
			http.StatusForbidden,
			"recall import refuses to validate or write against the default agentsview data directory; set allow_production_import=true only when intentionally targeting that archive",
		)
		return
	}
	result, err := s.db.ImportAcceptedRecallEntriesJSONLWithOptions(
		r.Context(),
		r.Body,
		db.RecallImportOptions{
			DryRun:                  dryRun,
			RequireExistingSessions: requireExistingSessions,
			AllowProductionImport:   allowProductionImport,
		},
	)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		if handleReadOnly(w, err) {
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func recallImportRequiresExistingSessions(
	w http.ResponseWriter, r *http.Request,
) (bool, bool) {
	requireExisting, ok := parseBoolParam(w, r, "require_existing_sessions")
	if !ok {
		return false, false
	}
	allowPlaceholder, ok := parseBoolParam(w, r, "allow_placeholder_sessions")
	if !ok {
		return false, false
	}
	if requireExisting {
		return true, true
	}
	return !allowPlaceholder, true
}
