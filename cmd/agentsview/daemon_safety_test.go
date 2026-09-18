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
	assert := assert.New(t)
	require := require.New(t)

	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	lock, ok := acquireBackgroundLaunchLock(dir)
	require.True(ok)
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = lock.Unlock()
		}
	})

	path, err := writeRuntimeRecordForTest(
		dir, testWritableRecord(os.Getpid(), ""),
	)
	require.NoError(err)

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
		require.Fail("waiter never attempted the held launch lock")
	}
	select {
	case observation := <-resultCh:
		assert.Fail("contender returned while mutating owner held lock",
			"observation: %+v", observation)
		return
	case <-time.After(75 * time.Millisecond):
	}

	require.NoError(os.Remove(path), "simulated stop removes old writer")
	require.NoError(lock.Unlock())
	locked = false

	select {
	case observation := <-resultCh:
		assert.False(observation.LockHeld)
		assert.Empty(observation.Records,
			"removed writer must not be reported after owner releases lock")
		assert.False(observation.Starting)
	case <-time.After(time.Second):
		require.Fail("waiter did not inspect final state after lock release")
	}
}

func TestDaemonWaitForLaunchContentionRejectsUnconfirmedLiveWriter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	lock, ok := acquireBackgroundLaunchLock(dir)
	require.True(ok)
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
	require.NoError(err)

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
		require.Fail("waiter never attempted the held launch lock")
	}
	require.NoError(lock.Unlock())
	locked = false

	var observation daemonLaunchObservation
	select {
	case observation = <-resultCh:
	case <-time.After(2 * time.Second):
		require.Fail("waiter did not inspect writer after lock release")
	}
	assert.Empty(observation.Records)
	require.Len(observation.UnconfirmedRecords, 1)
	assert.Equal(pid, observation.UnconfirmedRecords[0].PID)

	var out bytes.Buffer
	err = reportDaemonLaunchContention(&out, dir, observation, time.Now())
	require.Error(err)
	assert.ErrorContains(err, fmt.Sprintf("pid %d", pid))
	assert.ErrorContains(err, path)
	assert.ErrorContains(err, "verify")
	assert.ErrorContains(err, "terminate")
	assert.NotContains(out.String(), "already running")
	assert.True(daemon.ProcessAlive(pid))
}

func TestDaemonWaitForLaunchContentionRejectsIncompatibleResponsiveWriter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := runtimeTestDir(t)
	endpoint, probed := newPingDaemonWithProbeSignal(t)
	rec := testWritableRecord(os.Getpid(), "/runtime/incompatible.json")
	rec.Address = endpoint.Addr
	rec.Metadata[runtimeHost] = endpoint.Host
	rec.Metadata[runtimePort] = strconv.Itoa(endpoint.Port)
	rec.Metadata[runtimeAPIVersion] = strconv.Itoa(daemonAPIVersion + 1)

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
		require.Fail("responsive writer was not probed")
	}
	var out bytes.Buffer
	err := reportDaemonLaunchContention(&out, dir, observation, time.Now())
	require.Error(err)
	assert.ErrorContains(err, "incompatible")
	assert.ErrorContains(err, fmt.Sprintf("pid %d", rec.PID))
	assert.ErrorContains(err, "daemon restart")
	assert.NotContains(out.String(), "already running")
}

func TestDaemonWaitForLaunchContentionRejectsCompatibleUnresponsiveWriter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := runtimeTestDir(t)
	rec := testWritableRecord(os.Getpid(), "/runtime/unresponsive.json")
	rec.Address = "127.0.0.1:1"
	rec.Metadata[runtimePort] = "1"
	createTime, ok := processCreateTimeMillis(rec.PID)
	require.True(ok)
	rec.Metadata[runtimeCreateTime] = strconv.FormatInt(createTime, 10)

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
	require.Error(err)
	assert.ErrorContains(err, "not responding")
	assert.ErrorContains(err, fmt.Sprintf("pid %d", rec.PID))
	assert.ErrorContains(err, "daemon restart")
	assert.NotContains(out.String(), "already running")
}

func TestDaemonWaitForLaunchContentionSurfacesReadOnlyConfigError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := runtimeTestDir(t)
	waitDeps := defaultDaemonLaunchWaitDeps()
	waitDeps.loadReadOnlyConfig = func() (config.Config, error) {
		return config.Config{}, errors.New("read-only config failed")
	}

	observation := waitForDaemonLaunchContentionWithDeps(dir, waitDeps)
	require.Error(observation.Err)
	assert.ErrorContains(observation.Err, "read-only config failed")
	var out bytes.Buffer
	err := reportDaemonLaunchContention(&out, dir, observation, time.Now())
	require.Error(err)
	assert.ErrorContains(err, "read-only config failed")
}

func TestDaemonStopRevalidatesIdentityBeforeEverySignal(t *testing.T) {
	assert := assert.New(t)

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
	assert.ErrorContains(err, "partial stop")
	assert.ErrorContains(err, "stopped pid 201")
	assert.ErrorContains(err, "remaining pids 202, 203")
	assert.ErrorContains(err, "verify")
	assert.Equal([]int{201, 202, 203, 201, 202}, confirmations)
	assert.Equal([]int{201}, signalled,
		"changed and later identities must never be signalled")
}

func TestDaemonStopRevalidationFailureBeforeFirstSignalSaysAborted(t *testing.T) {
	assert := assert.New(t)

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
	assert.ErrorContains(err, "stop aborted before signaling")
	assert.ErrorContains(err, "no process was signalled")
	assert.ErrorContains(err, "remaining pids 204")
	assert.NotContains(err.Error(), "partial stop")
	assert.Zero(signals)
}

func TestDaemonStartSlowReadinessIsNotReportedRunning(t *testing.T) {
	assert := assert.New(t)

	deps, out := daemonCommandTestDeps(t)
	deps.startBackground = func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error) {
		return backgroundLaunchResult{
			Started: true, LogPath: "/tmp/slow-start.log", childPID: 301,
		}, nil
	}

	err := executeDaemonCommand(t, *deps, out, "start")
	require.Error(t, err)
	assert.ErrorContains(err, "startup is still in progress")
	assert.ErrorContains(err, "pid 301")
	assert.ErrorContains(err, "/tmp/slow-start.log")
	assert.NotContains(out.String(), "agentsview running")
}

func TestDaemonRestartSlowReadinessIsNotReportedSuccessful(t *testing.T) {
	for _, wasRunning := range []bool{false, true} {
		t.Run(fmt.Sprintf("was-running-%t", wasRunning), func(t *testing.T) {
			assert := assert.New(t)

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
			assert.ErrorContains(err, "startup is still in progress")
			assert.ErrorContains(err, "pid 303")
			assert.ErrorContains(err, "/tmp/slow-restart.log")
			assert.NotContains(out.String(), "restarted")
			assert.NotContains(out.String(), "started (was not running)")
		})
	}
}

func TestDaemonCanonicalCleanupFailureStopsRestart(t *testing.T) {
	for _, command := range []string{"stop", "restart"} {
		t.Run(command, func(t *testing.T) {
			assert := assert.New(t)

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
			assert.ErrorContains(err, "managed caddy")
			assert.ErrorContains(err, "caddy would not stop")
			assert.Contains(out.String(), "managed caddy cleanup failed")
			assert.Zero(starts, "restart must not start after cleanup failure")
		})
	}
}

func TestDaemonStatusReportsIncompatibleAndNotResponding(t *testing.T) {
	assert := assert.New(t)

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
	assert.Contains(out.String(), "incompatible")
	assert.Contains(out.String(), "API version")
	assert.Contains(out.String(), "not responding")
	assert.Contains(out.String(), "pid:     501")
}

func TestDaemonCanonicalCommandsSurfaceLaunchLockErrors(t *testing.T) {
	for _, command := range []string{"start", "stop", "restart"} {
		t.Run(command, func(t *testing.T) {
			assert := assert.New(t)

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
			assert.ErrorContains(err, "acquiring launch lock")
			assert.ErrorContains(err, "permission denied")
			assert.NotContains(err.Error(), "busy")
			assert.Zero(configLoads)
		})
	}
}

func TestDaemonMutationRejectsLoadedDataDirMismatch(t *testing.T) {
	for _, command := range []string{"start", "stop", "restart"} {
		t.Run(command, func(t *testing.T) {
			assert := assert.New(t)

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
			assert.ErrorContains(err, "data dir changed after launch lock")
			assert.ErrorContains(err, strconv.Quote(lockedDir))
			assert.ErrorContains(err, strconv.Quote(loadedDir))
			assert.Zero(discoveries)
			assert.Zero(signals)
			assert.Zero(starts)
		})
	}
}

func TestStopOrphanedCaddyChildWithWriterReportsCanonicalOutput(t *testing.T) {
	requirePOSIXSignals(t, "graceful SIGTERM termination is POSIX-specific")
	pid, reaped := startReapedSleepProcess(t)
	created, ok := processCreateTimeMillis(pid)
	require.True(t, ok)
	rec := daemon.RuntimeRecord{Metadata: map[string]string{
		runtimeCaddyPID:        strconv.Itoa(pid),
		runtimeCaddyCreateTime: strconv.FormatInt(created, 10),
	}}
	var out bytes.Buffer

	require.NoError(t, stopOrphanedCaddyChildWithWriter(&out, rec))
	<-reaped
	assert.Contains(t, out.String(), fmt.Sprintf("Stopped managed caddy (pid %d).", pid))
}

func TestStopOrphanedCaddyChildWithWriterRejectsUnknownIdentity(t *testing.T) {
	assert := assert.New(t)

	pid := startSleepProcess(t)
	rec := daemon.RuntimeRecord{Metadata: map[string]string{
		runtimeCaddyPID: strconv.Itoa(pid),
	}}
	var out bytes.Buffer

	err := stopOrphanedCaddyChildWithWriter(&out, rec)
	require.Error(t, err)
	assert.ErrorContains(err, "identity could not be confirmed")
	assert.ErrorContains(err, fmt.Sprintf("pid %d", pid))
	assert.True(daemon.ProcessAlive(pid))
	assert.Empty(out.String())
}

func TestStopOrphanedCaddyChildLegacyWarnsOnUnknownIdentity(t *testing.T) {
	assert := assert.New(t)

	pid := startSleepProcess(t)
	rec := daemon.RuntimeRecord{Metadata: map[string]string{
		runtimeCaddyPID: strconv.Itoa(pid),
	}}

	out := captureStdout(t, func() { stopOrphanedCaddyChild(rec) })
	assert.Contains(out, "warning: could not stop managed caddy")
	assert.Contains(out, fmt.Sprintf("pid %d", pid))
	assert.Contains(out, "identity could not be confirmed")
	assert.True(daemon.ProcessAlive(pid))
}
