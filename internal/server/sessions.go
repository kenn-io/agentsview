package server

import (
	"errors"
	"net/http"

	"github.com/wesm/agentsview/internal/db"
)

func (s *Server) handleListSessions(
	w http.ResponseWriter, r *http.Request,
) {
	q := r.URL.Query()

	limit, ok := parseIntParam(w, r, "limit")
	if !ok {
		return
	}
	limit = clampLimit(limit, db.DefaultSessionLimit, db.MaxSessionLimit)

	minMsgs, ok := parseIntParam(w, r, "min_messages")
	if !ok {
		return
	}
	maxMsgs, ok := parseIntParam(w, r, "max_messages")
	if !ok {
		return
	}
	minUserMsgs, ok := parseIntParam(w, r, "min_user_messages")
	if !ok {
		return
	}

	date := q.Get("date")
	dateFrom := q.Get("date_from")
	dateTo := q.Get("date_to")

	for _, d := range []string{date, dateFrom, dateTo} {
		if d != "" && !isValidDate(d) {
			writeError(w, http.StatusBadRequest,
				"invalid date format: use YYYY-MM-DD")
			return
		}
	}
	if dateFrom != "" && dateTo != "" && dateFrom > dateTo {
		writeError(w, http.StatusBadRequest,
			"date_from must not be after date_to")
		return
	}

	activeSince := q.Get("active_since")
	if activeSince != "" && !isValidTimestamp(activeSince) {
		writeError(w, http.StatusBadRequest,
			"invalid active_since: use RFC3339 timestamp")
		return
	}

	includeOneShot := q.Get("include_one_shot") == "true"
	includeChildren := q.Get("include_children") == "true"

	filter := db.SessionFilter{
		Project:         q.Get("project"),
		ExcludeProject:  q.Get("exclude_project"),
		Machine:         q.Get("machine"),
		Agent:           q.Get("agent"),
		Date:            date,
		DateFrom:        dateFrom,
		DateTo:          dateTo,
		ActiveSince:     activeSince,
		MinMessages:     minMsgs,
		MaxMessages:     maxMsgs,
		MinUserMessages: minUserMsgs,
		ExcludeOneShot:  !includeOneShot,
		IncludeChildren: includeChildren,
		Cursor:          q.Get("cursor"),
		Limit:           limit,
	}

	page, err := s.db.ListSessions(r.Context(), filter)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		if errors.Is(err, db.ErrInvalidCursor) {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleGetSession(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")
	session, err := s.db.GetSession(r.Context(), id)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if session == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) handleGetChildSessions(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")
	children, err := s.db.GetChildSessions(r.Context(), id)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if children == nil {
		children = []db.Session{}
	}
	writeJSON(w, http.StatusOK, children)
}

// handleSearchSession handles GET /api/v1/sessions/{id}/search?q=...
// Returns matching message ordinals in document order.
func (s *Server) handleSearchSession(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ordinals": []int{}})
		return
	}
	ordinals, err := s.db.SearchSession(r.Context(), id, q)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ordinals == nil {
		ordinals = []int{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ordinals": ordinals})
}
