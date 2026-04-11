package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/wesm/agentsview/internal/db"
)

type pinRequest struct {
	Note *string `json:"note,omitempty"`
}

func (s *Server) handlePinMessage(
	w http.ResponseWriter, r *http.Request,
) {
	sessionID := r.PathValue("id")
	messageIDStr := r.PathValue("messageId")
	messageID, err := strconv.ParseInt(messageIDStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid message id")
		return
	}

	var req pinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	id, err := s.db.PinMessage(sessionID, messageID, req.Note)
	if err != nil {
		if handleReadOnly(w, err) {
			return
		}
		log.Printf("pin message: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if id == 0 {
		writeError(w, http.StatusBadRequest,
			"message does not belong to this session")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) handleUnpinMessage(
	w http.ResponseWriter, r *http.Request,
) {
	sessionID := r.PathValue("id")
	messageIDStr := r.PathValue("messageId")
	messageID, err := strconv.ParseInt(messageIDStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid message id")
		return
	}

	if err := s.db.UnpinMessage(sessionID, messageID); err != nil {
		if handleReadOnly(w, err) {
			return
		}
		log.Printf("unpin message: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListPins(
	w http.ResponseWriter, r *http.Request,
) {
	project := r.URL.Query().Get("project")
	pins, err := s.db.ListPinnedMessages(r.Context(), "", project)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("list pins: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if pins == nil {
		pins = []db.PinnedMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pins": pins})
}

func (s *Server) handleListSessionPins(
	w http.ResponseWriter, r *http.Request,
) {
	sessionID := r.PathValue("id")
	pins, err := s.db.ListPinnedMessages(r.Context(), sessionID, "")
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("list session pins: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if pins == nil {
		pins = []db.PinnedMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pins": pins})
}
