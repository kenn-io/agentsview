package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/kit/daemon"
)

// replicaPusher runs a local sync then pushes to a replica, lazily
// connecting and reconnecting after errors so a transiently
// unreachable database never crashes the daemon.
type replicaPusher struct {
	// label prefixes log lines, e.g. "pg watch".
	label string
	// displayName names the replica product in operator-facing warnings.
	displayName string
	localSync   func(context.Context) error
	scopedSync  func(
		context.Context, syncpkg.WatchBatch, *syncpkg.WatchRecoveryScope,
		func() error,
	) error
	ensurePricing func(context.Context) error
	connect       func(context.Context) (storage.Pusher, error)
	target        storage.Pusher
	// vectorReconcileNeeded is true until a generation-wide vector
	// reconciliation succeeds in this watch process, and again after
	// any push error or a vector phase that skipped or deferred work.
	// While true, change pushes stay generation-wide so nothing waits
	// on the interval floor unnecessarily; while false, change pushes
	// scope their vector reads to the changed relational sessions.
	vectorReconcileNeeded bool
	// lastReconciledVectorGeneration is the replica generation id of the
	// last clean generation-wide reconciliation in this process. A scoped
	// push carrying it lets the vector phase promote itself to
	// generation-wide when the active generation id differs, so a re-embed
	// or a reset/drop-and-recreate (by any machine) never leaves a
	// generation partially populated.
	lastReconciledVectorGeneration int64
}

// push performs one local-sync-then-push cycle. On any replica error it
// drops the cached connection so the next call reconnects.
func (p *replicaPusher) push(
	ctx context.Context, reason pushReason, full bool,
) error {
	return p.pushBatch(ctx, reason, full, nil, nil)
}

func (p *replicaPusher) pushBatch(
	ctx context.Context,
	reason pushReason,
	full bool,
	batch *syncpkg.WatchBatch,
	recovery *syncpkg.WatchRecoveryScope,
) error {
	push := func() error { return p.pushAfterSync(ctx, reason, full) }
	if batch != nil {
		if p.scopedSync == nil {
			return errors.New("scoped local sync is unavailable")
		}
		return p.scopedSync(ctx, *batch, recovery, push)
	}
	if err := p.localSync(ctx); err != nil {
		return fmt.Errorf("local sync: %w", err)
	}
	return push()
}

func (p *replicaPusher) pushAfterSync(
	ctx context.Context, reason pushReason, full bool,
) error {
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
		t, err := p.connect(ctx)
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		p.target = t
	}
	if err := p.target.EnsureSchema(ctx); err != nil {
		p.reset()
		return fmt.Errorf("ensure schema: %w", err)
	}
	scoped := scopedVectorPush(reason, full, p.vectorReconcileNeeded)
	res, err := p.target.PushWithOptions(ctx, storage.PushOptions{
		Full:                           full,
		ScopeVectorsToChangedSessions:  scoped,
		LastReconciledVectorGeneration: p.lastReconciledVectorGeneration,
	}, nil)
	if err != nil {
		p.vectorReconcileNeeded = true
		p.reset()
		return fmt.Errorf("push: %w", err)
	}
	p.vectorReconcileNeeded, p.lastReconciledVectorGeneration = nextVectorReconcile(
		p.vectorReconcileNeeded,
		p.lastReconciledVectorGeneration, scoped, res,
	)
	if res.Errors > 0 {
		logReplicaWatchPushResult(p.label, p.displayName, res, reason)
		log.Printf(
			"%s: %d session(s) failed to push; will retry",
			p.label, res.Errors,
		)
		return fmt.Errorf("%d session(s) failed to push", res.Errors)
	}
	logReplicaWatchPushResult(p.label, p.displayName, res, reason)
	return nil
}

// scopedVectorPush reports whether a push may scope its vector phase
// to the changed relational sessions: only change-triggered, non-full
// pushes after a clean generation-wide reconciliation qualify.
// Startup, interval-floor, shutdown, and full pushes always reconcile
// generation-wide, which also owns eviction of replica-only state rows and
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
// scoped but the active generation id differed from the last reconciled one:
// the vector phase promotes that case, so the same predicate recovers it here
// without a separate result flag. The phase also promotes when its machine
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
	scoped bool, res storage.PushResult,
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

func logReplicaWatchPushResult(
	label, displayName string, res storage.PushResult, reason pushReason,
) {
	if res.SkippedConflicts > 0 {
		log.Printf(
			"%s: pushed %d sessions, %d messages, skipped %d ownership conflict(s), %d errors (%s)",
			label, res.SessionsPushed, res.MessagesPushed,
			res.SkippedConflicts, res.Errors, reason,
		)
		log.Printf(
			"%s: %d session(s) skipped due to %s ownership conflicts",
			label, res.SkippedConflicts, displayName,
		)
		return
	}
	if res.Errors > 0 {
		log.Printf(
			"%s: pushed %d sessions, %d messages, %d errors (%s)",
			label, res.SessionsPushed, res.MessagesPushed,
			res.Errors, reason,
		)
		return
	}
	log.Printf(
		"%s: pushed %d sessions, %d messages (%s)",
		label, res.SessionsPushed, res.MessagesPushed, reason,
	)
}

func (p *replicaPusher) reset() {
	if p.target != nil {
		_ = p.target.Close()
		p.target = nil
	}
}

// resolveWatchTarget validates the replica config and resolves the project
// filters for a watch run.
func resolveWatchTarget(
	backend storage.Replica,
	appCfg config.Config,
	cfg ReplicaPushConfig,
	targetName string,
) (
	target storage.ConfiguredReplica,
	projects, exclude []string,
	err error,
) {
	refs, err := storage.SelectTargets(backend, appCfg, targetName, false)
	if err != nil {
		return storage.ConfiguredReplica{}, nil, nil, err
	}
	target, err = backend.ResolveTarget(appCfg, refs[0])
	if err != nil {
		return storage.ConfiguredReplica{}, nil, nil, err
	}
	if target.Target.URL == "" {
		return storage.ConfiguredReplica{}, nil, nil,
			errors.New("url not configured")
	}
	if err := backend.ValidateTarget(target.Target); err != nil {
		return storage.ConfiguredReplica{}, nil, nil, err
	}
	projects, exclude, err = resolvePushProjects(target, cfg)
	if err != nil {
		return storage.ConfiguredReplica{}, nil, nil, err
	}
	return target, projects, exclude, nil
}

const (
	defaultWatchDebounce = 30 * time.Second
	defaultWatchInterval = 15 * time.Minute
)

// runReplicaPushWatch runs the long-lived auto-push daemon: an initial
// catch-up push, then pushes triggered by file changes (debounced)
// and a periodic floor tick, until interrupted.
func runReplicaPushWatch(
	backend storage.Replica,
	cfg ReplicaPushConfig,
	targetName string,
) error {
	name := backend.Name()
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	setupLogFileNamed(appCfg.DataDir, name+"-watch.log")

	target, projects, exclude, err := resolveWatchTarget(
		backend, appCfg, cfg, targetName,
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
		Prefix: name + "-watch",
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
			log.Printf("%s watch: releasing lock: %v", name, rerr)
		}
	}()

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	log.Printf(
		"%s watch: starting (machine=%q debounce=%s interval=%s)",
		name, target.Target.MachineName, debounce, interval,
	)

	writer, cleanup, err := resolveArchiveWriteBackend(ctx, appCfg)
	if err != nil {
		return fmt.Errorf("opening writer: %w", err)
	}
	defer cleanup()
	if err := writer.ReplicaPushWatch(
		ctx, backend, target, cfg, projects, exclude, debounce, interval,
	); err != nil {
		return err
	}
	return nil
}
