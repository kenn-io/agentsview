package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/agentsview/internal/archivepush"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/storage"
)

// replicaPusher pushes the archive to a replica from a watch process that
// owns the archive. Its session keeps one connection and reconnects after
// errors, so a replica that is unreachable for a while never stops the watch.
type replicaPusher struct {
	// label prefixes log lines, e.g. "pg watch".
	label string
	// displayName names the replica product in operator-facing warnings.
	displayName string
	session     archivepush.ReplicaSession
	vectors     vectorReconcile
}

// push copies the archive to the replica. Local sync runs separately, so the
// push sends whatever the archive holds when it starts.
func (p *replicaPusher) push(
	ctx context.Context, reason pushReason, full bool,
) error {
	return p.vectors.push(
		p.label, p.displayName, reason, full,
		func(opts storage.PushOptions) (storage.PushResult, error) {
			// Name the step that failed: a connect error needs a different
			// fix than an error partway through the copy.
			var stage archivepush.Stage
			onStage := p.session.OnStage
			defer func() { p.session.OnStage = onStage }()
			p.session.OnStage = func(s archivepush.Stage) func() {
				stage = s
				if onStage != nil {
					return onStage(s)
				}
				return nil
			}
			res, err := p.session.Push(ctx, opts, nil)
			if err == nil {
				return res, nil
			}
			if stage == archivepush.StageConnect || stage == archivepush.StagePush {
				err = fmt.Errorf("%s: %w", stage, err)
			}
			// The replica records a full push only once the copy starts.
			if stage != archivepush.StagePush {
				err = storage.PushNotStartedError{Err: err}
			}
			return res, err
		},
	)
}

// vectorReconcile decides, across the pushes of one watch process, when a
// change push may limit its vector phase to the changed sessions. Both archive
// owners use it: a local owner passes the decision to its pusher, and a daemon
// owner sends it with each push request because the daemon builds a new
// pusher for every request.
type vectorReconcile struct {
	// needed is true until a generation-wide vector reconciliation succeeds
	// in this watch process, and again after any push error or a vector
	// phase that skipped or deferred work. While true, change pushes stay
	// generation-wide so nothing waits on the interval floor unnecessarily;
	// while false, change pushes scope their vector reads to the changed
	// relational sessions.
	needed bool
	// lastGeneration is the replica generation id of the last clean
	// generation-wide reconciliation in this process. A scoped push carrying
	// it lets the vector phase promote itself to generation-wide when the
	// active generation id differs, so a re-embed or a reset/drop-and-recreate
	// (by any machine) never leaves a generation partially populated.
	lastGeneration int64
	// fullPending keeps a full watch's generation-wide vector repair until
	// a vector phase runs. repairSessions then keeps the repair for each
	// session that phase deferred until the session's vectors push. Only a
	// local owner sets fullPending; a daemon push request has no field for
	// it.
	fullPending    bool
	repairSessions map[string]struct{}
}

// newVectorReconcile starts generation-wide: nothing has been reconciled yet.
func newVectorReconcile() vectorReconcile {
	return vectorReconcile{needed: true}
}

// options returns the push options for the next push. A pending vector repair
// keeps the vector phase generation-wide.
func (v *vectorReconcile) options(reason pushReason, full bool) storage.PushOptions {
	repairPending := v.fullPending || len(v.repairSessions) > 0
	return storage.PushOptions{
		Full:                           full,
		FullVectors:                    v.fullPending,
		FullVectorSessions:             slices.Sorted(maps.Keys(v.repairSessions)),
		ScopeVectorsToChangedSessions:  scopedVectorPush(reason, full, v.needed || repairPending),
		LastReconciledVectorGeneration: v.lastGeneration,
	}
}

// record updates the state from the outcome of a push sent with opts.
func (v *vectorReconcile) record(
	opts storage.PushOptions, res storage.PushResult, err error,
) {
	if err != nil {
		v.needed = true
		return
	}
	if !res.Vectors.Skipped {
		v.repairSessions = nextVectorRepair(
			v.fullPending, v.repairSessions, res.Vectors.DeferredSessionIDs,
		)
		v.fullPending = false
	}
	v.needed, v.lastGeneration = nextVectorReconcile(
		v.needed, v.lastGeneration, opts.ScopeVectorsToChangedSessions, res,
	)
}

// push runs one watch push to a replica. send performs the push with the
// options chosen here; the two archive owners differ only in how they send it.
func (v *vectorReconcile) push(
	label, displayName string, reason pushReason, full bool,
	send func(storage.PushOptions) (storage.PushResult, error),
) error {
	opts := v.options(reason, full)
	res, err := send(opts)
	v.record(opts, res, err)
	if err != nil {
		return err
	}
	return completeReplicaWatchPush(label, displayName, res, reason)
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
// nextVectorRepair returns the sessions whose --full vector repair is still
// owed after a vector phase that ran: every deferred session when the phase
// was the generation-wide repair, else the owed sessions it deferred again.
// A persistently failing session therefore keeps only its own repair open.
func nextVectorRepair(
	generationWide bool, owed map[string]struct{}, deferred []string,
) map[string]struct{} {
	next := make(map[string]struct{})
	for _, id := range deferred {
		if _, ok := owed[id]; generationWide || ok {
			next[id] = struct{}{}
		}
	}
	return next
}

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
	if err := validateReplicaPushVectors(appCfg, cfg, target.Target.PushVectors); err != nil {
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
	appCfg, err := config.LoadMinimalFor(replicaConfigScope(backend, &cfg))
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	setupLogFileNamed(appCfg.DataDir, name+"-watch.log")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	run, cleanup, err := prepareReplicaPushWatch(ctx, backend, appCfg, cfg, targetName, false)
	if err != nil {
		return err
	}
	defer cleanup()
	return run()
}

// prepareReplicaPushWatch acquires ownership without starting ingestion. The
// caller must join run before cleanup closes the archive and releases ownership.
func prepareReplicaPushWatch(
	ctx context.Context, backend storage.Replica, appCfg config.Config,
	cfg ReplicaPushConfig, targetName string, requireLocal bool,
) (run func() error, cleanup func(), err error) {
	name := backend.Name()
	target, projects, exclude, err := resolveWatchTarget(backend, appCfg, cfg, targetName)
	if err != nil {
		return nil, nil, err
	}
	debounce, interval := watchTimings(cfg.Debounce, cfg.Interval)

	// Single-instance guard: only one watcher per data dir.
	lockPath := filepath.Join(appCfg.DataDir, name+"-watch.lock")
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return nil, nil, fmt.Errorf("locking %s: %w", lockPath, err)
	}
	if !locked {
		return nil, nil, fmt.Errorf("already locked (%s)", lockPath)
	}
	unlock := func() {
		if rerr := lock.Unlock(); rerr != nil {
			log.Printf("%s watch: releasing lock: %v", name, rerr)
		}
	}
	wake, stopLifecycle, err := installReplicaWatchLifecycle(
		appCfg.DataDir, name, target.Name,
	)
	if err != nil {
		unlock()
		return nil, nil, fmt.Errorf("starting lifecycle notifications: %w", err)
	}

	cfg.LifecycleWake = wake

	log.Printf(
		"%s watch: starting (machine=%q debounce=%s interval=%s)",
		name, target.Target.MachineName, debounce, interval,
	)

	// --embed builds in this process, so it never auto-starts a daemon; a
	// live daemon still wins the writer and rejects the request.
	appCfg.NoDaemonAutostart = cfg.Embed || requireLocal
	appCfg.RequireDaemon = cfg.NoStartDaemon
	writer, closeWriter, err := resolveArchiveWriteBackend(ctx, appCfg, transportIntentLongLived)
	if err != nil {
		stopLifecycle()
		unlock()
		return nil, nil, fmt.Errorf("opening writer: %w", err)
	}
	if _, local := writer.(*localArchiveWriteBackend); requireLocal && !local {
		closeWriter()
		stopLifecycle()
		unlock()
		return nil, nil, errors.New("push-watch requires the local archive writer")
	}
	return func() error {
		return writer.ReplicaPushWatch(ctx, backend, target, cfg, projects, exclude, debounce, interval)
	}, func() { closeWriter(); stopLifecycle(); unlock() }, nil
}
