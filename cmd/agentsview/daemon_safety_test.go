package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/kit/daemon"
)

func TestDaemonWaitForLaunchContentionDoesNotAcceptRecordWhileLockHeld(t *testing.T) {
	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	lock, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = lock.Unlock()
		}
	})

	path, err := writeRuntimeRecordForTest(
		dir, testWritableRecord(os.Getpid(), ""),
	)
	require.NoError(t, err)

	attempted := make(chan struct{})
	var attemptOnce sync.Once
	waitDeps := defaultDaemonLaunchWaitDeps()
	waitDeps.onAttempt = func() { attemptOnce.Do(func() { close(attempted) }) }
	resultCh := make(chan daemonLaunchObservation, 1)
	go func() {
		resultCh <- waitForDaemonLaunchContentionWithDeps(dir, waitDeps)
	}()
	select {
	case <-attempted:
	case <-time.After(time.Second):
		require.Fail(t, "waiter never attempted the held launch lock")
	}
	select {
	case observation := <-resultCh:
		assert.Fail(t, "contender returned while mutating owner held lock",
			"observation: %+v", observation)
		return
	case <-time.After(75 * time.Millisecond):
	}

	require.NoError(t, os.Remove(path), "simulated stop removes old writer")
	require.NoError(t, lock.Unlock())
	locked = false

	select {
	case observation := <-resultCh:
		assert.False(t, observation.LockHeld)
		assert.Empty(t, observation.Records,
			"removed writer must not be reported after owner releases lock")
		assert.False(t, observation.Starting)
	case <-time.After(time.Second):
		require.Fail(t, "waiter did not inspect final state after lock release")
	}
}

func TestDaemonWaitForLaunchContentionRejectsUnconfirmedLiveWriter(t *testing.T) {
	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	lock, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = lock.Unlock()
		}
	})

	pid := startSleepProcess(t)
	rec := testWritableRecord(pid, "")
	rec.Address = "127.0.0.1:1"
	rec.Metadata[runtimePort] = "1"
	path, err := writeRuntimeRecordForTest(dir, rec)
	require.NoError(t, err)

	attempted := make(chan struct{})
	var attemptOnce sync.Once
	waitDeps := defaultDaemonLaunchWaitDeps()
	waitDeps.onAttempt = func() { attemptOnce.Do(func() { close(attempted) }) }
	resultCh := make(chan daemonLaunchObservation, 1)
	go func() {
		resultCh <- waitForDaemonLaunchContentionWithDeps(dir, waitDeps)
	}()
	select {
	case <-attempted:
	case <-time.After(time.Second):
		require.Fail(t, "waiter never attempted the held launch lock")
	}
	require.NoError(t, lock.Unlock())
	locked = false

	var observation daemonLaunchObservation
	select {
	case observation = <-resultCh:
	case <-time.After(2 * time.Second):
		require.Fail(t, "waiter did not inspect writer after lock release")
	}
	assert.Empty(t, observation.Records)
	require.Len(t, observation.UnconfirmedRecords, 1)
	assert.Equal(t, pid, observation.UnconfirmedRecords[0].PID)

	var out bytes.Buffer
	err = reportDaemonLaunchContention(&out, dir, observation, time.Now())
	require.Error(t, err)
	assert.ErrorContains(t, err, fmt.Sprintf("pid %d", pid))
	assert.ErrorContains(t, err, path)
	assert.ErrorContains(t, err, "verify")
	assert.ErrorContains(t, err, "terminate")
	assert.NotContains(t, out.String(), "already running")
	assert.True(t, daemon.ProcessAlive(pid))
}

func TestDaemonWaitForLaunchContentionRejectsIncompatibleResponsiveWriter(t *testing.T) {
	dir := runtimeTestDir(t)
	endpoint, probed := newPingDaemonWithProbeSignal(t)
	rec := testWritableRecord(os.Getpid(), "/runtime/incompatible.json")
	rec.Address = endpoint.Addr
	rec.Metadata[runtimeHost] = endpoint.Host
	rec.Metadata[runtimePort] = fmt.Sprint(endpoint.Port)
	rec.Metadata[runtimeAPIVersion] = fmt.Sprint(daemonAPIVersion + 1)

	waitDeps := defaultDaemonLaunchWaitDeps()
	waitDeps.loadReadOnlyConfig = func() (config.Config, error) {
		return config.Config{DataDir: dir}, nil
	}
	waitDeps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
		return []daemon.RuntimeRecord{rec}, nil
	}

	observation := waitForDaemonLaunchContentionWithDeps(dir, waitDeps)
	select {
	case <-probed:
	case <-time.After(time.Second):
		require.Fail(t, "responsive writer was not probed")
	}
	var out bytes.Buffer
	err := reportDaemonLaunchContention(&out, dir, observation, time.Now())
	require.Error(t, err)
	assert.ErrorContains(t, err, "incompatible")
	assert.ErrorContains(t, err, fmt.Sprintf("pid %d", rec.PID))
	assert.ErrorContains(t, err, "daemon restart")
	assert.NotContains(t, out.String(), "already running")
}

func TestDaemonWaitForLaunchContentionRejectsCompatibleUnresponsiveWriter(t *testing.T) {
	dir := runtimeTestDir(t)
	rec := testWritableRecord(os.Getpid(), "/runtime/unresponsive.json")
	rec.Address = "127.0.0.1:1"
	rec.Metadata[runtimePort] = "1"
	createTime, ok := processCreateTimeMillis(rec.PID)
	require.True(t, ok)
	rec.Metadata[runtimeCreateTime] = fmt.Sprint(createTime)

	waitDeps := defaultDaemonLaunchWaitDeps()
	waitDeps.loadReadOnlyConfig = func() (config.Config, error) {
		return config.Config{DataDir: dir}, nil
	}
	waitDeps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
		return []daemon.RuntimeRecord{rec}, nil
	}

	observation := waitForDaemonLaunchContentionWithDeps(dir, waitDeps)
	var out bytes.Buffer
	err := reportDaemonLaunchContention(&out, dir, observation, time.Now())
	require.Error(t, err)
	assert.ErrorContains(t, err, "not responding")
	assert.ErrorContains(t, err, fmt.Sprintf("pid %d", rec.PID))
	assert.ErrorContains(t, err, "daemon restart")
	assert.NotContains(t, out.String(), "already running")
}

func TestDaemonWaitForLaunchContentionSurfacesReadOnlyConfigError(t *testing.T) {
	dir := runtimeTestDir(t)
	waitDeps := defaultDaemonLaunchWaitDeps()
	waitDeps.loadReadOnlyConfig = func() (config.Config, error) {
		return config.Config{}, errors.New("read-only config failed")
	}

	observation := waitForDaemonLaunchContentionWithDeps(dir, waitDeps)
	require.Error(t, observation.Err)
	assert.ErrorContains(t, observation.Err, "read-only config failed")
	var out bytes.Buffer
	err := reportDaemonLaunchContention(&out, dir, observation, time.Now())
	require.Error(t, err)
	assert.ErrorContains(t, err, "read-only config failed")
}

func TestDaemonStopRevalidatesIdentityBeforeEverySignal(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	records := []daemon.RuntimeRecord{
		testWritableRecord(201, "/runtime/201.json"),
		testWritableRecord(202, "/runtime/202.json"),
		testWritableRecord(203, "/runtime/203.json"),
	}
	deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) { return records, nil }
	identityChanged := false
	var confirmations, signalled []int
	deps.stopTargetConfirmed = func(rec daemon.RuntimeRecord, _ string) bool {
		confirmations = append(confirmations, rec.PID)
		return !identityChanged || rec.PID != 202
	}
	deps.stopProcess = func(rec daemon.RuntimeRecord, _ time.Duration) error {
		signalled = append(signalled, rec.PID)
		identityChanged = true
		return nil
	}

	err := executeDaemonCommand(t, *deps, out, "stop")
	require.Error(t, err)
	assert.ErrorContains(t, err, "partial stop")
	assert.ErrorContains(t, err, "stopped pid 201")
	assert.ErrorContains(t, err, "remaining pids 202, 203")
	assert.ErrorContains(t, err, "verify")
	assert.Equal(t, []int{201, 202, 203, 201, 202}, confirmations)
	assert.Equal(t, []int{201}, signalled,
		"changed and later identities must never be signalled")
}

func TestDaemonStopRevalidationFailureBeforeFirstSignalSaysAborted(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	rec := testWritableRecord(204, "/runtime/204.json")
	deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
		return []daemon.RuntimeRecord{rec}, nil
	}
	confirmations := 0
	deps.stopTargetConfirmed = func(daemon.RuntimeRecord, string) bool {
		confirmations++
		return confirmations == 1
	}
	signals := 0
	deps.stopProcess = func(daemon.RuntimeRecord, time.Duration) error {
		signals++
		return nil
	}

	err := executeDaemonCommand(t, *deps, out, "stop")
	require.Error(t, err)
	assert.ErrorContains(t, err, "stop aborted before signaling")
	assert.ErrorContains(t, err, "no process was signalled")
	assert.ErrorContains(t, err, "remaining pids 204")
	assert.NotContains(t, err.Error(), "partial stop")
	assert.Zero(t, signals)
}

func TestDaemonStartSlowReadinessIsNotReportedRunning(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.startBackground = func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error) {
		return backgroundLaunchResult{
			Started: true, LogPath: "/tmp/slow-start.log", childPID: 301,
		}, nil
	}

	err := executeDaemonCommand(t, *deps, out, "start")
	require.Error(t, err)
	assert.ErrorContains(t, err, "startup is still in progress")
	assert.ErrorContains(t, err, "pid 301")
	assert.ErrorContains(t, err, "/tmp/slow-start.log")
	assert.NotContains(t, out.String(), "agentsview running")
}

func TestDaemonRestartSlowReadinessIsNotReportedSuccessful(t *testing.T) {
	for _, wasRunning := range []bool{false, true} {
		t.Run(fmt.Sprintf("was-running-%t", wasRunning), func(t *testing.T) {
			deps, out := daemonCommandTestDeps(t)
			if wasRunning {
				deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
					return []daemon.RuntimeRecord{testWritableRecord(302, "/runtime/302.json")}, nil
				}
			}
			deps.startBackground = func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error) {
				return backgroundLaunchResult{
					Started: true, LogPath: "/tmp/slow-restart.log", childPID: 303,
				}, nil
			}

			err := executeDaemonCommand(t, *deps, out, "restart")
			require.Error(t, err)
			assert.ErrorContains(t, err, "startup is still in progress")
			assert.ErrorContains(t, err, "pid 303")
			assert.ErrorContains(t, err, "/tmp/slow-restart.log")
			assert.NotContains(t, out.String(), "restarted")
			assert.NotContains(t, out.String(), "started (was not running)")
		})
	}
}

func TestDaemonCanonicalCleanupFailureStopsRestart(t *testing.T) {
	for _, command := range []string{"stop", "restart"} {
		t.Run(command, func(t *testing.T) {
			deps, out := daemonCommandTestDeps(t)
			deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
				return []daemon.RuntimeRecord{testWritableRecord(401, "/runtime/401.json")}, nil
			}
			deps.stopCaddy = func(w io.Writer, rec daemon.RuntimeRecord) error {
				fmt.Fprintf(w, "managed caddy cleanup failed for owner %d\n", rec.PID)
				return errors.New("caddy would not stop")
			}
			starts := 0
			deps.startBackground = func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error) {
				starts++
				return backgroundLaunchResult{}, nil
			}

			err := executeDaemonCommand(t, *deps, out, command)
			require.Error(t, err)
			assert.ErrorContains(t, err, "managed caddy")
			assert.ErrorContains(t, err, "caddy would not stop")
			assert.Contains(t, out.String(), "managed caddy cleanup failed")
			assert.Zero(t, starts, "restart must not start after cleanup failure")
		})
	}
}

func TestDaemonStatusReportsIncompatibleAndNotResponding(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.statusRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
		rec := testWritableRecord(501, "/runtime/501.json")
		rec.Metadata[runtimeAPIVersion] = "0"
		return []daemon.RuntimeRecord{rec}, nil
	}
	deps.probeRecord = func(daemon.RuntimeRecord, string) (daemon.PingInfo, bool) {
		return daemon.PingInfo{}, false
	}

	require.NoError(t, executeDaemonCommand(t, *deps, out, "status"))
	assert.Contains(t, out.String(), "incompatible")
	assert.Contains(t, out.String(), "API version")
	assert.Contains(t, out.String(), "not responding")
	assert.Contains(t, out.String(), "pid:     501")
}

func TestDaemonCanonicalCommandsSurfaceLaunchLockErrors(t *testing.T) {
	for _, command := range []string{"start", "stop", "restart"} {
		t.Run(command, func(t *testing.T) {
			deps, out := daemonCommandTestDeps(t)
			deps.acquireLaunchLockWithError = func(string) (daemonLaunchLock, bool, error) {
				return nil, false, errors.New("permission denied")
			}
			configLoads := 0
			deps.loadConfig = func() (config.Config, error) {
				configLoads++
				return config.Config{}, nil
			}

			err := executeDaemonCommand(t, *deps, out, command)
			require.Error(t, err)
			assert.ErrorContains(t, err, "acquiring launch lock")
			assert.ErrorContains(t, err, "permission denied")
			assert.NotContains(t, err.Error(), "busy")
			assert.Zero(t, configLoads)
		})
	}
}

func TestDaemonMutationRejectsLoadedDataDirMismatch(t *testing.T) {
	for _, command := range []string{"start", "stop", "restart"} {
		t.Run(command, func(t *testing.T) {
			deps, out := daemonCommandTestDeps(t)
			lockedDir := t.TempDir()
			loadedDir := t.TempDir()
			deps.resolveDataDir = func() (string, error) { return lockedDir, nil }
			deps.mkdirAll = func(string, os.FileMode) error { return nil }
			deps.loadConfig = func() (config.Config, error) {
				return config.Config{
					DataDir: loadedDir,
					DBPath:  filepath.Join(loadedDir, "sessions.db"),
				}, nil
			}
			discoveries, signals, starts := 0, 0, 0
			deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
				discoveries++
				return nil, nil
			}
			deps.stopProcess = func(daemon.RuntimeRecord, time.Duration) error { signals++; return nil }
			deps.startBackground = func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error) {
				starts++
				return backgroundLaunchResult{}, nil
			}

			err := executeDaemonCommand(t, *deps, out, command)
			require.Error(t, err)
			assert.ErrorContains(t, err, "data dir changed after launch lock")
			assert.ErrorContains(t, err, strconv.Quote(lockedDir))
			assert.ErrorContains(t, err, strconv.Quote(loadedDir))
			assert.Zero(t, discoveries)
			assert.Zero(t, signals)
			assert.Zero(t, starts)
		})
	}
}

func TestStopOrphanedCaddyChildWithWriterReportsCanonicalOutput(t *testing.T) {
	requirePOSIXSignals(t, "graceful SIGTERM termination is POSIX-specific")
	pid, reaped := startReapedSleepProcess(t)
	created, ok := processCreateTimeMillis(pid)
	require.True(t, ok)
	rec := daemon.RuntimeRecord{Metadata: map[string]string{
		runtimeCaddyPID:        fmt.Sprint(pid),
		runtimeCaddyCreateTime: fmt.Sprint(created),
	}}
	var out bytes.Buffer

	require.NoError(t, stopOrphanedCaddyChildWithWriter(&out, rec))
	<-reaped
	assert.Contains(t, out.String(), fmt.Sprintf("Stopped managed caddy (pid %d).", pid))
}

func TestStopOrphanedCaddyChildWithWriterRejectsUnknownIdentity(t *testing.T) {
	pid := startSleepProcess(t)
	rec := daemon.RuntimeRecord{Metadata: map[string]string{
		runtimeCaddyPID: fmt.Sprint(pid),
	}}
	var out bytes.Buffer

	err := stopOrphanedCaddyChildWithWriter(&out, rec)
	require.Error(t, err)
	assert.ErrorContains(t, err, "identity could not be confirmed")
	assert.ErrorContains(t, err, fmt.Sprintf("pid %d", pid))
	assert.True(t, daemon.ProcessAlive(pid))
	assert.Empty(t, out.String())
}

func TestStopOrphanedCaddyChildLegacyWarnsOnUnknownIdentity(t *testing.T) {
	pid := startSleepProcess(t)
	rec := daemon.RuntimeRecord{Metadata: map[string]string{
		runtimeCaddyPID: fmt.Sprint(pid),
	}}

	out := captureStdout(t, func() { stopOrphanedCaddyChild(rec) })
	assert.Contains(t, out, "warning: could not stop managed caddy")
	assert.Contains(t, out, fmt.Sprintf("pid %d", pid))
	assert.Contains(t, out, "identity could not be confirmed")
	assert.True(t, daemon.ProcessAlive(pid))
}
