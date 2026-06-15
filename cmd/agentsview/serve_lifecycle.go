// ABOUTME: serve status and serve stop inspect and terminate a running
// ABOUTME: agentsview server using its kit daemon runtime record.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/kit/daemon"
)

// serveStopGraceTimeout bounds how long serve stop waits for a graceful
// shutdown after signalling before escalating to a forced kill.
const serveStopGraceTimeout = 10 * time.Second

// processIdentitySlack tolerates clock rounding between the OS-reported process
// create time and the record's StartedAt when confirming a stop target by
// process identity.
const processIdentitySlack = 2 * time.Second

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
		lines = append(lines, "  mode:    read-only")
	}
	return lines
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
		fmt.Printf("Stopped agentsview (pid %d).\n", rec.PID)
		stopped++
	}
	if stopped == 0 && skipped > 0 {
		fmt.Println(
			"No agentsview server was stopped; runtime records may be stale.",
		)
	}
}

// stopTargetConfirmed reports whether rec's live PID is safe to signal as the
// recorded agentsview daemon. It accepts the target when the daemon answers the
// ping probe, or, for a daemon that is alive but no longer answering, when the
// process start time predates the record. Either check rules out a PID that an
// unrelated process reused after the record was written.
func stopTargetConfirmed(rec daemon.RuntimeRecord, authToken string) bool {
	return daemonRecordPingConfirmed(rec, authToken) ||
		processStartedBeforeRecord(rec)
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

// processStartedBeforeRecord reports whether the process now holding rec.PID
// started no later than the record was written. The recorded daemon was alive
// when it wrote StartedAt, so its create time predates StartedAt; a process
// that reused the PID after the daemon died started afterward. Records without
// a StartedAt (legacy) cannot be checked this way and return false.
func processStartedBeforeRecord(rec daemon.RuntimeRecord) bool {
	if rec.StartedAt.IsZero() {
		return false
	}
	proc, err := process.NewProcess(int32(rec.PID))
	if err != nil {
		return false
	}
	createdMillis, err := proc.CreateTime()
	if err != nil {
		return false
	}
	created := time.UnixMilli(createdMillis)
	return !created.After(rec.StartedAt.Add(processIdentitySlack))
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
