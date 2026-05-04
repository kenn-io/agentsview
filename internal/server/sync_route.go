// ABOUTME: POST /api/v1/sessions/sync runs a one-off sync for the
// ABOUTME: session identified by body.path or body.id and returns
// ABOUTME: the resulting session detail.
package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/service"
)

func (s *Server) handleSyncSession(
	w http.ResponseWriter, r *http.Request,
) {
	var in service.SyncInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if (in.Path == "" && in.ID == "") ||
		(in.Path != "" && in.ID != "") {
		writeError(w, http.StatusBadRequest,
			"exactly one of 'path' or 'id' is required")
		return
	}
	detail, err := s.sessions.Sync(r.Context(), in)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		if handleReadOnly(w, err) {
			return
		}
		if errors.Is(err, db.ErrSessionExcluded) ||
			errors.Is(err, db.ErrSessionTrashed) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail)
}
