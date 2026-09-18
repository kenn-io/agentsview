package main

import (
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/kit/daemon"
)

func TestLiveDaemonRecordsFiltersDeadProcesses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := runtimeTestDir(t)

	// WriteDaemonRuntime stamps the record with this live test process PID.
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", false)
	require.NoError(err)

	// A record for a dead process must be excluded.
	dead := deadPID(t)
	_, err = writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:     dead,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
	})
	require.NoError(err)

	records := liveDaemonRecords(dir)
	require.Len(records, 1)
	assert.NotEqual(dead, records[0].PID)
	assert.NotEmpty(records[0].SourcePath,
		"List must populate SourcePath so stop can clean up the record")
}

func TestServeStatusLinesWritable(t *testing.T) {
	assert := assert.New(t)

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
	assert.Contains(out, "running at http://127.0.0.1:8080")
	assert.Contains(out, "pid:     4242")
	assert.Contains(out, "version: 9.9.9")
	assert.Contains(out, "uptime:")
	assert.NotContains(out, "read-only")
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

func TestRunServeStatusReportsIncompatibleWritableDaemon(t *testing.T) {
	assert := assert.New(t)

	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeVersion("1.0.0"),
		withRuntimeAPIVersion(0),
	))

	out := captureStdout(t, func() {
		runServeStatus(config.Config{DataDir: dir})
	})

	assert.Contains(out, "incompatible")
	assert.Contains(out, "running")
	assert.Contains(out, fmt.Sprintf("http://%s:%d", host, port))
	assert.Contains(out, strconv.Itoa(os.Getpid()))
	assert.Contains(out, "daemon version")
	assert.Contains(out, "binary version")
	assert.Contains(out, "API version")
	assert.Contains(out, "data version")
	assert.Contains(out, "compatibility")
	assert.Contains(out, "agentsview daemon restart")
	assert.Contains(out, "agentsview daemon stop")
	assert.NotContains(out, "not responding")
}

func TestRunServeStatusPrefersIncompatibleWritableOverReadOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := runtimeTestDir(t)
	readOnlyHost, readOnlyPort := testPingServer(t)
	_, err := WriteDaemonRuntime(
		dir, readOnlyHost, readOnlyPort, "1.0.0", true,
	)
	require.NoError(err)

	writablePID := startSleepProcess(t)
	writableEndpoint := newPingDaemonWithPID(t, writablePID)
	_, err = writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		writableEndpoint.Host, writableEndpoint.Port,
		withRuntimePID(writablePID),
		withRuntimeVersion("1.0.0"),
		withRuntimeAPIVersion(0),
	))
	require.NoError(err)

	out := captureStdout(t, func() {
		runServeStatus(config.Config{DataDir: dir})
	})

	assert.Contains(out, "incompatible")
	assert.Contains(out, strconv.Itoa(writablePID))
	assert.Contains(out, "agentsview daemon restart")
	assert.NotContains(out, "mode:    read-only")
}

func TestRunServeStatusPrefersStartingOverReadOnly(t *testing.T) {
	dir := runtimeTestDir(t)
	readOnlyHost, readOnlyPort := testPingServer(t)
	_, err := WriteDaemonRuntime(
		dir, readOnlyHost, readOnlyPort, "1.0.0", true,
	)
	require.NoError(t, err)
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })

	out := captureStdout(t, func() {
		runServeStatus(config.Config{DataDir: dir})
	})

	assert.Contains(t, out, "agentsview is starting up.")
	assert.NotContains(t, out, "mode:    read-only")
}

func TestRunServeStatusReportsStartupProgress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := runtimeTestDir(t)
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })

	// Backdated started_at keeps the elapsed assertion stable.
	state, err := json.Marshal(startupState{
		PID:       os.Getpid(),
		StartedAt: time.Now().Add(-90 * time.Second),
		Phase:     "full resync",
		Detail:    "claude: 12/38 sessions (32%)",
		LogPath:   serveLogPath(dir),
		UpdatedAt: time.Now(),
	})
	require.NoError(err, "marshal startup state")
	require.NoError(os.WriteFile(startupStatePath(dir), state, 0o600),
		"write startup state")

	out := captureStdout(t, func() {
		runServeStatus(config.Config{DataDir: dir})
	})

	assert.Contains(out, "agentsview is starting up.")
	assert.Contains(out, fmt.Sprintf("pid:     %d", os.Getpid()))
	// runServeStatus computes elapsed from a live clock, so allow a
	// few seconds of scheduler slack; exact rendering is covered by
	// the fixed-clock serveStartingStatusLines unit test.
	assert.Regexp(`elapsed: 1m3[0-5]s`, out)
	assert.Contains(out,
		"phase:   full resync: claude: 12/38 sessions (32%)")
	assert.Contains(out, "log:     "+serveLogPath(dir))
}

func TestRunServeStatusStartingWithoutStateFallsBack(t *testing.T) {
	dir := runtimeTestDir(t)
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
	require.NoError(t, os.WriteFile(
		startupStatePath(dir), []byte("{corrupt"), 0o600,
	), "plant corrupt state file")

	out := captureStdout(t, func() {
		runServeStatus(config.Config{DataDir: dir})
	})

	assert.Contains(t, out, "agentsview is starting up.")
	assert.NotContains(t, out, "phase:")
}

func TestRunServeStatus_StartupStateFallbackWithoutRuntimeRecord(t *testing.T) {
	assert := assert.New(t)

	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10))

	out := captureStdout(t, func() {
		runServeStatus(config.Config{DataDir: dir})
	})

	assert.Contains(out, "agentsview running at")
	assert.Contains(out, fmt.Sprintf("http://%s:%d", host, port))
	assert.Contains(out, "runtime record unwritten")
}

func TestRunServeStatus_StartupStateFallbackStaleStateDoesNotClaimRunning(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), "1")

	out := captureStdout(t, func() {
		runServeStatus(config.Config{DataDir: dir})
	})

	assert.NotContains(t, out, "agentsview running at")
	assert.Contains(t, out, "agentsview is starting up.")
	assert.Contains(t, out, "stale fallback")
}

func TestRunServeStop_StartupStateFallbackWithoutRuntimeRecord(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10))

	rt := FindWritableDaemonRuntime(dir)
	require.NotNil(t, rt)
	assert.True(t, stopTargetConfirmed(rt.Record, ""))
}

func TestRunServeStop_StartupStateFallbackRequiresIdentity(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), "1")

	assert.Nil(t, FindWritableDaemonRuntime(dir), "stale fallback must not authorize stop")
}

func TestUnmarkDaemonStartingRemovesStartupState(t *testing.T) {
	dir := runtimeTestDir(t)
	MarkDaemonStarting(dir)
	newStartupStateWriter(dir, time.Now).SetPhase("opening database")
	require.NotNil(t, readStartupState(dir), "state written during startup")

	UnmarkDaemonStarting(dir)
	assert.Nil(t, readStartupState(dir),
		"state must not outlive the start lock")
}

func newPingDaemonWithPID(t *testing.T, pid int) testDaemonEndpoint {
	t.Helper()
	ts := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "test",
		PID:     pid,
	}))
	t.Cleanup(ts.Close)
	return serverEndpoint(t, ts)
}

func TestAcquireBackgroundLaunchLockSerializes(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()

	first, ok := acquireBackgroundLaunchLock(dir)
	require.True(ok, "first launch must acquire the lock")

	_, ok = acquireBackgroundLaunchLock(dir)
	assert.False(t, ok, "a concurrent launch must not acquire the lock")

	require.NoError(first.Unlock())

	third, ok := acquireBackgroundLaunchLock(dir)
	require.True(ok, "lock must be reacquirable after release")
	require.NoError(third.Unlock())
}

func TestServeCommandHasLifecycleSubcommands(t *testing.T) {
	cmd := newServeCommand()
	names := map[string]bool{}
	for _, sub := range cmd.Commands() {
		names[sub.Name()] = true
	}
	assert.True(t, names["status"], "serve must expose a status subcommand")
	assert.True(t, names["stop"], "serve must expose a stop subcommand")
	assert.True(t, names["restart"], "serve must expose a restart subcommand")
}

func TestServeRestartHelpExplainsWriterOnlyAsymmetry(t *testing.T) {
	assert := assert.New(t)

	out, err := executeCommand(newRootCommand(), "serve", "restart", "--help")
	require.NoError(t, err)
	assert.Contains(out, "writable SQLite background daemon")
	assert.Contains(out, "config.toml")
	assert.Contains(out,
		"Unlike `agentsview serve stop`, this command intentionally leaves "+
			"read-only PostgreSQL and DuckDB servers running.")
}

func TestStopWritableDaemonsForUpdateStopsAllAndRestartsOne(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := runtimeTestDir(t)
	secondPID := startSleepProcess(t)

	now := time.Now()
	_, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   "127.0.0.1:18080",
		Service:   daemonService,
		Version:   "1.0.0",
		StartedAt: now.Add(-time.Minute),
		Metadata: map[string]string{
			runtimeHost:        "127.0.0.1",
			runtimePort:        "18080",
			runtimeReadOnly:    "false",
			runtimeRequireAuth: "true",
			runtimeNoSync:      "true",
		},
	})
	require.NoError(err)
	_, err = writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:       secondPID,
		Network:   daemon.NetworkTCP,
		Address:   "127.0.0.1:18081",
		Service:   daemonService,
		Version:   "1.0.0",
		StartedAt: now,
		Metadata: map[string]string{
			runtimeHost:        "127.0.0.1",
			runtimePort:        "18081",
			runtimeReadOnly:    "false",
			runtimeRequireAuth: "false",
			runtimeNoSync:      "false",
		},
	})
	require.NoError(err)

	oldStop := stopDaemonRuntimeForUpgrade
	var stopped []int
	stopDaemonRuntimeForUpgrade = func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = append(stopped, rt.Record.PID)
		return nil
	}
	t.Cleanup(func() { stopDaemonRuntimeForUpgrade = oldStop })

	result, err := stopWritableDaemonsForUpdate(config.Config{DataDir: dir})
	require.NoError(err)
	assert.True(result.Stopped)
	assert.Equal("127.0.0.1", result.Host)
	assert.Equal(18080, result.Port)
	assert.True(result.RequireAuth)
	assert.True(result.RequireAuthKnown)
	assert.True(result.NoSync)
	assert.ElementsMatch([]int{os.Getpid(), secondPID}, stopped)
}

func TestStopWritableDaemonsForUpdateUsesStartupStateFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(ok)
	writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10))

	oldStop := stopDaemonRuntimeForUpgrade
	var stopped *DaemonRuntime
	stopDaemonRuntimeForUpgrade = func(_ config.Config, rt *DaemonRuntime) error {
		stopped = rt
		return nil
	}
	t.Cleanup(func() { stopDaemonRuntimeForUpgrade = oldStop })

	result, err := stopWritableDaemonsForUpdate(config.Config{DataDir: dir})
	require.NoError(err)
	assert.True(result.Stopped)
	assert.Equal(host, result.Host)
	assert.Equal(port, result.Port)
	require.NotNil(stopped)
	assert.Equal(os.Getpid(), stopped.Record.PID)
}

func TestStopDaemonProcessTerminatesAndCleansRecord(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	requirePOSIXSignals(t, "graceful SIGTERM termination is POSIX-specific")
	dir := runtimeTestDir(t)

	pid, reaped := startReapedSleepProcess(t)

	_, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:     pid,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
	})
	require.NoError(err)

	require.NoError(stopDaemonProcess(onlyLiveRuntimeRecord(t, dir), 5*time.Second))
	<-reaped
	assert.False(daemon.ProcessAlive(pid))
	assert.Empty(liveDaemonRecords(dir),
		"runtime record must be removed after stop")
}

func TestStopDaemonRuntimeForUpgradeChecksExplicitPortBeforeStop(t *testing.T) {
	requirePOSIXSignals(t, "uses a child process to observe replacement signals")
	for _, tt := range []struct {
		name        string
		explicit    bool
		ownEndpoint bool
		wildcard    bool
		host        string
		otherHost   string
		ephemeral   bool
		wantError   bool
	}{
		{name: "occupied explicit port preserves incumbent", explicit: true, wantError: true},
		{name: "incumbent endpoint can be replaced", explicit: true, ownEndpoint: true},
		{name: "incumbent wildcard can narrow to loopback", explicit: true, ownEndpoint: true, wildcard: true},
		{name: "same endpoint through localhost", explicit: true, ownEndpoint: true, host: "localhost"},
		{name: "widening preserves incumbent on shared port", explicit: true, ownEndpoint: true, host: "0.0.0.0", otherHost: "127.0.0.2", wantError: true},
		{name: "implicit port retains fallback"},
		{name: "explicit zero retains automatic selection", explicit: true, ephemeral: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			dir := runtimeTestDir(t)
			pid, _ := startReapedSleepProcess(t)
			endpoint := newPingDaemonWithPID(t, pid)
			writeRuntimeRecordFixture(t, dir, runtimeRecordForEndpoint(
				endpoint, withRuntimePID(pid),
			))
			rt := FindDaemonRuntime(dir)
			require.NotNil(rt)
			if tt.wildcard {
				rt.Host = "0.0.0.0"
			}
			listener, port := heldLoopbackPort(t)
			t.Cleanup(func() { listener.Close() })
			if tt.ownEndpoint {
				port = endpoint.Port
			} else if tt.ephemeral {
				port = 0
			}
			if tt.otherHost != "" {
				other, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(tt.otherHost, strconv.Itoa(port)))
				if err != nil {
					t.Skipf("second loopback address unavailable: %v", err)
				}
				t.Cleanup(func() { other.Close() })
			}
			cfg := config.Config{DataDir: dir, Host: endpoint.Host, Port: port, PortExplicit: tt.explicit}
			if tt.host != "" {
				cfg.Host = tt.host
			}
			err := stopDaemonRuntimeForUpgradeImpl(cfg, rt)
			if tt.wantError {
				assert.ErrorContains(err, "requested port")
				assert.True(daemon.ProcessAlive(pid), "failed preflight must leave the incumbent running")
				assert.FileExists(rt.Record.SourcePath)
				return
			}
			require.NoError(err)
			assert.False(daemon.ProcessAlive(pid))
			assert.NoFileExists(rt.Record.SourcePath)
		})
	}
}

func TestStopDaemonProcessKeepsRecordWhenProcessSurvives(t *testing.T) {
	requirePOSIXSignals(t, "relies on POSIX zombie semantics for ProcessAlive")
	dir := runtimeTestDir(t)

	// startSleepProcess does not reap until cleanup: once signalled, the child
	// becomes a zombie, which daemon.ProcessAlive reports as still alive. That
	// drives stopDaemonProcess down its "survived the kill" path.
	pid := startSleepProcess(t)

	_, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:     pid,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
	})
	require.NoError(t, err)

	err = stopDaemonProcess(onlyLiveRuntimeRecord(t, dir), 100*time.Millisecond)
	require.Error(t, err, "must report failure when the process does not exit")
	assert.NotEmpty(t, liveDaemonRecords(dir),
		"runtime record must be kept when the daemon is still alive")
}

func TestStopDaemonProcessDoesNotForceKillUnknownIdentity(t *testing.T) {
	requirePOSIXSignals(t, "SIGTERM-ignore and SIGKILL behavior is POSIX-specific")
	setStartProbeTickForTest(t, 10*time.Millisecond)

	tests := []struct {
		name       string
		createTime string
	}{
		{name: "missing"},
		{name: "malformed", createTime: "not-a-time"},
		{name: "zero", createTime: "0"},
		{name: "negative", createTime: "-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)

			dir := runtimeTestDir(t)
			pid, reaped := startReapedTERMIgnoringProcess(t)
			path, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
				PID:      pid,
				Network:  daemon.NetworkTCP,
				Address:  "127.0.0.1:1",
				Metadata: map[string]string{runtimeCreateTime: tt.createTime},
			})
			require.NoError(t, err)

			err = stopDaemonProcess(onlyLiveRuntimeRecord(t, dir), 50*time.Millisecond)
			assert.Error(err)
			assert.ErrorContains(err, "identity")
			assert.True(daemon.ProcessAlive(pid),
				"unknown identity must not authorize SIGKILL")
			select {
			case <-reaped:
				assert.Fail("unknown identity process was killed")
			default:
			}
			assert.FileExists(path,
				"unknown identity record must remain for manual recovery")
		})
	}
}

func TestStopDaemonProcessForceKillsMatchedIdentity(t *testing.T) {
	require := require.New(t)

	requirePOSIXSignals(t, "SIGTERM-ignore and SIGKILL behavior is POSIX-specific")
	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	pid, reaped := startReapedTERMIgnoringProcess(t)
	createTime, ok := processCreateTimeMillis(pid)
	require.True(ok)
	path, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:     pid,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(createTime, 10),
		},
	})
	require.NoError(err)

	require.NoError(stopDaemonProcess(
		onlyLiveRuntimeRecord(t, dir), 50*time.Millisecond,
	))
	<-reaped
	assert.False(t, daemon.ProcessAlive(pid))
	assertPathRemoved(t, path,
		"matched identity record should be removed after force kill")
}

func TestStopDaemonProcessKeepsRecordWhenIdentityBecomesUnknownAfterForceKill(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	requirePOSIXSignals(t, "relies on POSIX zombie semantics for ProcessAlive")
	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	pid := startTERMIgnoringProcess(t)
	path, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:      pid,
		Network:  daemon.NetworkTCP,
		Address:  "127.0.0.1:1",
		Metadata: map[string]string{runtimeCreateTime: "1234"},
	})
	require.NoError(err)
	rec := onlyLiveRuntimeRecord(t, dir)

	identityCalls := 0
	identityState := func(gotPID int, recorded string) processCreateTimeState {
		assert.Equal(pid, gotPID)
		assert.Equal("1234", recorded)
		identityCalls++
		if identityCalls == 1 {
			return processCreateTimeMatch
		}
		return processCreateTimeUnknown
	}

	err = stopDaemonProcessWithIdentity(
		rec, 50*time.Millisecond, identityState,
	)
	require.Error(err)
	assert.ErrorContains(err, "identity")
	assert.ErrorContains(err, "after force kill")
	assert.Equal(2, identityCalls)
	assert.FileExists(path,
		"unknown post-kill identity must preserve the runtime record")
}

func TestStopDaemonProcessKeepsRecordWhenMatchedProcessSurvivesForceKill(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	requirePOSIXSignals(t, "relies on POSIX zombie semantics for ProcessAlive")
	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	pid := startTERMIgnoringProcess(t)
	path, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:      pid,
		Network:  daemon.NetworkTCP,
		Address:  "127.0.0.1:1",
		Metadata: map[string]string{runtimeCreateTime: "1234"},
	})
	require.NoError(err)
	rec := onlyLiveRuntimeRecord(t, dir)

	identityCalls := 0
	identityState := func(gotPID int, recorded string) processCreateTimeState {
		assert.Equal(pid, gotPID)
		assert.Equal("1234", recorded)
		identityCalls++
		return processCreateTimeMatch
	}

	err = stopDaemonProcessWithIdentity(
		rec, 50*time.Millisecond, identityState,
	)
	require.Error(err)
	assert.ErrorContains(err, "still running after force kill")
	assert.Equal(2, identityCalls,
		"identity must be confirmed before and after force kill")
	assert.FileExists(path,
		"surviving matched process must retain its ownership record")
}

func TestStopDaemonProcessRemovesRecordWhenPIDReused(t *testing.T) {
	requirePOSIXSignals(t, "relies on POSIX zombie semantics for ProcessAlive")
	dir := runtimeTestDir(t)

	pid := startSleepProcess(t)

	// A create time that cannot match the live process models the daemon
	// having exited with the PID reused by something unrelated. The record
	// must be removed (not kept) so later commands do not think the DB is
	// still owned.
	_, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:      pid,
		Network:  daemon.NetworkTCP,
		Address:  "127.0.0.1:1",
		Metadata: map[string]string{runtimeCreateTime: "1"},
	})
	require.NoError(t, err)

	err = stopDaemonProcess(onlyLiveRuntimeRecord(t, dir), 100*time.Millisecond)
	require.NoError(t, err,
		"a reused PID means the daemon exited; stop should succeed")
	assert.Empty(t, liveDaemonRecords(dir),
		"the stale record for a reused PID must be removed")
}

func TestStopDaemonProcessSparesReusedPIDBeforeForceKill(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	requirePOSIXSignals(t, "relies on POSIX signal semantics")
	dir := runtimeTestDir(t)

	// A process that ignores SIGTERM stays alive through the grace wait,
	// reaching the force-kill escalation. With a mismatched create time it
	// stands in for a live process that reused the daemon's PID. The pre-kill
	// identity check must spare it instead of escalating to SIGKILL.
	pid := startTERMIgnoringProcess(t)

	_, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:      pid,
		Network:  daemon.NetworkTCP,
		Address:  "127.0.0.1:1",
		Metadata: map[string]string{runtimeCreateTime: "1"},
	})
	require.NoError(err)

	err = stopDaemonProcess(onlyLiveRuntimeRecord(t, dir), 100*time.Millisecond)
	require.NoError(err,
		"a reused PID means the daemon exited; stop should succeed")
	assert.Empty(liveDaemonRecords(dir),
		"the stale record for a reused PID must be removed")
	assert.True(daemon.ProcessAlive(pid),
		"the reused PID must not be force-killed")
}

func TestWriteDaemonRuntimePersistsCaddyMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := runtimeTestDir(t)
	// Use this process as a stand-in caddy child: it is alive with a readable
	// create time. We never signal it here.
	_, err := WriteDaemonRuntime(dir, "127.0.0.1", 65535, "test", false, os.Getpid())
	require.NoError(err)

	rec := onlyLiveRuntimeRecord(t, dir)
	assert.Equal(strconv.Itoa(os.Getpid()), rec.Metadata[runtimeCaddyPID])
	ct, ok := processCreateTimeMillis(os.Getpid())
	require.True(ok)
	assert.Equal(strconv.FormatInt(ct, 10), rec.Metadata[runtimeCaddyCreateTime])
}

func TestWriteDaemonRuntimeOmitsCaddyMetadataWhenAbsent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := runtimeTestDir(t)
	_, err := WriteDaemonRuntime(dir, "127.0.0.1", 65535, "test", false)
	require.NoError(err)
	_, has := onlyLiveRuntimeRecord(t, dir).Metadata[runtimeCaddyPID]
	assert.False(has, "no caddy pid means no caddy metadata")

	// A zero caddy pid must also be omitted.
	dir2 := runtimeTestDir(t)
	_, err = WriteDaemonRuntime(dir2, "127.0.0.1", 65535, "test", false, 0)
	require.NoError(err)
	_, has = onlyLiveRuntimeRecord(t, dir2).Metadata[runtimeCaddyPID]
	assert.False(has)
}

func TestStopOrphanedCaddyChildTerminatesConfirmed(t *testing.T) {
	requirePOSIXSignals(t, "graceful SIGTERM termination is POSIX-specific")
	pid, reaped := startReapedSleepProcess(t)

	ct, ok := processCreateTimeMillis(pid)
	require.True(t, ok)
	rec := daemon.RuntimeRecord{
		PID: os.Getpid(),
		Metadata: map[string]string{
			runtimeCaddyPID:        strconv.Itoa(pid),
			runtimeCaddyCreateTime: strconv.FormatInt(ct, 10),
		},
	}

	stopOrphanedCaddyChild(rec)
	<-reaped
	assert.False(t, daemon.ProcessAlive(pid),
		"a confirmed orphaned caddy child must be terminated")
}

func TestStopOrphanedCaddyChildSkipsMismatchedCreateTime(t *testing.T) {
	requirePOSIXSignals(t, "graceful SIGTERM termination is POSIX-specific")
	pid := startSleepProcess(t)

	rec := daemon.RuntimeRecord{
		PID: os.Getpid(),
		Metadata: map[string]string{
			runtimeCaddyPID:        strconv.Itoa(pid),
			runtimeCaddyCreateTime: "1", // deliberately wrong: models a reused PID
		},
	}

	stopOrphanedCaddyChild(rec)
	assert.True(t, daemon.ProcessAlive(pid),
		"a reused caddy PID must not be signalled")
}

func TestCaddyStopRecordCarriesCreateTime(t *testing.T) {
	rec := caddyStopRecord(4321, "1700000000000")
	assert.Equal(t, 4321, rec.PID)
	assert.Equal(t, "1700000000000", rec.Metadata[runtimeCreateTime],
		"the caddy create time must be carried as runtimeCreateTime so the "+
			"pre-force-kill identity check guards a reused caddy PID")
	assert.Empty(t, rec.SourcePath,
		"a caddy stop record has no source file to remove")
}

func TestStopOrphanedCaddyChildNoMetadataIsNoop(t *testing.T) {
	assert.NotPanics(t, func() {
		stopOrphanedCaddyChild(daemon.RuntimeRecord{PID: os.Getpid()})
	})
}

func TestDaemonRecordPingConfirmedRespondingDaemon(t *testing.T) {
	host, port := testPingServer(t)
	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, strconv.Itoa(port)),
		Service: daemonService,
	}
	assert.True(t, daemonRecordPingConfirmed(rec, ""))
}

func TestDaemonRecordPingConfirmedUnresponsivePID(t *testing.T) {
	// A live PID (this process) but a record pointing at a port with no
	// agentsview daemon. The probe must fail.
	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
	}
	assert.False(t, daemonRecordPingConfirmed(rec, ""))
}

func TestDaemonRecordPingConfirmedRequiresAuthToken(t *testing.T) {
	host, port := testAuthenticatedPingServer(t, "secret")
	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, strconv.Itoa(port)),
		Service: daemonService,
	}
	assert.False(t, daemonRecordPingConfirmed(rec, ""),
		"a require_auth daemon must not be confirmed without the token")
	assert.True(t, daemonRecordPingConfirmed(rec, "secret"))
}

func TestWriteDaemonRuntimePersistsCreateTimeForStop(t *testing.T) {
	assert := assert.New(t)

	dir := runtimeTestDir(t)
	_, err := WriteDaemonRuntime(dir, "127.0.0.1", 65535, "test", false)
	require.NoError(t, err)

	rec := onlyLiveRuntimeRecord(t, dir)
	assert.NotEmpty(rec.Metadata[runtimeCreateTime],
		"WriteDaemonRuntime must persist the process create time")

	// No server answers at that port, so ping confirmation fails; the
	// persisted create time must still confirm the record belongs to this
	// live process so a wedged daemon is stoppable.
	assert.False(daemonRecordPingConfirmed(rec, ""))
	assert.True(stopTargetConfirmed(rec, ""))
}

func TestProcessIdentityConfirmed(t *testing.T) {
	assert := assert.New(t)

	live, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok, "must be able to read this process's create time")

	// Exact create-time match: the recorded daemon is still on this PID.
	assert.True(processIdentityConfirmed(daemon.RuntimeRecord{
		PID: os.Getpid(),
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(live, 10),
		},
	}))

	// Different create time models a PID reused by another process: there is
	// no slack window, so a mismatch is always rejected.
	assert.False(processIdentityConfirmed(daemon.RuntimeRecord{
		PID: os.Getpid(),
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(live+1, 10),
		},
	}))

	// Missing or unparseable metadata cannot be confirmed this way.
	assert.False(processIdentityConfirmed(daemon.RuntimeRecord{
		PID: os.Getpid(),
	}))
	assert.False(processIdentityConfirmed(daemon.RuntimeRecord{
		PID:      os.Getpid(),
		Metadata: map[string]string{runtimeCreateTime: "not-a-number"},
	}))
}

func TestStopTargetConfirmedHungDaemonByCreateTime(t *testing.T) {
	// A daemon that is alive but no longer answers the ping probe (dead
	// address) must still be confirmed by its persisted create time, so a
	// wedged server remains stoppable.
	live, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(live, 10),
		},
	}
	assert.False(t, daemonRecordPingConfirmed(rec, ""),
		"precondition: the dead address must not ping-confirm")
	assert.True(t, stopTargetConfirmed(rec, ""),
		"a hung-but-alive daemon must remain stoppable via create-time identity")
}

func TestStopTargetConfirmedRejectsReusedPID(t *testing.T) {
	// No ping and a create time that does not match this process: neither
	// check confirms, so stop must not signal the process holding the PID.
	live, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(live+5000, 10),
		},
	}
	assert.False(t, stopTargetConfirmed(rec, ""))

	// A legacy record with no create time also cannot be confirmed.
	assert.False(t, stopTargetConfirmed(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
	}, ""))
}
