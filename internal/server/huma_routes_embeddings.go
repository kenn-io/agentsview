package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/agentsview/internal/vector"

	"github.com/danielgtaylor/huma/v2"
)

// EmbeddingsRuntime owns the writable daemon's live vector setup.
type EmbeddingsRuntime interface {
	Manager(store string) EmbeddingsManager
	// ManagerSetup reports store's manager with the setup state read
	// alongside it, so a missing manager can be told from one setup withheld.
	ManagerSetup(store string) (EmbeddingsManager, EmbeddingsSetupStatus)
	IncludeAutomated() bool
	SetupStatus(context.Context) (EmbeddingsSetupStatus, error)
	Setup(action string) error
}

type EmbeddingsSetupStatus = vector.SetupStatus

func WithEmbeddingsRuntime(runtime EmbeddingsRuntime) Option {
	return func(s *Server) { s.embeddingsRuntime = runtime }
}

type embeddingsSetupInput struct {
	Body struct {
		Action string `json:"action" enum:"turn-on,use-built-in" required:"true"`
	}
}

func (s *Server) humaEmbeddingsSetup(_ context.Context, in *embeddingsSetupInput) (*embeddingsBuildOutput, error) {
	if s.embeddingsRuntime == nil {
		return nil, s.embeddingsUnavailableError()
	}
	if err := s.embeddingsRuntime.Setup(in.Body.Action); err != nil {
		if errors.Is(err, vector.ErrBuildRunning) {
			return nil, embeddingsActionError(err)
		}
		if errors.Is(err, vector.ErrInvalidBuildRequest) {
			return nil, apiError(http.StatusBadRequest, err.Error())
		}
		return nil, internalError("set up embeddings", err)
	}
	return &embeddingsBuildOutput{Status: http.StatusAccepted, Body: embeddingsBuildResponse{Started: true}}, nil
}

// EmbeddingsManager is the subset of *vector.Manager's API the embeddings
// build lifecycle routes need. Declaring it here (rather than depending on
// *vector.Manager directly) lets tests substitute a fake. TryBuild is
// intentionally excluded: it is the scheduler's synchronous entry point
// (Task 16), not part of the HTTP surface.
type EmbeddingsManager interface {
	StartBuild(req vector.BuildRequest) error
	Wait()
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

// WithEmbeddingsStoreManager registers a non-default embedding store manager.
func WithEmbeddingsStoreManager(name string, m EmbeddingsManager) Option {
	return func(s *Server) {
		if s.embeddingsStores == nil {
			s.embeddingsStores = make(map[string]EmbeddingsManager)
		}
		s.embeddingsStores[name] = m
	}
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

// embeddingsSetupBusyCode marks the 409 a build or generation request gets
// while setup runs, so a client can tell it from a build that will finish.
const embeddingsSetupBusyCode = "embeddings_setup_busy"

// embeddingsManagerUnavailableError refuses a request with 409 while setup is
// replacing the managers, so the caller retries once setup finishes instead
// of reading the 501 as a missing provider.
func (s *Server) embeddingsManagerUnavailableError(ctx context.Context) error {
	if s.embeddingsRuntime != nil {
		if setup, err := s.embeddingsRuntime.SetupStatus(ctx); err == nil && setup.Busy {
			return embeddingsSetupBusyError()
		}
	}
	return s.embeddingsUnavailableError()
}

// ErrEmbeddingsSetupBusy is the build or generation refusal from a manager
// setup began replacing after the route looked it up.
var ErrEmbeddingsSetupBusy = fmt.Errorf("%w: search by meaning is being set up", vector.ErrBuildRunning)

func embeddingsSetupBusyError() error {
	return apiErrorWithCode(http.StatusConflict, embeddingsSetupBusyCode, "search by meaning is being set up; retry the build or generation request when it finishes")
}

// embeddingsBuildRunningError keeps the setup-busy code on a build or
// generation action refused after the manager lookup.
func embeddingsBuildRunningError(err error) error {
	if errors.Is(err, ErrEmbeddingsSetupBusy) {
		return embeddingsSetupBusyError()
	}
	return apiError(http.StatusConflict, err.Error())
}

func (s *Server) registerEmbeddingsRoutes() {
	group := huma.NewGroup(s.api, "/api/v1/embeddings")
	configureRouteGroup(group, "Embeddings")
	registerRoute(group, http.MethodPost, "/setup", "Set up search by meaning", s.humaEmbeddingsSetup, s.humaTimeout(), func(op *huma.Operation) { op.DefaultStatus = http.StatusAccepted })

	registerRoute(group, http.MethodPost, "/build", "Start an embeddings build", s.humaEmbeddingsBuild,
		s.humaTimeout(), func(op *huma.Operation) { op.DefaultStatus = http.StatusAccepted })
	s.get(group, "/status", "Embeddings build status", s.humaEmbeddingsStatus)
	s.get(group, "/generations", "List embedding generations", s.humaEmbeddingsGenerations)
	s.post(group, "/generations/{id}/activate", "Activate an embedding generation",
		s.humaEmbeddingsActivate)
	s.post(group, "/generations/{id}/retire", "Retire an embedding generation",
		s.humaEmbeddingsRetire)
}

// embeddingsBuildRequest mirrors vector.BuildRequest for the HTTP surface,
// with IncludeAutomated as a tri-state: omitted (null) means "use the
// daemon's configured [vector].include_automated scope", matching how
// scheduler and CLI builds resolve it; an explicit value overrides the
// config for this build only.
type embeddingsBuildRequest struct {
	Store            string `json:"store,omitempty"`
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

type embeddingsStoreInput struct {
	Store string `query:"store"`
}

type embeddingsGenerationActionRequest struct {
	Force bool `json:"force,omitempty"`
}

type embeddingsGenerationActionInput struct {
	ID    int64  `path:"id" required:"true" doc:"Generation ordinal ID"`
	Store string `query:"store"`
	Body  embeddingsGenerationActionRequest
}

func (s *Server) embeddingsManagerFor(store string) EmbeddingsManager {
	if s.embeddingsRuntime != nil {
		return s.embeddingsRuntime.Manager(store)
	}
	if store == "" || store == vector.MessageIndexSpec().Name {
		return s.embeddingsManager
	}
	return s.embeddingsStores[store]
}

// embeddingsManagerAfterSetup resolves the manager after setup inspection,
// reading its setup state under the same runtime lock so replacement cannot
// leave an idle snapshot paired with a withheld manager.
func (s *Server) embeddingsManagerAfterSetup(store string, setup *EmbeddingsSetupStatus) EmbeddingsManager {
	if setup == nil {
		return s.embeddingsManagerFor(store)
	}
	manager, current := s.embeddingsRuntime.ManagerSetup(store)
	if manager == nil && (current.Busy || current.Error != "") {
		*setup = current
	}
	return manager
}

func (s *Server) humaEmbeddingsBuild(
	ctx context.Context, in *embeddingsBuildInput,
) (*embeddingsBuildOutput, error) {
	manager := s.embeddingsManagerFor(in.Body.Store)
	if manager == nil {
		return nil, s.embeddingsManagerUnavailableError(ctx)
	}
	includeAutomated := s.embeddingsIncludeAutomatedDefault
	if s.embeddingsRuntime != nil {
		includeAutomated = s.embeddingsRuntime.IncludeAutomated()
	}
	if in.Body.IncludeAutomated != nil {
		includeAutomated = *in.Body.IncludeAutomated
	}
	spec := vector.MessageIndexSpec()
	if in.Body.Store == vector.RecallIndexSpec().Name {
		spec = vector.RecallIndexSpec()
	}
	includeAutomated = spec.NormalizeIncludeAutomated(includeAutomated)
	req := vector.BuildRequest{
		FullRebuild:      in.Body.FullRebuild,
		Backstop:         in.Body.Backstop,
		RepairInvalid:    in.Body.RepairInvalid,
		IncludeAutomated: includeAutomated,
		Using:            in.Body.Using,
	}
	release, ok := s.idle.BeginWork()
	if !ok {
		return nil, apiError(http.StatusServiceUnavailable, "server is shutting down")
	}
	if err := manager.StartBuild(req); err != nil {
		release()
		if errors.Is(err, vector.ErrBuildRunning) {
			return nil, embeddingsBuildRunningError(err)
		}
		if errors.Is(err, vector.ErrUnknownServer) ||
			errors.Is(err, vector.ErrInvalidBuildRequest) {
			return nil, apiError(http.StatusBadRequest, err.Error())
		}
		return nil, internalError("start embeddings build", err)
	}
	go func() {
		manager.Wait()
		release()
	}()
	return &embeddingsBuildOutput{
		Status: http.StatusAccepted,
		Body:   embeddingsBuildResponse{Started: true},
	}, nil
}

func (s *Server) humaEmbeddingsStatus(
	ctx context.Context, in *embeddingsStoreInput,
) (*jsonOutput[vector.BuildStatus], error) {
	var body vector.BuildStatus
	if s.embeddingsRuntime != nil {
		setup, err := s.embeddingsRuntime.SetupStatus(ctx)
		if err != nil {
			setup.Error = err.Error()
		}
		body.Setup = &setup
		if setup.Busy || !setup.Enabled {
			return &jsonOutput[vector.BuildStatus]{Body: body}, nil
		}
	}
	manager := s.embeddingsManagerAfterSetup(in.Store, body.Setup)
	if manager == nil {
		if body.Setup != nil && (body.Setup.Error != "" || body.Setup.Busy) {
			return &jsonOutput[vector.BuildStatus]{Body: body}, nil
		}
		return nil, s.embeddingsUnavailableError()
	}
	setup := body.Setup
	body = manager.Status()
	body.Setup = setup
	return &jsonOutput[vector.BuildStatus]{Body: body}, nil
}

func (s *Server) humaEmbeddingsGenerations(
	ctx context.Context, in *embeddingsStoreInput,
) (*jsonOutput[embeddingsGenerationsResponse], error) {
	manager := s.embeddingsManagerFor(in.Store)
	if manager == nil {
		return nil, s.embeddingsManagerUnavailableError(ctx)
	}
	gens, err := manager.Generations(ctx)
	if err != nil {
		return nil, embeddingsGenerationsError(err)
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
	manager := s.embeddingsManagerFor(in.Store)
	if manager == nil {
		return nil, s.embeddingsManagerUnavailableError(ctx)
	}
	if err := manager.Activate(ctx, in.ID, in.Body.Force); err != nil {
		return nil, embeddingsActionError(err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaEmbeddingsRetire(
	ctx context.Context, in *embeddingsGenerationActionInput,
) (*noContentOutput, error) {
	manager := s.embeddingsManagerFor(in.Store)
	if manager == nil {
		return nil, s.embeddingsManagerUnavailableError(ctx)
	}
	if err := manager.Retire(ctx, in.ID, in.Body.Force); err != nil {
		return nil, embeddingsActionError(err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

// embeddingsGenerationsError keeps the setup-busy 409 on a list refused after
// the manager lookup.
func embeddingsGenerationsError(err error) error {
	if errors.Is(err, ErrEmbeddingsSetupBusy) {
		return embeddingsSetupBusyError()
	}
	return internalError("list embedding generations", err)
}

// embeddingsActionError maps Activate/Retire's sentinels — ErrBuildRunning
// and ErrGenerationRefused to 409 Conflict, ErrGenerationNotFound to 404 Not
// Found — with the underlying message, and anything else to a generic
// internal error.
func embeddingsActionError(err error) error {
	if errors.Is(err, vector.ErrBuildRunning) || errors.Is(err, vector.ErrGenerationRefused) {
		return embeddingsBuildRunningError(err)
	}
	if errors.Is(err, vector.ErrGenerationNotFound) {
		return apiError(http.StatusNotFound, err.Error())
	}
	return internalError("embeddings generation action", err)
}
