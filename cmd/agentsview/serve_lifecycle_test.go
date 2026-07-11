package main

import (
	"encoding/json"
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
	dir := runtimeTestDir(t)

	// WriteDaemonRuntime stamps the record with this live test process PID.
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", false)
	require.NoError(t, err)

	// A record for a dead process must be excluded.
	dead := deadPID(t)
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

func TestRunServeStatusReportsIncompatibleWritableDaemon(t *testing.T) {
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

	assert.Contains(t, out, "incompatible")
	assert.Contains(t, out, "running")
	assert.Contains(t, out, fmt.Sprintf("http://%s:%d", host, port))
	assert.Contains(t, out, strconv.Itoa(os.Getpid()))
	assert.Contains(t, out, "daemon version")
	assert.Contains(t, out, "binary version")
	assert.Contains(t, out, "API version")
	assert.Contains(t, out, "data version")
	assert.Contains(t, out, "compatibility")
	assert.Contains(t, out, "agentsview daemon restart")
	assert.Contains(t, out, "agentsview daemon stop")
	assert.NotContains(t, out, "not responding")
}

func TestRunServeStatusPrefersIncompatibleWritableOverReadOnly(t *testing.T) {
	dir := runtimeTestDir(t)
	readOnlyHost, readOnlyPort := testPingServer(t)
	_, err := WriteDaemonRuntime(
		dir, readOnlyHost, readOnlyPort, "1.0.0", true,
	)
	require.NoError(t, err)

	writablePID := startSleepProcess(t)
	writableEndpoint := newPingDaemonWithPID(t, writablePID)
	_, err = writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		writableEndpoint.Host, writableEndpoint.Port,
		withRuntimePID(writablePID),
		withRuntimeVersion("1.0.0"),
		withRuntimeAPIVersion(0),
	))
	require.NoError(t, err)

	out := captureStdout(t, func() {
		runServeStatus(config.Config{DataDir: dir})
	})

	assert.Contains(t, out, "incompatible")
	assert.Contains(t, out, strconv.Itoa(writablePID))
	assert.Contains(t, out, "agentsview daemon restart")
	assert.NotContains(t, out, "mode:    read-only")
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
	require.NoError(t, err, "marshal startup state")
	require.NoError(t, os.WriteFile(startupStatePath(dir), state, 0o600),
		"write startup state")

	out := captureStdout(t, func() {
		runServeStatus(config.Config{DataDir: dir})
	})

	assert.Contains(t, out, "agentsview is starting up.")
	assert.Contains(t, out, fmt.Sprintf("pid:     %d", os.Getpid()))
	// runServeStatus computes elapsed from a live clock, so allow a
	// few seconds of scheduler slack; exact rendering is covered by
	// the fixed-clock serveStartingStatusLines unit test.
	assert.Regexp(t, `elapsed: 1m3[0-5]s`, out)
	assert.Contains(t, out,
		"phase:   full resync: claude: 12/38 sessions (32%)")
	assert.Contains(t, out, "log:     "+serveLogPath(dir))
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
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10))

	out := captureStdout(t, func() {
		runServeStatus(config.Config{DataDir: dir})
	})

	assert.Contains(t, out, "agentsview running at")
	assert.Contains(t, out, fmt.Sprintf("http://%s:%d", host, port))
	assert.Contains(t, out, "runtime record unwritten")
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
	assert.True(t, names["restart"], "serve must expose a restart subcommand")
}

func TestServeRestartHelpExplainsWriterOnlyAsymmetry(t *testing.T) {
	out, err := executeCommand(newRootCommand(), "serve", "restart", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "writable SQLite background daemon")
	assert.Contains(t, out, "config.toml")
	assert.Contains(t, out,
		"Unlike `agentsview serve stop`, this command intentionally leaves "+
			"read-only PostgreSQL and DuckDB servers running.")
}

func TestStopWritableDaemonsForUpdateStopsAllAndRestartsOne(t *testing.T) {
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
	require.NoError(t, err)
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
	require.NoError(t, err)

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
	require.NoError(t, err)
	assert.True(t, result.Stopped)
	assert.Equal(t, "127.0.0.1", result.Host)
	assert.Equal(t, 18080, result.Port)
	assert.True(t, result.RequireAuth)
	assert.True(t, result.RequireAuthKnown)
	assert.True(t, result.NoSync)
	assert.ElementsMatch(t, []int{os.Getpid(), secondPID}, stopped)
}

func TestStopDaemonProcessTerminatesAndCleansRecord(t *testing.T) {
	requirePOSIXSignals(t, "graceful SIGTERM termination is POSIX-specific")
	dir := runtimeTestDir(t)

	pid, reaped := startReapedSleepProcess(t)

	_, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:     pid,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
	})
	require.NoError(t, err)

	require.NoError(t, stopDaemonProcess(onlyLiveRuntimeRecord(t, dir), 5*time.Second))
	<-reaped
	assert.False(t, daemon.ProcessAlive(pid))
	assert.Empty(t, liveDaemonRecords(dir),
		"runtime record must be removed after stop")
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
			assert.Error(t, err)
			assert.ErrorContains(t, err, "identity")
			assert.True(t, daemon.ProcessAlive(pid),
				"unknown identity must not authorize SIGKILL")
			select {
			case <-reaped:
				assert.Fail(t, "unknown identity process was killed")
			default:
			}
			assert.FileExists(t, path,
				"unknown identity record must remain for manual recovery")
		})
	}
}

func TestStopDaemonProcessForceKillsMatchedIdentity(t *testing.T) {
	requirePOSIXSignals(t, "SIGTERM-ignore and SIGKILL behavior is POSIX-specific")
	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	pid, reaped := startReapedTERMIgnoringProcess(t)
	createTime, ok := processCreateTimeMillis(pid)
	require.True(t, ok)
	path, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:     pid,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(createTime, 10),
		},
	})
	require.NoError(t, err)

	require.NoError(t, stopDaemonProcess(
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
	require.NoError(t, err)
	rec := onlyLiveRuntimeRecord(t, dir)

	identityCalls := 0
	identityState := func(gotPID int, recorded string) processCreateTimeState {
		assert.Equal(t, pid, gotPID)
		assert.Equal(t, "1234", recorded)
		identityCalls++
		if identityCalls == 1 {
			return processCreateTimeMatch
		}
		return processCreateTimeUnknown
	}

	err = stopDaemonProcessWithIdentity(
		rec, 50*time.Millisecond, identityState,
	)
	require.Error(t, err)
	assert.ErrorContains(t, err, "identity")
	assert.ErrorContains(t, err, "after force kill")
	assert.Equal(t, 2, identityCalls)
	assert.FileExists(t, path,
		"unknown post-kill identity must preserve the runtime record")
}

func TestStopDaemonProcessKeepsRecordWhenMatchedProcessSurvivesForceKill(
	t *testing.T,
) {
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
	require.NoError(t, err)
	rec := onlyLiveRuntimeRecord(t, dir)

	identityCalls := 0
	identityState := func(gotPID int, recorded string) processCreateTimeState {
		assert.Equal(t, pid, gotPID)
		assert.Equal(t, "1234", recorded)
		identityCalls++
		return processCreateTimeMatch
	}

	err = stopDaemonProcessWithIdentity(
		rec, 50*time.Millisecond, identityState,
	)
	require.Error(t, err)
	assert.ErrorContains(t, err, "still running after force kill")
	assert.Equal(t, 2, identityCalls,
		"identity must be confirmed before and after force kill")
	assert.FileExists(t, path,
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
	require.NoError(t, err)

	err = stopDaemonProcess(onlyLiveRuntimeRecord(t, dir), 100*time.Millisecond)
	require.NoError(t, err,
		"a reused PID means the daemon exited; stop should succeed")
	assert.Empty(t, liveDaemonRecords(dir),
		"the stale record for a reused PID must be removed")
	assert.True(t, daemon.ProcessAlive(pid),
		"the reused PID must not be force-killed")
}

func TestWriteDaemonRuntimePersistsCaddyMetadata(t *testing.T) {
	dir := runtimeTestDir(t)
	// Use this process as a stand-in caddy child: it is alive with a readable
	// create time. We never signal it here.
	_, err := WriteDaemonRuntime(dir, "127.0.0.1", 65535, "test", false, os.Getpid())
	require.NoError(t, err)

	rec := onlyLiveRuntimeRecord(t, dir)
	assert.Equal(t, strconv.Itoa(os.Getpid()), rec.Metadata[runtimeCaddyPID])
	ct, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	assert.Equal(t,
		strconv.FormatInt(ct, 10), rec.Metadata[runtimeCaddyCreateTime])
}

func TestWriteDaemonRuntimeOmitsCaddyMetadataWhenAbsent(t *testing.T) {
	dir := runtimeTestDir(t)
	_, err := WriteDaemonRuntime(dir, "127.0.0.1", 65535, "test", false)
	require.NoError(t, err)
	_, has := onlyLiveRuntimeRecord(t, dir).Metadata[runtimeCaddyPID]
	assert.False(t, has, "no caddy pid means no caddy metadata")

	// A zero caddy pid must also be omitted.
	dir2 := runtimeTestDir(t)
	_, err = WriteDaemonRuntime(dir2, "127.0.0.1", 65535, "test", false, 0)
	require.NoError(t, err)
	_, has = onlyLiveRuntimeRecord(t, dir2).Metadata[runtimeCaddyPID]
	assert.False(t, has)
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
	dir := runtimeTestDir(t)
	_, err := WriteDaemonRuntime(dir, "127.0.0.1", 65535, "test", false)
	require.NoError(t, err)

	rec := onlyLiveRuntimeRecord(t, dir)
	assert.NotEmpty(t, rec.Metadata[runtimeCreateTime],
		"WriteDaemonRuntime must persist the process create time")

	// No server answers at that port, so ping confirmation fails; the
	// persisted create time must still confirm the record belongs to this
	// live process so a wedged daemon is stoppable.
	assert.False(t, daemonRecordPingConfirmed(rec, ""))
	assert.True(t, stopTargetConfirmed(rec, ""))
}

func TestProcessIdentityConfirmed(t *testing.T) {
	live, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok, "must be able to read this process's create time")

	// Exact create-time match: the recorded daemon is still on this PID.
	assert.True(t, processIdentityConfirmed(daemon.RuntimeRecord{
		PID: os.Getpid(),
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(live, 10),
		},
	}))

	// Different create time models a PID reused by another process: there is
	// no slack window, so a mismatch is always rejected.
	assert.False(t, processIdentityConfirmed(daemon.RuntimeRecord{
		PID: os.Getpid(),
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(live+1, 10),
		},
	}))

	// Missing or unparseable metadata cannot be confirmed this way.
	assert.False(t, processIdentityConfirmed(daemon.RuntimeRecord{
		PID: os.Getpid(),
	}))
	assert.False(t, processIdentityConfirmed(daemon.RuntimeRecord{
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
