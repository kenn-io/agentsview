package server

import (
	"net/http"
)

// handleSessionTiming serves GET /api/v1/sessions/{id}/timing,
// returning the per-session timing summary computed by the store.
// A missing session yields 404 (db.Store.GetSessionTiming returns
// (nil, nil) when the session does not exist, mirroring GetSession).
func (s *Server) handleSessionTiming(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")
	timing, err := s.db.GetSessionTiming(r.Context(), id)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if timing == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, timing)
}
