package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

	resultCh := make(chan daemonLaunchObservation, 1)
	go func() { resultCh <- waitForDaemonLaunchContention(dir) }()
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

func TestDaemonStopRevalidatesIdentityBeforeEverySignal(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	records := []daemon.RuntimeRecord{
		testWritableRecord(201, "/runtime/201.json"),
		testWritableRecord(202, "/runtime/202.json"),
		testWritableRecord(203, "/runtime/203.json"),
	}
	deps.writableRecords = func(string) ([]daemon.RuntimeRecord, error) { return records, nil }
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
				deps.writableRecords = func(string) ([]daemon.RuntimeRecord, error) {
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
			deps.writableRecords = func(string) ([]daemon.RuntimeRecord, error) {
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
	deps.statusRecords = func(string) ([]daemon.RuntimeRecord, error) {
		rec := testWritableRecord(501, "/runtime/501.json")
		rec.Metadata[runtimeAPIVersion] = "0"
		return []daemon.RuntimeRecord{rec}, nil
	}
	deps.probeRecord = func(daemon.RuntimeRecord, string) bool { return false }

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
			deps.writableRecords = func(string) ([]daemon.RuntimeRecord, error) {
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
			assert.ErrorContains(t, err, lockedDir)
			assert.ErrorContains(t, err, loadedDir)
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
