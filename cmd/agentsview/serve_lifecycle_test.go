package main

import (
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestLiveDaemonRecordsFiltersDeadProcesses(t *testing.T) {
	dir := runtimeTestDir(t)

	// WriteDaemonRuntime stamps the record with this live test process PID.
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", false)
	require.NoError(t, err)

	// A record for a process that has already exited must be excluded.
	dead := startReapedProcess(t)
	_, err = writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:     dead,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
	})
	require.NoError(t, err)

	records := liveDaemonRecords(dir)
	require.Len(t, records, 1)
	assert.NotEqual(t, dead, records[0].PID)
	assert.NotEmpty(t, records[0].SourcePath,
		"List must populate SourcePath so stop can clean up the record")
}

func TestServeStatusLinesWritable(t *testing.T) {
	rt := &DaemonRuntime{
		Record: daemon.RuntimeRecord{
			PID:       4242,
			Version:   "9.9.9",
			StartedAt: time.Now().Add(-90 * time.Second),
		},
		Host: "127.0.0.1",
		Port: 8080,
	}

	out := strings.Join(serveStatusLines(rt), "\n")
	assert.Contains(t, out, "running at http://127.0.0.1:8080")
	assert.Contains(t, out, "pid:     4242")
	assert.Contains(t, out, "version: 9.9.9")
	assert.Contains(t, out, "uptime:")
	assert.NotContains(t, out, "read-only")
}

func TestServeStatusLinesReadOnly(t *testing.T) {
	rt := &DaemonRuntime{
		Record:   daemon.RuntimeRecord{PID: 7},
		Host:     "127.0.0.1",
		Port:     9000,
		ReadOnly: true,
	}

	out := strings.Join(serveStatusLines(rt), "\n")
	assert.Contains(t, out, "mode:    read-only")
	assert.NotContains(t, out, "uptime:", "zero StartedAt must omit uptime")
}

func TestAcquireBackgroundLaunchLockSerializes(t *testing.T) {
	dir := t.TempDir()

	first, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok, "first launch must acquire the lock")

	_, ok = acquireBackgroundLaunchLock(dir)
	assert.False(t, ok, "a concurrent launch must not acquire the lock")

	require.NoError(t, first.Unlock())

	third, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok, "lock must be reacquirable after release")
	require.NoError(t, third.Unlock())
}

func TestServeCommandHasLifecycleSubcommands(t *testing.T) {
	cmd := newServeCommand()
	names := map[string]bool{}
	for _, sub := range cmd.Commands() {
		names[sub.Name()] = true
	}
	assert.True(t, names["status"], "serve must expose a status subcommand")
	assert.True(t, names["stop"], "serve must expose a stop subcommand")
}

func TestStopDaemonProcessTerminatesAndCleansRecord(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("graceful SIGTERM termination is POSIX-specific")
	}
	dir := runtimeTestDir(t)

	target := exec.Command("sleep", "60")
	require.NoError(t, target.Start())
	pid := target.Process.Pid
	// Reap concurrently so the process leaves the zombie state once it
	// exits; daemon.ProcessAlive treats an unreaped zombie as alive.
	reaped := make(chan struct{})
	go func() {
		_ = target.Wait()
		close(reaped)
	}()
	t.Cleanup(func() { _ = target.Process.Kill() })

	_, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:     pid,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
	})
	require.NoError(t, err)

	records := liveDaemonRecords(dir)
	require.Len(t, records, 1)

	require.NoError(t, stopDaemonProcess(records[0], 5*time.Second))
	<-reaped
	assert.False(t, daemon.ProcessAlive(pid))
	assert.Empty(t, liveDaemonRecords(dir),
		"runtime record must be removed after stop")
}

func TestDaemonRecordIdentityConfirmedRespondingDaemon(t *testing.T) {
	host, port := testPingServer(t)
	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, strconv.Itoa(port)),
		Service: daemonService,
	}
	assert.True(t, daemonRecordIdentityConfirmed(rec, ""))
}

func TestDaemonRecordIdentityConfirmedUnresponsivePID(t *testing.T) {
	// A live PID (this process) but a record pointing at a port with no
	// agentsview daemon. The probe must fail so stop does not signal a
	// process that merely reused a stale record's PID.
	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
	}
	assert.False(t, daemonRecordIdentityConfirmed(rec, ""))
}

func TestDaemonRecordIdentityConfirmedRequiresAuthToken(t *testing.T) {
	host, port := testAuthenticatedPingServer(t, "secret")
	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, strconv.Itoa(port)),
		Service: daemonService,
	}
	assert.False(t, daemonRecordIdentityConfirmed(rec, ""),
		"a require_auth daemon must not be confirmed without the token")
	assert.True(t, daemonRecordIdentityConfirmed(rec, "secret"))
}

// startReapedProcess starts and fully reaps a short-lived process, returning a
// PID that is guaranteed dead.
func startReapedProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit")
	}
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.NoError(t, cmd.Wait())
	return pid
}
