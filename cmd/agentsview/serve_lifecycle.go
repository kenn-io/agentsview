// ABOUTME: serve status and serve stop inspect and terminate a running
// ABOUTME: agentsview server using its kit daemon runtime record.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/kit/daemon"
)

// serveStopGraceTimeout bounds how long serve stop waits for a graceful
// shutdown after signalling before escalating to a forced kill.
const serveStopGraceTimeout = 10 * time.Second

var stopDaemonRuntimeForUpgrade = stopDaemonRuntimeForUpgradeImpl

type daemonStopOperations struct {
	confirmed func(daemon.RuntimeRecord, string) bool
	stop      func(daemon.RuntimeRecord, time.Duration) error
	cleanup   func(io.Writer, daemon.RuntimeRecord) error
}

// stopWritableDaemonRecordsSafely prevalidates the identity of every target
// before signalling any of them. This prevents a corrupt multi-writer runtime
// set from being only partially stopped when one record is stale or its PID
// has been reused.
func stopWritableDaemonRecordsSafely(
	w io.Writer,
	cfg config.Config,
	records []daemon.RuntimeRecord,
	ops daemonStopOperations,
) error {
	_, unconfirmedRecords := partitionConfirmedDaemonRecords(
		records, cfg.AuthToken, ops.confirmed,
	)
	var unconfirmed []string
	for _, rec := range unconfirmedRecords {
		detail := fmt.Sprintf("pid %d", rec.PID)
		if rec.SourcePath != "" {
			detail += " (runtime record " + rec.SourcePath + ")"
		}
		unconfirmed = append(unconfirmed, detail)
	}
	if len(unconfirmed) > 0 {
		return fmt.Errorf(
			"cannot confirm every writable agentsview daemon: %s; no process was signalled; verify each process and terminate it manually before retrying",
			strings.Join(unconfirmed, ", "),
		)
	}
	stopped := make([]int, 0, len(records))
	for i, rec := range records {
		if !ops.confirmed(rec, cfg.AuthToken) {
			if len(stopped) == 0 {
				return fmt.Errorf(
					"stop aborted before signaling; no process was signalled; remaining pids %s; pid %d identity changed; verify remaining processes and terminate them manually before retrying",
					formatRecordPIDList(records[i:]), rec.PID,
				)
			}
			return partialDaemonStopError(
				stopped, records[i:],
				fmt.Errorf("pid %d identity changed before signaling", rec.PID),
			)
		}
		if err := ops.stop(rec, serveStopGraceTimeout); err != nil {
			return partialDaemonStopError(
				stopped, records[i:], fmt.Errorf("stopping pid %d: %w", rec.PID, err),
			)
		}
		stopped = append(stopped, rec.PID)
		fmt.Fprintf(w, "Stopped agentsview (pid %d).\n", rec.PID)
		if err := ops.cleanup(w, rec); err != nil {
			return partialDaemonStopError(
				stopped, records[i+1:],
				fmt.Errorf("managed caddy cleanup for pid %d: %w", rec.PID, err),
			)
		}
	}
	return nil
}

func partitionConfirmedDaemonRecords(
	records []daemon.RuntimeRecord,
	authToken string,
	confirmed func(daemon.RuntimeRecord, string) bool,
) (confirmedRecords, unconfirmedRecords []daemon.RuntimeRecord) {
	for _, rec := range records {
		if confirmed(rec, authToken) {
			confirmedRecords = append(confirmedRecords, rec)
		} else {
			unconfirmedRecords = append(unconfirmedRecords, rec)
		}
	}
	return confirmedRecords, unconfirmedRecords
}

func formatRecordPIDList(records []daemon.RuntimeRecord) string {
	pids := make([]int, 0, len(records))
	for _, rec := range records {
		pids = append(pids, rec.PID)
	}
	return formatPIDList(pids)
}

func partialDaemonStopError(
	stopped []int, remaining []daemon.RuntimeRecord, cause error,
) error {
	stoppedText := "none"
	if len(stopped) > 0 {
		stoppedText = formatPIDList(stopped)
	}
	remainingPIDs := make([]int, 0, len(remaining))
	for _, rec := range remaining {
		remainingPIDs = append(remainingPIDs, rec.PID)
	}
	remainingText := "none"
	if len(remainingPIDs) > 0 {
		remainingText = formatPIDList(remainingPIDs)
	}
	return fmt.Errorf(
		"partial stop: stopped pid%s %s; remaining pids %s; %w; verify remaining processes and terminate them manually before retrying",
		pluralSuffix(len(stopped)), stoppedText, remainingText, cause,
	)
}

func formatPIDList(pids []int) string {
	parts := make([]string, 0, len(pids))
	for _, pid := range pids {
		parts = append(parts, strconv.Itoa(pid))
	}
	return strings.Join(parts, ", ")
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

// runServeStatus reports whether a server owns this data dir, and where to
// reach it. It always exits zero; the output distinguishes the states.
func runServeStatus(cfg config.Config) {
	var readOnly *DaemonRuntime
	if rt := FindDaemonRuntime(cfg.DataDir, cfg.AuthToken); rt != nil {
		if rt.ReadOnly {
			readOnly = rt
		} else {
			for _, line := range serveStatusLines(rt) {
				fmt.Println(line)
			}
			return
		}
	}
	if rt, compatErr := findIncompatibleWritableDaemonRuntime(
		cfg.DataDir, cfg.AuthToken,
	); rt != nil {
		for _, line := range serveIncompatibleDaemonStatusLines(rt, compatErr) {
			fmt.Println(line)
		}
		return
	}
	if IsDaemonStarting(cfg.DataDir) {
		fmt.Println("agentsview is starting up.")
		st := readStartupState(cfg.DataDir)
		for _, line := range serveStartingStatusLines(st, time.Now()) {
			fmt.Println(line)
		}
		if st != nil && st.Host != "" && st.Port > 0 && st.RuntimeError != "" {
			fmt.Println("  stale fallback: endpoint or process identity could not be confirmed")
		}
		return
	}
	if readOnly != nil {
		for _, line := range serveStatusLines(readOnly) {
			fmt.Println(line)
		}
		return
	}
	if recs := liveDaemonRecords(cfg.DataDir); len(recs) > 0 {
		fmt.Printf(
			"agentsview process running (pid %d) but not responding "+
				"to health checks.\n",
			recs[0].PID,
		)
		return
	}
	fmt.Println("No agentsview server is running.")
}

// serveStatusLines renders the human-readable status of a discovered daemon.
func serveStatusLines(rt *DaemonRuntime) []string {
	lines := []string{
		fmt.Sprintf("agentsview running at %s", urlFromDaemonRuntime(rt)),
		fmt.Sprintf("  pid:     %d", rt.Record.PID),
	}
	if rt.Record.Version != "" {
		lines = append(lines, fmt.Sprintf("  version: %s", rt.Record.Version))
	}
	if !rt.Record.StartedAt.IsZero() {
		uptime := time.Since(rt.Record.StartedAt).Round(time.Second)
		lines = append(lines, fmt.Sprintf("  uptime:  %s", uptime))
	}
	if rt.ReadOnly {
		lines = append(lines, "  mode:    read-only")
	}
	if rt.RuntimeFallback {
		lines = append(lines, "  runtime record unwritten: "+rt.RuntimeError)
	}
	return lines
}

// serveStartingStatusLines renders the detail lines for a daemon that
// holds the start lock, from its published startup state. A nil state
// (legacy daemon version, unreadable or mid-write file) yields no
// extra lines; the state is only trusted while the start lock is
// held, so staleness needs no handling here.
func serveStartingStatusLines(st *startupState, now time.Time) []string {
	if st == nil {
		return nil
	}
	var lines []string
	if st.PID > 0 {
		lines = append(lines, fmt.Sprintf("  pid:     %d", st.PID))
	}
	if !st.StartedAt.IsZero() {
		if elapsed := now.Sub(st.StartedAt).Round(time.Second); elapsed >= 0 {
			lines = append(lines, fmt.Sprintf("  elapsed: %s", elapsed))
		}
	}
	if st.Phase != "" {
		phase := st.Phase
		if st.Detail != "" {
			phase += ": " + st.Detail
		}
		lines = append(lines, "  phase:   "+phase)
	}
	if st.LogPath != "" {
		lines = append(lines, "  log:     "+st.LogPath)
	}
	return lines
}

func serveIncompatibleDaemonStatusLines(
	rt *DaemonRuntime, compatErr error,
) []string {
	decision := serveReplacementDecision{
		Action:           serveReplacementRefuse,
		Runtime:          rt,
		CompatibilityErr: compatErr,
		Reason:           serveDaemonRefusalReason(rt, compatErr),
	}
	lines := serveDaemonDecisionLines(
		"agentsview found an incompatible running writable daemon.",
		decision,
	)
	return append(lines,
		"Run `agentsview daemon restart` to replace it, or "+
			"`agentsview daemon stop` to stop it first.",
	)
}

// runServeStop terminates every agentsview server owning this data dir whose
// identity it can confirm. A record is signalled only once its PID is confirmed
// to be the recorded daemon -- either it answers the ping probe, or its process
// start time predates the record (proving the PID was not reused by an
// unrelated process). This keeps a hung-but-alive daemon stoppable while never
// signalling a stale record whose PID belongs to something else.
func runServeStop(cfg config.Config) {
	records, _ := localWritableDaemonRecordsWithFallback(
		cfg.DataDir, cfg.AuthToken,
	)
	if len(records) == 0 {
		if IsDaemonStarting(cfg.DataDir) {
			fatal("serve stop: a server is starting; retry once it is ready")
		}
		fmt.Println("No agentsview server is running.")
		return
	}
	stopped, skipped := 0, 0
	for _, rec := range records {
		if !stopTargetConfirmed(rec, cfg.AuthToken) {
			fmt.Printf(
				"Skipping pid %d: cannot confirm it is the recorded "+
					"agentsview daemon (stale record or reused pid).\n",
				rec.PID,
			)
			skipped++
			continue
		}
		if err := stopDaemonProcess(rec, serveStopGraceTimeout); err != nil {
			fatal("serve stop: stopping pid %d: %v", rec.PID, err)
		}
		stopOrphanedCaddyChild(rec)
		fmt.Printf("Stopped agentsview (pid %d).\n", rec.PID)
		stopped++
	}
	if stopped == 0 && skipped > 0 {
		fmt.Println(
			"No agentsview server was stopped; runtime records may be stale.",
		)
	}
}

func stopDaemonRuntimeForUpgradeImpl(
	cfg config.Config, rt *DaemonRuntime,
) error {
	if rt == nil {
		return nil
	}
	if !stopTargetConfirmed(rt.Record, cfg.AuthToken) {
		return fmt.Errorf(
			"cannot confirm pid %d is the recorded agentsview daemon",
			rt.Record.PID,
		)
	}
	if err := stopDaemonProcess(rt.Record, serveStopGraceTimeout); err != nil {
		return fmt.Errorf("stopping pid %d: %w", rt.Record.PID, err)
	}
	stopOrphanedCaddyChild(rt.Record)
	return nil
}

func stopWritableDaemonsForUpdate(
	cfg config.Config,
) (updateDaemonStopResult, error) {
	records, _ := localWritableDaemonRecordsWithFallback(
		cfg.DataDir, cfg.AuthToken,
	)
	var result updateDaemonStopResult
	for _, rec := range records {
		rt := daemonRuntimeFromRecord(rec)
		if rt.ReadOnly {
			continue
		}
		// A data dir supports one writable daemon. If multiple live writable
		// records somehow exist, stop every old writer before the update but
		// restart one replacement using the first runtime's externally visible
		// settings. Restarting multiple writers would recreate the invalid
		// state this path is trying to collapse.
		if !result.Stopped {
			result.Host = rt.Host
			result.Port = rt.Port
			result.RequireAuth = rt.RequireAuth
			result.RequireAuthKnown = rt.RequireAuthKnown
			result.NoSync = rt.NoSync
		}
		if err := stopDaemonRuntimeForUpgrade(cfg, rt); err != nil {
			return result, err
		}
		result.Stopped = true
	}
	if !result.Stopped && IsDaemonStarting(cfg.DataDir) {
		return result, fmt.Errorf(
			"agentsview server is starting; retry the update once it is ready",
		)
	}
	return result, nil
}

// stopTargetConfirmed reports whether rec's live PID is safe to signal as the
// recorded agentsview daemon. It accepts the target when the daemon answers the
// ping probe, or, for a daemon that is alive but no longer answering, when the
// process create time exactly matches the one recorded at startup. Either check
// rules out a PID that an unrelated process reused after the record was
// written.
func stopTargetConfirmed(rec daemon.RuntimeRecord, authToken string) bool {
	return daemonRecordPingConfirmed(rec, authToken) ||
		processIdentityConfirmed(rec)
}

// daemonRecordPingConfirmed reports whether rec's PID answers the kit ping
// probe as the agentsview daemon it claims to be.
func daemonRecordPingConfirmed(
	rec daemon.RuntimeRecord, authToken string,
) bool {
	_, confirmed := probeDaemonRecord(rec, authToken)
	return confirmed
}

// probeDaemonRecord returns the ping identity when rec's PID answers as the
// agentsview daemon it claims to be.
func probeDaemonRecord(
	rec daemon.RuntimeRecord, authToken string,
) (daemon.PingInfo, bool) {
	info, err := probeRuntime(
		context.Background(), rec, authToken, daemon.ProbeOptions{
			ExpectedService: daemonService,
			Timeout:         500 * time.Millisecond,
		},
	)
	return info, err == nil && info.PID == rec.PID
}

// processIdentityConfirmed reports whether the process now holding rec.PID is
// the same one that wrote the record, by matching the OS create time persisted
// at startup against the live process's current create time.
func processIdentityConfirmed(rec daemon.RuntimeRecord) bool {
	return processCreateTimeMatches(rec.PID, rec.Metadata[runtimeCreateTime])
}

// processCreateTimeMatches reports whether pid's current OS create time equals
// recordedMillis. The match is exact: the create time is fixed for a given
// process, so a PID reused by a different process yields a different value and
// is rejected -- there is no slack window an impostor could fall into. An empty
// or unparseable recordedMillis (legacy, or unreadable at write time) returns
// false.
func processCreateTimeMatches(pid int, recordedMillis string) bool {
	return processCreateTimeStateForPID(
		pid, recordedMillis,
	) == processCreateTimeMatch
}

// stopOrphanedCaddyChild terminates a managed Caddy child recorded in rec if it
// is still alive after the server stopped. When the server shuts down
// gracefully it stops Caddy itself, so by here Caddy is already gone and this
// is a no-op. When the server had to be force-killed (it ignored SIGTERM until
// the grace timeout, then took SIGKILL), its cleanup never ran and Caddy would
// otherwise keep holding the public port. The create time is matched exactly
// before signalling, so a reused Caddy PID is never touched.
func stopOrphanedCaddyChild(rec daemon.RuntimeRecord) {
	if err := stopOrphanedCaddyChildWithWriter(os.Stdout, rec); err != nil {
		raw := rec.Metadata[runtimeCaddyPID]
		fmt.Printf(
			"warning: could not stop managed caddy (pid %s): %v\n", raw, err,
		)
	}
}

// stopOrphanedCaddyChildWithWriter is the canonical, error-returning managed
// Caddy cleanup path. Callers choose the output writer and decide whether a
// cleanup failure is fatal to a larger lifecycle transition.
func stopOrphanedCaddyChildWithWriter(
	w io.Writer, rec daemon.RuntimeRecord,
) error {
	raw := rec.Metadata[runtimeCaddyPID]
	if raw == "" {
		return nil
	}
	pid, err := strconv.Atoi(raw)
	if err != nil || pid <= 0 {
		return nil
	}
	if !daemon.ProcessAlive(pid) {
		return nil
	}
	caddyCreateTime := rec.Metadata[runtimeCaddyCreateTime]
	switch processCreateTimeStateForPID(pid, caddyCreateTime) {
	case processCreateTimeMismatch:
		return nil
	case processCreateTimeUnknown:
		return fmt.Errorf(
			"managed caddy pid %d identity could not be confirmed; refusing to signal it",
			pid,
		)
	case processCreateTimeMatch:
		// Exact identity authorizes shutdown below.
	default:
		return fmt.Errorf(
			"managed caddy pid %d identity could not be confirmed; refusing to signal it",
			pid,
		)
	}
	if err := stopDaemonProcess(
		caddyStopRecord(pid, caddyCreateTime), serveStopGraceTimeout,
	); err != nil {
		return err
	}
	fmt.Fprintf(w, "Stopped managed caddy (pid %d).\n", pid)
	return nil
}

// caddyStopRecord builds the record used to stop a managed Caddy child. It has
// no SourcePath, so stopDaemonProcess only signals and waits and removes no
// record file. The Caddy create time is carried as runtimeCreateTime so
// stopDaemonProcess's pre-force-kill identity check guards a Caddy PID that was
// reused during the grace wait.
func caddyStopRecord(pid int, createTime string) daemon.RuntimeRecord {
	return daemon.RuntimeRecord{
		PID:      pid,
		Metadata: map[string]string{runtimeCreateTime: createTime},
	}
}

// stopDaemonProcess signals the daemon to shut down, waits up to grace for it
// to exit, then escalates to a forced kill. Before escalating it re-checks that
// the live PID is still the recorded daemon, so a PID reused during the grace
// wait is never killed. It cleans up the runtime record if the process leaves
// one behind.
func stopDaemonProcess(rec daemon.RuntimeRecord, grace time.Duration) error {
	return stopDaemonProcessWithIdentity(
		rec, grace, processCreateTimeStateForPID,
	)
}

func stopDaemonProcessWithIdentity(
	rec daemon.RuntimeRecord,
	grace time.Duration,
	identityStateForPID func(int, string) processCreateTimeState,
) error {
	proc, err := os.FindProcess(rec.PID)
	if err != nil {
		return fmt.Errorf("finding process: %w", err)
	}
	if err := terminateProcess(proc); err != nil {
		return fmt.Errorf("signalling shutdown: %w", err)
	}
	if waitForProcessExit(rec.PID, grace) {
		removeRuntimeRecordFile(rec)
		return nil
	}
	identityState := identityStateForPID(
		rec.PID, rec.Metadata[runtimeCreateTime],
	)
	switch identityState {
	case processCreateTimeMismatch:
		// The PID is alive but its identity no longer matches the record: the
		// daemon exited during the grace wait and the PID was reused by an
		// unrelated process. Do not force-kill the impostor; just drop the
		// stale record.
		removeRuntimeRecordFile(rec)
		return nil
	case processCreateTimeUnknown:
		return processIdentityUnconfirmedError(rec, "after graceful shutdown")
	case processCreateTimeMatch:
		// Only an exact identity match authorizes force-kill escalation.
	default:
		return processIdentityUnconfirmedError(rec, "after graceful shutdown")
	}
	if err := proc.Kill(); err != nil {
		return fmt.Errorf("force killing: %w", err)
	}
	if waitForProcessExit(rec.PID, grace) {
		removeRuntimeRecordFile(rec)
		return nil
	}
	identityState = identityStateForPID(
		rec.PID, rec.Metadata[runtimeCreateTime],
	)
	switch identityState {
	case processCreateTimeMismatch:
		// The daemon exited after SIGKILL and the PID was reused. The record
		// is now proven stale, so remove it without signalling the new owner.
		removeRuntimeRecordFile(rec)
		return nil
	case processCreateTimeUnknown:
		return processIdentityUnconfirmedError(rec, "after force kill")
	case processCreateTimeMatch:
		// The recorded daemon genuinely outlived even SIGKILL. Keep the
		// runtime record so other commands still see it owns the DB rather
		// than racing it.
		return fmt.Errorf("process %d still running after force kill", rec.PID)
	default:
		return processIdentityUnconfirmedError(rec, "after force kill")
	}
}

func processIdentityUnconfirmedError(
	rec daemon.RuntimeRecord, phase string,
) error {
	if rec.SourcePath != "" {
		return fmt.Errorf(
			"process %d identity could not be confirmed %s; refusing further signaling; runtime record preserved at %s for manual recovery",
			rec.PID, phase, rec.SourcePath,
		)
	}
	return fmt.Errorf(
		"process %d identity could not be confirmed %s; refusing further signaling; inspect the process manually",
		rec.PID, phase,
	)
}

// waitForProcessExit polls until pid is gone or timeout elapses. It reports
// whether the process exited.
func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !daemon.ProcessAlive(pid) {
			return true
		}
		time.Sleep(startProbeTick())
	}
	return !daemon.ProcessAlive(pid)
}

// removeRuntimeRecordFile deletes the daemon's runtime record. A graceful
// shutdown removes its own record; a forced kill does not, so clean up the
// stale file to keep discovery accurate.
func removeRuntimeRecordFile(rec daemon.RuntimeRecord) {
	if rec.SourcePath == "" {
		return
	}
	_ = os.Remove(rec.SourcePath)
}
