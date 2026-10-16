package server

import (
	"compress/gzip"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/klauspost/compress/zstd"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/remotesync"
)

var remoteSyncZstdPool sync.Pool

func (s *Server) registerRemoteSyncRoutes() {
	group := huma.NewGroup(s.api, "/api/v1/remote-sync")
	configureRouteGroup(group, "RemoteSync")
	s.get(group, "/targets", "Resolve remote sync targets", s.humaRemoteSyncTargets)
}

type remoteSyncTargetsInput struct {
	ProtocolVersion string `header:"X-AgentsView-Remote-Sync-Version" doc:"Required remote-sync protocol version"`
}

type remoteSyncTargetsOutput struct {
	ProtocolVersion string `header:"X-AgentsView-Remote-Sync-Version"`
	Body            remotesync.TargetSet
}

func (s *Server) humaRemoteSyncTargets(
	ctx context.Context,
	in *remoteSyncTargetsInput,
) (*remoteSyncTargetsOutput, error) {
	if err := s.requireSourceSync(ctx); err != nil {
		return nil, err
	}
	requestHeader := make(http.Header)
	requestHeader.Set(remotesync.ProtocolHeader, in.ProtocolVersion)
	if err := remotesync.ValidateProtocolHeader(requestHeader); err != nil {
		return nil, apiError(http.StatusUpgradeRequired, err.Error())
	}
	if _, ok := s.db.(*db.DB); !ok {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	targets, err := remotesync.ResolveTargets(s.ingestionConfig())
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	return &remoteSyncTargetsOutput{
		ProtocolVersion: strconv.Itoa(remotesync.ProtocolVersion),
		Body:            targets,
	}, nil
}

func requireRemoteSyncProtocol(w http.ResponseWriter, r *http.Request) bool {
	if err := remotesync.ValidateProtocolHeader(r.Header); err != nil {
		http.Error(w, err.Error(), http.StatusUpgradeRequired)
		return false
	}
	remotesync.SetProtocolHeader(w.Header())
	return true
}

func (s *Server) remoteSyncManifestHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireRemoteSyncProtocol(w, r) {
		return
	}
	if _, ok := s.db.(*db.DB); !ok {
		http.Error(w, "not available in remote mode", http.StatusNotImplemented)
		return
	}
	var req remotesync.TargetSet
	if err := json.UnmarshalRead(r.Body, &req); err != nil {
		http.Error(w, "invalid manifest request", http.StatusBadRequest)
		return
	}
	manifest, err := remotesync.ServeManifest(r.Context(), s.ingestionConfig(), req)
	switch {
	case errors.Is(err, remotesync.ErrTargetNotAllowed):
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	case errors.Is(err, remotesync.ErrManifestUnsupported):
		// 501 is in the client's manifest-unsupported set.
		http.Error(w, err.Error(), http.StatusNotImplemented)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "gzip")
	gz := gzip.NewWriter(w)
	if err := json.MarshalEncode(jsontext.NewEncoder(gz), manifest); err != nil {
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
	if !requireRemoteSyncProtocol(w, r) {
		return
	}
	if _, ok := s.db.(*db.DB); !ok {
		http.Error(w, "not available in remote mode", http.StatusNotImplemented)
		return
	}
	var req remotesync.ArchiveRequest
	if err := json.UnmarshalRead(r.Body, &req); err != nil {
		http.Error(w, "invalid archive request", http.StatusBadRequest)
		return
	}
	plan, err := remotesync.PlanArchive(s.ingestionConfig(), req)
	switch {
	case errors.Is(err, remotesync.ErrTargetNotAllowed),
		errors.Is(err, remotesync.ErrFileNotAllowed):
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	addVaryHeader(w.Header(), "Accept-Encoding")
	archiveWriter := &streamErrorAwareResponseWriter{ResponseWriter: w}
	out := io.Writer(archiveWriter)
	var compressor io.WriteCloser
	encoding := remoteSyncArchiveEncoding(r.Header)
	switch encoding {
	case "zstd":
		encoder, _ := remoteSyncZstdPool.Get().(*zstd.Encoder)
		if encoder == nil {
			encoder, err = zstd.NewWriter(archiveWriter,
				zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			encoder.Reset(archiveWriter)
		}
		defer func() {
			encoder.Reset(io.Discard)
			remoteSyncZstdPool.Put(encoder)
		}()
		compressor = encoder
	case "gzip":
		compressor = gzip.NewWriter(archiveWriter)
	}
	if compressor != nil {
		w.Header().Set("Content-Encoding", encoding)
		out = compressor
	}
	err = plan.Write(r.Context(), out)
	if err == nil && compressor != nil {
		err = compressor.Close()
	}
	if err != nil {
		// Do NOT close the compressor on error: Close flushes a header and
		// trailer to the response, which would mark the response as
		// written and turn a clean failure into a 200 with a valid
		// empty compressed stream.
		if archiveWriter.wrote {
			log.Printf("remote sync archive stream failed: %v", err)
			return
		}
		// Nothing streamed yet: drop the encoding so the error body
		// is readable, then fail the request.
		w.Header().Del("Content-Encoding")
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// remoteSyncArchiveEncoding prefers zstd when the client's weights are equal.
// headerContainsToken ignores weights; Huma's negotiator can select q=0 on a tie.
func remoteSyncArchiveEncoding(header http.Header) string {
	best, bestWeight := "", 0.0
	for part := range strings.SplitSeq(strings.Join(header.Values("Accept-Encoding"), ","), ",") {
		encoding, params, err := mime.ParseMediaType(part)
		if err != nil || (encoding != "zstd" && encoding != "gzip") {
			continue
		}
		weight := 1.0
		if value, ok := params["q"]; ok {
			weight, err = strconv.ParseFloat(value, 64)
			if err != nil {
				continue
			}
		}
		if weight > 0 && weight <= 1 && (weight > bestWeight || weight == bestWeight && encoding == "zstd") {
			best, bestWeight = encoding, weight
		}
	}
	return best
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
