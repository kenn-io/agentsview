package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/postgres"
)

// pushProgressLogInterval bounds how often the daemon-side push handlers log
// progress to debug.log. A package var so tests can shrink it.
var pushProgressLogInterval = 15 * time.Second

// pushProgressStreamInterval bounds how often a push handler forwards progress
// reports onto its SSE stream: the session loop reports per session, which
// would otherwise emit one event per row. A package var so tests can shrink it.
var pushProgressStreamInterval = 200 * time.Millisecond

// newPushProgressStreamSender wraps send so it forwards at most one progress
// report per pushProgressStreamInterval.
func newPushProgressStreamSender[P any](send func(P)) func(P) {
	var last time.Time
	return func(p P) {
		if time.Since(last) < pushProgressStreamInterval {
			return
		}
		last = time.Now()
		send(p)
	}
}

// newPGPushProgressLogger returns an onProgress callback that logs pg push
// progress at most once per pushProgressLogInterval, phase-aware so the
// vector phase is distinguishable from the session phase.
func newPGPushProgressLogger() func(postgres.PushProgress) {
	var last time.Time
	return func(p postgres.PushProgress) {
		if time.Since(last) < pushProgressLogInterval {
			return
		}
		last = time.Now()
		switch p.Phase {
		case "preparing":
			if p.SessionsTotal == 0 {
				log.Printf("pg push: preparing (sync state, metadata, fingerprints)")
				return
			}
			log.Printf("pg push: preparing %d/%d session(s)",
				p.SessionsDone, p.SessionsTotal)
		case "vectors":
			log.Printf("pg push: vectors %d/%d session(s) scanned, %d chunks",
				p.VectorSessionsDone, p.VectorSessionsTotal, p.VectorChunksPushed)
		default:
			log.Printf("pg push: %d/%d session(s), %d messages",
				p.SessionsDone, p.SessionsTotal, p.MessagesDone)
		}
	}
}

// newDuckDBPushProgressLogger is newPGPushProgressLogger's DuckDB analog.
func newDuckDBPushProgressLogger() func(duckdbsync.PushProgress) {
	var last time.Time
	return func(p duckdbsync.PushProgress) {
		if time.Since(last) < pushProgressLogInterval {
			return
		}
		last = time.Now()
		log.Printf("duckdb push: %d/%d session(s), %d messages",
			p.SessionsDone, p.SessionsTotal, p.MessagesDone)
	}
}

func (s *Server) registerPushRoutes() {
	group := newRouteGroup(s.api, "/api/v1/push", "Push")

	stream(
		s, group, http.MethodPost, "/pg",
		"Push to PostgreSQL", s.humaPGPush, streamJSONResponse(),
	)
	stream(
		s, group, http.MethodPost, "/duckdb",
		"Push to DuckDB", s.humaDuckDBPush, streamJSONResponse(),
	)
}

// runPushStream executes run once and writes its outcome to the client: SSE
// progress/done/error events when the request negotiated an event stream
// (the CLI's daemon-delegated push renders these live), a single JSON body
// otherwise. run receives the progress callback to thread into the push;
// it is a throttled SSE sender in stream mode and nil in JSON mode — the
// daemon-side debug.log progress logger is composed by the caller.
func runPushStream[T any](
	hctx huma.Context,
	run func(onProgress func(T)) (any, error),
) {
	if strings.Contains(hctx.Header("Accept"), "text/event-stream") {
		if sse, ok := newHumaSSEStream(hctx); ok {
			send := newPushProgressStreamSender(func(p T) {
				sse.SendJSON("progress", p)
			})
			result, err := run(send)
			if err != nil {
				sse.SendJSON("error", map[string]string{"error": err.Error()})
				return
			}
			sse.SendJSON("done", result)
			return
		}
	}
	result, err := run(nil)
	if err != nil {
		writeHumaJSON(hctx, http.StatusInternalServerError,
			map[string]string{"error": err.Error()})
		return
	}
	writeHumaJSON(hctx, http.StatusOK, result)
}

// composePushProgress fans one progress report out to each non-nil callback.
func composePushProgress[P any](fns ...func(P)) func(P) {
	return func(p P) {
		for _, fn := range fns {
			if fn != nil {
				fn(p)
			}
		}
	}
}

type daemonPushInput struct {
	Body daemonPushRequest
}

type daemonPushRequest struct {
	Full                   bool                 `json:"full"`
	Projects               []string             `json:"projects,omitempty"`
	ExcludeProjects        []string             `json:"exclude_projects,omitempty"`
	PG                     *config.PGConfig     `json:"pg,omitempty"`
	DuckDB                 *config.DuckDBConfig `json:"duckdb,omitempty"`
	SyncStateTarget        string               `json:"sync_state_target,omitempty"`
	MigrateLegacySyncState bool                 `json:"migrate_legacy_sync_state,omitempty"`
	// NoVectors carries the CLI --no-vectors flag, which has no daemon-side
	// flag of its own, into the push handler's vector-source gate.
	NoVectors bool `json:"no_vectors,omitempty"`
}

// WithVectorPushSource wires the local vectors.db push source used by the
// daemon's pg push handler. Nil (the default) leaves the vector push phase
// disabled, e.g. when [vector] is not configured.
func WithVectorPushSource(src postgres.VectorPushSource) Option {
	return func(s *Server) { s.vectorPushSource = src }
}

func (s *Server) localPushTarget() (*db.DB, error) {
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil, apiError(
			http.StatusNotImplemented,
			"not available in remote mode",
		)
	}
	return local, nil
}

// pgPushVectorSource returns the vector push source to attach for this push,
// or nil when the phase is gated off: no source is wired ([vector] disabled),
// the target opts out via push_vectors=false, or the caller passed
// --no-vectors. A nil source leaves postgres.Sync's vector phase skipped.
func (s *Server) pgPushVectorSource(
	pgCfg config.PGConfig, noVectors bool,
) postgres.VectorPushSource {
	if s.vectorPushSource == nil || !pgCfg.PushVectorsEnabled() || noVectors {
		return nil
	}
	return s.vectorPushSource
}

func (s *Server) pgPushConfig(req daemonPushRequest) (config.PGConfig, error) {
	if req.PG != nil {
		return *req.PG, nil
	}
	return s.cfg.ResolvePG()
}

func (s *Server) duckDBPushConfig(
	req daemonPushRequest,
) (config.DuckDBConfig, error) {
	if req.DuckDB != nil {
		return *req.DuckDB, nil
	}
	return s.cfg.ResolveDuckDB()
}

func duckDBPushSyncOptions(
	req daemonPushRequest,
	duckCfg config.DuckDBConfig,
) duckdbsync.SyncOptions {
	syncStateTarget := req.SyncStateTarget
	if syncStateTarget == "" {
		syncStateTarget = duckdbsync.SyncStateTargetForConfig(duckCfg)
	}
	return duckdbsync.SyncOptions{
		Projects:        req.Projects,
		ExcludeProjects: req.ExcludeProjects,
		SyncStateTarget: syncStateTarget,
	}
}

func (s *Server) humaPGPush(
	ctx context.Context,
	in *daemonPushInput,
) (*huma.StreamResponse, error) {
	if err := postgres.ValidateProjectFilters(
		in.Body.Projects,
		in.Body.ExcludeProjects,
	); err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	local, err := s.localPushTarget()
	if err != nil {
		return nil, err
	}
	pgCfg, err := s.pgPushConfig(in.Body)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	if pgCfg.URL == "" {
		return nil, apiError(http.StatusBadRequest, "pg push: url not configured")
	}

	engine := s.syncEngineForLocal(local)
	vectorSource := s.pgPushVectorSource(pgCfg, in.Body.NoVectors)
	body := in.Body
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		runPushStream(hctx, func(
			streamProgress func(postgres.PushProgress),
		) (any, error) {
			onProgress := composePushProgress(
				newPGPushProgressLogger(), streamProgress,
			)
			var result postgres.PushResult
			_, err := engine.SyncThenRun(ctx, body.Full, nil,
				func(forceFull bool) error {
					if refreshErr := s.ensurePricing(ctx, local); refreshErr != nil {
						if ctxErr := ctx.Err(); ctxErr != nil {
							return ctxErr
						}
						log.Printf("pricing refresh: %v", refreshErr)
					}
					if ctxErr := ctx.Err(); ctxErr != nil {
						return ctxErr
					}
					ps, err := postgres.New(
						pgCfg.URL, pgCfg.Schema, local,
						pgCfg.MachineName, pgCfg.AllowInsecure,
						postgres.SyncOptions{
							Projects:               body.Projects,
							ExcludeProjects:        body.ExcludeProjects,
							SyncStateTarget:        body.SyncStateTarget,
							MigrateLegacySyncState: body.MigrateLegacySyncState,
							VectorSource:           vectorSource,
						},
					)
					if err != nil {
						return err
					}
					defer ps.Close()
					if err := ps.EnsureSchema(ctx); err != nil {
						return err
					}
					result, err = ps.Push(ctx, forceFull, onProgress)
					return err
				})
			return result, err
		})
	}}, nil
}

func (s *Server) humaDuckDBPush(
	ctx context.Context,
	in *daemonPushInput,
) (*huma.StreamResponse, error) {
	if err := postgres.ValidateProjectFilters(
		in.Body.Projects,
		in.Body.ExcludeProjects,
	); err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	local, err := s.localPushTarget()
	if err != nil {
		return nil, err
	}
	duckCfg, err := s.duckDBPushConfig(in.Body)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	if err := duckdbsync.ValidatePushTarget(duckCfg); err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}

	engine := s.syncEngineForLocal(local)
	opts := duckDBPushSyncOptions(in.Body, duckCfg)
	body := in.Body
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		runPushStream(hctx, func(
			streamProgress func(duckdbsync.PushProgress),
		) (any, error) {
			onProgress := composePushProgress(
				newDuckDBPushProgressLogger(), streamProgress,
			)
			var result duckdbsync.PushResult
			_, err := engine.SyncThenRun(ctx, body.Full, nil,
				func(forceFull bool) error {
					var syncer *duckdbsync.Sync
					var err error
					if duckCfg.URL != "" {
						syncer, err = duckdbsync.NewFromConfig(
							duckCfg, local, opts,
						)
					} else {
						syncer, err = duckdbsync.New(
							duckCfg.Path, local, duckCfg.MachineName,
							opts,
						)
					}
					if err != nil {
						return err
					}
					defer syncer.Close()
					if err := syncer.EnsureSchema(ctx); err != nil {
						return err
					}
					result, err = syncer.Push(ctx, forceFull, onProgress)
					return err
				})
			return result, err
		})
	}}, nil
}
