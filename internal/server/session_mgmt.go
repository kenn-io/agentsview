package server

import (
	"encoding/json"
	"log"
	"net/http"
)

// --- Rename ---

type renameRequest struct {
	DisplayName *string `json:"display_name"`
}

func (s *Server) handleRenameSession(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")

	session, err := s.db.GetSession(r.Context(), id)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("rename session lookup: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if session == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	var req renameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Treat empty string as "clear rename".
	if req.DisplayName != nil && *req.DisplayName == "" {
		req.DisplayName = nil
	}

	if err := s.db.RenameSession(id, req.DisplayName); err != nil {
		log.Printf("rename session: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	updated, err := s.db.GetSession(r.Context(), id)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("rename session readback: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if updated == nil {
		// Session was concurrently trashed/deleted between rename and readback.
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// --- Soft Delete (move to trash) ---

func (s *Server) handleDeleteSession(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")

	// GetSession filters out already-deleted sessions, so we use
	// a direct DB lookup that bypasses the filter.
	session, err := s.db.GetSessionFull(r.Context(), id)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("delete session lookup: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if session == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	if err := s.db.SoftDeleteSession(id); err != nil {
		log.Printf("soft delete session: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Restore from trash ---

func (s *Server) handleRestoreSession(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")

	n, err := s.db.RestoreSession(id)
	if err != nil {
		log.Printf("restore session: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		// Either the session doesn't exist or it's not in trash.
		writeError(w, http.StatusNotFound,
			"session not found or not in trash")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Permanent delete (from trash) ---

func (s *Server) handlePermanentDeleteSession(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")

	// Atomically delete only if the session is in the trash.
	// This avoids a TOCTOU race between checking deleted_at and
	// performing the delete.
	n, err := s.db.DeleteSessionIfTrashed(id)
	if err != nil {
		log.Printf("permanent delete session: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		writeError(w, http.StatusConflict,
			"session not found or not in trash")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- List trashed sessions ---

func (s *Server) handleListTrash(
	w http.ResponseWriter, r *http.Request,
) {
	sessions, err := s.db.ListTrashedSessions(r.Context())
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("list trashed sessions: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// --- Empty trash ---

func (s *Server) handleEmptyTrash(
	w http.ResponseWriter, r *http.Request,
) {
	count, err := s.db.EmptyTrash()
	if err != nil {
		log.Printf("empty trash: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": count})
}
