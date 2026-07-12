package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/daemon"
)

type testDaemonLaunchLock struct {
	unlocked bool
}

func (l *testDaemonLaunchLock) Unlock() error {
	l.unlocked = true
	return nil
}

func daemonCommandTestDeps(t *testing.T) (*daemonCommandDeps, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	out := new(bytes.Buffer)
	deps := defaultDaemonCommandDeps()
	deps.resolveDataDir = func() (string, error) { return dir, nil }
	deps.mkdirAll = func(path string, mode os.FileMode) error {
		assert.Equal(t, dir, path)
		return nil
	}
	deps.loadConfig = func() (config.Config, error) {
		return config.Config{DataDir: dir, DBPath: filepath.Join(dir, "sessions.db")}, nil
	}
	deps.loadReadOnlyConfig = deps.loadConfig
	deps.acquireLaunchLockWithError = func(string) (daemonLaunchLock, bool, error) {
		return &testDaemonLaunchLock{}, true, nil
	}
	deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) { return nil, nil }
	deps.statusRecords = func(string, string) ([]daemon.RuntimeRecord, error) { return nil, nil }
	deps.isStarting = func(string) bool { return false }
	deps.readStartupState = func(string) *startupState { return nil }
	deps.startBackground = func(
		cfg config.Config, args []string, opts serveReplacementOptions,
		policy backgroundLaunchPolicy,
	) (backgroundLaunchResult, error) {
		return backgroundLaunchResult{
			Runtime: &DaemonRuntime{
				Record: daemon.RuntimeRecord{PID: 314},
				Host:   "127.0.0.1", Port: 8080,
			},
			Started: true, LogPath: filepath.Join(dir, "serve.log"), childPID: 314,
		}, nil
	}
	deps.waitContendedLaunch = func(string) daemonLaunchObservation {
		return daemonLaunchObservation{LockHeld: true}
	}
	deps.stopTargetConfirmed = func(daemon.RuntimeRecord, string) bool { return true }
	deps.stopProcess = func(daemon.RuntimeRecord, time.Duration) error { return nil }
	deps.stopCaddy = func(io.Writer, daemon.RuntimeRecord) error { return nil }
	deps.validateConfig = func(config.Config) error { return nil }
	deps.checkDataVersion = func(string) error { return nil }
	deps.probeRecord = func(rec daemon.RuntimeRecord, _ string) (daemon.PingInfo, bool) {
		return daemon.PingInfo{PID: rec.PID}, true
	}
	deps.now = func() time.Time { return time.Unix(200, 0) }
	return &deps, out
}

func executeDaemonCommand(
	t *testing.T, deps daemonCommandDeps, out io.Writer, args ...string,
) error {
	t.Helper()
	cmd := newDaemonCommandWithDeps(deps)
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func executeServeCommand(
	t *testing.T, deps daemonCommandDeps, out io.Writer, args ...string,
) error {
	t.Helper()
	cmd := newServeCommandWithDaemonDeps(deps)
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func testWritableRecord(pid int, path string) daemon.RuntimeRecord {
	return daemon.RuntimeRecord{
		PID: pid, Service: daemonService, Version: "1.2.3",
		Network: daemon.NetworkTCP, Address: fmt.Sprintf("127.0.0.1:%d", 8000+pid),
		SourcePath: path,
		StartedAt:  time.Unix(100, 0),
		Metadata: map[string]string{
			runtimeHost: "127.0.0.1", runtimePort: fmt.Sprint(8000 + pid),
			runtimeAPIVersion:  fmt.Sprint(daemonAPIVersion),
			runtimeDataVersion: fmt.Sprint(db.CurrentDataVersion()),
		},
	}
}

func TestDaemonCommandRegistrationAndSurface(t *testing.T) {
	root := newRootCommand()
	daemonCmd, _, err := root.Find([]string{"daemon"})
	require.NoError(t, err)
	require.Equal(t, "daemon", daemonCmd.Name())
	assert.Equal(t, groupCore, daemonCmd.GroupID)

	names := make(map[string]bool)
	for _, cmd := range daemonCmd.Commands() {
		names[cmd.Name()] = true
		assert.Nil(t, cmd.Flags().Lookup("host"))
		assert.Nil(t, cmd.Flags().Lookup("no-sync"))
	}
	assert.Equal(t, map[string]bool{
		"restart": true, "start": true, "status": true, "stop": true,
	}, names)

	help, err := executeCommand(root, "--help")
	require.NoError(t, err)
	assert.Contains(t, help, "daemon")
}

func TestDaemonCommandRejectsArgumentsAndServeFlags(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	for _, subcommand := range []string{"start", "status", "stop", "restart"} {
		t.Run(subcommand+" arg", func(t *testing.T) {
			out.Reset()
			err := executeDaemonCommand(t, *deps, out, subcommand, "extra")
			require.Error(t, err)
			assert.ErrorContains(t, err, "unknown command")
		})
		for _, flag := range []string{"--host", "--no-sync"} {
			t.Run(subcommand+" "+flag, func(t *testing.T) {
				out.Reset()
				err := executeDaemonCommand(t, *deps, out, subcommand, flag)
				require.Error(t, err)
				assert.ErrorContains(t, err, "unknown flag")
			})
		}
	}
}

func TestDaemonStartUsesConfigOnlyPolicyAndIsIdempotent(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	var gotArgs []string
	var gotPolicy backgroundLaunchPolicy
	deps.startBackground = func(
		cfg config.Config, args []string, _ serveReplacementOptions,
		policy backgroundLaunchPolicy,
	) (backgroundLaunchResult, error) {
		gotArgs = append([]string(nil), args...)
		gotPolicy = policy
		return backgroundLaunchResult{
			Runtime: &DaemonRuntime{Record: daemon.RuntimeRecord{PID: 41}, Host: "127.0.0.1", Port: 9090},
		}, nil
	}

	require.NoError(t, executeDaemonCommand(t, *deps, out, "start"))
	assert.Equal(t, []string{"serve"}, gotArgs)
	assert.True(t, gotPolicy.ConfigOnly)
	assert.Equal(t, "daemon start", gotPolicy.Operation)
	assert.Contains(t, out.String(), "already running")
	assert.Contains(t, out.String(), "pid 41")
}

func TestDaemonStopUsesStartupStateFallbackWhileStarting(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.isStarting = func(string) bool { return true }
	deps.readStartupState = func(string) *startupState {
		return &startupState{PID: 77, Phase: "starting HTTP server"}
	}
	var stopped daemon.RuntimeRecord
	deps.writableRuntime = func(string, string) *DaemonRuntime {
		return &DaemonRuntime{Record: testWritableRecord(77, ""), RuntimeFallback: true}
	}
	deps.stopProcess = func(rec daemon.RuntimeRecord, _ time.Duration) error {
		stopped = rec
		return nil
	}

	err := executeDaemonCommand(t, *deps, out, "stop")
	require.NoError(t, err)
	assert.Equal(t, 77, stopped.PID)
	assert.Contains(t, out.String(), "Stopped agentsview (pid 77).")
	assert.NotContains(t, out.String(), "starting up")
}

func TestDaemonStopUsesStartupStateFallbackWhenRuntimeStoreInspectionFails(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
		return nil, errors.New("store unavailable")
	}
	deps.writableRuntime = func(string, string) *DaemonRuntime {
		return &DaemonRuntime{Record: testWritableRecord(76, ""), RuntimeFallback: true}
	}
	var stopped daemon.RuntimeRecord
	deps.stopProcess = func(rec daemon.RuntimeRecord, _ time.Duration) error {
		stopped = rec
		return nil
	}

	require.NoError(t, executeDaemonCommand(t, *deps, out, "stop"))
	assert.Equal(t, 76, stopped.PID)
	assert.Contains(t, out.String(), "Stopped agentsview (pid 76).")
}

func TestDaemonStopRegisteredRuntimePreservesStartupGuard(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.isStarting = func(string) bool { return true }
	deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
		return []daemon.RuntimeRecord{testWritableRecord(78, "runtime.json")}, nil
	}

	err := executeDaemonCommand(t, *deps, out, "stop")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "startup is still in progress")
}

func TestDaemonStartUsesStartupStateFallbackWhileStarting(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.isStarting = func(string) bool { return true }
	deps.writableRuntime = func(string, string) *DaemonRuntime {
		return &DaemonRuntime{
			Record:          testWritableRecord(79, ""),
			Host:            "127.0.0.1",
			Port:            8079,
			RuntimeFallback: true,
		}
	}
	starts := 0
	deps.startBackground = func(
		config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy,
	) (backgroundLaunchResult, error) {
		starts++
		return backgroundLaunchResult{}, nil
	}

	require.NoError(t, executeDaemonCommand(t, *deps, out, "start"))
	assert.Zero(t, starts)
	assert.Contains(t, out.String(), "already running")
	assert.Contains(t, out.String(), "8079")
}

func TestDaemonStartPersistentStartupNeverLaunches(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.isStarting = func(string) bool { return true }
	deps.readStartupState = func(string) *startupState {
		return &startupState{PID: 77, StartedAt: time.Unix(100, 0), LogPath: "/tmp/serve.log", Phase: "sync"}
	}
	started := 0
	deps.startBackground = func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error) {
		started++
		return backgroundLaunchResult{}, nil
	}

	err := executeDaemonCommand(t, *deps, out, "start")
	require.Error(t, err)
	assert.ErrorContains(t, err, "startup is still in progress")
	assert.ErrorContains(t, err, "pid 77")
	assert.ErrorContains(t, err, "/tmp/serve.log")
	assert.ErrorContains(t, err, "verify")
	assert.Equal(t, 0, started)
}

func TestDaemonStartLateStartupLockDoesNotReportSuccess(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	starting := false
	deps.isStarting = func(string) bool { return starting }
	deps.readStartupState = func(string) *startupState {
		return &startupState{PID: 78, LogPath: "/tmp/late-serve.log"}
	}
	deps.startBackground = func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error) {
		starting = true
		return backgroundLaunchResult{}, nil
	}

	err := executeDaemonCommand(t, *deps, out, "start")
	require.Error(t, err)
	assert.ErrorContains(t, err, "startup is still in progress")
	assert.ErrorContains(t, err, "pid 78")
	assert.NotContains(t, out.String(), "pid 0")
}

func TestDaemonStartLaunchContentionTerminalStates(t *testing.T) {
	tests := []struct {
		name        string
		observation daemonLaunchObservation
		wantErr     string
		wantOut     string
	}{
		{
			name: "published writable",
			observation: daemonLaunchObservation{Records: []daemon.RuntimeRecord{
				testWritableRecord(21, "/runtime/21.json"),
			}},
			wantOut: "already running",
		},
		{
			name: "multiple published writables",
			observation: daemonLaunchObservation{Records: []daemon.RuntimeRecord{
				testWritableRecord(31, "/runtime/31.json"),
				testWritableRecord(32, "/runtime/32.json"),
			}},
			wantErr: "multiple writable agentsview daemons",
		},
		{
			name: "persistent startup snapshot",
			observation: daemonLaunchObservation{LockHeld: true, Starting: true, Snapshot: &startupState{
				PID: 22, StartedAt: time.Unix(100, 0), LogPath: "/tmp/serve.log", Phase: "opening database",
			}},
			wantErr: "startup is still in progress",
		},
		{
			name:        "persistent launch lock without snapshot",
			observation: daemonLaunchObservation{LockHeld: true},
			wantErr:     "launch is still in progress",
		},
		{
			name:        "owner failed without publishing",
			observation: daemonLaunchObservation{},
			wantErr:     "startup failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, out := daemonCommandTestDeps(t)
			loadCalls, startCalls := 0, 0
			deps.acquireLaunchLockWithError = func(string) (daemonLaunchLock, bool, error) { return nil, false, nil }
			deps.waitContendedLaunch = func(string) daemonLaunchObservation { return tt.observation }
			deps.loadConfig = func() (config.Config, error) { loadCalls++; return config.Config{}, nil }
			deps.startBackground = func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error) {
				startCalls++
				return backgroundLaunchResult{}, nil
			}

			err := executeDaemonCommand(t, *deps, out, "start")
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Contains(t, out.String(), tt.wantOut)
			} else {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantErr)
			}
			assert.Equal(t, 0, loadCalls, "contender must not read or write config")
			assert.Equal(t, 0, startCalls, "contender must never start a second writer")
		})
	}
}

func TestDaemonStatusRendersStoppedStartingReadOnlyAndIncompatible(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*daemonCommandDeps)
		wanted   []string
		unwanted []string
	}{
		{name: "stopped", setup: func(*daemonCommandDeps) {}, wanted: []string{"No agentsview daemon is running"}},
		{name: "starting", setup: func(d *daemonCommandDeps) {
			d.isStarting = func(string) bool { return true }
			d.readStartupState = func(string) *startupState {
				return &startupState{PID: 9, StartedAt: time.Unix(100, 0), Phase: "sync", LogPath: "/tmp/log"}
			}
		}, wanted: []string{"starting", "pid:     9", "phase:   sync", "/tmp/log"}},
		{name: "read only", setup: func(d *daemonCommandDeps) {
			d.statusRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
				rec := testWritableRecord(10, "/runtime/10.json")
				rec.Metadata[runtimeReadOnly] = "true"
				return []daemon.RuntimeRecord{rec}, nil
			}
		}, wanted: []string{"No agentsview daemon is running."}, unwanted: []string{"running at", "mode:    read-only"}},
		{name: "incompatible", setup: func(d *daemonCommandDeps) {
			d.statusRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
				rec := testWritableRecord(11, "/runtime/11.json")
				rec.Metadata[runtimeAPIVersion] = "0"
				return []daemon.RuntimeRecord{rec}, nil
			}
			d.probeRecord = func(rec daemon.RuntimeRecord, _ string) (daemon.PingInfo, bool) {
				return daemon.PingInfo{PID: rec.PID}, true
			}
		}, wanted: []string{"incompatible", "pid:     11", "API version"}},
		{name: "running", setup: func(d *daemonCommandDeps) {
			d.statusRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
				return []daemon.RuntimeRecord{testWritableRecord(12, "/runtime/12.json")}, nil
			}
			d.probeRecord = func(rec daemon.RuntimeRecord, _ string) (daemon.PingInfo, bool) {
				return daemon.PingInfo{PID: rec.PID}, true
			}
		}, wanted: []string{"running at http://127.0.0.1:8012", "pid:     12", "version: 1.2.3", "uptime:"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, out := daemonCommandTestDeps(t)
			tt.setup(deps)
			require.NoError(t, executeDaemonCommand(t, *deps, out, "status"))
			for _, wanted := range tt.wanted {
				assert.Contains(t, out.String(), wanted)
			}
			for _, unwanted := range tt.unwanted {
				assert.NotContains(t, out.String(), unwanted)
			}
		})
	}
}

func TestDaemonStartContentionStartLockWithoutSnapshotUsesRecoveryError(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.acquireLaunchLockWithError = func(string) (daemonLaunchLock, bool, error) { return nil, false, nil }
	deps.waitContendedLaunch = func(string) daemonLaunchObservation {
		return daemonLaunchObservation{LockHeld: true, Starting: true}
	}

	err := executeDaemonCommand(t, *deps, out, "start")
	require.Error(t, err)
	assert.ErrorContains(t, err, "startup is still in progress")
	assert.ErrorContains(t, err, "runtime publication may have failed")
	assert.ErrorContains(t, err, startupStateFileName)
	assert.NotContains(t, err.Error(), "launch is still in progress")
}

func TestDaemonStartContentionHeldStartLockWinsOverPreexistingRecord(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.acquireLaunchLockWithError = func(string) (daemonLaunchLock, bool, error) { return nil, false, nil }
	deps.waitContendedLaunch = func(string) daemonLaunchObservation {
		return daemonLaunchObservation{
			LockHeld: true,
			Starting: true,
			Snapshot: &startupState{PID: 140, LogPath: "/tmp/serve.log"},
			Records: []daemon.RuntimeRecord{
				testWritableRecord(141, "/runtime/141.json"),
			},
		}
	}
	configLoads, starts := 0, 0
	deps.loadConfig = func() (config.Config, error) {
		configLoads++
		return config.Config{}, nil
	}
	deps.startBackground = func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error) {
		starts++
		return backgroundLaunchResult{}, nil
	}

	err := executeDaemonCommand(t, *deps, out, "start")
	require.Error(t, err)
	assert.ErrorContains(t, err, "startup is still in progress")
	assert.ErrorContains(t, err, "pid 140")
	assert.NotContains(t, out.String(), "already running")
	assert.Zero(t, configLoads)
	assert.Zero(t, starts)
}

func TestDaemonStatusListsEveryWriterAndSurfacesInspectionErrors(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.statusRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
		return []daemon.RuntimeRecord{
			testWritableRecord(31, "/runtime/31.json"),
			testWritableRecord(32, "/runtime/32.json"),
		}, nil
	}
	require.NoError(t, executeDaemonCommand(t, *deps, out, "status"))
	assert.Contains(t, out.String(), "single-writer invariant")
	assert.Contains(t, out.String(), "pid:     31")
	assert.Contains(t, out.String(), "pid:     32")

	out.Reset()
	deps.statusRecords = func(string, string) ([]daemon.RuntimeRecord, error) { return nil, errors.New("store unavailable") }
	err := executeDaemonCommand(t, *deps, out, "status")
	require.Error(t, err)
	assert.ErrorContains(t, err, "store unavailable")

	deps.loadReadOnlyConfig = func() (config.Config, error) { return config.Config{}, errors.New("bad config") }
	err = executeDaemonCommand(t, *deps, out, "status")
	require.Error(t, err)
	assert.ErrorContains(t, err, "bad config")
}

func TestDaemonStatusUsesStartupStateFallbackWhenRuntimeStoreInspectionFails(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.statusRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
		return nil, errors.New("store unavailable")
	}
	deps.writableRuntime = func(string, string) *DaemonRuntime {
		return &DaemonRuntime{
			Record:          testWritableRecord(54, ""),
			Host:            "127.0.0.1",
			Port:            8054,
			RuntimeFallback: true,
		}
	}

	require.NoError(t, executeDaemonCommand(t, *deps, out, "status"))
	assert.Contains(t, out.String(), "pid:     54")
	assert.Contains(t, out.String(), "runtime record unwritten")
}

func TestDaemonStatusNotRespondingIsUseful(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.statusRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
		return []daemon.RuntimeRecord{testWritableRecord(55, "/runtime/55.json")}, nil
	}
	deps.probeRecord = func(daemon.RuntimeRecord, string) (daemon.PingInfo, bool) {
		return daemon.PingInfo{}, false
	}
	require.NoError(t, executeDaemonCommand(t, *deps, out, "status"))
	assert.Contains(t, out.String(), "not responding")
	assert.Contains(t, out.String(), "pid:     55")
	assert.Contains(t, out.String(), "/runtime/55.json")
}

func TestDaemonStatusUsesPingVersionAsAuthoritative(t *testing.T) {
	endpoint := newPingDaemon(t)
	for _, tt := range []struct {
		name          string
		recordVersion string
	}{
		{name: "missing runtime version"},
		{name: "stale runtime version", recordVersion: "stale-record-version"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := testWritableRecord(os.Getpid(), "")
			rec.Version = tt.recordVersion
			rec.Address = endpoint.Addr
			rec.Metadata[runtimeHost] = endpoint.Host
			rec.Metadata[runtimePort] = strconv.Itoa(endpoint.Port)

			deps, out := daemonCommandTestDeps(t)
			deps.statusRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
				return []daemon.RuntimeRecord{rec}, nil
			}
			deps.probeRecord = probeDaemonRecord

			require.NoError(t, executeDaemonCommand(t, *deps, out, "status"))
			assert.Contains(t, out.String(), "version: test")
			assert.NotContains(t, out.String(), "stale-record-version")
		})
	}
}

func TestDaemonStatusDiscoversAuthenticatedLegacyDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	const token = "status-secret"
	endpoint, legacyPath := writeAuthenticatedProbeableLegacyRuntime(
		t, dir, token, legacyStateFile{Version: version},
	)
	deps, out := daemonCommandTestDeps(t)
	deps.loadReadOnlyConfig = func() (config.Config, error) {
		return config.Config{DataDir: dir, AuthToken: token}, nil
	}
	deps.statusRecords = daemonStatusRecords

	require.NoError(t, executeDaemonCommand(t, *deps, out, "status"))
	assert.Contains(t, out.String(), "running at")
	assert.Contains(t, out.String(), fmt.Sprintf("pid:     %d", os.Getpid()))
	assert.Contains(t, out.String(), fmt.Sprintf(":%d", endpoint.Port))
	assertPathRemoved(t, legacyPath, "authenticated legacy state should migrate")
}

func TestDaemonStopDiscoversAuthenticatedLegacyDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	const token = "stop-secret"
	_, legacyPath := writeAuthenticatedProbeableLegacyRuntime(
		t, dir, token, legacyStateFile{Version: version},
	)
	deps, out := daemonCommandTestDeps(t)
	deps.resolveDataDir = func() (string, error) { return dir, nil }
	deps.mkdirAll = func(path string, _ os.FileMode) error {
		assert.Equal(t, dir, path)
		return nil
	}
	deps.loadConfig = func() (config.Config, error) {
		return config.Config{DataDir: dir, AuthToken: token}, nil
	}
	deps.writableRecords = writableDaemonRecords
	var stopped []int
	deps.stopProcess = func(rec daemon.RuntimeRecord, _ time.Duration) error {
		stopped = append(stopped, rec.PID)
		return nil
	}

	require.NoError(t, executeDaemonCommand(t, *deps, out, "stop"))
	assert.Equal(t, []int{os.Getpid()}, stopped)
	assert.Contains(t, out.String(), fmt.Sprintf("pid %d", os.Getpid()))
	assertPathRemoved(t, legacyPath, "authenticated legacy state should migrate")
}

func TestDaemonStopPrevalidatesEveryWriterBeforeSignalling(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	records := []daemon.RuntimeRecord{
		testWritableRecord(61, "/runtime/61.json"),
		testWritableRecord(62, "/runtime/62.json"),
	}
	deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) { return records, nil }
	var prevalidated []int
	deps.stopTargetConfirmed = func(rec daemon.RuntimeRecord, _ string) bool {
		prevalidated = append(prevalidated, rec.PID)
		return rec.PID != 61
	}
	var signalled []int
	deps.stopProcess = func(rec daemon.RuntimeRecord, _ time.Duration) error {
		signalled = append(signalled, rec.PID)
		return nil
	}

	err := executeDaemonCommand(t, *deps, out, "stop")
	require.Error(t, err)
	assert.ErrorContains(t, err, "pid 61")
	assert.ErrorContains(t, err, "/runtime/61.json")
	assert.ErrorContains(t, err, "verify")
	assert.Equal(t, []int{61, 62}, prevalidated, "every target must be checked before stop can return")
	assert.Empty(t, signalled)
}

func TestDaemonStopStartingConfigFailureAndContentionSignalNobody(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*daemonCommandDeps)
		want  string
	}{
		{name: "launch contention", setup: func(d *daemonCommandDeps) {
			d.acquireLaunchLockWithError = func(string) (daemonLaunchLock, bool, error) { return nil, false, nil }
		}, want: "launch lock"},
		{name: "config failure", setup: func(d *daemonCommandDeps) {
			d.loadConfig = func() (config.Config, error) { return config.Config{}, errors.New("bad config") }
		}, want: "bad config"},
		{name: "start lock with confirmed record", setup: func(d *daemonCommandDeps) {
			d.isStarting = func(string) bool { return true }
			d.readStartupState = func(string) *startupState { return &startupState{PID: 71, LogPath: "/tmp/serve.log"} }
			d.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
				return []daemon.RuntimeRecord{testWritableRecord(72, "/runtime/72.json")}, nil
			}
		}, want: "startup is still in progress"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, out := daemonCommandTestDeps(t)
			tt.setup(deps)
			signals := 0
			deps.stopProcess = func(daemon.RuntimeRecord, time.Duration) error { signals++; return nil }
			err := executeDaemonCommand(t, *deps, out, "stop")
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
			assert.Zero(t, signals)
		})
	}
}

func TestDaemonStopMultipleWritersAndRepeatIsIdempotent(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	records := []daemon.RuntimeRecord{
		testWritableRecord(81, "/runtime/81.json"),
		testWritableRecord(82, "/runtime/82.json"),
	}
	deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) { return records, nil }
	var stopped, caddy []int
	deps.stopProcess = func(rec daemon.RuntimeRecord, _ time.Duration) error { stopped = append(stopped, rec.PID); return nil }
	deps.stopCaddy = func(_ io.Writer, rec daemon.RuntimeRecord) error {
		caddy = append(caddy, rec.PID)
		return nil
	}
	require.NoError(t, executeDaemonCommand(t, *deps, out, "stop"))
	assert.Equal(t, []int{81, 82}, stopped)
	assert.Equal(t, []int{81, 82}, caddy)
	assert.Contains(t, out.String(), "pid 81")
	assert.Contains(t, out.String(), "pid 82")

	records = nil
	out.Reset()
	require.NoError(t, executeDaemonCommand(t, *deps, out, "stop"))
	assert.Contains(t, out.String(), "not running")
}

func TestDaemonRestartValidatesBeforeStoppingAndUsesFreshConfig(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	dataDir := t.TempDir()
	deps.resolveDataDir = func() (string, error) { return dataDir, nil }
	deps.mkdirAll = func(path string, _ os.FileMode) error {
		assert.Equal(t, dataDir, path)
		return nil
	}
	rec := testWritableRecord(91, "/runtime/91.json")
	rec.Metadata[runtimeNoSync] = "true"
	deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) { return []daemon.RuntimeRecord{rec}, nil }
	var order []string
	deps.loadConfig = func() (config.Config, error) {
		order = append(order, "config")
		return config.Config{DataDir: dataDir, DBPath: "/fresh/sessions.db", NoSync: true}, nil
	}
	deps.checkDataVersion = func(path string) error {
		order = append(order, "data")
		assert.Equal(t, "/fresh/sessions.db", path)
		return nil
	}
	deps.stopProcess = func(daemon.RuntimeRecord, time.Duration) error { order = append(order, "stop"); return nil }
	deps.startBackground = func(cfg config.Config, args []string, _ serveReplacementOptions, policy backgroundLaunchPolicy) (backgroundLaunchResult, error) {
		order = append(order, "start")
		assert.True(t, cfg.NoSync, "loader result reaches config-only lower layer, which clears runtime NoSync")
		assert.Equal(t, []string{"serve"}, args)
		assert.True(t, policy.ConfigOnly)
		assert.Equal(t, "daemon restart", policy.Operation)
		return backgroundLaunchResult{Runtime: &DaemonRuntime{Record: daemon.RuntimeRecord{PID: 92}, Host: "127.0.0.1", Port: 8080}, Started: true, LogPath: "/fresh/serve.log"}, nil
	}

	require.NoError(t, executeDaemonCommand(t, *deps, out, "restart"))
	assert.Equal(t, []string{"config", "data", "stop", "start"}, order)
	assert.Contains(t, out.String(), "restarted")
}

func TestDaemonRestartStoppedStartsAndReadOnlySurvives(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	dir := runtimeTestDir(t)
	rec := testWritableRecord(os.Getpid(), "")
	rec.Metadata[runtimeReadOnly] = "true"
	path, err := writeRuntimeRecordForTest(dir, rec)
	require.NoError(t, err)
	deps.resolveDataDir = func() (string, error) { return dir, nil }
	deps.mkdirAll = func(path string, mode os.FileMode) error {
		assert.Equal(t, dir, path)
		return nil
	}
	deps.loadConfig = func() (config.Config, error) {
		return config.Config{DataDir: dir, DBPath: filepath.Join(dir, "sessions.db")}, nil
	}
	deps.writableRecords = writableDaemonRecords
	stopCalls := 0
	deps.stopProcess = func(daemon.RuntimeRecord, time.Duration) error { stopCalls++; return nil }
	require.NoError(t, executeDaemonCommand(t, *deps, out, "restart"))
	assert.Zero(t, stopCalls)
	assert.FileExists(t, path, "restart must preserve the read-only runtime")
	assert.Contains(t, out.String(), "started (was not running)")
}

func TestServeRestartDelegatesToCanonicalWriterOnlyRestart(t *testing.T) {
	requirePOSIXSignals(t, "requires POSIX sleep/process signals")
	deps, out := daemonCommandTestDeps(t)
	dir := runtimeTestDir(t)
	pid, _ := startReapedSleepProcess(t)
	createTime, ok := processCreateTimeMillis(pid)
	require.True(t, ok, "read-only child create time must be available")
	rec := testWritableRecord(pid, "")
	rec.Metadata[runtimeReadOnly] = "true"
	rec.Metadata[runtimeCreateTime] = strconv.FormatInt(createTime, 10)
	path, err := writeRuntimeRecordForTest(dir, rec)
	require.NoError(t, err)
	deps.resolveDataDir = func() (string, error) { return dir, nil }
	deps.mkdirAll = func(path string, _ os.FileMode) error {
		assert.Equal(t, dir, path)
		return nil
	}
	deps.loadConfig = func() (config.Config, error) {
		return config.Config{DataDir: dir, DBPath: filepath.Join(dir, "sessions.db")}, nil
	}
	deps.writableRecords = writableDaemonRecords
	stopCalls := 0
	deps.stopProcess = func(daemon.RuntimeRecord, time.Duration) error {
		stopCalls++
		return nil
	}
	var gotPolicy backgroundLaunchPolicy
	deps.startBackground = func(
		_ config.Config, args []string, _ serveReplacementOptions,
		policy backgroundLaunchPolicy,
	) (backgroundLaunchResult, error) {
		assert.Equal(t, []string{"serve"}, args)
		gotPolicy = policy
		return backgroundLaunchResult{
			Runtime: &DaemonRuntime{
				Record: daemon.RuntimeRecord{PID: 315},
				Host:   "127.0.0.1", Port: 8080,
			},
			Started: true,
		}, nil
	}

	require.NoError(t, executeServeCommand(t, *deps, out, "restart"))
	assert.Zero(t, stopCalls, "read-only servers must not be stopped")
	assert.True(t, daemon.ProcessAlive(pid), "read-only server must remain alive")
	assert.FileExists(t, path, "read-only runtime record must survive restart")
	assert.True(t, gotPolicy.ConfigOnly)
	assert.Equal(t, "daemon restart", gotPolicy.Operation)
	assert.Contains(t, out.String(), "started (was not running)")
}

func TestServeRestartRejectsServeFlagsAndArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "host", args: []string{"restart", "--host", "0.0.0.0"}},
		{name: "no sync", args: []string{"restart", "--no-sync"}},
		{name: "host before subcommand", args: []string{"--host", "0.0.0.0", "restart"}},
		{name: "no sync before subcommand", args: []string{"--no-sync", "restart"}},
		{name: "argument", args: []string{"restart", "extra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, out := daemonCommandTestDeps(t)
			starts := 0
			deps.startBackground = func(
				config.Config, []string, serveReplacementOptions,
				backgroundLaunchPolicy,
			) (backgroundLaunchResult, error) {
				starts++
				return backgroundLaunchResult{}, nil
			}

			err := executeServeCommand(t, *deps, out, tt.args...)
			require.Error(t, err)
			assert.Zero(t, starts)
		})
	}
}

func TestDaemonRestartFailurePathsSignalNobody(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*daemonCommandDeps)
		want  string
	}{
		{name: "data version", setup: func(d *daemonCommandDeps) {
			d.checkDataVersion = func(string) error { return errors.New("data too new") }
		}, want: "data too new"},
		{name: "persistent start", setup: func(d *daemonCommandDeps) {
			d.isStarting = func(string) bool { return true }
			d.readStartupState = func(string) *startupState { return &startupState{PID: 111, LogPath: "/tmp/log"} }
		}, want: "startup is still in progress"},
		{name: "unconfirmed", setup: func(d *daemonCommandDeps) {
			d.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
				return []daemon.RuntimeRecord{testWritableRecord(112, "/runtime/112.json")}, nil
			}
			d.stopTargetConfirmed = func(daemon.RuntimeRecord, string) bool { return false }
		}, want: "pid 112"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, out := daemonCommandTestDeps(t)
			tt.setup(deps)
			signals := 0
			deps.stopProcess = func(daemon.RuntimeRecord, time.Duration) error { signals++; return nil }
			err := executeDaemonCommand(t, *deps, out, "restart")
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
			assert.Zero(t, signals)
		})
	}
}

func TestDaemonRestartUsesStartupStateFallbackWhileStarting(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.isStarting = func(string) bool { return true }
	deps.writableRuntime = func(string, string) *DaemonRuntime {
		return &DaemonRuntime{
			Record:          testWritableRecord(113, ""),
			Host:            "127.0.0.1",
			Port:            8113,
			RuntimeFallback: true,
		}
	}
	var stopped daemon.RuntimeRecord
	deps.stopProcess = func(rec daemon.RuntimeRecord, _ time.Duration) error {
		stopped = rec
		return nil
	}
	deps.startBackground = func(
		_ config.Config, args []string, _ serveReplacementOptions,
		policy backgroundLaunchPolicy,
	) (backgroundLaunchResult, error) {
		assert.Equal(t, []string{"serve"}, args)
		assert.Equal(t, "daemon restart", policy.Operation)
		return backgroundLaunchResult{
			Runtime: &DaemonRuntime{
				Record: daemon.RuntimeRecord{PID: 315},
				Host:   "127.0.0.1", Port: 8080,
			},
			Started: true,
		}, nil
	}

	require.NoError(t, executeDaemonCommand(t, *deps, out, "restart"))
	assert.Equal(t, 113, stopped.PID)
	assert.Contains(t, out.String(), "restarted")
	assert.NotContains(t, out.String(), "startup is still in progress")
}

func TestDaemonRestartUsesStartupStateFallbackWhenRuntimeStoreInspectionFails(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
		return nil, errors.New("store unavailable")
	}
	deps.writableRuntime = func(string, string) *DaemonRuntime {
		return &DaemonRuntime{Record: testWritableRecord(114, ""), RuntimeFallback: true}
	}
	var stopped daemon.RuntimeRecord
	deps.stopProcess = func(rec daemon.RuntimeRecord, _ time.Duration) error {
		stopped = rec
		return nil
	}

	require.NoError(t, executeDaemonCommand(t, *deps, out, "restart"))
	assert.Equal(t, 114, stopped.PID)
	assert.Contains(t, out.String(), "restarted")
}

func TestDaemonMutationsPreserveRuntimeStoreInspectionErrorWithoutFallback(t *testing.T) {
	for _, command := range []string{"stop", "restart"} {
		t.Run(command, func(t *testing.T) {
			deps, out := daemonCommandTestDeps(t)
			deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
				return nil, errors.New("store unavailable")
			}

			err := executeDaemonCommand(t, *deps, out, command)
			require.Error(t, err)
			assert.ErrorContains(t, err, "store unavailable")
		})
	}
}

func TestDaemonRestartInvalidServeConfigDoesNotStopOrStart(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Config
		wantErr string
	}{
		{
			name:    "non-loopback host without auth",
			cfg:     config.Config{Host: "0.0.0.0", Port: 8080},
			wantErr: "require_auth",
		},
		{
			name:    "unsupported proxy",
			cfg:     config.Config{Host: "127.0.0.1", Port: 8080, Proxy: config.ProxyConfig{Mode: "nginx"}},
			wantErr: "unsupported proxy mode",
		},
		{
			name: "https proxy without TLS files",
			cfg: config.Config{
				Host: "127.0.0.1", Port: 8080,
				PublicURL: "https://viewer.example.test",
				Proxy:     config.ProxyConfig{Mode: "caddy", Bin: os.Args[0]},
			},
			wantErr: "requires both tls_cert and tls_key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, out := daemonCommandTestDeps(t)
			tt.cfg.DataDir = t.TempDir()
			tt.cfg.DBPath = filepath.Join(tt.cfg.DataDir, "sessions.db")
			deps.resolveDataDir = func() (string, error) { return tt.cfg.DataDir, nil }
			deps.mkdirAll = func(path string, _ os.FileMode) error {
				assert.Equal(t, tt.cfg.DataDir, path)
				return nil
			}
			deps.loadConfig = func() (config.Config, error) { return tt.cfg, nil }
			deps.validateConfig = validateServeConfig
			deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
				return []daemon.RuntimeRecord{testWritableRecord(121, "/runtime/121.json")}, nil
			}
			dataChecks, signals, starts := 0, 0, 0
			deps.checkDataVersion = func(string) error { dataChecks++; return nil }
			deps.stopProcess = func(daemon.RuntimeRecord, time.Duration) error { signals++; return nil }
			deps.startBackground = func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error) {
				starts++
				return backgroundLaunchResult{}, nil
			}

			err := executeDaemonCommand(t, *deps, out, "restart")
			require.Error(t, err)
			assert.ErrorContains(t, err, "daemon restart: invalid config")
			assert.ErrorContains(t, err, tt.wantErr)
			assert.Zero(t, dataChecks)
			assert.Zero(t, signals)
			assert.Zero(t, starts)
		})
	}
}

func TestDaemonRestartChildFailureIncludesLogAndHeldLock(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	lock := &testDaemonLaunchLock{}
	lockAcquisitions := 0
	deps.acquireLaunchLockWithError = func(string) (daemonLaunchLock, bool, error) {
		lockAcquisitions++
		return lock, true, nil
	}
	deps.writableRecords = func(string, string) ([]daemon.RuntimeRecord, error) {
		return []daemon.RuntimeRecord{testWritableRecord(131, "/runtime/131.json")}, nil
	}
	var order []string
	deps.stopProcess = func(rec daemon.RuntimeRecord, _ time.Duration) error {
		assert.Equal(t, 131, rec.PID)
		assert.False(t, lock.unlocked, "restart must hold the launch lock while stopping")
		order = append(order, "stop")
		return nil
	}
	deps.startBackground = func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error) {
		assert.False(t, lock.unlocked, "restart must retain launch lock across stop/start gap")
		order = append(order, "start")
		return backgroundLaunchResult{LogPath: "/tmp/serve.log"}, errors.New("child exited")
	}
	err := executeDaemonCommand(t, *deps, out, "restart")
	require.Error(t, err)
	assert.ErrorContains(t, err, "child exited")
	assert.ErrorContains(t, err, "/tmp/serve.log")
	assert.Equal(t, []string{"stop", "start"}, order)
	assert.Equal(t, 1, lockAcquisitions)
	assert.True(t, lock.unlocked)
}

func TestDaemonStartConfigFailureReturnsError(t *testing.T) {
	deps, out := daemonCommandTestDeps(t)
	deps.loadConfig = func() (config.Config, error) { return config.Config{}, errors.New("config invalid") }
	err := executeDaemonCommand(t, *deps, out, "start")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "config invalid"))
}
