package server

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

func (s *Server) registerMemoryRefreshRoute() {
	s.handleHTTP(&huma.Operation{
		Method:  http.MethodPost,
		Path:    "/api/v1/memory/refresh",
		Summary: "Queue conversation-memory refresh",
		Hidden:  true,
	}, s.handleMemoryRefresh)
}

func (s *Server) handleMemoryRefresh(w http.ResponseWriter, _ *http.Request) {
	if s.memoryRefreshRequest == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = sjson(w, map[string]string{
			"error": "automatic refresh is unavailable on this server",
		})
		return
	}

	s.memoryRefreshRequest()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = sjson(w, map[string]bool{"queued": true})
}
