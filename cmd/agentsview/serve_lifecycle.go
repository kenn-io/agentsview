// ABOUTME: serve status and serve stop inspect and terminate a running
// ABOUTME: agentsview server using its kit daemon runtime record.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/kit/daemon"
)

// serveStopGraceTimeout bounds how long serve stop waits for a graceful
// shutdown after signalling before escalating to a forced kill.
const serveStopGraceTimeout = 10 * time.Second

var stopDaemonRuntimeForUpgrade = stopDaemonRuntimeForUpgradeImpl

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
		"Run `agentsview serve --replace` to replace it, or "+
			"`agentsview serve stop` to stop it first.",
	)
}

// runServeStop terminates every agentsview server owning this data dir whose
// identity it can confirm. A record is signalled only once its PID is confirmed
// to be the recorded daemon -- either it answers the ping probe, or its process
// start time predates the record (proving the PID was not reused by an
// unrelated process). This keeps a hung-but-alive daemon stoppable while never
// signalling a stale record whose PID belongs to something else.
func runServeStop(cfg config.Config) {
	records := liveDaemonRecords(cfg.DataDir)
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
	records := liveDaemonRecords(cfg.DataDir)
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
	info, err := probeRuntime(
		context.Background(), rec, authToken, daemon.ProbeOptions{
			ExpectedService: daemonService,
			Timeout:         500 * time.Millisecond,
		},
	)
	return err == nil && info.PID == rec.PID
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
	if recordedMillis == "" {
		return false
	}
	recorded, err := strconv.ParseInt(recordedMillis, 10, 64)
	if err != nil {
		return false
	}
	live, ok := processCreateTimeMillis(pid)
	if !ok {
		return false
	}
	return live == recorded
}

// stopOrphanedCaddyChild terminates a managed Caddy child recorded in rec if it
// is still alive after the server stopped. When the server shuts down
// gracefully it stops Caddy itself, so by here Caddy is already gone and this
// is a no-op. When the server had to be force-killed (it ignored SIGTERM until
// the grace timeout, then took SIGKILL), its cleanup never ran and Caddy would
// otherwise keep holding the public port. The create time is matched exactly
// before signalling, so a reused Caddy PID is never touched.
func stopOrphanedCaddyChild(rec daemon.RuntimeRecord) {
	raw := rec.Metadata[runtimeCaddyPID]
	if raw == "" {
		return
	}
	pid, err := strconv.Atoi(raw)
	if err != nil || pid <= 0 {
		return
	}
	if !daemon.ProcessAlive(pid) {
		return
	}
	caddyCreateTime := rec.Metadata[runtimeCaddyCreateTime]
	if !processCreateTimeMatches(pid, caddyCreateTime) {
		return
	}
	if err := stopDaemonProcess(
		caddyStopRecord(pid, caddyCreateTime), serveStopGraceTimeout,
	); err != nil {
		fmt.Printf(
			"warning: could not stop managed caddy (pid %d): %v\n", pid, err,
		)
		return
	}
	fmt.Printf("Stopped managed caddy (pid %d).\n", pid)
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
	if !recordedDaemonStillPresent(rec) {
		// The PID is alive but its identity no longer matches the record: the
		// daemon exited during the grace wait and the PID was reused by an
		// unrelated process. Do not force-kill the impostor; just drop the
		// stale record.
		removeRuntimeRecordFile(rec)
		return nil
	}
	if err := proc.Kill(); err != nil {
		return fmt.Errorf("force killing: %w", err)
	}
	if !waitForProcessExit(rec.PID, grace) && recordedDaemonStillPresent(rec) {
		// The recorded daemon genuinely outlived even SIGKILL. Keep the
		// runtime record so other commands still see it owns the DB rather
		// than racing it. (A live PID whose identity no longer matches the
		// record was reused after the daemon exited, so fall through and
		// remove the stale record.)
		return fmt.Errorf("process %d still running after force kill", rec.PID)
	}
	removeRuntimeRecordFile(rec)
	return nil
}

// recordedDaemonStillPresent reports whether rec.PID still belongs to the
// recorded daemon. With a persisted create time it is an exact identity match,
// so a PID reused by an unrelated process is reported as not present. Without a
// create time (legacy record) it conservatively assumes the live PID is still
// the daemon.
func recordedDaemonStillPresent(rec daemon.RuntimeRecord) bool {
	raw := rec.Metadata[runtimeCreateTime]
	if raw == "" {
		return true
	}
	return processCreateTimeMatches(rec.PID, raw)
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
