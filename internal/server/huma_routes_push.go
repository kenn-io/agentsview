package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
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

// newReplicaPushProgressLogger returns an onProgress callback that logs
// replica push progress at most once per pushProgressLogInterval, phase-aware
// so the vector phase is distinguishable from the session phase. name is the
// backend's CLI verb, e.g. "pg".
func newReplicaPushProgressLogger(name string) func(storage.PushProgress) {
	var last time.Time
	return func(p storage.PushProgress) {
		if time.Since(last) < pushProgressLogInterval {
			return
		}
		last = time.Now()
		switch p.Phase {
		case "preparing":
			if p.SessionsTotal == 0 {
				log.Printf("%s push: preparing (sync state, metadata, fingerprints)", name)
				return
			}
			log.Printf("%s push: preparing %d/%d session(s)",
				name, p.SessionsDone, p.SessionsTotal)
		case "vectors":
			log.Printf("%s push: vectors %d/%d session(s) scanned, %d chunks",
				name, p.VectorSessionsDone, p.VectorSessionsTotal, p.VectorChunksPushed)
		default:
			log.Printf("%s push: %d/%d session(s), %d messages",
				name, p.SessionsDone, p.SessionsTotal, p.MessagesDone)
		}
	}
}

// newMirrorPushProgressLogger is newReplicaPushProgressLogger's mirror analog.
func newMirrorPushProgressLogger(name string) func(storage.MirrorPushProgress) {
	var last time.Time
	return func(p storage.MirrorPushProgress) {
		if time.Since(last) < pushProgressLogInterval {
			return
		}
		last = time.Now()
		log.Printf("%s push: %d/%d session(s), %d messages",
			name, p.SessionsDone, p.SessionsTotal, p.MessagesDone)
	}
}

// registerPushRoutes exposes one daemon-delegated push route per registered
// backend: /api/v1/push/<replica name> for each replica and
// /api/v1/push/<mirror name> for the mirror. A server built without backends
// has no push routes.
func (s *Server) registerPushRoutes() {
	group := huma.NewGroup(s.api, "/api/v1/push")
	configureRouteGroup(group, "Push")

	for _, replica := range s.replicas {
		s.stream(group, http.MethodPost, "/"+replica.Name(),
			"Push to "+replica.DisplayName(),
			s.replicaPushHandler(replica), streamJSONResponse(),
		)
	}
	if s.mirror != nil {
		s.stream(group, http.MethodPost, "/"+s.mirror.Name(),
			"Push to "+s.mirror.DisplayName(),
			s.humaMirrorPush, streamJSONResponse(),
		)
	}
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
		if errors.Is(err, db.ErrWriterClosed) {
			hctx.SetHeader("Retry-After", writerClosedRetryAfterSeconds)
			writeHumaJSON(hctx, http.StatusServiceUnavailable,
				map[string]string{"error": err.Error()})
			return
		}
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
	Full            bool     `json:"full"`
	Projects        []string `json:"projects,omitempty"`
	ExcludeProjects []string `json:"exclude_projects,omitempty"`
	// Replica is the remote target a replica push writes to. Omitted, the
	// daemon pushes to its own default configured target for that backend.
	Replica *daemonReplicaTarget `json:"replica,omitempty"`
	DuckDB  *config.DuckDBConfig `json:"duckdb,omitempty"`
	// SyncStateTarget and MigrateLegacySyncState scope the archive-side push
	// watermarks; see storage.ReplicaTargetRef.
	SyncStateTarget        string `json:"sync_state_target,omitempty"`
	MigrateLegacySyncState bool   `json:"migrate_legacy_sync_state,omitzero"`
	// NoVectors carries the CLI --no-vectors flag, which has no daemon-side
	// flag of its own, into the push handler's vector-source gate.
	NoVectors bool `json:"no_vectors,omitzero"`
	// ScopeVectorsToChangedSessions is set by change-triggered watch
	// pushes so the vector phase reads state only for the changed
	// relational sessions (see storage.PushOptions).
	ScopeVectorsToChangedSessions bool `json:"scope_vectors_to_changed_sessions,omitzero"`
	// LastReconciledVectorGeneration travels with a scoped push so this
	// request's fresh pusher can promote to generation-wide when the active
	// generation id has changed (see storage.PushOptions).
	LastReconciledVectorGeneration int64 `json:"last_reconciled_vector_generation,omitzero"`
	// Automatic is set by the CLI's watch-mode mirror pushes: a mirror
	// held by a live serve process defers instead of rebuilding the whole
	// archive on every changed batch, and archive-scale diagnostics are
	// skipped (see storage.MirrorPushOptions.Automatic). Explicit pushes
	// leave it unset and do neither.
	Automatic     bool                        `json:"automatic,omitzero"`
	WatchBatch    *syncpkg.WatchBatch         `json:"watch_batch,omitempty"`
	WatchRecovery *syncpkg.WatchRecoveryScope `json:"watch_recovery,omitempty"`
}

// daemonReplicaTarget is the wire shape of a delegated push's remote target.
// push_vectors is optional: omitted means the target accepts the vector
// phase, matching the config default, and an explicit false opts out.
type daemonReplicaTarget struct {
	URL           string `json:"url"`
	Schema        string `json:"schema,omitempty"`
	MachineName   string `json:"machine_name"`
	AllowInsecure bool   `json:"allow_insecure,omitzero"`
	PushVectors   *bool  `json:"push_vectors,omitempty"`
}

func (t daemonReplicaTarget) target() storage.ReplicaTarget {
	return storage.ReplicaTarget{
		URL:           t.URL,
		Schema:        t.Schema,
		MachineName:   t.MachineName,
		AllowInsecure: t.AllowInsecure,
		PushVectors:   t.PushVectors == nil || *t.PushVectors,
	}
}

// WithVectorPushSource wires the local vectors.db push source used by the
// daemon's replica push handlers. Nil (the default) leaves the vector push
// phase disabled, e.g. when [vector] is not configured.
func WithVectorPushSource(src storage.VectorPushSource) Option {
	return func(s *Server) { s.vectorPushSource = src }
}

// WithReplicas registers the remote replica backends the daemon can push to.
// Each gets a /api/v1/push/<name> route.
func WithReplicas(replicas ...storage.Replica) Option {
	return func(s *Server) { s.replicas = append(s.replicas, replicas...) }
}

// WithMirror registers the derived mirror backend the daemon can push to.
func WithMirror(mirror storage.Mirror) Option {
	return func(s *Server) { s.mirror = mirror }
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

// replicaPushVectorSource returns the vector push source to attach for this
// push, or nil when the phase is gated off: no source is wired ([vector]
// disabled), the target opts out via push_vectors=false, or the caller passed
// --no-vectors. A nil source leaves the pusher's vector phase skipped.
func (s *Server) replicaPushVectorSource(
	target storage.ReplicaTarget, noVectors bool,
) storage.VectorPushSource {
	if s.vectorPushSource == nil || !target.PushVectors || noVectors {
		return nil
	}
	return s.vectorPushSource
}

// replicaPushTarget resolves the target a replica push writes to: the one in
// the request, else the daemon's own default configured target.
func (s *Server) replicaPushTarget(
	replica storage.Replica, req daemonPushRequest,
) (storage.ReplicaTarget, error) {
	if req.Replica != nil {
		return req.Replica.target(), nil
	}
	target, err := storage.DefaultTarget(replica, s.cfg)
	if err != nil {
		return storage.ReplicaTarget{}, err
	}
	return target.Target, nil
}

// duckDBPushConfig resolves the DuckDB config a daemon push writes to. The
// mirror PATH is always the server's own resolved configuration: the request
// body is attacker-reachable for any authenticated API caller, and honoring a
// caller-supplied path verbatim would let a push's rebuild rename a DuckDB
// mirror over any file the daemon can write (including the primary
// sessions.db). Non-path fields from a request-supplied config (machine
// name, filters, url — the latter still rejected by ValidatePushTarget)
// keep applying as before; a request that names a different path than the
// server's is rejected instead of redirected.
//
// The CLI never sends a path (it defers to the server's pinned path): the
// normalization below absolutizes relative paths against THIS process's
// cwd, so a configured relative path could absolutize differently in the
// CLI and the daemon and spuriously fail the equality check. The mismatch
// rejection stays for third-party API callers.
func (s *Server) mirrorPushConfig(
	req daemonPushRequest,
) (config.DuckDBConfig, error) {
	resolved, err := s.cfg.ResolveDuckDB()
	if err != nil {
		return config.DuckDBConfig{}, err
	}
	if req.DuckDB == nil {
		return resolved, nil
	}
	duckCfg := *req.DuckDB
	requested := normalizeDuckDBMirrorPath(duckCfg.Path)
	if requested != "" && requested != normalizeDuckDBMirrorPath(resolved.Path) {
		return config.DuckDBConfig{}, fmt.Errorf(
			"daemon duckdb pushes write only the server-configured mirror path %s; "+
				"requested path %s is not allowed — change the server's "+
				"[duckdb].path (or AGENTSVIEW_DUCKDB_PATH) to push to a "+
				"different file", resolved.Path, duckCfg.Path,
		)
	}
	duckCfg.Path = resolved.Path
	return duckCfg, nil
}

// normalizeDuckDBMirrorPath canonicalizes a mirror path for the equality
// check above; "" stays "" so an unset request path always defers to the
// server's own path.
func normalizeDuckDBMirrorPath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

func mirrorPushOptions(req daemonPushRequest) storage.MirrorPushOptions {
	return storage.MirrorPushOptions{
		Projects:        req.Projects,
		ExcludeProjects: req.ExcludeProjects,
		Automatic:       req.Automatic,
	}
}

func validatePushWatchScope(
	ctx context.Context, req daemonPushRequest, cfg config.Config,
) error {
	if req.WatchBatch == nil {
		if req.WatchRecovery != nil {
			return errors.New("watch recovery requires a watch batch")
		}
		return nil
	}
	if err := syncpkg.ValidateWatchBatch(*req.WatchBatch, req.WatchRecovery); err != nil {
		return err
	}
	roots, err := pushWatchScopeRoots(ctx, cfg)
	if err != nil {
		return err
	}
	validate := func(kind, path string) error {
		if path == "" {
			return nil
		}
		if unsafeWindowsWatchPath(path) || !watchPathWithinRoots(path, roots) {
			return fmt.Errorf("%s %q is outside configured provider roots", kind, path)
		}
		return nil
	}
	for _, path := range req.WatchBatch.Paths {
		if err := validate("watch path", path); err != nil {
			return err
		}
	}
	for _, root := range req.WatchBatch.ReconcileRoots {
		if err := validate("watch reconciliation root", root); err != nil {
			return err
		}
	}
	for _, rename := range req.WatchBatch.Renames {
		if err := validate("watch rename path", rename.Path); err != nil {
			return err
		}
		if err := validate("watch rename root", rename.Root); err != nil {
			return err
		}
	}
	if req.WatchRecovery != nil {
		for _, root := range req.WatchRecovery.AvailableRoots {
			if err := validate("watch recovery root", root); err != nil {
				return err
			}
		}
		for _, root := range req.WatchRecovery.DeferredRoots {
			if err := validate("watch recovery root", root); err != nil {
				return err
			}
		}
	}
	return nil
}

func pushWatchScopeRoots(
	ctx context.Context, cfg config.Config,
) ([]string, error) {
	seen := make(map[string]struct{})
	var roots []string
	add := func(root string) {
		if root == "" || unsafeWindowsWatchPath(root) {
			return
		}
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			return
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	for _, factory := range cfg.LocalProviderFactories() {
		agent := factory.Definition().Type
		configured := cfg.ResolveDirs(agent)
		if len(configured) == 0 {
			continue
		}
		for _, root := range configured {
			add(root)
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots: configured,
		})
		planned, err := parser.ResolveWatchRoots(ctx, provider)
		if err != nil {
			return nil, fmt.Errorf("resolve %s watch roots: %w", agent, err)
		}
		for _, root := range planned {
			add(root.Path)
		}
	}
	return roots, nil
}

func unsafeWindowsWatchPath(path string) bool {
	slash := strings.ReplaceAll(strings.TrimSpace(path), `\`, "/")
	lower := strings.ToLower(slash)
	return strings.HasPrefix(slash, "//") ||
		strings.HasPrefix(lower, "/device/") ||
		strings.HasPrefix(lower, "/??/")
}

func watchPathWithinRoots(path string, roots []string) bool {
	path, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	for _, root := range roots {
		root, err := filepath.Abs(filepath.Clean(root))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// syncThenRunForPush brings the local archive current and then runs the push
// work serialized against sync passes. Full and stale-archive pushes are
// archive-scale: with the worker-backed resync runner wired they route the
// rebuild through the worker (matching /api/v1/resync and the pre-session-sync
// resync) instead of running an in-process resync in the long-lived daemon,
// and the push work then runs under the engine's exclusive sync lock — after
// the deferred-signal flush — so it still observes the post-rebuild archive
// and stays serialized against watcher and periodic syncs. A validated batch
// on a current archive uses SyncWatchBatchThenRun, applying only its bounded
// changed scope before the push under that same lock. Requests without a batch
// retain SyncThenRun. Without a runner (tests, remote-mode-less setups), full
// and stale cases keep the in-process SyncThenRun path.
func (s *Server) syncThenRunForPush(
	ctx context.Context,
	engine *syncpkg.Engine,
	local *db.DB,
	full bool,
	watchBatch *syncpkg.WatchBatch,
	watchRecovery *syncpkg.WatchRecoveryScope,
	work func(forceFull bool) error,
) error {
	if watchBatch != nil && !full && !local.NeedsResync() {
		stats, err := engine.SyncWatchBatchThenRun(
			ctx, *watchBatch, watchRecovery,
			func() error { return work(false) },
		)
		if err == nil {
			err = requireProcessingComplete(stats)
		}
		return err
	}
	if s.localResyncRunner == nil || (!full && !local.NeedsResync()) {
		currentArchive := !full && !local.NeedsResync()
		stats, err := engine.SyncThenRun(ctx, full, nil, work)
		if err != nil || stats.ProcessingComplete() {
			return err
		}
		incomplete := requireProcessingComplete(stats)
		if !currentArchive {
			return incomplete
		}
		// Local sync owns retries for failed sources. An unscoped mirror push
		// must report its own outcome so the client does not repeat a completed
		// copy or lose row-level errors from its result. SyncThenRun skips work
		// on incomplete processing, so copy the archive under its lock here.
		pushErr := engine.RunExclusiveFlushed(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if local.NeedsResync() {
				return incomplete
			}
			return work(false)
		})
		if pushErr != nil {
			return errors.Join(incomplete, pushErr)
		}
		log.Printf("local ingestion warning during archive push: %v", incomplete)
		return nil
	}
	if _, err := s.runResyncWithFallback(ctx, engine, nil); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return engine.RunExclusiveFlushed(func() error { return work(true) })
}

// replicaPushHandler builds the daemon-delegated push handler for one
// replica backend. The handler validates the request before the stream body
// flushes a 200, brings the archive current, then runs the backend's pusher
// against the daemon's archive under the sync lock.
func (s *Server) replicaPushHandler(
	replica storage.Replica,
) func(context.Context, *daemonPushInput) (*huma.StreamResponse, error) {
	return func(
		ctx context.Context, in *daemonPushInput,
	) (*huma.StreamResponse, error) {
		return s.humaReplicaPush(ctx, replica, in)
	}
}

func (s *Server) humaReplicaPush(
	ctx context.Context,
	replica storage.Replica,
	in *daemonPushInput,
) (*huma.StreamResponse, error) {
	name := replica.Name()
	if err := storage.ValidateProjectFilters(
		in.Body.Projects,
		in.Body.ExcludeProjects,
	); err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	local, err := s.localPushTarget()
	if err != nil {
		return nil, err
	}
	// Reject before the stream body flushes a 200: SSE clients (the daemon
	// CLI always negotiates SSE) must see the 503 + Retry-After, not a
	// generic error event.
	if local.WriterClosed() {
		return nil, writerClosedError()
	}
	target, err := s.replicaPushTarget(replica, in.Body)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	if target.URL == "" {
		return nil, apiError(http.StatusBadRequest, name+" push: url not configured")
	}
	// Reject a target the backend would refuse before the stream flushes a
	// 200 and before the local sync pass runs.
	if err := replica.ValidateTarget(target); err != nil {
		return nil, apiError(http.StatusBadRequest, name+" push: "+err.Error())
	}
	if err := validatePushWatchScope(ctx, in.Body, s.ingestionConfig()); err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}

	engine := s.syncEngineForLocal(ctx, local)
	vectorSource := s.replicaPushVectorSource(target, in.Body.NoVectors)
	body := in.Body
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		runPushStream(hctx, func(
			streamProgress func(storage.PushProgress),
		) (any, error) {
			onProgress := composePushProgress(
				newReplicaPushProgressLogger(name), streamProgress,
			)
			var result storage.PushResult
			err := s.syncThenRunForPush(
				ctx, engine, local, body.Full, body.WatchBatch, body.WatchRecovery,
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
					pusher, err := replica.NewPusher(
						ctx, target, local, storage.PusherOptions{
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
					defer pusher.Close()
					if err := pusher.EnsureSchema(ctx); err != nil {
						return err
					}
					result, err = pusher.PushWithOptions(ctx, storage.PushOptions{
						Full: forceFull,
						ScopeVectorsToChangedSessions: body.
							ScopeVectorsToChangedSessions,
						LastReconciledVectorGeneration: body.
							LastReconciledVectorGeneration,
					}, onProgress)
					return err
				},
			)
			return result, err
		})
	}}, nil
}

func (s *Server) humaMirrorPush(
	ctx context.Context,
	in *daemonPushInput,
) (*huma.StreamResponse, error) {
	if err := storage.ValidateProjectFilters(
		in.Body.Projects,
		in.Body.ExcludeProjects,
	); err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	local, err := s.localPushTarget()
	if err != nil {
		return nil, err
	}
	// Reject before the stream body flushes a 200 so SSE clients see the
	// 503 + Retry-After (mirrors humaReplicaPush).
	if local.WriterClosed() {
		return nil, writerClosedError()
	}
	duckCfg, err := s.mirrorPushConfig(in.Body)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	if err := s.mirror.ValidatePushTarget(duckCfg); err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}

	engine := s.syncEngineForLocal(ctx, local)
	opts := mirrorPushOptions(in.Body)
	body := in.Body
	name := s.mirror.Name()
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		runPushStream(hctx, func(
			streamProgress func(storage.MirrorPushProgress),
		) (any, error) {
			onProgress := composePushProgress(
				newMirrorPushProgressLogger(name), streamProgress,
			)
			var result storage.MirrorPushResult
			err := s.syncThenRunForPush(ctx, engine, local, body.Full, nil, nil,
				func(forceFull bool) error {
					var pushErr error
					result, pushErr = s.mirror.Push(
						ctx, duckCfg, local, opts, forceFull, onProgress,
					)
					return pushErr
				})
			return result, err
		})
	}}, nil
}
