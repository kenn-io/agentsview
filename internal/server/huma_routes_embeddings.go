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

// WithEmbeddingsManager wires the implementation behind the embeddings build
// lifecycle routes (/api/v1/embeddings/...). The routes are registered even
// when no manager is present so OpenAPI and generated clients expose the full
// API surface; handlers return 501 until vector serving is configured.
func WithEmbeddingsManager(m EmbeddingsManager) Option {
	return func(s *Server) { s.embeddingsManager = m }
}

// WithEmbeddingsUnavailableReason records why the embeddings routes are
// unavailable, so their 501 responses carry an actionable message instead of
// the generic "embeddings manager not available" — e.g. when the daemon
// disabled vector serving at startup because another process held
// vectors.write.lock.
func WithEmbeddingsUnavailableReason(reason string) Option {
	return func(s *Server) { s.embeddingsUnavailableReason = reason }
}

// WithEmbeddingsIncludeAutomatedDefault records the daemon's configured
// [vector].include_automated scope, applied to build requests that leave
// include_automated unset. Scheduler and CLI builds resolve the same config
// value themselves; without this, an HTTP build request omitting the field
// would silently build with include_automated=false regardless of config.
func WithEmbeddingsIncludeAutomatedDefault(includeAutomated bool) Option {
	return func(s *Server) { s.embeddingsIncludeAutomatedDefault = includeAutomated }
}

// embeddingsUnavailableError is the 501 every embeddings route returns while
// no manager is wired, carrying the recorded cause when one exists.
func (s *Server) embeddingsUnavailableError() error {
	msg := s.embeddingsUnavailableReason
	if msg == "" {
		msg = "embeddings manager not available"
	}
	return apiError(http.StatusNotImplemented, msg)
}

func (s *Server) registerEmbeddingsRoutes() {
	group := newRouteGroup(s.api, "/api/v1/embeddings", "Embeddings")

	post(s, group, "/build", "Start an embeddings build", s.humaEmbeddingsBuild)
	get(s, group, "/status", "Embeddings build status", s.humaEmbeddingsStatus)
	get(s, group, "/generations", "List embedding generations", s.humaEmbeddingsGenerations)
	post(s, group, "/generations/{id}/activate", "Activate an embedding generation",
		s.humaEmbeddingsActivate)
	post(s, group, "/generations/{id}/retire", "Retire an embedding generation",
		s.humaEmbeddingsRetire)
}

// embeddingsBuildRequest mirrors vector.BuildRequest for the HTTP surface,
// with IncludeAutomated as a tri-state: omitted (null) means "use the
// daemon's configured [vector].include_automated scope", matching how
// scheduler and CLI builds resolve it; an explicit value overrides the
// config for this build only.
type embeddingsBuildRequest struct {
	FullRebuild      bool   `json:"full_rebuild,omitempty"`
	Backstop         bool   `json:"backstop,omitempty"`
	RepairInvalid    bool   `json:"repair_invalid,omitempty"`
	IncludeAutomated *bool  `json:"include_automated,omitempty"`
	Using            string `json:"using,omitempty"`
}

type embeddingsBuildInput struct {
	Body embeddingsBuildRequest
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
	if s.embeddingsManager == nil {
		return nil, s.embeddingsUnavailableError()
	}
	includeAutomated := s.embeddingsIncludeAutomatedDefault
	if in.Body.IncludeAutomated != nil {
		includeAutomated = *in.Body.IncludeAutomated
	}
	req := vector.BuildRequest{
		FullRebuild:      in.Body.FullRebuild,
		Backstop:         in.Body.Backstop,
		RepairInvalid:    in.Body.RepairInvalid,
		IncludeAutomated: includeAutomated,
		Using:            in.Body.Using,
	}
	if err := s.embeddingsManager.StartBuild(req); err != nil {
		if errors.Is(err, vector.ErrBuildRunning) {
			return nil, apiError(http.StatusConflict, err.Error())
		}
		if errors.Is(err, vector.ErrUnknownServer) ||
			errors.Is(err, vector.ErrInvalidBuildRequest) {
			return nil, apiError(http.StatusBadRequest, err.Error())
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
	if s.embeddingsManager == nil {
		return nil, s.embeddingsUnavailableError()
	}
	return &jsonOutput[vector.BuildStatus]{Body: s.embeddingsManager.Status()}, nil
}

func (s *Server) humaEmbeddingsGenerations(
	ctx context.Context, _ *emptyInput,
) (*jsonOutput[embeddingsGenerationsResponse], error) {
	if s.embeddingsManager == nil {
		return nil, s.embeddingsUnavailableError()
	}
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
	if s.embeddingsManager == nil {
		return nil, s.embeddingsUnavailableError()
	}
	if err := s.embeddingsManager.Activate(ctx, in.ID, in.Body.Force); err != nil {
		return nil, embeddingsActionError(err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaEmbeddingsRetire(
	ctx context.Context, in *embeddingsGenerationActionInput,
) (*noContentOutput, error) {
	if s.embeddingsManager == nil {
		return nil, s.embeddingsUnavailableError()
	}
	if err := s.embeddingsManager.Retire(ctx, in.ID, in.Body.Force); err != nil {
		return nil, embeddingsActionError(err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

// embeddingsActionError maps Activate/Retire's sentinels — ErrBuildRunning
// and ErrGenerationRefused to 409 Conflict, ErrGenerationNotFound to 404 Not
// Found — with the underlying message, and anything else to a generic
// internal error.
func embeddingsActionError(err error) error {
	if errors.Is(err, vector.ErrBuildRunning) || errors.Is(err, vector.ErrGenerationRefused) {
		return apiError(http.StatusConflict, err.Error())
	}
	if errors.Is(err, vector.ErrGenerationNotFound) {
		return apiError(http.StatusNotFound, err.Error())
	}
	return internalError("embeddings generation action", err)
}
