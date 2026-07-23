package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/kit/daemon"
)

// pgTarget is the subset of *postgres.Sync the pusher needs. It is an
// interface so the pusher can be tested without a live database.
type pgTarget interface {
	EnsureSchema(ctx context.Context) error
	PushWithOptions(
		ctx context.Context, opts postgres.PushOptions,
		onProgress func(postgres.PushProgress),
	) (postgres.PushResult, error)
	Close() error
}

// pgPusher runs a local sync then pushes to PostgreSQL, lazily
// connecting and reconnecting after errors so a transiently
// unreachable database never crashes the daemon.
type pgPusher struct {
	localSync     func(context.Context) error
	ensurePricing func(context.Context) error
	connect       func() (pgTarget, error)
	target        pgTarget
	// vectorReconcileNeeded is true until a generation-wide vector
	// reconciliation succeeds in this watch process, and again after
	// any push error or a vector phase that skipped or deferred work.
	// While true, change pushes stay generation-wide so nothing waits
	// on the interval floor unnecessarily; while false, change pushes
	// scope their vector reads to the changed relational sessions.
	vectorReconcileNeeded bool
	// lastReconciledVectorGeneration is the PG generation id of the last
	// clean generation-wide reconciliation in this process. A scoped push
	// carrying it lets the vector phase promote itself to generation-wide
	// when the active generation id differs, so a re-embed or a
	// reset/drop-and-recreate (by any machine) never leaves a generation
	// partially populated.
	lastReconciledVectorGeneration int64
}

// push performs one local-sync-then-push cycle. On any PG error it
// drops the cached connection so the next call reconnects.
func (p *pgPusher) push(
	ctx context.Context, reason pushReason, full bool,
) error {
	if err := p.localSync(ctx); err != nil {
		return fmt.Errorf("local sync: %w", err)
	}
	if p.ensurePricing != nil {
		if err := p.ensurePricing(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			log.Printf("warning: pricing refresh failed: %v", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.target == nil {
		t, err := p.connect()
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		p.target = t
	}
	// EnsureSchema is idempotent and memoized inside *postgres.Sync,
	// so calling it every cycle is cheap after the first success.
	if err := p.target.EnsureSchema(ctx); err != nil {
		p.reset()
		return fmt.Errorf("ensure schema: %w", err)
	}
	scoped := scopedVectorPush(reason, full, p.vectorReconcileNeeded)
	res, err := p.target.PushWithOptions(ctx, postgres.PushOptions{
		Full:                           full,
		ScopeVectorsToChangedSessions:  scoped,
		LastReconciledVectorGeneration: p.lastReconciledVectorGeneration,
	}, nil)
	if err != nil {
		p.vectorReconcileNeeded = true
		p.reset()
		return fmt.Errorf("push: %w", err)
	}
	p.vectorReconcileNeeded, p.lastReconciledVectorGeneration =
		nextVectorReconcile(
			p.vectorReconcileNeeded,
			p.lastReconciledVectorGeneration, scoped, res,
		)
	if res.Errors > 0 {
		logPGWatchPushResult(res, reason)
		log.Printf(
			"pg watch: %d session(s) failed to push; will retry",
			res.Errors,
		)
		return fmt.Errorf("%d session(s) failed to push", res.Errors)
	}
	logPGWatchPushResult(res, reason)
	return nil
}

// scopedVectorPush reports whether a push may scope its vector phase
// to the changed relational sessions: only change-triggered, non-full
// pushes after a clean generation-wide reconciliation qualify.
// Startup, interval-floor, shutdown, and full pushes always reconcile
// generation-wide, which also owns eviction of PG-only state rows and
// pickup of vector-only changes (e.g. an embeddings build finishing
// with no relational change).
func scopedVectorPush(
	reason pushReason, full, reconcileNeeded bool,
) bool {
	return reason == reasonChange && !full && !reconcileNeeded
}

// nextVectorReconcile folds the post-push update of the reconcile bit and
// the last-reconciled generation id. A skipped or deferring vector phase
// leaves work behind, so the next push goes generation-wide and the id is
// unchanged; a clean generation-wide phase clears the bit and records the
// generation id it reconciled; a clean scoped phase leaves both as they were.
//
// The phase ran generation-wide when the caller did not scope it, or when it
// scoped but the active generation id differed from the last reconciled one —
// pushVectors promotes that case, so the same predicate recovers it here
// without a separate result flag. pushVectors also promotes when its machine
// push record is missing (vector tables recreated onto a reused id); that
// promotion is invisible here, and needs no recovery: the phase reconciled
// the reused id generation-wide, so the unchanged memo describes it.
//
// A generation-wide phase reporting a zero generation id clears nothing: a
// daemon predating the field omits it, and clearing on such a response
// would let the next change push scope with a zero memo, which the vector
// phase cannot promote if the generation was recreated meanwhile. Current
// servers report a nonzero id on every clean unskipped phase, so scoping
// resumes with the first response that carries one.
func nextVectorReconcile(
	current bool, lastGeneration int64,
	scoped bool, res postgres.PushResult,
) (bool, int64) {
	if res.Vectors.Skipped || res.Vectors.SessionsDeferred > 0 {
		return true, lastGeneration
	}
	generation := res.Vectors.GenerationID
	if generation != 0 && (!scoped || generation != lastGeneration) {
		return false, generation
	}
	return current, lastGeneration
}

func logPGWatchPushResult(res postgres.PushResult, reason pushReason) {
	if res.SkippedConflicts > 0 {
		log.Printf(
			"pg watch: pushed %d sessions, %d messages, skipped %d ownership conflict(s), %d errors (%s)",
			res.SessionsPushed, res.MessagesPushed,
			res.SkippedConflicts, res.Errors, reason,
		)
		log.Printf(
			"pg watch: %d session(s) skipped due to PostgreSQL ownership conflicts",
			res.SkippedConflicts,
		)
		return
	}
	if res.Errors > 0 {
		log.Printf(
			"pg watch: pushed %d sessions, %d messages, %d errors (%s)",
			res.SessionsPushed, res.MessagesPushed,
			res.Errors, reason,
		)
		return
	}
	log.Printf(
		"pg watch: pushed %d sessions, %d messages (%s)",
		res.SessionsPushed, res.MessagesPushed, reason,
	)
}

func (p *pgPusher) reset() {
	if p.target != nil {
		_ = p.target.Close()
		p.target = nil
	}
}

// resolveWatchTargets validates PG config and resolves the project
// filters for a watch run.
func resolveWatchTargets(
	appCfg config.Config,
	cfg PGPushConfig,
	targetName string,
) (
	target pgTargetSelection,
	projects, exclude []string,
	err error,
) {
	targets, err := resolvePGTargetSelections(
		appCfg, targetName, false,
	)
	if err != nil {
		return pgTargetSelection{}, nil, nil, err
	}
	target = targets[0]
	target, err = resolvePGTargetConfig(appCfg, target)
	if err != nil {
		return pgTargetSelection{}, nil, nil, err
	}
	if target.PG.URL == "" {
		return pgTargetSelection{}, nil, nil,
			fmt.Errorf("url not configured")
	}
	projects, exclude, err = resolvePushProjects(target.PG, cfg)
	if err != nil {
		return pgTargetSelection{}, nil, nil, err
	}
	return target, projects, exclude, nil
}

const (
	defaultWatchDebounce = 30 * time.Second
	defaultWatchInterval = 15 * time.Minute
)

// runPGPushWatch runs the long-lived auto-push daemon: an initial
// catch-up push, then pushes triggered by file changes (debounced)
// and a periodic floor tick, until interrupted.
func runPGPushWatch(
	cfg PGPushConfig,
	targetName string,
) error {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	setupLogFileNamed(appCfg.DataDir, "pg-watch.log")

	target, projects, exclude, err := resolveWatchTargets(
		appCfg, cfg, targetName,
	)
	if err != nil {
		return err
	}

	debounce := cfg.Debounce
	if debounce <= 0 {
		debounce = defaultWatchDebounce
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultWatchInterval
	}

	// Single-instance guard: only one watcher per data dir.
	lockPath, err := (daemon.RuntimeStore{
		Dir:    appCfg.DataDir,
		Prefix: "pg-watch",
	}).LockPath()
	if err != nil {
		return err
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return fmt.Errorf("locking %s: %w", lockPath, err)
	}
	if !locked {
		return fmt.Errorf("already locked (%s)", lockPath)
	}
	defer func() {
		if rerr := lock.Unlock(); rerr != nil {
			log.Printf("pg watch: releasing lock: %v", rerr)
		}
	}()

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	log.Printf(
		"pg watch: starting (machine=%q debounce=%s interval=%s)",
		target.PG.MachineName, debounce, interval,
	)

	backend, cleanup, err := resolveArchiveWriteBackend(ctx, appCfg)
	if err != nil {
		return fmt.Errorf("opening writer: %w", err)
	}
	defer cleanup()
	if err := backend.PGPushWatch(
		ctx, target, cfg, projects, exclude, debounce, interval,
	); err != nil {
		return err
	}
	return nil
}
