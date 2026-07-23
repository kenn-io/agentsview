package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

func handleInvalidRecallQuery(w http.ResponseWriter, err error) bool {
	if errors.Is(err, db.ErrInvalidRecallQuery) {
		writeError(w, http.StatusBadRequest, err.Error())
		return true
	}
	return false
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
		ReviewState:         q.Get("review_state"),
		ExtractorMethod:     q.Get("extractor_method"),
		SourceSessionID:     q.Get("source_session_id"),
		SourceEpisodeID:     q.Get("source_episode_id"),
		SourceRunID:         q.Get("source_run_id"),
		SupersedesEntryID:   q.Get("supersedes_entry_id"),
		SupersededByEntryID: q.Get("superseded_by_entry_id"),
		TrustedOnly:         trustedOnly,
		Limit:               limit,
	}
	query = db.NormalizeRecallQuery(query)
	if rawCursor := q.Get("cursor"); rawCursor != "" {
		cursor, err := decodeRecallListCursor(rawCursor)
		if err != nil || cursor.FilterHash != recallListFilterHash(query) {
			writeError(w, http.StatusBadRequest, "invalid recall cursor")
			return
		}
		query.CursorUpdatedAt = cursor.UpdatedAt
		query.CursorID = cursor.ID
	}
	if err := db.ValidateRecallQuery(query); err != nil {
		if handleInvalidRecallQuery(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pageLimit := query.Limit
	if pageLimit <= 0 {
		pageLimit = db.DefaultRecallEntryLimit
	}
	if pageLimit > db.MaxRecallEntryLimit {
		pageLimit = db.MaxRecallEntryLimit
	}
	query.ProbeNext = true
	entries, err := s.db.ListRecallEntries(r.Context(), query)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		if handleInvalidRecallQuery(w, err) {
			return
		}
		if handleReadOnly(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hasMore := len(entries) > pageLimit
	if hasMore {
		entries = entries[:pageLimit]
	}
	results := make([]db.RecallResult, 0, len(entries))
	for _, entry := range entries {
		results = append(results, db.RecallResult{RecallEntry: entry})
	}
	nextCursor := ""
	if hasMore {
		last := entries[len(entries)-1]
		nextCursor = encodeRecallListCursor(recallListCursor{
			UpdatedAt:  last.UpdatedAt,
			ID:         last.ID,
			FilterHash: recallListFilterHash(query),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":      results,
		"trusted_only": query.TrustedOnly,
		"next_cursor":  nextCursor,
	})
}

type recallListCursor struct {
	UpdatedAt  string `json:"updated_at"`
	ID         string `json:"id"`
	FilterHash string `json:"filter_hash"`
}

func recallListFilterHash(query db.RecallQuery) string {
	query.CursorUpdatedAt = ""
	query.CursorID = ""
	query.ProbeNext = false
	data, _ := json.Marshal(query)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func encodeRecallListCursor(cursor recallListCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeRecallListCursor(raw string) (recallListCursor, error) {
	var cursor recallListCursor
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursor, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return cursor, err
	}
	if cursor.UpdatedAt == "" || cursor.ID == "" ||
		cursor.FilterHash == "" {
		return recallListCursor{}, db.ErrInvalidRecallQuery
	}
	return cursor, nil
}

func (s *Server) handleRecallExtractionStatus(
	w http.ResponseWriter, r *http.Request,
) {
	if s.recallExtractionStatus == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
		})
		return
	}
	status, err := s.recallExtractionStatus.Status(r.Context())
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	generations := make(
		[]recallExtractGenerationStatus, 0, len(status.Generations),
	)
	for _, generation := range status.Generations {
		generations = append(generations, recallExtractGenerationStatus{
			Fingerprint: generation.Fingerprint,
			State:       generation.State,
			Model:       generation.Model,
			Segmenter:   generation.Segmenter,
			CreatedAt:   generation.CreatedAt,
			UpdatedAt:   generation.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, recallExtractionStatusResponse{
		Configured:      true,
		Fingerprint:     status.Fingerprint,
		Generations:     generations,
		Stats:           status.Stats,
		EligibleBacklog: status.EligibleBacklog,
	})
}

type recallExtractGenerationStatus struct {
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	Model       string `json:"model"`
	Segmenter   string `json:"segmenter"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type recallExtractionStatusResponse struct {
	Configured      bool                            `json:"configured"`
	Fingerprint     string                          `json:"fingerprint,omitempty"`
	Generations     []recallExtractGenerationStatus `json:"generations,omitempty"`
	Stats           db.ExtractProgressStats         `json:"stats"`
	EligibleBacklog int                             `json:"eligible_backlog"`
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
	var req service.RecallQuery
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := service.ValidateRecallEntryLimit(req.Limit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.IncludeContext {
		if _, err := service.NormalizeRecallContextMaxBytes(
			req.ContextMaxBytes,
		); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if _, err := service.NormalizeRecallQuerySurface(req.Surface); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := service.QueryRecallStore(r.Context(), s.db, req)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		if handleInvalidRecallQuery(w, err) {
			return
		}
		if handleReadOnly(w, err) {
			return
		}
		if errors.Is(err, db.ErrSemanticTransient) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if errors.Is(err, db.ErrSemanticUnavailable) {
			writeError(w, http.StatusNotImplemented, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
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
	if result.Imported > 0 && s.recallCorpusMutationNotify != nil {
		s.recallCorpusMutationNotify()
	}
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
