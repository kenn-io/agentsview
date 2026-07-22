package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/sync"
)

// syncWorkerChildEnvVar marks a process spawned as a sync-worker child by the
// daemon's writer handoff. The daemon closes its writer and releases the write
// lock before spawning, so the child skips the live-writable-daemon rejection
// (the daemon is knowingly yielding); the write-owner flock remains the real
// guard.
const syncWorkerChildEnvVar = "AGENTSVIEW_SYNC_WORKER"

// runningAsSyncWorker reports whether this process is a sync-worker child
// spawned by a daemon that has yielded write ownership for the pass.
func runningAsSyncWorker() bool {
	return os.Getenv(syncWorkerChildEnvVar) == "1"
}

// workerLine is one NDJSON record on the sync-worker's stdout. Every stdout
// line unmarshals to a workerLine; log diagnostics are redirected to debug.log
// (setupLogFile) and anything else lands on inherited stderr, so the parent can
// parse stdout strictly. A run streams zero or more Progress lines followed by
// exactly one Result line.
type workerLine struct {
	Progress *sync.Progress `json:"progress,omitempty"`
	Result   *workerResult  `json:"result,omitempty"`
}

// workerResult is the single terminal record a sync-worker run emits. Status is
// "ok" only when the pass completed and discovery was authoritative; the parent
// treats anything else, or a missing/duplicate result, as a failed run.
type workerResult struct {
	Status            string `json:"status"` // "ok" | "aborted" | "failed"
	Synced            int    `json:"synced"`
	Skipped           int    `json:"skipped"`
	Failed            int    `json:"failed"`
	Tombstoned        int    `json:"tombstoned,omitempty"`
	DiscoveryComplete bool   `json:"discoveryComplete"`
	Error             string `json:"error,omitempty"`
	// Stats carries the complete public SyncStats payload so the daemon's
	// /sync and /resync responses keep result parity with in-process passes
	// (total sessions, orphan counts, warnings, anomalies). The summary
	// counters above remain the authoritative status inputs.
	Stats *sync.SyncStats `json:"stats,omitempty"`
}

// newSyncWorkerCommand registers the hidden self-exec'd worker. The daemon runs
// it as a short-lived child so archive-scale allocation high-water returns to
// the OS when the child exits, instead of pinning the daemon's RSS.
func newSyncWorkerCommand() *cobra.Command {
	var mode string
	cmd := &cobra.Command{
		Use:          "sync-worker",
		Short:        "Run one heavy sync pass and stream a terminal result",
		Hidden:       true,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Load config the same flag-aware way `serve` does so config flags
			// the daemon forwarded into the child argv (see syncWorkerChildArgs)
			// override env/config.toml identically to a serve --background child.
			cfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("sync-worker: loading config: %w", err)
			}
			// Route log output to debug.log like every other subcommand.
			// The daemon passes its own stderr to this child, so engine
			// log.Printf diagnostics would otherwise flood the serve
			// console instead of landing in the debug log.
			setupLogFile(cfg.DataDir)
			return runSyncWorker(cfg, mode, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(
		&mode, "mode", "",
		"worker mode: startup, sync, resync-build, audit",
	)
	if err := cmd.MarkFlagRequired("mode"); err != nil {
		panic(err)
	}
	// Register the serve config flags so forwarded overrides parse and apply.
	config.RegisterServePFlags(cmd.Flags())
	return cmd
}

// runSyncWorker runs one worker pass with a background context.
func runSyncWorker(cfg config.Config, mode string, out io.Writer) error {
	return runSyncWorkerContext(context.Background(), cfg, mode, out)
}

// runSyncWorkerContext dispatches on mode, streaming NDJSON progress and exactly
// one terminal result. It returns nil only when the terminal result is Status
// "ok" with authoritative discovery; the child's exit code follows this error.
func runSyncWorkerContext(
	ctx context.Context, cfg config.Config, mode string, out io.Writer,
) error {
	enc := json.NewEncoder(out)
	// Retain the first encode error: a dropped terminal-result line means the
	// parent never sees the outcome, so the worker must exit non-zero even if the
	// pass itself succeeded. The parent also treats a missing result as a
	// protocol failure, but the worker's own exit contract must not lie.
	var encErr error
	emit := func(line workerLine) {
		if err := enc.Encode(line); err != nil && encErr == nil {
			encErr = err
		}
	}
	onProgress := func(p sync.Progress) { emit(workerLine{Progress: &p}) }
	var err error
	switch mode {
	case "startup", "sync", "audit":
		// All three share the sync body. Only "startup" may resync-and-swap: it
		// runs before the daemon opens the DB, so no live reader pins the old
		// inode. "sync" (live foreground pass) and "audit" (daily safety net)
		// run inside a writer handoff while the daemon's readers stay open, so
		// they must refuse a stale-version archive rather than swap it out from
		// under those readers; the real resync path is the resync-build flow,
		// which swaps and resets caches daemon-side.
		err = runSyncWorkerStartup(ctx, cfg, mode, emit, onProgress)
	case "resync-build":
		err = runSyncWorkerResyncBuild(ctx, cfg, mode, emit, onProgress)
	default:
		return fmt.Errorf("unknown sync-worker mode %q", mode)
	}
	if err != nil {
		return err
	}
	if encErr != nil {
		return fmt.Errorf("sync worker %s: writing terminal result: %w", mode, encErr)
	}
	return nil
}

// runSyncWorkerStartup performs a full sync (or full resync when the data
// version changed) as a self-contained pass, then emits the terminal result. It
// mirrors the sync/resync branch in runServe minus daemon-only wiring (watcher,
// emitter, backfills). mode only labels the terminal error.
func runSyncWorkerStartup(
	ctx context.Context,
	cfg config.Config,
	mode string,
	emit func(workerLine),
	onProgress func(sync.Progress),
) error {
	database, writeLock, err := openWorkerWriteDB(cfg)
	if err != nil {
		return err
	}
	defer closeWriteDB(database, writeLock)

	// Remove stale temp DB from a prior crashed resync before ResyncAll
	// stages a fresh one, matching runServe's startup cleanup.
	cleanResyncTemp(cfg.DBPath)

	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	defer engine.Close()

	if database.NeedsResync() && mode != "startup" {
		// A resync would CloseConnections + rename the archive file, but the
		// daemon's reader pool still points at the old inode and only the writer
		// is reopened by path afterward. Swapping here would strand readers on a
		// deleted file; refuse and let the resync-build flow do the swap.
		return refuseWorkerResync(mode, emit)
	}

	var result workerResult
	switch {
	case database.NeedsResync():
		stats := engine.ResyncAll(ctx, onProgress)
		if stats.Aborted && ctx.Err() == nil {
			// Mirror the in-process startup path (runInitialResync,
			// syncThenRunLocked): a safety-aborted resync still catches up
			// incrementally against the existing archive instead of leaving
			// safely applicable updates unsynchronized.
			stats = engine.SyncAll(ctx, onProgress)
		}
		result = workerResultFromStats(ctx, stats)
	case mode == "audit":
		// The audit is the safety net for watcher deletions the daemon
		// missed, so it must run the authoritative reconciliation that
		// tombstones sessions whose sources disappeared; SyncAll never
		// tombstones. Unavailable scopes are deferred to their probes:
		// reconciling them would read an unmounted or missing subtree as
		// an empty discovery and tombstone every session beneath it.
		var stats sync.SyncStats
		var tombstoned int
		var auditErr error
		if auditRoots := reconcileRootPaths(cfg); len(auditRoots) > 0 {
			stats, tombstoned, auditErr = engine.ReconcileWatchRootsWithStats(
				ctx, auditRoots, false,
			)
		}
		result = workerResultFromStats(ctx, stats)
		result.Tombstoned = tombstoned
		if auditErr != nil && result.Status == "ok" {
			result.Status = "failed"
			result.Error = auditErr.Error()
		}
	default:
		result = workerResultFromStats(ctx, engine.SyncAll(ctx, onProgress))
	}

	emit(workerLine{Result: &result})
	if result.Status != "ok" || !result.DiscoveryComplete {
		return fmt.Errorf("sync worker %s: %s", mode, result.Status)
	}
	return nil
}

// runSyncWorkerResyncBuild builds a replacement archive at the resync temp path
// from a read-only view of the original, then emits the terminal result. The
// daemon holds the write barrier and performs the swap, so the worker never
// opens the live archive writable and needs no write-owner flock. It leaves the
// built database on disk at ResyncTempPath for the daemon to install.
func runSyncWorkerResyncBuild(
	ctx context.Context,
	cfg config.Config,
	mode string,
	emit func(workerLine),
	onProgress func(sync.Progress),
) error {
	// Remove a stale temp DB from a prior crashed resync before building a fresh
	// one, matching runServe's startup cleanup and the in-process build.
	cleanResyncTemp(cfg.DBPath)

	// The worker is a fresh process, so the classifier singleton holds only
	// built-in patterns until config is applied. The build classifies every
	// session in the replacement archive (insert-time plus the forced
	// is_automated backfill), so user patterns must be installed first. The
	// other worker modes inherit this through openWorkerWriteDB -> openDB.
	applyClassifierConfig(cfg)

	origRO, err := db.OpenReadOnly(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("resync-build: open read-only archive: %w", err)
	}
	defer origRO.Close()

	engine := sync.NewEngine(origRO, workerEngineConfig(cfg))
	defer engine.Close()

	_, stats, buildErr := engine.ResyncBuild(ctx, onProgress)
	result := resyncBuildResultFromStats(ctx, stats, buildErr)
	emit(workerLine{Result: &result})
	if result.Status != "ok" || !result.DiscoveryComplete {
		if buildErr != nil {
			return fmt.Errorf("sync worker %s: %w", mode, buildErr)
		}
		return fmt.Errorf("sync worker %s: %s", mode, result.Status)
	}
	return nil
}

// resyncBuildResultFromStats maps a resync build outcome to a terminal result.
// Unlike a live sync pass, a completed build tolerates a minority of permanent
// parse failures: shouldAbortResyncSwap already folded that judgment into
// stats.Aborted, so Failed alone must not fail the pass and make the daemon
// discard an otherwise valid replacement of a stale-version archive.
//
// Operational failures (buildErr) classify as "failed" even when the engine
// also set stats.Aborted, such as a temp-DB create failure: "aborted" is
// reserved for cancellation and explicit no-error safety aborts, because the
// daemon responds to "aborted" by falling back to an incremental sync.
func resyncBuildResultFromStats(
	ctx context.Context, stats sync.SyncStats, buildErr error,
) workerResult {
	statsCopy := stats
	result := workerResult{
		Synced:            stats.Synced,
		Skipped:           stats.Skipped,
		Failed:            stats.Failed,
		DiscoveryComplete: stats.AuthoritativeDiscoveryComplete(),
		Stats:             &statsCopy,
	}
	switch {
	case ctx.Err() != nil:
		result.Status = "aborted"
		result.DiscoveryComplete = false
		result.Error = ctx.Err().Error()
	case buildErr != nil:
		result.Status = "failed"
		result.Error = buildErr.Error()
	case stats.Aborted:
		result.Status = "aborted"
		result.DiscoveryComplete = false
	case !result.DiscoveryComplete:
		result.Status = "failed"
	default:
		result.Status = "ok"
	}
	return result
}

// refuseWorkerResync emits a failed terminal result for a live-archive worker
// pass (sync/audit) that found a stale-version archive it must not swap, and
// returns a matching error so the child exits non-zero. The daemon surfaces the
// message: a resync is required and only the resync-build flow may perform it.
func refuseWorkerResync(mode string, emit func(workerLine)) error {
	const msg = "archive data version changed; a full resync is required. " +
		"The live-archive worker will not swap the archive out from under the " +
		"running daemon: restart the daemon to resync at startup, or trigger a " +
		"resync"
	result := workerResult{Status: "failed", Error: msg}
	emit(workerLine{Result: &result})
	return fmt.Errorf("sync worker %s: %s", mode, msg)
}

// workerResultFromStats maps engine stats to a terminal result. Cancellation or
// a safety abort is "aborted"; a completed pass with hard parse failures or
// non-authoritative discovery is "failed"; otherwise "ok".
func workerResultFromStats(
	ctx context.Context, stats sync.SyncStats,
) workerResult {
	statsCopy := stats
	result := workerResult{
		Synced:            stats.Synced,
		Skipped:           stats.Skipped,
		Failed:            stats.Failed,
		DiscoveryComplete: stats.AuthoritativeDiscoveryComplete(),
		Stats:             &statsCopy,
	}
	switch {
	case ctx.Err() != nil || stats.Aborted:
		result.Status = "aborted"
		// Aborted discovery is never authoritative, even if the counters
		// happened to complete a provider listing before cancellation.
		result.DiscoveryComplete = false
		if ctx.Err() != nil {
			result.Error = ctx.Err().Error()
		}
	case stats.Failed > 0 || !result.DiscoveryComplete:
		result.Status = "failed"
	default:
		result.Status = "ok"
	}
	return result
}

// openWorkerWriteDB reuses the daemon's direct-write open path — including the
// db.write.lock acquisition — but returns errors instead of exiting. Callers
// must tear down through closeWriteDB so a failed database close (undrained
// connections) retains the write-owner flock instead of letting another
// process acquire writer ownership alongside a surviving SQLite connection.
func openWorkerWriteDB(cfg config.Config) (*db.DB, *writeOwnerLock, error) {
	return openWriteDB(context.Background(), cfg)
}

// workerEngineConfig mirrors the sync.EngineConfig literal in runServe minus the
// daemon-only callbacks (emitter, watcher reconciliation, deferred maintenance).
func workerEngineConfig(cfg config.Config) sync.EngineConfig {
	return sync.EngineConfig{
		AgentDirs:               cfg.AgentDirs,
		IncludeCwdPrefixes:      cfg.SyncIncludeCwdPrefixes,
		Machine:                 cfg.LocalMachineName,
		BlockedResultCategories: cfg.ResultContentBlockedCategories,
	}
}
