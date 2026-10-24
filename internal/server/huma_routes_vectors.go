package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/klauspost/compress/zstd"
	"go.kenn.io/agentsview/internal/vector"
)

type hubVectorStore interface {
	HubVectorGeneration(context.Context) (vector.HubGeneration, error)
	LookupHubVectors(context.Context, vector.HubLookupRequest) (vector.HubLookupReply, error)
}

type hubLookupInput struct{ Body vector.HubLookupRequest }

func (s *Server) registerVectorRoutes() {
	group := huma.NewGroup(s.api, "/api/v1/vectors")
	configureRouteGroup(group, "Vectors")
	s.get(group, "/generation", "Get the selected vector generation", s.humaVectorGeneration)
	registerRoute(group, http.MethodPost, "/lookup", "Look up pinned document vectors", s.humaVectorLookup, s.humaTimeout(), maxBodyBytes(2<<20))
}

func (s *Server) humaVectorGeneration(ctx context.Context, _ *emptyInput) (*jsonOutput[vector.HubGeneration], error) {
	store, ok := s.db.(hubVectorStore)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "vector lookup requires pg serve")
	}
	generation, err := store.HubVectorGeneration(ctx)
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	return &jsonOutput[vector.HubGeneration]{Body: generation}, nil
}

func (s *Server) humaVectorLookup(ctx context.Context, in *hubLookupInput) (*jsonOutput[vector.HubLookupReply], error) {
	store, ok := s.db.(hubVectorStore)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "vector lookup requires pg serve")
	}
	reply, err := store.LookupHubVectors(ctx, in.Body)
	if errors.Is(err, vector.ErrHubGenerationChanged) {
		return nil, &apiResponseError{Status: http.StatusConflict, Code: "generation_changed", Message: "generation_changed"}
	}
	if errors.Is(err, vector.ErrHubLookupInvalid) {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	return &jsonOutput[vector.HubLookupReply]{Body: reply}, nil
}

// Typed routes retain their JSON schema while compression happens on the wire.
type hubZstdWriter struct {
	http.ResponseWriter
	encoder *zstd.Encoder
	status  int
}

func (w *hubZstdWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status == http.StatusOK {
		w.encoder, _ = zstd.NewWriter(w.ResponseWriter, zstd.WithEncoderConcurrency(1))
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", "zstd")
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *hubZstdWriter) Write(raw []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.encoder != nil {
		return w.encoder.Write(raw)
	}
	return w.ResponseWriter.Write(raw)
}
