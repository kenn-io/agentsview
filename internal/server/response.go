package server

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log"
	"net/http"
)

// writeJSON writes v as JSON with the given HTTP status code.
// Logs a warning if JSON encoding fails.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.MarshalEncode(jsontext.NewEncoder(w), v); err != nil {
		log.Printf("writeJSON: encoding response: %v", err)
	}
}

// writeError writes a JSON error response with the given status
// and message.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
