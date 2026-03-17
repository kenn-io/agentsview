package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/wesm/agentsview/internal/db"
)

// writeJSON writes v as JSON with the given HTTP status code.
// Logs a warning if JSON encoding fails.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: encoding response: %v", err)
	}
}

// writeError writes a JSON error response with the given status
// and message.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// handleReadOnly checks for db.ErrReadOnly and writes a 501.
// Returns true if the error was handled.
func handleReadOnly(w http.ResponseWriter, err error) bool {
	if errors.Is(err, db.ErrReadOnly) {
		writeError(w, http.StatusNotImplemented,
			"not available in remote mode")
		return true
	}
	return false
}

// handleContextError checks for context.Canceled and
// context.DeadlineExceeded. On cancellation it returns true
// silently (client disconnected). On deadline exceeded it
// writes a 504 and returns true. Behind withTimeout the 504
// goes into the TimeoutHandler buffer and is discarded if
// the middleware fires first.
func handleContextError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(
			w, http.StatusGatewayTimeout, "gateway timeout",
		)
		return true
	}
	return false
}
