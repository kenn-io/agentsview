package server

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/agentsview/internal/vector"
)

// EmbeddingsManager is the subset of *vector.Manager's API the embeddings
// build lifecycle routes need. Declaring it here (rather than depending on
// *vector.Manager directly) lets tests substitute a fake. TryBuild is
// intentionally excluded: it is the scheduler's synchronous entry point
// (Task 16), not part of the HTTP surface.
type EmbeddingsManager interface {
	StartBuild(req vector.BuildRequest) error
	Status() vector.BuildStatus
	Generations(ctx context.Context) ([]vector.GenerationInfo, error)
	Activate(ctx context.Context, id int64, force bool) error
	Retire(ctx context.Context, id int64, force bool) error
}

// WithEmbeddingsManager wires the embeddings build lifecycle routes
// (/api/v1/embeddings/...) into the server. Nil (the default) leaves those
// routes unregistered, matching how other optional route groups behave
// when their backing dependency is unavailable.
func WithEmbeddingsManager(m EmbeddingsManager) Option {
	return func(s *Server) { s.embeddingsManager = m }
}

func (s *Server) registerEmbeddingsRoutes() {
	if s.embeddingsManager == nil {
		return
	}
	group := newRouteGroup(s.api, "/api/v1/embeddings", "Embeddings")

	post(s, group, "/build", "Start an embeddings build", s.humaEmbeddingsBuild)
	get(s, group, "/status", "Embeddings build status", s.humaEmbeddingsStatus)
	get(s, group, "/generations", "List embedding generations", s.humaEmbeddingsGenerations)
	post(s, group, "/generations/{id}/activate", "Activate an embedding generation",
		s.humaEmbeddingsActivate)
	post(s, group, "/generations/{id}/retire", "Retire an embedding generation",
		s.humaEmbeddingsRetire)
}

type embeddingsBuildInput struct {
	Body vector.BuildRequest
}

type embeddingsBuildResponse struct {
	Started bool `json:"started"`
}

type embeddingsBuildOutput struct {
	Status int `status:"202"`
	Body   embeddingsBuildResponse
}

type embeddingsGenerationsResponse struct {
	Generations []vector.GenerationInfo `json:"generations"`
}

type embeddingsGenerationActionRequest struct {
	Force bool `json:"force,omitempty"`
}

type embeddingsGenerationActionInput struct {
	ID   int64 `path:"id" required:"true" doc:"Generation ordinal ID"`
	Body embeddingsGenerationActionRequest
}

func (s *Server) humaEmbeddingsBuild(
	_ context.Context, in *embeddingsBuildInput,
) (*embeddingsBuildOutput, error) {
	if err := s.embeddingsManager.StartBuild(in.Body); err != nil {
		if errors.Is(err, vector.ErrBuildRunning) {
			return nil, apiError(http.StatusConflict, err.Error())
		}
		return nil, internalError("start embeddings build", err)
	}
	return &embeddingsBuildOutput{
		Status: http.StatusAccepted,
		Body:   embeddingsBuildResponse{Started: true},
	}, nil
}

func (s *Server) humaEmbeddingsStatus(
	_ context.Context, _ *emptyInput,
) (*jsonOutput[vector.BuildStatus], error) {
	return &jsonOutput[vector.BuildStatus]{Body: s.embeddingsManager.Status()}, nil
}

func (s *Server) humaEmbeddingsGenerations(
	ctx context.Context, _ *emptyInput,
) (*jsonOutput[embeddingsGenerationsResponse], error) {
	gens, err := s.embeddingsManager.Generations(ctx)
	if err != nil {
		return nil, internalError("list embedding generations", err)
	}
	if gens == nil {
		gens = []vector.GenerationInfo{}
	}
	return &jsonOutput[embeddingsGenerationsResponse]{
		Body: embeddingsGenerationsResponse{Generations: gens},
	}, nil
}

func (s *Server) humaEmbeddingsActivate(
	ctx context.Context, in *embeddingsGenerationActionInput,
) (*noContentOutput, error) {
	if err := s.embeddingsManager.Activate(ctx, in.ID, in.Body.Force); err != nil {
		return nil, embeddingsActionError(err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaEmbeddingsRetire(
	ctx context.Context, in *embeddingsGenerationActionInput,
) (*noContentOutput, error) {
	if err := s.embeddingsManager.Retire(ctx, in.ID, in.Body.Force); err != nil {
		return nil, embeddingsActionError(err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

// embeddingsActionError maps Activate/Retire's refusal sentinels
// (ErrBuildRunning, ErrGenerationRefused) to 409 Conflict with the
// underlying message, and anything else to a generic internal error.
func embeddingsActionError(err error) error {
	if errors.Is(err, vector.ErrBuildRunning) || errors.Is(err, vector.ErrGenerationRefused) {
		return apiError(http.StatusConflict, err.Error())
	}
	return internalError("embeddings generation action", err)
}
