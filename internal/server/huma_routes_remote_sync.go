package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/remotesync"
)

func (s *Server) registerRemoteSyncRoutes() {
	group := newRouteGroup(s.api, "/api/v1/remote-sync", "RemoteSync")
	get(s, group, "/targets", "Resolve remote sync targets", s.humaRemoteSyncTargets)
	s.mux.HandleFunc("/api/v1/remote-sync/archive", s.remoteSyncArchiveHTTP)
	s.mux.HandleFunc("/api/v1/remote-sync/manifest", s.remoteSyncManifestHTTP)
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

func (s *Server) remoteSyncManifestHTTP(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "invalid manifest request", http.StatusBadRequest)
		return
	}
	allowed := remotesync.ResolveTargets(s.cfg)
	manifestTargets, ok := remotesync.SelectAllowedTargets(allowed, req)
	if !ok {
		http.Error(w, "remote sync target is not allowed", http.StatusForbidden)
		return
	}
	if manifestTargets.HasSanitizedFileScopedAgents() {
		// Sanitized file-scoped agents (Windsurf) stream a transformed
		// subset of their directory through WriteArchive; the manifest
		// cannot model that per-file transformation, and advertising
		// the raw tree would let a delta request fetch unsanitized
		// files. Refuse the manifest so the client falls back to the
		// full-archive flow (501 is in the client's
		// manifest-unsupported set). Verbatim file-scoped agents
		// (RooCode) are fine: BuildManifest lists exactly their curated
		// files.
		http.Error(w, "manifest unavailable for sanitized file-scoped agents",
			http.StatusNotImplemented)
		return
	}
	manifest, err := remotesync.BuildManifest(manifestTargets)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "gzip")
	gz := gzip.NewWriter(w)
	if err := json.NewEncoder(gz).Encode(manifest); err != nil {
		log.Printf("remote sync manifest stream failed: %v", err)
		_ = gz.Close()
		return
	}
	if err := gz.Close(); err != nil {
		log.Printf("remote sync manifest stream failed: %v", err)
	}
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
	var req remotesync.ArchiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid archive request", http.StatusBadRequest)
		return
	}
	allowed := remotesync.ResolveTargets(s.cfg)
	archiveTargets, ok := remotesync.SelectAllowedTargets(allowed, req.TargetSet)
	if !ok {
		http.Error(w, "remote sync target is not allowed", http.StatusForbidden)
		return
	}
	// A present delta file list selects delta mode even when empty:
	// an explicit empty delta yields an empty tar, not the full corpus.
	deltaMode := req.DeltaFiles != nil
	var files []string
	if deltaMode {
		files, ok = remotesync.SelectAllowedFiles(allowed, req.DeltaFiles)
		if !ok {
			http.Error(w, "remote sync file is not allowed", http.StatusForbidden)
			return
		}
	}
	w.Header().Set("Content-Type", "application/x-tar")
	archiveWriter := &streamErrorAwareResponseWriter{ResponseWriter: w}
	out := io.Writer(archiveWriter)
	var gz *gzip.Writer
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz = gzip.NewWriter(archiveWriter)
		out = gz
	}
	var err error
	if deltaMode {
		err = remotesync.WriteArchiveFiles(out, allowed, files)
	} else {
		err = remotesync.WriteArchive(out, archiveTargets)
	}
	if err == nil && gz != nil {
		err = gz.Close()
	}
	if err != nil {
		// Do NOT close gz on error: Close flushes a gzip header and
		// trailer to the response, which would mark the response as
		// written and turn a clean failure into a 200 with a valid
		// empty gzip stream.
		if archiveWriter.wrote {
			log.Printf("remote sync archive stream failed: %v", err)
			return
		}
		// Nothing streamed yet: drop the gzip claim so the error body
		// is readable, then fail the request.
		w.Header().Del("Content-Encoding")
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
