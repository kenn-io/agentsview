// ABOUTME: after-sync embedding scheduler and the daemon's vector subsystem
// ABOUTME: wiring — index open, encoder/Manager construction, searcher adapter.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	kitvec "go.kenn.io/kit/vector"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/vector"
)

// vectorsWriteLockRetryInterval and vectorsWriteLockRetryTimeout bound how
// long setupVectorServing waits for vectors.write.lock before giving up and
// disabling vector serving for this daemon run. Package vars so tests can
// shrink them.
var (
	vectorsWriteLockRetryInterval = 200 * time.Millisecond
	vectorsWriteLockRetryTimeout  = 5 * time.Second
)

// acquireVectorsWriteLockWithRetry tries to acquire vectors.write.lock,
// retrying briefly if another process (typically a long-running direct
// `embeddings build`) currently holds it. It returns ok=false, err=nil — not
// an error — once the retry window elapses with the lock still held, so
// setupVectorServing can degrade (disable vector serving for this run)
// rather than fail daemon startup: a long direct build must never block the
// daemon from booting.
func acquireVectorsWriteLockWithRetry(
	ctx context.Context, dataDir string,
) (*writeOwnerLock, bool, error) {
	deadline := time.Now().Add(vectorsWriteLockRetryTimeout)
	for {
		lock, err := tryAcquireNamedLock(dataDir, vectorsWriteLockFile)
		if err == nil {
			return lock, true, nil
		}
		var held writeOwnerLockHeldError
		if !errors.As(err, &held) {
			return nil, false, err
		}
		if !time.Now().Before(deadline) {
			return nil, false, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, nil
		case <-time.After(vectorsWriteLockRetryInterval):
		}
	}
}

// embedDebounceInterval is the fixed quiet period the after-sync scheduler
// waits, after the last sync-completion signal, before running a build.
const embedDebounceInterval = 30 * time.Second

// embedManager is the subset of *vector.Manager the scheduler needs,
// letting tests substitute a fake that records TryBuild calls instead of
// driving a real build.
type embedManager interface {
	TryBuild(ctx context.Context, req vector.BuildRequest) (bool, error)
}

// embedScheduler debounces sync-completion signals into background
// embedding builds: a burst of Notify calls collapses into one TryBuild
// after debounce has elapsed with no further signal, and a backstop ticker
// periodically forces a full mirror reconciliation regardless of sync
// activity.
type embedScheduler struct {
	mgr      embedManager
	debounce time.Duration
	backstop time.Duration
	// includeAutomated is the configured [vector].include_automated scope,
	// carried into every scheduler-driven BuildRequest so scheduled builds
	// stay config-authoritative rather than drifting from a CLI-only
	// override (see EmbeddingsBuildOptions.IncludeAutomatedSet).
	includeAutomated bool

	dirty chan struct{}
	stop  chan struct{}
	done  chan struct{}
}

// newEmbedScheduler builds a scheduler over mgr. backstop <= 0 disables the
// periodic backstop ticker entirely, leaving only the after-sync debounce
// path. includeAutomated is the configured [vector].include_automated scope
// applied to every build this scheduler triggers.
func newEmbedScheduler(mgr embedManager, debounce, backstop time.Duration, includeAutomated bool) *embedScheduler {
	return &embedScheduler{
		mgr:              mgr,
		debounce:         debounce,
		backstop:         backstop,
		includeAutomated: includeAutomated,
		dirty:            make(chan struct{}, 1),
		stop:             make(chan struct{}),
		done:             make(chan struct{}),
	}
}

// Notify signals that new data may need embedding. It never blocks: dirty
// has capacity 1, so a burst of calls while Run is busy (or not yet
// started) coalesces into a single pending signal.
func (s *embedScheduler) Notify() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// Stop signals Run to exit and blocks until it has, so a caller that
// closes the underlying Index right after Stop can never race a build
// still in flight.
func (s *embedScheduler) Stop() {
	close(s.stop)
	<-s.done
}

// Run is the scheduler's goroutine body: it debounces Notify signals into
// TryBuild calls and, independently, fires a Backstop TryBuild on every
// backstop tick. It returns when ctx is done or Stop is called.
func (s *embedScheduler) Run(ctx context.Context) {
	defer close(s.done)

	debounceTimer := time.NewTimer(s.debounce)
	stopTimer(debounceTimer)
	defer debounceTimer.Stop()

	var backstopC <-chan time.Time
	if s.backstop > 0 {
		ticker := time.NewTicker(s.backstop)
		defer ticker.Stop()
		backstopC = ticker.C
	}

	// pendingBackstop remembers a backstop tick that collided with a build
	// already running elsewhere (a long manual `embeddings build`, or the
	// HTTP API) and so was dropped: without it, that reconciliation pass
	// would be silently deferred until the next backstop tick (24h by
	// default) instead of running as soon as any build slot frees up. It
	// is read and written only from this single goroutine, so it needs no
	// synchronization of its own.
	var pendingBackstop bool

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-s.dirty:
			resetTimer(debounceTimer, s.debounce)
		case <-debounceTimer.C:
			req := vector.BuildRequest{Backstop: pendingBackstop, IncludeAutomated: s.includeAutomated}
			started, err := s.mgr.TryBuild(ctx, req)
			if err != nil {
				log.Printf("embed scheduler: build failed: %v", err)
			}
			if !started {
				// A build was already running elsewhere; re-arm rather
				// than drop the pass entirely. pendingBackstop, if set,
				// stays set so the retry still carries it.
				resetTimer(debounceTimer, s.debounce)
				continue
			}
			// Only clear a carried backstop once the build both started and
			// succeeded: a started-but-failed build (started=true, err!=nil)
			// never actually ran the full reconciliation it carried, so
			// clearing pendingBackstop here would silently defer that
			// reconciliation to the next backstop tick (24h by default)
			// instead of retrying on the very next debounced build.
			if err == nil {
				pendingBackstop = false
			}
		case <-backstopC:
			started, err := s.mgr.TryBuild(ctx,
				vector.BuildRequest{Backstop: true, IncludeAutomated: s.includeAutomated})
			if err != nil {
				log.Printf("embed scheduler: backstop build failed: %v", err)
			}
			// Same started-but-failed rule as the debounced path above:
			// a build that started but errored never completed its
			// reconciliation, so the pass must still be retried rather
			// than deferred to the next backstop tick.
			pendingBackstop = !started || err != nil
		}
	}
}

// stopTimer stops t, draining an already-fired-but-unread channel value so
// a following Reset starts from a clean state.
func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// resetTimer stops and drains t before rearming it for d, the safe
// stop-then-reset sequence for a timer whose channel may already hold an
// unread tick.
func resetTimer(t *time.Timer, d time.Duration) {
	stopTimer(t)
	t.Reset(d)
}

// teeEmitter fans a sync completion out to the production SSE emitter and,
// when after-sync embedding is enabled, the embed scheduler. The scheduler
// side never blocks (embedScheduler.Notify is non-blocking), so wrapping
// the emitter this way cannot slow down the sync pipeline.
type teeEmitter struct {
	primary      sync.Emitter
	scheduler    *embedScheduler
	runAfterSync bool
}

func (t teeEmitter) Emit(scope string) {
	t.primary.Emit(scope)
	if t.runAfterSync {
		t.scheduler.Notify()
	}
}

// searcherAdapter implements db.VectorSearcher over a vector.Index,
// translating its error taxonomy into db.ErrSemanticUnavailable-wrapped
// errors and enforcing the config-drift staleness gate before every query.
type searcherAdapter struct {
	ix          *vector.Index
	enc         kitvec.EncodeFunc
	fingerprint string
}

// newSearcherAdapter builds a searcherAdapter for gen's configured
// embedding identity.
func newSearcherAdapter(ix *vector.Index, enc kitvec.EncodeFunc, gen kitvec.Generation) searcherAdapter {
	return searcherAdapter{ix: ix, enc: enc, fingerprint: gen.Fingerprint()}
}

// SemanticSearch implements db.VectorSearcher. A stale active generation
// (the configured model/dimension no longer matches what was last built)
// is a hard error checked before querying at all, rather than silently
// searching the mismatched old generation.
func (a searcherAdapter) SemanticSearch(
	ctx context.Context, query string, limit int,
) ([]db.VectorHit, error) {
	stale, err := a.ix.StaleActive(ctx, a.fingerprint)
	if err != nil {
		// StaleActive shares Search's error taxonomy (notably
		// vector.ErrMirrorVersionMismatch from a version-mismatched
		// read-only vectors.db), so it is translated the same way;
		// errors outside the taxonomy pass through with this context.
		return nil, translateSearchError(
			fmt.Errorf("checking embedding index staleness: %w", err))
	}
	if stale {
		return nil, fmt.Errorf(
			"%w: index is stale (embedding config changed): run "+
				"'agentsview embeddings build --full-rebuild'",
			db.ErrSemanticUnavailable)
	}

	hits, err := a.ix.Search(ctx, a.enc, query, limit)
	if err != nil {
		return nil, translateSearchError(err)
	}

	out := make([]db.VectorHit, len(hits))
	for i, h := range hits {
		out[i] = db.VectorHit{
			SessionID:    h.SessionID,
			Ordinal:      h.Ordinal,
			OrdinalStart: h.OrdinalStart,
			OrdinalEnd:   h.OrdinalEnd,
			Subordinate:  h.Subordinate,
			Score:        h.Score,
			Snippet:      h.Snippet,
		}
	}
	return out, nil
}

// ResolveMessageUnits implements db.VectorSearcher by delegating to the
// index's resolver, translating its error taxonomy (notably
// vector.ErrMirrorVersionMismatch from a version-mismatched read-only
// vectors.db) the same way SemanticSearch does. It needs no staleness gate
// of its own: the hybrid path always calls SemanticSearch — which enforces
// the gate — before resolving FTS hits.
func (a searcherAdapter) ResolveMessageUnits(
	ctx context.Context, refs []db.MessageRef,
) ([]db.UnitRef, error) {
	units, err := a.ix.ResolveMessageUnits(ctx, refs)
	if err != nil {
		return nil, translateSearchError(err)
	}
	return units, nil
}

// translateSearchError maps vector.Index.Search's error taxonomy to
// server-facing sentinels. ErrNoActiveGeneration and BuildingError both
// mean nothing is queryable yet, so they map to db.ErrSemanticUnavailable
// (ErrNoActiveGeneration needs no extra cause text: db.ErrSemanticUnavailable's
// own message already is the "run the build" remediation).
// ErrMirrorVersionMismatch (a read-only vectors.db written by an
// incompatible mirror schema version) also maps to
// db.ErrSemanticUnavailable, carrying the sentinel's rebuild-required
// message as the cause. A QueryEncodeError means the index itself is ready
// but this particular query-time embed call failed (the embeddings endpoint
// is down, slow, or erroring); that maps to the distinct
// db.ErrSemanticTransient so a caller can tell "not configured" apart from
// "configured, but this request failed and can be retried".
func translateSearchError(err error) error {
	var buildingErr *vector.BuildingError
	var queryEncErr *vector.QueryEncodeError
	switch {
	case errors.As(err, &buildingErr):
		return fmt.Errorf("%w: index is building: %d%% complete",
			db.ErrSemanticUnavailable, buildingErr.Percent)
	case errors.Is(err, vector.ErrMirrorVersionMismatch):
		return fmt.Errorf("%w: %v", db.ErrSemanticUnavailable, err)
	case errors.Is(err, vector.ErrNoActiveGeneration):
		return db.ErrSemanticUnavailable
	case errors.As(err, &queryEncErr):
		// Double-wrap so callers can still match the underlying cause —
		// notably context.Canceled/DeadlineExceeded from a dead client —
		// alongside the transient sentinel.
		return fmt.Errorf("%w: %w", db.ErrSemanticTransient, queryEncErr.Err)
	default:
		return err
	}
}

// vectorServing bundles what runServe needs to wire the vector subsystem
// into the daemon. All fields are zero when [vector] is disabled, so
// callers can treat it uniformly without a separate enabled check.
type vectorServing struct {
	ServerOpts []server.Option
	Scheduler  *embedScheduler
	Close      func() error
}

// setupVectorServing acquires vectors.write.lock, opens vectors.db
// read-write, builds the embeddings encoder and Manager, wires database's
// semantic searcher, and constructs the after-sync scheduler. database is
// passed directly as the Manager's UnitSource since *db.DB already
// implements vector.UnitSource.
//
// The write lock is held for the daemon's lifetime (released by the
// returned Close) so a concurrent direct `embeddings build` cannot race the
// daemon's own builds over vectors.db — both writers park evicted rows at
// sentinel ordinals, and a race between them can trip unique-index
// conflicts or silently discard embeddings. If the lock is already held
// (typically by a long-running direct build), setupVectorServing retries
// briefly and, failing that, disables vector serving for this run — logging
// a warning — rather than blocking or failing daemon startup.
func setupVectorServing(
	ctx context.Context, cfg config.Config, database *db.DB,
) (vectorServing, error) {
	if !cfg.Vector.Enabled {
		return vectorServing{}, nil
	}

	lock, ok, err := acquireVectorsWriteLockWithRetry(ctx, cfg.DataDir)
	if err != nil {
		return vectorServing{}, fmt.Errorf("acquiring vectors write lock: %w", err)
	}
	if !ok {
		log.Printf(
			"serve: vectors.write.lock held by another process after %s; "+
				"disabling vector serving for this run",
			vectorsWriteLockRetryTimeout,
		)
		// The 501s this leaves behind must say why and how to recover: the
		// generic "embeddings manager not available" reads like a missing
		// feature, when the actual fix is restarting the daemon once the
		// lock-holding process (typically a direct build) exits.
		return vectorServing{ServerOpts: []server.Option{
			server.WithEmbeddingsUnavailableReason(
				"vector serving is disabled for this daemon run: another " +
					"process (typically a direct 'agentsview embeddings build') " +
					"held vectors.write.lock at startup; wait for it to finish, " +
					"then restart the daemon"),
		}}, nil
	}

	ix, err := vector.Open(
		ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), false, cfg.Vector.Embeddings.MaxInputChars,
	)
	if err != nil {
		_ = lock.Close()
		return vectorServing{}, fmt.Errorf("opening vectors.db: %w", err)
	}

	encoders, err := vectorDocumentEncoderSet(cfg.Vector.Embeddings)
	if err != nil {
		ix.Close()
		_ = lock.Close()
		return vectorServing{}, err
	}
	// Search-time query encoding always uses the default server; builds may
	// pick any named entry via BuildRequest.Using.
	queryEnc, err := newVectorQueryEncoder(cfg.Vector.Embeddings, "")
	if err != nil {
		ix.Close()
		_ = lock.Close()
		return vectorServing{}, err
	}

	backstop, err := time.ParseDuration(cfg.Vector.Embed.BackstopInterval)
	if err != nil {
		ix.Close()
		_ = lock.Close()
		return vectorServing{}, fmt.Errorf(
			"parsing [vector.embed] backstop_interval %q: %w",
			cfg.Vector.Embed.BackstopInterval, err)
	}

	gen := vectorGeneration(cfg.Vector.Embeddings)
	mgr := vector.NewManager(ix, database, encoders, gen)
	database.SetVectorSearcher(newSearcherAdapter(ix, queryEnc, gen))
	scheduler := newEmbedScheduler(mgr, embedDebounceInterval, backstop, cfg.Vector.IncludeAutomated)

	return vectorServing{
		ServerOpts: []server.Option{
			server.WithEmbeddingsManager(mgr),
			server.WithEmbeddingsIncludeAutomatedDefault(cfg.Vector.IncludeAutomated),
		},
		Scheduler: scheduler,
		Close: func() error {
			ixErr := ix.Close()
			lockErr := lock.Close()
			if ixErr != nil {
				return ixErr
			}
			return lockErr
		},
	}, nil
}

// installDirectVectorSearcher wires a read-only vectors.db into d's
// semantic searcher for direct (non-daemon) CLI reads, e.g. `session
// search --semantic` with no daemon running. It is a no-op — leaving d
// without a VectorSearcher, so callers see db.ErrSemanticUnavailable
// naturally — when [vector] is disabled, vectors.db does not exist yet, or
// vectors.db cannot be opened at all (e.g. corrupt or truncated).
//
// A vectors.db written by an incompatible mirror schema version is NOT one
// of those no-op cases: the read-only open succeeds with the mismatch
// recorded on the Index, so the searcher is wired and every semantic query
// surfaces the rebuild-required error (vector.ErrMirrorVersionMismatch,
// mapped by translateSearchError onto db.ErrSemanticUnavailable with the
// remediation attached) instead of semantic search silently reading as
// "not enabled".
//
// Vector wiring failures never fail direct service construction: every
// direct read command (e.g. `session list`) opens vectors.db eagerly
// through this path, so a bad vectors.db must not break unrelated reads
// against an otherwise-healthy sessions.db archive. Failures are logged as
// a warning and degrade to semantic search returning
// db.ErrSemanticUnavailable, matching the disabled/missing-file cases.
//
// The returned close func is nil whenever no searcher was wired (the
// no-op cases above, and the degraded-on-error case); otherwise the
// caller must call it when done with d to release the read-only index
// handle.
func installDirectVectorSearcher(cfg config.Config, d *db.DB) func() error {
	if !cfg.Vector.Enabled {
		return nil
	}
	path := cfg.Vector.ResolvedDBPath(cfg.DataDir)
	if _, err := os.Stat(path); err != nil {
		return nil
	}

	ix, err := vector.Open(context.Background(), path, true, cfg.Vector.Embeddings.MaxInputChars)
	if err != nil {
		log.Printf(
			"warning: opening vectors.db for semantic search: %v; "+
				"continuing without semantic search", err,
		)
		return nil
	}
	// Query encoding uses the default server.
	enc, err := newVectorQueryEncoder(cfg.Vector.Embeddings, "")
	if err != nil {
		ix.Close()
		log.Printf(
			"warning: building embeddings encoder for semantic search: %v; "+
				"continuing without semantic search", err,
		)
		return nil
	}
	d.SetVectorSearcher(newSearcherAdapter(ix, enc, vectorGeneration(cfg.Vector.Embeddings)))
	return ix.Close
}
