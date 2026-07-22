package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/pflag"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/sync"
)

// workerLineMaxBytes caps a single NDJSON line from the worker. A malformed or
// unbounded line is a protocol failure, not a reason to grow memory without
// limit.
const workerLineMaxBytes = 1 << 20 // 1 MB

// Write-owner lock reacquisition retries with exponential backoff after a worker
// pass. A contended lock (another process grabbed it during the handoff) is
// transient, so the daemon keeps retrying rather than stranding itself
// read-only until restart. A package var so tests can shrink the initial delay.
var (
	reacquireBackoffInitial = 250 * time.Millisecond
	reacquireBackoffMax     = 10 * time.Second
)

// errWorkerSpawn marks a failure to start the worker process (locating the
// executable, wiring stdout, or exec). Daemon call sites fall back to the
// in-process path only when no worker ran (this error or errWorkerHandoff,
// see workerNeverRan); a worker that ran and reported a non-ok result is
// surfaced as-is rather than re-run in process.
var errWorkerSpawn = errors.New("sync worker spawn failed")

// errWorkerHandoff marks a pre-launch handoff failure: closing the writer or
// releasing the write-owner lock ahead of a worker pass failed, so no worker
// process ever ran and no sync occurred. Both failure paths restore the writer
// (and keep the flock) before returning, so callers treat it like
// errWorkerSpawn: fall back to the in-process pass or retry later.
var errWorkerHandoff = errors.New("sync worker handoff failed")

// workerNeverRan reports whether err marks a pass in which no worker process
// ran: a spawn failure or a pre-launch handoff failure. Daemon call sites gate
// their in-process fallback on it; a worker that ran and failed is surfaced
// as-is rather than re-run.
func workerNeverRan(err error) bool {
	return errors.Is(err, errWorkerSpawn) || errors.Is(err, errWorkerHandoff)
}

// launchSyncWorker is the worker-launch seam. Production self-execs the binary;
// tests stub it to exercise runWorkerWritePass without spawning a process.
var launchSyncWorker = launchSyncWorkerProcess

// closeWriterForHandoff indirects db.CloseWriter so tests can reproduce the
// close-failure posture (writer closed, error returned) without pinning
// internal pool state.
var closeWriterForHandoff = (*db.DB).CloseWriter

// closeWriterForPass closes the writer ahead of a worker handoff. A failed
// close abandons the pass: the error is returned, the caller keeps the
// write-owner flock and never launches the worker, and db retains the
// undrained pool so a later close still cannot release ownership while that
// connection survives. The writer is then reopened so the daemon keeps
// serving writes instead of returning ErrWriterClosed until restart —
// reopening cannot admit a second writer because ownership was never handed
// off and the surviving connection belongs to this process.
func closeWriterForPass(
	recoveryCtx context.Context, database *db.DB, what string,
) error {
	err := closeWriterForHandoff(database)
	if err == nil {
		return nil
	}
	if rerr := restoreArchiveAccess(
		recoveryCtx, "reopen writer after failed close for "+what,
		database.ReopenWriter,
	); rerr != nil {
		err = errors.Join(err, rerr)
	}
	return fmt.Errorf("close writer for %s: %w", what, err)
}

// runWorkerWritePass yields write ownership around a worker run against the live
// archive under the engine's exclusive sync lock. It performs no sync
// bookkeeping; foreground and deferred-startup sync passes use
// runWorkerSyncPass instead so completion is recorded with SyncThenRun parity.
// The daemon's in-memory skip cache is reloaded inside the same exclusive
// section (see workerWritePassLocked), so queued sync work never observes —
// or re-persists — skip state the worker made stale.
// recoveryCtx bounds post-pass lock and writer recovery: it must outlive the
// pass's own ctx (a request context for foreground syncs) but end at daemon
// shutdown, so persistent recovery failure cannot block exit while this pass
// holds the exclusive sync lock.
func runWorkerWritePass(
	ctx context.Context,
	recoveryCtx context.Context,
	cfg config.Config,
	engine *sync.Engine,
	database *db.DB,
	lock *writeOwnerLock,
	mode string,
	onLine func(workerLine),
) (workerResult, error) {
	var result workerResult
	err := engine.RunExclusive(func() error {
		var workerErr error
		result, workerErr = workerWritePassLocked(
			ctx, recoveryCtx, cfg, engine, database, lock, mode, onLine,
		)
		return workerErr
	})
	return result, err
}

// runWorkerSyncPass runs a "sync"-mode worker pass with SyncThenRun-equivalent
// completion semantics. When skipIfReconciled is set, the startup gate is
// rechecked while the exclusive lock is held — a foreground pass may have
// reconciled startup while this caller waited on the lock, and only a
// lock-held recheck closes that race. A worker that actually ran (spawn
// succeeded) records reconciliation and last-sync bookkeeping before the lock
// is released; the emit and startup callback fire after it, mirroring the
// in-process defer ordering. A failure in which no worker ran — a spawn
// failure or a pre-launch handoff failure — records nothing and reports
// ran=false, so the caller's in-process fallback keeps first-attempt
// semantics.
func runWorkerSyncPass(
	ctx context.Context,
	recoveryCtx context.Context,
	cfg config.Config,
	engine *sync.Engine,
	database *db.DB,
	lock *writeOwnerLock,
	skipIfReconciled bool,
	onLine func(workerLine),
) (stats sync.SyncStats, ran bool, err error) {
	recorded := false
	err = engine.RunExclusive(func() error {
		if skipIfReconciled && engine.StartupReconciled() {
			return nil
		}
		result, workerErr := workerWritePassLocked(
			ctx, recoveryCtx, cfg, engine, database, lock, "sync", onLine,
		)
		if workerNeverRan(workerErr) {
			return workerErr
		}
		ran = true
		stats = statsFromWorkerResult(result)
		engine.RecordStartupReconciledExclusive(stats, workerErr)
		recorded = true
		return workerErr
	})
	if recorded {
		engine.FinishStartupReconciled(stats)
	}
	return stats, ran, err
}

// workerWritePassLocked is the writer-handoff body: it closes the writer
// (readers keep serving), releases the write lock, runs the worker, then
// unconditionally reacquires the lock and reopens the writer. Losing write
// ownership permanently is worse than a failed pass, so reacquisition and
// reopen run even when the worker fails. The caller holds the engine's
// exclusive sync lock. The two pre-launch exits — a failed writer close and a
// failed lock release — wrap errWorkerHandoff: no worker ran, and both paths
// restore the writer (keeping the flock) before returning, so callers may
// fall back in process or retry.
func workerWritePassLocked(
	ctx context.Context,
	recoveryCtx context.Context,
	cfg config.Config,
	engine *sync.Engine,
	database *db.DB,
	lock *writeOwnerLock,
	mode string,
	onLine func(workerLine),
) (workerResult, error) {
	if err := closeWriterForPass(recoveryCtx, database, mode+" pass"); err != nil {
		return workerResult{}, fmt.Errorf("%w: %w", errWorkerHandoff, err)
	}
	if err := lock.Release(); err != nil {
		// The writer is still closed; restore it so the daemon keeps
		// writing rather than stranding the archive read-only.
		if rerr := restoreArchiveAccess(
			recoveryCtx,
			"reopen writer after failed "+mode+" lock release",
			database.ReopenWriter,
		); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return workerResult{}, fmt.Errorf(
			"%w: release write lock for %s pass: %w", errWorkerHandoff, mode, err,
		)
	}

	result, workerErr := launchSyncWorker(ctx, cfg, mode, onLine)

	// Lock recovery must not die with the caller's context: foreground
	// syncs pass the HTTP request context, and a client disconnect
	// during contention would otherwise exit the retry loop with the
	// writer closed and the lock unreacquired until restart. Recovery
	// runs on the daemon-lifetime recoveryCtx: request cancellation is
	// ignored, but daemon shutdown stops the retries so a persistent
	// failure cannot block exit under the exclusive sync lock.
	if err := reacquireWriteOwnerLock(recoveryCtx, lock, mode); err != nil {
		return result, err
	}
	if err := restoreArchiveAccess(
		recoveryCtx,
		"reopen writer after "+mode+" pass",
		database.ReopenWriter,
	); err != nil {
		return result, err
	}
	// Any worker that actually launched may have rewritten the durable skip
	// cache (new entries for synced files, deleted hash-qualified keys for
	// tombstoned sources) — even a failed or lost-result run can have
	// committed before dying. Reload while the exclusive sync lock is still
	// held: a queued watcher sync persists the in-memory snapshot wholesale,
	// so a reload deferred past this lock would let it durably resurrect
	// entries the worker removed. A spawn failure means no worker ran, so the
	// in-memory state is kept as the freshest copy.
	if !errors.Is(workerErr, errWorkerSpawn) {
		if err := engine.ReloadSkipCache(); err != nil {
			workerErr = errors.Join(workerErr, err)
		}
	}
	return result, workerErr
}

// restoreArchiveAccess retries an archive-access restoration (writer reopen,
// full reopen after a failed swap) with exponential backoff until it succeeds
// or ctx is cancelled. Restoration is mandatory: abandoning it after one
// failed attempt leaves the daemon running with every write endpoint failing
// until restart, which is worse than a failed pass. Recovery sites call this
// with the daemon-lifetime recovery context, so request cancellation cannot
// abandon restoration but daemon shutdown still stops the retries.
func restoreArchiveAccess(
	ctx context.Context, what string, restore func() error,
) error {
	backoff := reacquireBackoffInitial
	for {
		err := restore()
		if err == nil {
			return nil
		}
		log.Printf("%s failed; retrying in %s: %v", what, backoff, err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%s: %w", what, ctx.Err())
		case <-timer.C:
		}
		backoff = min(backoff*2, reacquireBackoffMax)
	}
}

// reacquireWriteOwnerLock retakes the write-owner lock after a worker pass,
// retrying with exponential backoff until it succeeds or ctx is cancelled. A
// lock briefly contended by another process is transient; giving up on the
// first failure would leave the writer closed and every write 500ing until the
// daemon restarts, so the loop keeps trying and logs each failure loudly. It
// returns an error only when ctx is cancelled, since the writer stays closed.
func reacquireWriteOwnerLock(
	ctx context.Context, lock *writeOwnerLock, mode string,
) error {
	backoff := reacquireBackoffInitial
	for {
		if err := lock.Reacquire(); err == nil {
			return nil
		} else {
			log.Printf(
				"reacquire write lock after %s pass failed; retrying in %s: %v",
				mode, backoff, err,
			)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf(
				"reacquire write lock after %s pass: %w", mode, ctx.Err(),
			)
		case <-timer.C:
		}
		backoff = min(backoff*2, reacquireBackoffMax)
	}
}

// launchSyncWorkerProcess self-execs `sync-worker --mode=<mode>`, decodes the
// child's NDJSON stdout, forwards every line to onLine, and enforces the parent
// parse contract: exactly one valid terminal result with no malformed lines. It
// returns the terminal result and a non-nil error on any protocol violation or
// non-ok terminal status, regardless of the child's exit code.
func launchSyncWorkerProcess(
	ctx context.Context,
	cfg config.Config,
	mode string,
	onLine func(workerLine),
) (workerResult, error) {
	exe, err := os.Executable()
	if err != nil {
		return workerResult{}, fmt.Errorf(
			"%w: finding executable: %v", errWorkerSpawn, err,
		)
	}

	cmd := exec.CommandContext(ctx, exe, syncWorkerChildArgs(os.Args[1:], mode)...)
	// Config forwarding mirrors startServeBackgroundProcess: the child inherits
	// the parent environment (per-agent dir overrides, AGENTSVIEW_* vars), plus
	// the worker marker and the resolved data dir. syncWorkerChildArgs forwards
	// the parent's serve config flags so CLI overrides also reach the child.
	cmd.Env = append(os.Environ(), syncWorkerChildEnvVar+"=1")
	if cfg.DataDir != "" {
		cmd.Env = append(cmd.Env, "AGENTSVIEW_DATA_DIR="+cfg.DataDir)
	}
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return workerResult{}, fmt.Errorf(
			"%w: stdout pipe: %v", errWorkerSpawn, err,
		)
	}
	if err := cmd.Start(); err != nil {
		return workerResult{}, fmt.Errorf(
			"%w: starting process: %v", errWorkerSpawn, err,
		)
	}

	result, parseErr, waitErr := collectWorkerResult(cmd, stdout, onLine)

	if parseErr != nil {
		return result, fmt.Errorf("%s worker: %w", mode, parseErr)
	}
	if result.Status != "ok" || !result.DiscoveryComplete {
		detail := result.Status
		if result.Error != "" {
			detail = fmt.Sprintf("%s (%s)", result.Status, result.Error)
		}
		return result, fmt.Errorf("%s worker pass reported %s", mode, detail)
	}
	if waitErr != nil {
		return result, fmt.Errorf("%s worker process: %w", mode, waitErr)
	}
	return result, nil
}

// collectWorkerResult parses the worker's NDJSON stdout to completion, then
// waits for the process to exit. Stdout must be fully consumed before Wait: a
// scanner error (most likely a line beyond the workerLineMaxBytes cap) stops
// parsing mid-stream, and calling Wait with output still unread would deadlock
// — the child blocks writing to the full pipe and never exits. The remainder
// is discarded instead, bounded by what the child actually writes, so the
// protocol error surfaces alongside the worker's real exit status.
func collectWorkerResult(
	cmd *exec.Cmd, stdout io.Reader, onLine func(workerLine),
) (result workerResult, parseErr, waitErr error) {
	result, parseErr = readWorkerResult(stdout, onLine)
	if parseErr != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr = cmd.Wait()
	return result, parseErr, waitErr
}

// readWorkerResult scans the worker's NDJSON stdout, forwarding each decoded
// line to onLine, and enforces the terminal-record contract: every non-blank
// line must be a valid workerLine and exactly one must carry a Result. Any
// malformed line, or a result count other than one, is a protocol error.
func readWorkerResult(
	r io.Reader, onLine func(workerLine),
) (workerResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), workerLineMaxBytes)

	var result workerResult
	resultCount, malformed := 0, 0
	for sc.Scan() {
		raw := sc.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var line workerLine
		if err := json.Unmarshal(raw, &line); err != nil {
			malformed++
			continue
		}
		if onLine != nil {
			onLine(line)
		}
		if line.Result != nil {
			resultCount++
			result = *line.Result
		}
	}
	if err := sc.Err(); err != nil {
		return result, fmt.Errorf("reading worker output: %w", err)
	}
	if malformed > 0 {
		return result, fmt.Errorf(
			"sync worker emitted %d malformed output line(s)", malformed,
		)
	}
	if resultCount != 1 {
		return result, fmt.Errorf(
			"sync worker emitted %d terminal results, want exactly 1",
			resultCount,
		)
	}
	return result, nil
}

// syncWorkerChildArgs builds the child argv for the sync worker. It always
// carries the mode, and forwards the serve config flags the parent daemon was
// invoked with so CLI overrides reach the child identically to a serve
// --background child. Re-emitting the parsed flags (rather than copying raw
// tokens) drops serve-only lifecycle flags the worker does not accept and
// normalizes every value to an unambiguous --name=value form.
func syncWorkerChildArgs(parentArgs []string, mode string) []string {
	args := []string{"sync-worker", "--mode", mode}
	fs := pflag.NewFlagSet("sync-worker-forward", pflag.ContinueOnError)
	fs.ParseErrorsAllowlist.UnknownFlags = true
	config.RegisterServePFlags(fs)
	// Parse ignores the leading `serve` subcommand token (a positional) and any
	// serve-only flags (whitelisted as unknown); a parse error only means fewer
	// forwarded flags, never a failed spawn.
	_ = fs.Parse(parentArgs)
	fs.Visit(func(f *pflag.Flag) {
		args = append(args, "--"+f.Name+"="+f.Value.String())
	})
	return args
}
