package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/remotesync"
)

func (s *Server) registerRemoteSyncRoutes() {
	group := newRouteGroup(s.api, "/api/v1/remote-sync", "RemoteSync")
	get(s, group, "/targets", "Resolve remote sync targets", s.humaRemoteSyncTargets)
	s.mux.HandleFunc("/api/v1/remote-sync/archive", s.remoteSyncArchiveHTTP)
}

func (s *Server) humaRemoteSyncTargets(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[remotesync.TargetSet], error) {
	if _, ok := s.db.(*db.DB); !ok {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	return &jsonOutput[remotesync.TargetSet]{
		Body: remotesync.ResolveTargets(s.cfg),
	}, nil
}

func (s *Server) remoteSyncArchiveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.db.(*db.DB); !ok {
		http.Error(w, "not available in remote mode", http.StatusNotImplemented)
		return
	}
	var req remotesync.TargetSet
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid archive request", http.StatusBadRequest)
		return
	}
	allowed := remotesync.ResolveTargets(s.cfg)
	archiveTargets, ok := remotesync.SelectAllowedTargets(allowed, req)
	if !ok {
		http.Error(w, "remote sync target is not allowed", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	archiveWriter := &streamErrorAwareResponseWriter{ResponseWriter: w}
	if err := remotesync.WriteArchive(archiveWriter, archiveTargets); err != nil {
		if archiveWriter.wrote {
			log.Printf("remote sync archive stream failed: %v", err)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type streamErrorAwareResponseWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *streamErrorAwareResponseWriter) Write(p []byte) (int, error) {
	w.wrote = true
	n, err := w.ResponseWriter.Write(p)
	return n, err
}
