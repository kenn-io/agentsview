package server

import (
	"encoding/json"
	"log"
	"net/http"
)

func (s *Server) handleStarSession(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}

	// StarSession is atomic: INSERT...SELECT WHERE EXISTS avoids
	// the TOCTOU race of a separate GetSession + INSERT.
	ok, err := s.db.StarSession(id)
	if err != nil {
		if handleReadOnly(w, err) {
			return
		}
		log.Printf("star session: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUnstarSession(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}

	if err := s.db.UnstarSession(id); err != nil {
		if handleReadOnly(w, err) {
			return
		}
		log.Printf("unstar session: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListStarred(
	w http.ResponseWriter, r *http.Request,
) {
	ids, err := s.db.ListStarredSessionIDs(r.Context())
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("list starred: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_ids": ids,
	})
}

func (s *Server) handleBulkStar(
	w http.ResponseWriter, r *http.Request,
) {
	var body struct {
		SessionIDs []string `json:"session_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.SessionIDs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.db.BulkStarSessions(body.SessionIDs); err != nil {
		if handleReadOnly(w, err) {
			return
		}
		log.Printf("bulk star: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
