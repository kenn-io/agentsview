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

type memoryQueryRequest struct {
	Query                string `json:"query"`
	Project              string `json:"project"`
	CWD                  string `json:"cwd"`
	GitBranch            string `json:"git_branch"`
	Agent                string `json:"agent"`
	Type                 string `json:"type"`
	Scope                string `json:"scope"`
	Status               string `json:"status"`
	ExtractorMethod      string `json:"extractor_method"`
	SourceSessionID      string `json:"source_session_id"`
	SourceEpisodeID      string `json:"source_episode_id"`
	SourceRunID          string `json:"source_run_id"`
	SupersedesMemoryID   string `json:"supersedes_memory_id"`
	SupersededByMemoryID string `json:"superseded_by_memory_id"`
	TrustedOnly          bool   `json:"trusted_only"`
	Limit                int    `json:"limit"`
	IncludeContext       bool   `json:"include_context"`
	ContextMaxBytes      int    `json:"context_max_bytes"`
}

type memoryQueryResponse struct {
	Memories        []db.MemoryResult           `json:"memories"`
	TrustedOnly     bool                        `json:"trusted_only"`
	Summary         *service.MemoryQuerySummary `json:"summary,omitempty"`
	Context         string                      `json:"context,omitempty"`
	ContextMeta     *service.MemoryContextMeta  `json:"context_meta,omitempty"`
	ContextMemories []db.MemoryResult           `json:"context_memories,omitempty"`
	ContextSummary  *service.MemoryQuerySummary `json:"context_summary,omitempty"`
}

func (s *Server) handleListMemories(
	w http.ResponseWriter, r *http.Request,
) {
	q := r.URL.Query()
	limit, ok := parseIntParam(w, r, "limit")
	if !ok {
		return
	}
	if err := service.ValidateMemoryLimit(limit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	trustedOnly, ok := parseBoolParam(w, r, "trusted_only")
	if !ok {
		return
	}
	query := db.MemoryQuery{
		Text:                 q.Get("q"),
		Project:              q.Get("project"),
		CWD:                  q.Get("cwd"),
		GitBranch:            q.Get("git_branch"),
		Agent:                q.Get("agent"),
		Type:                 q.Get("type"),
		Scope:                q.Get("scope"),
		Status:               q.Get("status"),
		ExtractorMethod:      q.Get("extractor_method"),
		SourceSessionID:      q.Get("source_session_id"),
		SourceEpisodeID:      q.Get("source_episode_id"),
		SourceRunID:          q.Get("source_run_id"),
		SupersedesMemoryID:   q.Get("supersedes_memory_id"),
		SupersededByMemoryID: q.Get("superseded_by_memory_id"),
		TrustedOnly:          trustedOnly,
		Limit:                limit,
	}
	page, err := s.db.QueryMemories(r.Context(), query)
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
	if page.Memories == nil {
		page.Memories = []db.MemoryResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"memories":     page.Memories,
		"trusted_only": query.TrustedOnly,
	})
}

func (s *Server) handleGetMemory(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")
	memory, err := s.db.GetMemory(r.Context(), id)
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
	if memory == nil {
		writeError(w, http.StatusNotFound, "memory not found")
		return
	}
	writeJSON(w, http.StatusOK, memory)
}

func (s *Server) handleQueryMemories(
	w http.ResponseWriter, r *http.Request,
) {
	var req memoryQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := service.ValidateMemoryLimit(req.Limit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := s.db.QueryMemories(r.Context(), db.MemoryQuery{
		Text:                 req.Query,
		Project:              req.Project,
		CWD:                  req.CWD,
		GitBranch:            req.GitBranch,
		Agent:                req.Agent,
		Type:                 req.Type,
		Scope:                req.Scope,
		Status:               req.Status,
		ExtractorMethod:      req.ExtractorMethod,
		SourceSessionID:      req.SourceSessionID,
		SourceEpisodeID:      req.SourceEpisodeID,
		SourceRunID:          req.SourceRunID,
		SupersedesMemoryID:   req.SupersedesMemoryID,
		SupersededByMemoryID: req.SupersededByMemoryID,
		TrustedOnly:          req.TrustedOnly,
		Limit:                req.Limit,
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
	if page.Memories == nil {
		page.Memories = []db.MemoryResult{}
	}
	resp := memoryQueryResponse{
		Memories:    page.Memories,
		TrustedOnly: req.TrustedOnly,
		Summary:     service.BuildMemoryQuerySummary(page.Memories),
	}
	if req.IncludeContext {
		contextText, contextMeta, err := service.BuildMemoryContext(
			page.Memories, req.ContextMaxBytes, req.Query,
		)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp.Context = contextText
		resp.ContextMeta = contextMeta
		resp.ContextMemories = service.MemoryContextResults(
			page.Memories, contextMeta,
		)
		resp.ContextSummary = service.BuildMemoryContextSummary(
			page.Memories, contextMeta,
		)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleImportMemories(
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
	requireExistingSessions, ok := memoryImportRequiresExistingSessions(w, r)
	if !ok {
		return
	}
	if !allowProductionImport &&
		(config.IsDefaultAgentsviewDataDir(s.cfg.DataDir) ||
			config.IsDefaultAgentsviewDBPath(s.cfg.DBPath)) {
		writeError(
			w,
			http.StatusForbidden,
			"memory import refuses to validate or write against the default agentsview data directory; set allow_production_import=true only when intentionally targeting that archive",
		)
		return
	}
	result, err := s.db.ImportAcceptedMemoriesJSONLWithOptions(
		r.Context(),
		r.Body,
		db.MemoryImportOptions{
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

func memoryImportRequiresExistingSessions(
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
