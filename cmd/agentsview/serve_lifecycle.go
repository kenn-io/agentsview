// ABOUTME: serve status and serve stop inspect and terminate a running
// ABOUTME: agentsview server using its kit daemon runtime record.
package main

import (
	"fmt"
	"os"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/kit/daemon"
)

// serveStopGraceTimeout bounds how long serve stop waits for a graceful
// shutdown after signalling before escalating to a forced kill.
const serveStopGraceTimeout = 10 * time.Second

// runServeStatus reports whether a server owns this data dir, and where to
// reach it. It always exits zero; the output distinguishes the states.
func runServeStatus(cfg config.Config) {
	if rt := FindDaemonRuntime(cfg.DataDir, cfg.AuthToken); rt != nil {
		for _, line := range serveStatusLines(rt) {
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
	if IsDaemonStarting(cfg.DataDir) {
		fmt.Println("agentsview is starting up.")
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
		lines = append(lines, "  mode:    read-only (pg serve)")
	}
	return lines
}

// runServeStop terminates every live agentsview server owning this data dir.
func runServeStop(cfg config.Config) {
	records := liveDaemonRecords(cfg.DataDir)
	if len(records) == 0 {
		if IsDaemonStarting(cfg.DataDir) {
			fatal("serve stop: a server is starting; retry once it is ready")
		}
		fmt.Println("No agentsview server is running.")
		return
	}
	for _, rec := range records {
		if err := stopDaemonProcess(rec, serveStopGraceTimeout); err != nil {
			fatal("serve stop: stopping pid %d: %v", rec.PID, err)
		}
		fmt.Printf("Stopped agentsview (pid %d).\n", rec.PID)
	}
}

// stopDaemonProcess signals the daemon to shut down, waits up to grace for it
// to exit, then escalates to a forced kill. It cleans up the runtime record if
// the process leaves one behind.
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
	if err := proc.Kill(); err != nil {
		return fmt.Errorf("force killing: %w", err)
	}
	waitForProcessExit(rec.PID, grace)
	removeRuntimeRecordFile(rec)
	return nil
}

// waitForProcessExit polls until pid is gone or timeout elapses. It reports
// whether the process exited.
func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !daemon.ProcessAlive(pid) {
			return true
		}
		time.Sleep(startProbeTick)
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
