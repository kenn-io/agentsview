package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/daemon"
)

func TestServeBackgroundChildArgsRemovesBackgroundFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "bare flag",
			args: []string{"serve", "--background", "--port", "0"},
			want: []string{"serve", "--port", "0"},
		},
		{
			name: "equals form",
			args: []string{"serve", "--background=true", "--host", "0.0.0.0"},
			want: []string{"serve", "--host", "0.0.0.0"},
		},
		{
			// The legacy normalizer rewrites -background to --background
			// before Cobra parses, so the raw child args still carry the
			// single-dash form. It must be stripped too, or the child
			// re-backgrounds itself in an unbounded loop.
			name: "legacy single-dash flag",
			args: []string{"serve", "-background", "--port", "0"},
			want: []string{"serve", "--port", "0"},
		},
		{
			name: "legacy single-dash equals form",
			args: []string{"serve", "-background=true", "--port", "0"},
			want: []string{"serve", "--port", "0"},
		},
		{
			name: "keeps similarly named flags",
			args: []string{
				"serve",
				"--public-url",
				"https://viewer.example.test/background",
			},
			want: []string{
				"serve",
				"--public-url",
				"https://viewer.example.test/background",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, serveBackgroundChildArgs(tt.args))
		})
	}
}

func TestServeBackgroundChildArgsRemovesReplaceFlag(t *testing.T) {
	got := serveBackgroundChildArgs([]string{
		"serve", "--background", "--replace", "--port", "0",
	})
	assert.Equal(t, []string{"serve", "--port", "0"}, got)

	got = serveBackgroundChildArgs([]string{
		"serve", "-background=true", "-replace=true", "--host", "127.0.0.1",
	})
	assert.Equal(t, []string{"serve", "--host", "127.0.0.1"}, got)
}

func TestRunServeBackgroundReplaceOverridesDevRefusal(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "dev")

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = true
		assert.Equal(t, "1.0.0", rt.Record.Version)
		RemoveDaemonRuntime(dir)
		return nil
	})

	newHost, newPort := testPingServer(t)
	oldStart := startServeBackgroundProcessForRun
	startServeBackgroundProcessForRun = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		assert.NotContains(t, arguments, "--replace")
		_, err := WriteDaemonRuntime(dir, newHost, newPort, "dev", false)
		require.NoError(t, err)
		cmd := exec.Command("sleep", "2")
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForRun = oldStart
		RemoveDaemonRuntime(dir)
	})

	runServeBackground(
		config.Config{DataDir: dir},
		[]string{"serve", "--background", "--replace"},
		serveReplacementOptions{Replace: true},
	)

	assert.True(t, stopped)
}

func TestRunServeBackgroundGeneratesAuthTokenForRemoteSync(t *testing.T) {
	dir := testDataDir(t)
	host, port := testPingServer(t)
	var gotCfg config.Config

	oldStart := startServeBackgroundProcessForRun
	startServeBackgroundProcessForRun = func(
		cfg config.Config, _ []string,
	) (*exec.Cmd, string, error) {
		gotCfg = cfg
		_, err := WriteDaemonRuntime(dir, host, port, version, false)
		require.NoError(t, err)
		cmd := exec.Command("sleep", "2")
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForRun = oldStart
		RemoveDaemonRuntime(dir)
	})

	runServeBackground(
		config.Config{DataDir: dir},
		[]string{"serve", "--background"},
		serveReplacementOptions{},
	)

	require.NotEmpty(t, gotCfg.AuthToken)
	assert.False(t, gotCfg.RequireAuth)
	data, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(data), `auth_token = "`)
}

func TestRunServeBackgroundReplaceWaitsForExternalStartLock(t *testing.T) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := runtimeTestDir(t)
	oldEndpoint, oldProbed := newPingDaemonWithProbeSignal(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		oldEndpoint.Host, oldEndpoint.Port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "dev")
	unlockStart := holdExternalDaemonStartLock(t, dir)

	forbidStopDaemonRuntimeForUpgrade(t,
		"background replacement must not stop while foreground owns start lock")
	oldStart := startServeBackgroundProcessForRun
	startServeBackgroundProcessForRun = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		t.Fatal("background replacement must not spawn while waiting on foreground")
		return nil, "", nil
	}
	t.Cleanup(func() { startServeBackgroundProcessForRun = oldStart })

	newHost, newPort := testPingServer(t)
	published := make(chan error, 1)
	go func() {
		select {
		case <-oldProbed:
		case <-time.After(2 * time.Second):
			published <- fmt.Errorf("old daemon was not probed")
			return
		}
		published <- publishDaemonRuntimeAndUnlockWhenVisible(
			dir, newHost, newPort, "dev", unlockStart,
		)
	}()

	out := captureStdout(t, func() {
		runServeBackground(
			config.Config{DataDir: dir},
			[]string{"serve", "--background", "--replace"},
			serveReplacementOptions{Replace: true},
		)
	})

	require.NoError(t, <-published)
	assert.Contains(t, out, "agentsview already running at")
	assert.Contains(t, out, fmt.Sprintf(":%d", newPort))
}

func publishDaemonRuntimeAndUnlockWhenVisible(
	dataDir, host string, port int, version string, unlock func(),
) error {
	err := publishDaemonRuntimeWhenVisible(dataDir, host, port, version)
	unlock()
	return err
}

func publishDaemonRuntimeWhenVisible(
	dataDir, host string, port int, version string,
) error {
	RemoveDaemonRuntime(dataDir)
	_, err := WriteDaemonRuntime(dataDir, host, port, version, false)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rt := FindDaemonRuntime(dataDir); rt != nil &&
			!rt.ReadOnly && rt.Port == port {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf(
		"published daemon runtime %s:%d was not visible",
		host, port,
	)
}

func TestRunServeBackgroundReplaceContinuesAfterExternalStartupAbort(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := runtimeTestDir(t)
	oldHost, oldPort := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		oldHost, oldPort, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "dev")
	unlockStart := holdExternalDaemonStartLock(t, dir)

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = true
		assert.Equal(t, "1.0.0", rt.Record.Version)
		RemoveDaemonRuntime(dir)
		return nil
	})

	newHost, newPort := testPingServer(t)
	oldStart := startServeBackgroundProcessForRun
	startServeBackgroundProcessForRun = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		assert.NotContains(t, arguments, "--replace")
		_, err := WriteDaemonRuntime(dir, newHost, newPort, "dev", false)
		require.NoError(t, err)
		cmd := exec.Command("sleep", "2")
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForRun = oldStart
		RemoveDaemonRuntime(dir)
	})

	released := make(chan struct{})
	go func() {
		time.Sleep(2 * startProbeTick())
		unlockStart()
		close(released)
	}()

	runServeBackground(
		config.Config{DataDir: dir},
		[]string{"serve", "--background", "--replace"},
		serveReplacementOptions{Replace: true},
	)

	<-released
	assert.True(t, stopped)
}

func TestRunServeBackgroundReplaceKeepsSameVersionTargetAfterStartupAbort(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := runtimeTestDir(t)
	oldHost, oldPort := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		oldHost, oldPort, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "1.0.0")
	unlockStart := holdExternalDaemonStartLock(t, dir)

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = true
		assert.Equal(t, oldPort, rt.Port)
		RemoveDaemonRuntime(dir)
		return nil
	})

	newHost, newPort := testPingServer(t)
	oldStart := startServeBackgroundProcessForRun
	startServeBackgroundProcessForRun = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		assert.NotContains(t, arguments, "--replace")
		_, err := WriteDaemonRuntime(dir, newHost, newPort, "1.0.0", false)
		require.NoError(t, err)
		cmd := exec.Command("sleep", "2")
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForRun = oldStart
		RemoveDaemonRuntime(dir)
	})

	released := make(chan struct{})
	go func() {
		time.Sleep(2 * startProbeTick())
		unlockStart()
		close(released)
	}()

	runServeBackground(
		config.Config{DataDir: dir},
		[]string{"serve", "--background", "--replace"},
		serveReplacementOptions{Replace: true},
	)

	<-released
	assert.True(t, stopped)
}

func TestRunServeBackgroundReplaceKeepsUnresponsiveTargetAfterStartupAbort(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := runtimeTestDir(t)
	ln, oldPort := freeTCPListener(t)
	require.NoError(t, ln.Close())
	_, err := WriteDaemonRuntime(dir, "127.0.0.1", oldPort, "1.0.0", false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })
	require.Nil(t, FindDaemonRuntime(dir),
		"precondition: runtime record must be live but unprobeable")
	setTestVersion(t, "1.0.0")
	unlockStart := holdExternalDaemonStartLock(t, dir)

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = true
		assert.Equal(t, oldPort, rt.Port)
		assert.True(t, stopTargetConfirmed(rt.Record, ""))
		RemoveDaemonRuntime(dir)
		return nil
	})

	newHost, newPort := testPingServer(t)
	oldStart := startServeBackgroundProcessForRun
	startServeBackgroundProcessForRun = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		assert.NotContains(t, arguments, "--replace")
		_, err := WriteDaemonRuntime(dir, newHost, newPort, "1.0.0", false)
		require.NoError(t, err)
		cmd := exec.Command("sleep", "2")
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForRun = oldStart
		RemoveDaemonRuntime(dir)
	})

	released := make(chan struct{})
	go func() {
		time.Sleep(2 * startProbeTick())
		unlockStart()
		close(released)
	}()

	runServeBackground(
		config.Config{DataDir: dir},
		[]string{"serve", "--background", "--replace"},
		serveReplacementOptions{Replace: true},
	)

	<-released
	assert.True(t, stopped)
}

func TestRunServeBackgroundRejectsTooNewDatabaseBeforeStop(t *testing.T) {
	dir := runtimeTestDir(t)
	dbPath := writeTooNewSQLiteDB(t, dir)

	stopMarker := filepath.Join(dir, "stop-called")
	cmd := exec.Command(
		os.Args[0],
		"-test.run=TestRunServeBackgroundRejectsTooNewDatabaseBeforeStopHelper",
		"--",
	)
	cmd.Env = append(
		os.Environ(),
		"AGENTSVIEW_BACKGROUND_TOO_NEW_HELPER=1",
		"AGENTSVIEW_BACKGROUND_TOO_NEW_DIR="+dir,
		"AGENTSVIEW_BACKGROUND_TOO_NEW_DB="+dbPath,
		"AGENTSVIEW_BACKGROUND_TOO_NEW_MARKER="+stopMarker,
	)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "helper should fatal on too-new database\n%s", out)
	assert.Contains(t, string(out), "database data version")
	assert.NoFileExists(t, stopMarker,
		"too-new database must be rejected before stop")
	runtimeFiles, err := filepath.Glob(filepath.Join(dir, "daemon.*.json"))
	require.NoError(t, err)
	assert.Len(t, runtimeFiles, 1,
		"old daemon runtime should remain when preflight fails")
}

func TestStartServeBackgroundRejectsMultipleWritableDaemons(t *testing.T) {
	requirePOSIXSignals(t, "requires long-lived child processes")
	dir := runtimeTestDir(t)
	setTestVersion(t, "1.0.0")
	host, port := testPingServer(t)
	pids := []int{os.Getpid(), startSleepProcess(t)}
	_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		host, port, withRuntimePID(pids[0]), withRuntimeVersion("1.0.0"),
	))
	require.NoError(t, err)
	_, err = writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		"127.0.0.1", 9101, withRuntimePID(pids[1]),
	))
	require.NoError(t, err)

	result, err := startServeBackground(
		config.Config{DataDir: dir},
		[]string{"serve", "--background"},
		serveReplacementOptions{},
		backgroundLaunchPolicy{ConfigOnly: true, Operation: "daemon start"},
	)

	require.Error(t, err)
	assert.ErrorContains(t, err, "multiple writable agentsview daemons")
	assert.ErrorContains(t, err, strconv.Itoa(pids[0]))
	assert.ErrorContains(t, err, strconv.Itoa(pids[1]))
	assert.False(t, result.Started)
	assert.Nil(t, result.Runtime)
}

func TestStartServeBackgroundCountsAuthenticatedLegacyWriterForUniqueness(
	t *testing.T,
) {
	requirePOSIXSignals(t, "requires a long-lived child process")
	dir := runtimeTestDir(t)
	setTestVersion(t, "1.0.0")
	const token = "uniqueness-secret"
	_, legacyPath := writeAuthenticatedProbeableLegacyRuntime(
		t, dir, token, legacyStateFile{Version: "1.0.0"},
	)
	secondPID := startSleepProcess(t)
	_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		"127.0.0.1", 9102, withRuntimePID(secondPID),
	))
	require.NoError(t, err)

	result, err := startServeBackground(
		config.Config{DataDir: dir, AuthToken: token},
		[]string{"serve", "--background"},
		serveReplacementOptions{},
		backgroundLaunchPolicy{ConfigOnly: true, Operation: "daemon start"},
	)

	require.Error(t, err)
	assert.ErrorContains(t, err, "multiple writable agentsview daemons")
	assert.ErrorContains(t, err, strconv.Itoa(os.Getpid()))
	assert.ErrorContains(t, err, strconv.Itoa(secondPID))
	assert.False(t, result.Started)
	assert.Nil(t, result.Runtime)
	assertPathRemoved(t, legacyPath, "authenticated legacy state should migrate")
}

func TestStartServeBackgroundValidatesConfigBeforeReplacementStop(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", false)
	require.NoError(t, err)
	setTestVersion(t, "1.1.0")

	stopCalls := 0
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, _ *DaemonRuntime,
	) error {
		stopCalls++
		RemoveDaemonRuntime(dir)
		return nil
	})
	startCalls := 0
	oldStart := startServeBackgroundProcessForRun
	startServeBackgroundProcessForRun = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		startCalls++
		return nil, "test.log", errors.New("replacement child started")
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForRun = oldStart
		RemoveDaemonRuntime(dir)
	})

	result, err := startServeBackground(
		config.Config{DataDir: dir, Host: "0.0.0.0"},
		[]string{"serve", "--background"},
		serveReplacementOptions{},
		backgroundLaunchPolicy{ConfigOnly: true, Operation: "daemon start"},
	)

	require.Error(t, err)
	assert.ErrorContains(t, err, "require_auth")
	assert.Zero(t, stopCalls, "invalid config must preserve the incumbent")
	assert.Zero(t, startCalls, "invalid config must not launch a child")
	assert.False(t, result.Started)
	require.NotNil(t, FindDaemonRuntime(dir),
		"incumbent runtime must remain discoverable")
}

func TestRunServeBackgroundRejectsTooNewDatabaseBeforeStopHelper(t *testing.T) {
	if os.Getenv("AGENTSVIEW_BACKGROUND_TOO_NEW_HELPER") != "1" {
		return
	}

	dir := os.Getenv("AGENTSVIEW_BACKGROUND_TOO_NEW_DIR")
	dbPath := os.Getenv("AGENTSVIEW_BACKGROUND_TOO_NEW_DB")
	stopMarker := os.Getenv("AGENTSVIEW_BACKGROUND_TOO_NEW_MARKER")
	require.NotEmpty(t, dir)
	require.NotEmpty(t, dbPath)
	require.NotEmpty(t, stopMarker)

	host, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", false)
	require.NoError(t, err)

	stopDaemonRuntimeForUpgrade = func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		require.NotNil(t, rt)
		require.NoError(t, os.WriteFile(stopMarker, []byte("stop"), 0o600))
		if rt.Record.SourcePath != "" {
			_ = os.Remove(rt.Record.SourcePath)
		}
		return nil
	}
	startServeBackgroundProcessForRun = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		return nil, "", fmt.Errorf("start should not run")
	}

	runServeBackground(
		config.Config{DataDir: dir, DBPath: dbPath},
		[]string{"serve", "--background", "--replace"},
		serveReplacementOptions{Replace: true},
	)
}

func TestServeBackgroundReplaceCommandUsesParentReplacementUnderLaunchLock(
	t *testing.T,
) {
	dir := testDataDir(t)
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", false)
	require.NoError(t, err)
	setTestVersion(t, "1.0.0")

	oldArgs := os.Args
	os.Args = []string{
		"agentsview", "serve", "--background", "--replace", "--port", "0",
	}
	t.Cleanup(func() { os.Args = oldArgs })

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = true
		require.NotNil(t, rt)
		lock, ok := acquireBackgroundLaunchLock(dir)
		if ok {
			_ = lock.Unlock()
		}
		assert.False(t, ok,
			"background replacement stop must run under launch lock")
		RemoveDaemonRuntime(dir)
		return nil
	})

	newHost, newPort := testPingServer(t)
	oldStart := startServeBackgroundProcessForRun
	var gotArgs []string
	startServeBackgroundProcessForRun = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		gotArgs = append([]string(nil), arguments...)
		assert.NotContains(t, arguments, "--replace")
		assert.NotContains(t, arguments, "--background")
		_, err := WriteDaemonRuntime(dir, newHost, newPort, "1.0.0", false)
		require.NoError(t, err)
		cmd := exec.Command("sleep", "2")
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForRun = oldStart
		RemoveDaemonRuntime(dir)
	})

	_, err = executeCommand(
		newRootCommand(), "serve", "--background", "--replace", "--port", "0",
	)

	require.NoError(t, err)
	assert.True(t, stopped)
	assert.Equal(t, []string{"serve", "--port", "0"}, gotArgs)
}

func TestServeCommandParsesBackgroundFlag(t *testing.T) {
	dataDir := testDataDir(t)

	cmd := newServeCommand()
	require.NoError(t,
		cmd.Flags().Parse([]string{"--background", "--port", "9090"}),
	)
	got, err := cmd.Flags().GetBool("background")
	require.NoError(t, err)
	assert.True(t, got)

	cfg := mustLoadConfig(cmd)
	assert.Equal(t, 9090, cfg.Port)
	assert.Equal(t, filepath.Join(dataDir, "sessions.db"), cfg.DBPath)
}

func TestServeCommandParsesHiddenSkipInitialSyncFlag(t *testing.T) {
	cmd := newServeCommand()
	require.NoError(t, cmd.Flags().Parse([]string{"--skip-initial-sync"}))
	got, err := cmd.Flags().GetBool("skip-initial-sync")
	require.NoError(t, err)
	assert.True(t, got)
	assert.True(t, cmd.Flags().Lookup("skip-initial-sync").Hidden)
}

func TestServeBackgroundArgsWithNoSyncKeepsExplicitFalse(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "long false",
			args: []string{"serve", "--no-sync=false"},
		},
		{
			name: "legacy false",
			args: []string{"serve", "-no-sync=false"},
		},
		{
			name: "numeric false",
			args: []string{"serve", "--no-sync=0"},
		},
		{
			name: "short false",
			args: []string{"serve", "--no-sync=f"},
		},
		{
			name: "upper short false",
			args: []string{"serve", "--no-sync=F"},
		},
		{
			name: "upper false",
			args: []string{"serve", "--no-sync=FALSE"},
		},
		{
			name: "title false",
			args: []string{"serve", "--no-sync=False"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.args, serveBackgroundArgsWithNoSync(tt.args, true))
		})
	}
}

func TestRunningAsBackgroundChild(t *testing.T) {
	assert.False(t, runningAsBackgroundChild())
	t.Setenv(backgroundChildEnvVar, "1")
	assert.True(t, runningAsBackgroundChild())
}

func TestEnsureBackgroundServeExistingDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, host, port, "test", false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(
		context.Background(), &cfg, 100*time.Millisecond,
	)
	require.NoError(t, err)
	require.NotNil(t, rt)
	assert.Equal(t, host, rt.Host)
	assert.Equal(t, port, rt.Port)
}

func TestEnsureBackgroundServeGeneratesAuthTokenForRemoteSync(t *testing.T) {
	dir := testDataDir(t)
	host, port := testPingServer(t)
	var gotCfg config.Config

	oldStartProcess := startServeBackgroundProcessForEnsure
	startServeBackgroundProcessForEnsure = func(
		cfg config.Config, _ []string,
	) (*exec.Cmd, string, error) {
		gotCfg = cfg
		_, err := WriteDaemonRuntime(dir, host, port, version, false)
		if err != nil {
			return nil, "", err
		}
		cmd := exec.Command("sleep", "2")
		if err := cmd.Start(); err != nil {
			return nil, "", err
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForEnsure = oldStartProcess
		RemoveDaemonRuntime(dir)
	})

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)

	require.NoError(t, err)
	require.NotNil(t, rt)
	require.NotEmpty(t, gotCfg.AuthToken)
	assert.Equal(t, gotCfg.AuthToken, cfg.AuthToken)
	assert.False(t, gotCfg.RequireAuth)
	data, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(data), `auth_token = "`)
}

func TestEnsureBackgroundServeChecksTooNewDatabaseBeforeReplacingCompatibleDaemon(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	dbPath := writeTooNewSQLiteDB(t, dir)
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", false)
	require.NoError(t, err)
	setTestVersion(t, "1.1.0")

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		config.Config, *DaemonRuntime,
	) error {
		stopped = true
		RemoveDaemonRuntime(dir)
		return nil
	})
	oldStartProcess := startServeBackgroundProcessForEnsure
	startServeBackgroundProcessForEnsure = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		return nil, "", fmt.Errorf("start should not run")
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForEnsure = oldStartProcess
		RemoveDaemonRuntime(dir)
	})

	cfg := config.Config{DataDir: dir, DBPath: dbPath}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)

	require.Error(t, err)
	assert.True(t, db.IsDataVersionTooNew(err))
	assert.Nil(t, rt)
	assert.False(t, stopped)
	assert.NotNil(t, FindDaemonRuntime(dir))
}

func TestEnsureBackgroundServeChecksTooNewDatabaseBeforeReplacingIncompatibleDaemon(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	dbPath := writeTooNewSQLiteDB(t, dir)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeVersion("1.0.0"),
		withRuntimeAPIVersion(0),
	))
	setTestVersion(t, "1.1.0")

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		config.Config, *DaemonRuntime,
	) error {
		stopped = true
		RemoveDaemonRuntime(dir)
		return nil
	})
	oldStartProcess := startServeBackgroundProcessForEnsure
	startServeBackgroundProcessForEnsure = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		return nil, "", fmt.Errorf("start should not run")
	}
	t.Cleanup(func() { startServeBackgroundProcessForEnsure = oldStartProcess })

	cfg := config.Config{DataDir: dir, DBPath: dbPath}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)

	require.Error(t, err)
	assert.True(t, db.IsDataVersionTooNew(err))
	assert.Nil(t, rt)
	assert.False(t, stopped)
	found, compatErr := FindIncompatibleDaemonRuntime(dir)
	require.NotNil(t, found)
	require.Error(t, compatErr)
}

func TestEnsureBackgroundServeReplacementWaitsForExternalStartLock(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	tests := []struct {
		name         string
		writeRuntime func(t *testing.T, dir, host string, port int)
	}{
		{
			name: "compatible older daemon",
			writeRuntime: func(t *testing.T, dir, host string, port int) {
				t.Helper()
				_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", false)
				require.NoError(t, err)
				t.Cleanup(func() { RemoveDaemonRuntime(dir) })
			},
		},
		{
			name: "incompatible older daemon",
			writeRuntime: func(t *testing.T, dir, host string, port int) {
				t.Helper()
				writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
					host, port,
					withRuntimeVersion("1.0.0"),
					withRuntimeAPIVersion(0),
				))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := runtimeTestDir(t)
			oldHost, oldPort := testPingServer(t)
			tt.writeRuntime(t, dir, oldHost, oldPort)
			setTestVersion(t, "1.1.0")
			unlockStart := holdExternalDaemonStartLock(t, dir)

			forbidStopDaemonRuntimeForUpgrade(t,
				"auto-start replacement must not stop while foreground owns start lock")
			oldStart := startServeBackgroundProcessForEnsure
			startServeBackgroundProcessForEnsure = func(
				config.Config, []string,
			) (*exec.Cmd, string, error) {
				t.Fatal("auto-start must not spawn while waiting on foreground")
				return nil, "", nil
			}
			t.Cleanup(func() {
				startServeBackgroundProcessForEnsure = oldStart
			})

			newHost, newPort := testPingServer(t)
			published := make(chan error, 1)
			go func() {
				time.Sleep(2 * startProbeTick())
				published <- publishDaemonRuntimeAndUnlockWhenVisible(
					dir, newHost, newPort, "1.1.0", unlockStart,
				)
			}()

			cfg := config.Config{DataDir: dir}
			rt, err := ensureBackgroundServe(
				context.Background(), &cfg, time.Second,
			)

			require.NoError(t, <-published)
			require.NoError(t, err)
			require.NotNil(t, rt)
			assert.Equal(t, newPort, rt.Port)
		})
	}
}

func TestEnsureBackgroundServeReprobesWhenExternalStartupFinishesBeforeWait(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	setTestVersion(t, "1.1.0")
	unlockStart := holdExternalDaemonStartLock(t, dir)

	newDaemon := newPingDaemon(t)
	published := make(chan error, 1)
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "test",
	})
	var publishOnce sync.Once
	oldServer := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		publishOnce.Do(func() {
			RemoveDaemonRuntime(dir)
			_, err := WriteDaemonRuntime(
				dir, newDaemon.Host, newDaemon.Port, "1.1.0", false,
			)
			unlockStart()
			published <- err
		})
		ping.ServeHTTP(w, r)
	}))
	t.Cleanup(oldServer.Close)
	oldDaemon := serverEndpoint(t, oldServer)
	_, err := WriteDaemonRuntime(
		dir, oldDaemon.Host, oldDaemon.Port, "1.0.0", false,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	forbidStopDaemonRuntimeForUpgrade(t,
		"auto-start replacement must re-probe after foreground startup wins")
	oldStart := startServeBackgroundProcessForEnsure
	startServeBackgroundProcessForEnsure = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		t.Fatal("auto-start must not spawn after foreground startup wins")
		return nil, "", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForEnsure = oldStart
	})

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(
		context.Background(), &cfg, time.Second,
	)

	select {
	case publishErr := <-published:
		require.NoError(t, publishErr)
	case <-time.After(time.Second):
		t.Fatal("old daemon probe did not publish replacement runtime")
	}
	require.NoError(t, err)
	require.NotNil(t, rt)
	assert.Equal(t, newDaemon.Port, rt.Port)
}

func TestWaitForBackgroundServeReady_UsesStartupStateFallbackWithoutRuntimeRecord(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10))

	waitCh := make(chan error)
	rt, err := waitForBackgroundServeReady(
		context.Background(), dir, "", waitCh, time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, rt)
	assert.Equal(t, port, rt.Port)
}

func TestWaitForBackgroundServeReadyAttachedObservesProgressWithoutTimeout(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	startedAt := time.Now().Add(-2 * time.Second)
	data, err := json.Marshal(startupState{
		PID: 321, StartedAt: startedAt, Phase: "initial sync", Detail: "12/40 sessions",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(startupStatePath(dir), data, 0o600))
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })

	observed := make(chan *startupState, 1)
	waitCh := make(chan error)
	resultCh := make(chan *DaemonRuntime, 1)
	errCh := make(chan error, 1)
	go func() {
		rt, waitErr := waitForBackgroundServeReadyWithPolicy(
			context.Background(), dir, "", waitCh, 20*time.Millisecond,
			backgroundServeReadyWaitPolicy{
				Attached: true,
				Observe: func(st *startupState, _ time.Duration) {
					if st != nil {
						select {
						case observed <- st:
						default:
						}
					}
				},
			},
		)
		resultCh <- rt
		errCh <- waitErr
	}()

	select {
	case st := <-observed:
		assert.Equal(t, "initial sync", st.Phase)
		assert.Equal(t, "12/40 sessions", st.Detail)
	case <-time.After(time.Second):
		t.Fatal("attached readiness wait did not observe startup progress")
	}
	select {
	case err := <-errCh:
		t.Fatalf("attached readiness wait returned at legacy timeout: %v", err)
	case <-time.After(40 * time.Millisecond):
	}

	host, port := testPingServer(t)
	_, err = WriteDaemonRuntime(dir, host, port, version, false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	select {
	case err := <-errCh:
		require.NoError(t, err)
		rt := <-resultCh
		require.NotNil(t, rt)
		assert.Equal(t, port, rt.Port)
	case <-time.After(time.Second):
		t.Fatal("attached readiness wait did not return authoritative runtime")
	}
}

func TestWaitForBackgroundServeReadyAttachedChildExitAndCancellation(t *testing.T) {
	setStartProbeTickForTest(t, 10*time.Millisecond)

	t.Run("child exit", func(t *testing.T) {
		waitCh := make(chan error, 1)
		waitCh <- errors.New("exit status 7")
		rt, err := waitForBackgroundServeReadyWithPolicy(
			context.Background(), runtimeTestDir(t), "", waitCh,
			20*time.Millisecond, backgroundServeReadyWaitPolicy{Attached: true},
		)
		require.Error(t, err)
		assert.ErrorContains(t, err, "exit status 7")
		assert.Nil(t, rt)
	})

	t.Run("context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rt, err := waitForBackgroundServeReadyWithPolicy(
			ctx, runtimeTestDir(t), "", make(chan error),
			20*time.Millisecond, backgroundServeReadyWaitPolicy{Attached: true},
		)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, rt)
	})
}

func TestEnsureBackgroundServeLaunchLoserReplacesStaleDaemonAfterStartup(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := runtimeTestDir(t)
	oldHost, oldPort := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, oldHost, oldPort, "1.0.0", false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })
	setTestVersion(t, "1.1.0")

	launchLock, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	t.Cleanup(func() { _ = launchLock.Unlock() })
	unlockStart := holdExternalDaemonStartLock(t, dir)

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = true
		assert.Equal(t, "1.0.0", rt.Record.Version)
		RemoveDaemonRuntime(dir)
		return nil
	})

	newHost, newPort := testPingServer(t)
	oldStart := startServeBackgroundProcessForEnsure
	startServeBackgroundProcessForEnsure = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		_, err := WriteDaemonRuntime(dir, newHost, newPort, "1.1.0", false)
		if err != nil {
			return nil, "", err
		}
		cmd := exec.Command("sleep", "2")
		if err := cmd.Start(); err != nil {
			return nil, "", err
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() { startServeBackgroundProcessForEnsure = oldStart })

	released := make(chan struct{})
	go func() {
		time.Sleep(2 * startProbeTick())
		// The parent launch lock clears before the child finishes startup.
		// Preserve that lifecycle ordering now that unlockStart waits until
		// the child lock is observably released.
		_ = launchLock.Unlock()
		unlockStart()
		close(released)
	}()

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)

	<-released
	require.NoError(t, err)
	require.NotNil(t, rt)
	assert.True(t, stopped)
	assert.Equal(t, newPort, rt.Port)
}

func TestEnsureBackgroundServeReplacesStaleDaemonAfterExternalStartupAbort(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := runtimeTestDir(t)
	oldHost, oldPort := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, oldHost, oldPort, "1.0.0", false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })
	setTestVersion(t, "1.1.0")
	unlockStart := holdExternalDaemonStartLock(t, dir)

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = true
		assert.Equal(t, "1.0.0", rt.Record.Version)
		RemoveDaemonRuntime(dir)
		return nil
	})

	newHost, newPort := testPingServer(t)
	oldStart := startServeBackgroundProcessForEnsure
	startServeBackgroundProcessForEnsure = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		_, err := WriteDaemonRuntime(dir, newHost, newPort, "1.1.0", false)
		if err != nil {
			return nil, "", err
		}
		cmd := exec.Command("sleep", "2")
		if err := cmd.Start(); err != nil {
			return nil, "", err
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() { startServeBackgroundProcessForEnsure = oldStart })

	released := make(chan struct{})
	go func() {
		time.Sleep(2 * startProbeTick())
		unlockStart()
		close(released)
	}()

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)

	<-released
	require.NoError(t, err)
	require.NotNil(t, rt)
	assert.True(t, stopped)
	assert.Equal(t, newPort, rt.Port)
}

func TestEnsureBackgroundServeIncompatibleDaemonReturnsError(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeVersion("old"),
		withRuntimeAPIVersion(0),
	))

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(
		context.Background(), &cfg, 100*time.Millisecond,
	)
	require.Error(t, err)
	assert.Nil(t, rt)
	assert.Contains(t, err.Error(), "incompatible daemon")
	assert.Contains(t, err.Error(), "agentsview daemon stop")
}

func TestEnsureBackgroundServeIgnoresIncompatibleReadOnlyDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeReadOnly(true),
		withRuntimeAPIVersion(0),
	))

	newHost, newPort := testPingServer(t)
	oldStartProcess := startServeBackgroundProcessForEnsure
	startServeBackgroundProcessForEnsure = func(
		_ config.Config, _ []string,
	) (*exec.Cmd, string, error) {
		if _, err := WriteDaemonRuntime(
			dir, newHost, newPort, "test", false,
		); err != nil {
			return nil, "", err
		}
		cmd := exec.Command("sleep", "2")
		if err := cmd.Start(); err != nil {
			return nil, "", err
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForEnsure = oldStartProcess
		RemoveDaemonRuntime(dir)
	})

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)
	require.NoError(t, err)
	require.NotNil(t, rt)
	assert.False(t, rt.ReadOnly)
	assert.Equal(t, newHost, rt.Host)
	assert.Equal(t, newPort, rt.Port)
}

func TestEnsureBackgroundServeLaunchLoserReportsIncompatibleDaemon(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := runtimeTestDir(t)
	launchLock, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	t.Cleanup(func() { _ = launchLock.Unlock() })

	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeVersion("old"),
		withRuntimeAPIVersion(0),
	))

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(
		context.Background(), &cfg, 50*time.Millisecond,
	)
	require.Error(t, err)
	assert.Nil(t, rt)
	assert.Contains(t, err.Error(), "incompatible daemon")
	assert.Contains(t, err.Error(), "agentsview daemon stop")
}

func TestEnsureBackgroundServeLaunchLoserWaitsThroughReplacementGap(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := runtimeTestDir(t)
	launchLock, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	t.Cleanup(func() { _ = launchLock.Unlock() })

	oldHost, oldPort := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		oldHost, oldPort,
		withRuntimeVersion("old"),
		withRuntimeAPIVersion(0),
	))

	newHost, newPort := testPingServer(t)
	published := make(chan error, 1)
	go func() {
		time.Sleep(2 * startProbeTick())
		published <- publishDaemonRuntimeWhenVisible(
			dir, newHost, newPort, version,
		)
	}()

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(
		context.Background(), &cfg, 2*time.Second,
	)
	require.NoError(t, err)
	require.NoError(t, <-published)
	require.NotNil(t, rt)
	assert.Equal(t, newHost, rt.Host)
	assert.Equal(t, newPort, rt.Port)
}

func TestEnsureBackgroundServeChecksTooNewDatabaseAfterStartupWait(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := runtimeTestDir(t)
	dbPath := writeTooNewSQLiteDB(t, dir)
	setTestVersion(t, "1.1.0")

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		config.Config, *DaemonRuntime,
	) error {
		stopped = true
		RemoveDaemonRuntime(dir)
		return nil
	})
	oldStartProcess := startServeBackgroundProcessForEnsure
	startServeBackgroundProcessForEnsure = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		return nil, "", fmt.Errorf("start should not run")
	}
	t.Cleanup(func() { startServeBackgroundProcessForEnsure = oldStartProcess })

	unlockStart := holdExternalDaemonStartLock(t, dir)
	oldHost, oldPort := testPingServer(t)
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(2 * startProbeTick())
		_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
			oldHost, oldPort,
			withRuntimeVersion("1.0.0"),
			withRuntimeAPIVersion(0),
		))
		unlockStart()
		errCh <- err
	}()

	cfg := config.Config{DataDir: dir, DBPath: dbPath}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)

	require.NoError(t, <-errCh)
	require.Error(t, err)
	assert.True(t, db.IsDataVersionTooNew(err))
	assert.Nil(t, rt)
	assert.False(t, stopped)
	found, compatErr := FindIncompatibleDaemonRuntime(dir)
	require.NotNil(t, found)
	require.Error(t, compatErr)
}

func TestEnsureBackgroundServeLaunchLoserIgnoresReadOnlyRuntimeDuringReplacement(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := runtimeTestDir(t)
	releaseLaunchLock := holdExternalBackgroundLaunchLock(t, dir)
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
	oldStartProcess := startServeBackgroundProcessForEnsure
	startServeBackgroundProcessForEnsure = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		return nil, "", fmt.Errorf("start should not run")
	}
	t.Cleanup(func() { startServeBackgroundProcessForEnsure = oldStartProcess })

	readOnlyHost, readOnlyPort := testPingServer(t)
	_, err := WriteDaemonRuntime(
		dir, readOnlyHost, readOnlyPort, version, true,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	writableHost, writablePort := testPingServer(t)
	published := make(chan error, 1)
	go func() {
		time.Sleep(2 * startProbeTick())
		err := publishDaemonRuntimeWhenVisible(
			dir, writableHost, writablePort, version,
		)
		UnmarkDaemonStarting(dir)
		releaseLaunchLock()
		published <- err
	}()

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(
		context.Background(), &cfg, 2*time.Second,
	)
	require.NoError(t, err)
	require.NoError(t, <-published)
	require.NotNil(t, rt)
	assert.False(t, rt.ReadOnly)
	assert.Equal(t, writableHost, rt.Host)
	assert.Equal(t, writablePort, rt.Port)
}

func TestEnsureBackgroundServeReplacesIncompatibleDaemonAfterStartupWait(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := runtimeTestDir(t)
	oldVersion := version
	version = "1.1.0"
	t.Cleanup(func() { version = oldVersion })

	oldStop := stopDaemonRuntimeForUpgrade
	var stopped bool
	stopDaemonRuntimeForUpgrade = func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = true
		assert.Equal(t, "1.0.0", rt.Record.Version)
		RemoveDaemonRuntime(dir)
		return nil
	}
	t.Cleanup(func() { stopDaemonRuntimeForUpgrade = oldStop })

	newHost, newPort := testPingServer(t)
	oldStartProcess := startServeBackgroundProcessForEnsure
	var started bool
	startServeBackgroundProcessForEnsure = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		started = true
		if _, err := WriteDaemonRuntime(
			dir, newHost, newPort, "1.1.0", false,
		); err != nil {
			return nil, "", err
		}
		cmd := exec.Command("sleep", "2")
		if err := cmd.Start(); err != nil {
			return nil, "", err
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForEnsure = oldStartProcess
		RemoveDaemonRuntime(dir)
	})

	unlockStart := holdExternalDaemonStartLock(t, dir)
	oldHost, oldPort := testPingServer(t)
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(2 * startProbeTick())
		_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
			oldHost, oldPort,
			withRuntimeVersion("1.0.0"),
			withRuntimeAPIVersion(0),
		))
		unlockStart()
		errCh <- err
	}()

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)
	require.NoError(t, <-errCh)
	require.NoError(t, err)
	require.NotNil(t, rt)
	assert.True(t, stopped)
	assert.True(t, started)
	assert.Equal(t, newPort, rt.Port)
}

func TestEnsureBackgroundServeRejectsMultipleWritableDaemons(t *testing.T) {
	requirePOSIXSignals(t, "requires long-lived child processes")
	dir := runtimeTestDir(t)
	setTestVersion(t, "1.0.0")
	host, port := testPingServer(t)
	pids := []int{os.Getpid(), startSleepProcess(t)}
	_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		host, port, withRuntimePID(pids[0]), withRuntimeVersion("1.0.0"),
	))
	require.NoError(t, err)
	_, err = writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		"127.0.0.1", 9201, withRuntimePID(pids[1]),
	))
	require.NoError(t, err)

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(
		context.Background(), &cfg, 25*time.Millisecond,
	)

	require.Error(t, err)
	assert.ErrorContains(t, err, "multiple writable agentsview daemons")
	assert.ErrorContains(t, err, strconv.Itoa(pids[0]))
	assert.ErrorContains(t, err, strconv.Itoa(pids[1]))
	assert.Nil(t, rt)
}

func TestEnsureBackgroundServeValidatesConfigBeforeReplacementStop(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", false)
	require.NoError(t, err)
	setTestVersion(t, "1.1.0")

	stopCalls := 0
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, _ *DaemonRuntime,
	) error {
		stopCalls++
		RemoveDaemonRuntime(dir)
		return nil
	})
	startCalls := 0
	oldStart := startServeBackgroundProcessForEnsure
	startServeBackgroundProcessForEnsure = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		startCalls++
		return nil, "test.log", errors.New("replacement child started")
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForEnsure = oldStart
		RemoveDaemonRuntime(dir)
	})

	cfg := config.Config{DataDir: dir, Host: "0.0.0.0"}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)

	require.Error(t, err)
	assert.ErrorContains(t, err, "require_auth")
	assert.Zero(t, stopCalls, "invalid config must preserve the incumbent")
	assert.Zero(t, startCalls, "invalid config must not launch a child")
	assert.Nil(t, rt)
	require.NotNil(t, FindDaemonRuntime(dir),
		"incumbent runtime must remain discoverable")
}

func TestEnsureBackgroundServePassesNoSyncToChild(t *testing.T) {
	dir := runtimeTestDir(t)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	host, port := testPingServer(t)

	oldStartProcess := startServeBackgroundProcessForEnsure
	var gotArgs []string
	startServeBackgroundProcessForEnsure = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		gotArgs = append([]string(nil), arguments...)
		if _, err := WriteDaemonRuntime(
			dir, host, port, "test", false,
		); err != nil {
			return nil, "", err
		}
		cmd := exec.Command("sleep", "2")
		if err := cmd.Start(); err != nil {
			return nil, "", err
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForEnsure = oldStartProcess
		RemoveDaemonRuntime(dir)
	})

	cfg := config.Config{DataDir: dir, NoSync: true}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)
	require.NoError(t, err)
	require.NotNil(t, rt)
	assert.Equal(t, []string{"serve", "--no-sync"}, gotArgs)
}

func TestEnsureBackgroundServePassesSkipInitialSyncToChild(t *testing.T) {
	dir := runtimeTestDir(t)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	host, port := testPingServer(t)

	oldStartProcess := startServeBackgroundProcessForEnsure
	var gotArgs []string
	startServeBackgroundProcessForEnsure = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		gotArgs = append([]string(nil), arguments...)
		if _, err := WriteDaemonRuntime(
			dir, host, port, "test", false,
		); err != nil {
			return nil, "", err
		}
		cmd := exec.Command("sleep", "2")
		if err := cmd.Start(); err != nil {
			return nil, "", err
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForEnsure = oldStartProcess
		RemoveDaemonRuntime(dir)
	})

	cfg := config.Config{DataDir: dir, SkipInitialSync: true}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)
	require.NoError(t, err)
	require.NotNil(t, rt)
	assert.Equal(t, []string{"serve", "--skip-initial-sync"}, gotArgs)
}

func TestEnsureBackgroundServePreservesNoSyncWhenReplacingOlderDaemon(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntimeWithAuthAndNoSync(
		dir, host, port, "1.0.0", false, false, true,
	)
	require.NoError(t, err)

	oldVersion := version
	version = "1.1.0"
	t.Cleanup(func() { version = oldVersion })

	oldStop := stopDaemonRuntimeForUpgrade
	stopDaemonRuntimeForUpgrade = func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		assert.True(t, rt.NoSync)
		RemoveDaemonRuntime(dir)
		return nil
	}
	t.Cleanup(func() { stopDaemonRuntimeForUpgrade = oldStop })

	newHost, newPort := testPingServer(t)
	oldStartProcess := startServeBackgroundProcessForEnsure
	var gotArgs []string
	startServeBackgroundProcessForEnsure = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		gotArgs = append([]string(nil), arguments...)
		if _, err := WriteDaemonRuntime(
			dir, newHost, newPort, "1.1.0", false,
		); err != nil {
			return nil, "", err
		}
		cmd := exec.Command("sleep", "2")
		if err := cmd.Start(); err != nil {
			return nil, "", err
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForEnsure = oldStartProcess
		RemoveDaemonRuntime(dir)
	})

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)
	require.NoError(t, err)
	require.NotNil(t, rt)
	assert.Equal(t, newPort, rt.Port)
	assert.Equal(t, []string{"serve", "--no-sync"}, gotArgs)
}

func TestRunServeBackgroundPreservesNoSyncWhenReplacingOlderDaemon(
	t *testing.T,
) {
	tests := []struct {
		name         string
		writeRuntime func(t *testing.T, dir, host string, port int)
	}{
		{
			name: "compatible",
			writeRuntime: func(t *testing.T, dir, host string, port int) {
				t.Helper()
				_, err := WriteDaemonRuntimeWithAuthAndNoSync(
					dir, host, port, "1.0.0", false, false, true,
				)
				require.NoError(t, err)
			},
		},
		{
			name: "incompatible older API",
			writeRuntime: func(t *testing.T, dir, host string, port int) {
				t.Helper()
				_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
					host, port,
					withRuntimeVersion("1.0.0"),
					withRuntimeRequireAuth(false),
					withRuntimeNoSync(true),
					withRuntimeAPIVersion(0),
				))
				require.NoError(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := runtimeTestDir(t)
			oldHost, oldPort := testPingServer(t)
			tt.writeRuntime(t, dir, oldHost, oldPort)

			oldVersion := version
			version = "1.1.0"
			t.Cleanup(func() { version = oldVersion })

			oldStop := stopDaemonRuntimeForUpgrade
			stopDaemonRuntimeForUpgrade = func(
				_ config.Config, rt *DaemonRuntime,
			) error {
				assert.True(t, rt.NoSync)
				RemoveDaemonRuntime(dir)
				return nil
			}
			t.Cleanup(func() { stopDaemonRuntimeForUpgrade = oldStop })

			newHost, newPort := testPingServer(t)
			oldStart := startServeBackgroundProcessForRun
			var gotArgs []string
			startServeBackgroundProcessForRun = func(
				_ config.Config, arguments []string,
			) (*exec.Cmd, string, error) {
				gotArgs = serveBackgroundChildArgs(arguments)
				if _, err := WriteDaemonRuntime(
					dir, newHost, newPort, "1.1.0", false,
				); err != nil {
					return nil, "", err
				}
				cmd := exec.Command("sleep", "2")
				if err := cmd.Start(); err != nil {
					return nil, "", err
				}
				t.Cleanup(func() { _ = cmd.Process.Kill() })
				return cmd, "test.log", nil
			}
			t.Cleanup(func() {
				startServeBackgroundProcessForRun = oldStart
				RemoveDaemonRuntime(dir)
			})

			runServeBackground(
				config.Config{DataDir: dir},
				[]string{"serve", "--background"},
				serveReplacementOptions{},
			)

			assert.Equal(t, []string{"serve", "--no-sync"}, gotArgs)
		})
	}
}

func TestRunServeBackgroundConfigOnlyDoesNotAdoptReplacedDaemonNoSync(
	t *testing.T,
) {
	tests := []struct {
		name         string
		writeRuntime func(t *testing.T, dir, host string, port int)
	}{
		{
			name: "compatible",
			writeRuntime: func(t *testing.T, dir, host string, port int) {
				t.Helper()
				_, err := WriteDaemonRuntimeWithAuthAndNoSync(
					dir, host, port, "1.0.0", false, false, true,
				)
				require.NoError(t, err)
			},
		},
		{
			name: "incompatible older API",
			writeRuntime: func(t *testing.T, dir, host string, port int) {
				t.Helper()
				_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
					host, port,
					withRuntimeVersion("1.0.0"),
					withRuntimeRequireAuth(false),
					withRuntimeNoSync(true),
					withRuntimeAPIVersion(0),
				))
				require.NoError(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := runtimeTestDir(t)
			oldHost, oldPort := testPingServer(t)
			tt.writeRuntime(t, dir, oldHost, oldPort)
			setTestVersion(t, "1.1.0")

			stubStopDaemonRuntimeForUpgrade(t, func(
				cfg config.Config, rt *DaemonRuntime,
			) error {
				assert.False(t, cfg.NoSync)
				assert.True(t, rt.NoSync)
				RemoveDaemonRuntime(dir)
				return nil
			})

			newHost, newPort := testPingServer(t)
			oldStart := startServeBackgroundProcessForRun
			var gotArgs []string
			startServeBackgroundProcessForRun = func(
				cfg config.Config, arguments []string,
			) (*exec.Cmd, string, error) {
				assert.False(t, cfg.NoSync)
				gotArgs = append([]string(nil), arguments...)
				_, err := WriteDaemonRuntime(
					dir, newHost, newPort, "1.1.0", false,
				)
				if err != nil {
					return nil, "test.log", err
				}
				cmd := exec.Command("sleep", "2")
				if err := cmd.Start(); err != nil {
					return nil, "test.log", err
				}
				t.Cleanup(func() { _ = cmd.Process.Kill() })
				return cmd, "test.log", nil
			}
			t.Cleanup(func() {
				startServeBackgroundProcessForRun = oldStart
				RemoveDaemonRuntime(dir)
			})

			result, err := startServeBackground(
				config.Config{DataDir: dir},
				[]string{"serve", "--background", "--no-sync"},
				serveReplacementOptions{},
				backgroundLaunchPolicy{
					ConfigOnly: true,
					Operation:  "daemon start",
				},
			)

			require.NoError(t, err)
			assert.True(t, result.Started)
			require.NotNil(t, result.Runtime)
			assert.Equal(t, newPort, result.Runtime.Port)
			assert.Equal(t, []string{"serve"}, gotArgs)
		})
	}
}

func TestRunServeBackgroundConfigOnlyIgnoresConfigNoSync(t *testing.T) {
	dir := runtimeTestDir(t)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	host, port := testPingServer(t)

	oldStart := startServeBackgroundProcessForRun
	var gotArgs []string
	startServeBackgroundProcessForRun = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		gotArgs = append([]string(nil), arguments...)
		_, err := WriteDaemonRuntime(dir, host, port, version, false)
		if err != nil {
			return nil, "test.log", err
		}
		cmd := exec.Command("sleep", "2")
		if err := cmd.Start(); err != nil {
			return nil, "test.log", err
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForRun = oldStart
		RemoveDaemonRuntime(dir)
	})

	result, err := startServeBackground(
		config.Config{DataDir: dir, NoSync: true},
		nil,
		serveReplacementOptions{},
		backgroundLaunchPolicy{ConfigOnly: true, Operation: "daemon start"},
	)

	require.NoError(t, err)
	assert.True(t, result.Started)
	assert.Equal(t, []string{"serve"}, gotArgs)
}

func TestRunServeBackgroundConfigOnlyReadOnlyRuntimeStartsWritableChild(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	readOnlyHost, readOnlyPort := testPingServer(t)
	readOnlyPath, err := WriteDaemonRuntime(
		dir, readOnlyHost, readOnlyPort, version, true,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(readOnlyPath) })

	writablePID := startSleepProcess(t)
	writableEndpoint := newPingDaemonWithPID(t, writablePID)
	oldStart := startServeBackgroundProcessForRun
	var gotArgs []string
	var writablePath string
	startServeBackgroundProcessForRun = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		gotArgs = append([]string(nil), arguments...)
		var err error
		writablePath, err = writeRuntimeRecordForTest(
			dir,
			daemonRuntimeRecord(
				writableEndpoint.Host,
				writableEndpoint.Port,
				withRuntimePID(writablePID),
				withRuntimeVersion(version),
			),
		)
		if err != nil {
			return nil, "test.log", err
		}
		cmd := exec.Command("sleep", "2")
		if err := cmd.Start(); err != nil {
			return nil, "test.log", err
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForRun = oldStart
		if writablePath != "" {
			_ = os.Remove(writablePath)
		}
	})

	result, err := startServeBackground(
		config.Config{DataDir: dir},
		nil,
		serveReplacementOptions{},
		backgroundLaunchPolicy{ConfigOnly: true, Operation: "daemon start"},
	)

	require.NoError(t, err)
	assert.True(t, result.Started)
	require.NotNil(t, result.Runtime)
	assert.False(t, result.Runtime.ReadOnly)
	assert.Equal(t, writableEndpoint.Host, result.Runtime.Host)
	assert.Equal(t, writableEndpoint.Port, result.Runtime.Port)
	assert.Equal(t, []string{"serve"}, gotArgs)

	records := liveDaemonRecords(dir)
	require.Len(t, records, 2)
	readOnlyModes := make([]bool, 0, len(records))
	pids := make([]int, 0, len(records))
	for _, rec := range records {
		readOnlyModes = append(readOnlyModes, daemonRuntimeFromRecord(rec).ReadOnly)
		pids = append(pids, rec.PID)
	}
	assert.ElementsMatch(t, []bool{true, false}, readOnlyModes)
	assert.ElementsMatch(t, []int{os.Getpid(), writablePID}, pids)
}

func TestBackgroundStartResultDistinguishesExistingDaemonAndStartedChild(
	t *testing.T,
) {
	t.Run("existing daemon", func(t *testing.T) {
		dir := runtimeTestDir(t)
		host, port := testPingServer(t)
		_, err := WriteDaemonRuntime(dir, host, port, version, false)
		require.NoError(t, err)
		t.Cleanup(func() { RemoveDaemonRuntime(dir) })

		oldStart := startServeBackgroundProcessForRun
		startServeBackgroundProcessForRun = func(
			config.Config, []string,
		) (*exec.Cmd, string, error) {
			t.Fatal("existing writable daemon must be reused")
			return nil, "", nil
		}
		t.Cleanup(func() { startServeBackgroundProcessForRun = oldStart })

		result, err := startServeBackground(
			config.Config{DataDir: dir},
			[]string{"serve", "--background"},
			serveReplacementOptions{},
			backgroundLaunchPolicy{},
		)

		require.NoError(t, err)
		assert.False(t, result.Started)
		assert.Empty(t, result.LogPath)
		require.NotNil(t, result.Runtime)
		assert.Equal(t, host, result.Runtime.Host)
		assert.Equal(t, port, result.Runtime.Port)
	})

	t.Run("started child", func(t *testing.T) {
		dir := runtimeTestDir(t)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		host, port := testPingServer(t)

		oldStart := startServeBackgroundProcessForRun
		startServeBackgroundProcessForRun = func(
			_ config.Config, arguments []string,
		) (*exec.Cmd, string, error) {
			assert.Equal(t, []string{"serve"}, arguments)
			_, err := WriteDaemonRuntime(dir, host, port, version, false)
			if err != nil {
				return nil, "test.log", err
			}
			cmd := exec.Command("sleep", "2")
			if err := cmd.Start(); err != nil {
				return nil, "test.log", err
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			return cmd, "test.log", nil
		}
		t.Cleanup(func() {
			startServeBackgroundProcessForRun = oldStart
			RemoveDaemonRuntime(dir)
		})

		result, err := startServeBackground(
			config.Config{DataDir: dir},
			[]string{"serve", "--background"},
			serveReplacementOptions{},
			backgroundLaunchPolicy{},
		)

		require.NoError(t, err)
		assert.True(t, result.Started)
		assert.Equal(t, "test.log", result.LogPath)
		require.NotNil(t, result.Runtime)
		assert.Equal(t, host, result.Runtime.Host)
		assert.Equal(t, port, result.Runtime.Port)
	})
}

func TestStartServeBackgroundReturnsStartupErrorWithLogPath(t *testing.T) {
	dir := runtimeTestDir(t)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	startErr := errors.New("fork failed")
	logPath := filepath.Join(dir, "serve.log")
	launched := false

	oldStart := startServeBackgroundProcessForRun
	startServeBackgroundProcessForRun = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		assert.Equal(t, []string{"serve"}, arguments)
		return nil, logPath, startErr
	}
	t.Cleanup(func() { startServeBackgroundProcessForRun = oldStart })

	result, err := startServeBackground(
		config.Config{DataDir: dir},
		nil,
		serveReplacementOptions{},
		backgroundLaunchPolicy{
			ConfigOnly: true,
			Operation:  "daemon start",
			OnLaunch: func(int, string) {
				launched = true
			},
		},
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, startErr)
	assert.Equal(t, "daemon start: fork failed", err.Error())
	assert.False(t, result.Started)
	assert.Equal(t, logPath, result.LogPath)
	assert.Nil(t, result.Runtime)
	assert.False(t, launched, "process-creation failure must not report a launch")
}

func TestRunServeBackgroundLaunchErrorPreservesLegacyFatalOutput(t *testing.T) {
	dir := runtimeTestDir(t)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	logPath := filepath.Join(dir, "serve.log")

	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestRunServeBackgroundLaunchErrorHelper$",
	)
	cmd.Env = append(
		os.Environ(),
		"AGENTSVIEW_BACKGROUND_LAUNCH_ERROR_HELPER=1",
		"AGENTSVIEW_BACKGROUND_LAUNCH_ERROR_DIR="+dir,
		"AGENTSVIEW_BACKGROUND_LAUNCH_ERROR_LOG="+logPath,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Equal(
		t,
		"fatal: serve background: starting server: fork failed\n",
		string(out),
	)
}

func TestRunServeBackgroundLaunchErrorHelper(t *testing.T) {
	if os.Getenv("AGENTSVIEW_BACKGROUND_LAUNCH_ERROR_HELPER") != "1" {
		return
	}
	dir := os.Getenv("AGENTSVIEW_BACKGROUND_LAUNCH_ERROR_DIR")
	logPath := os.Getenv("AGENTSVIEW_BACKGROUND_LAUNCH_ERROR_LOG")
	require.NotEmpty(t, dir)
	require.NotEmpty(t, logPath)

	startServeBackgroundProcessForRun = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		return nil, logPath, errors.New("starting server: fork failed")
	}
	runServeBackground(
		config.Config{DataDir: dir},
		[]string{"serve", "--background"},
		serveReplacementOptions{},
	)
}

func TestRunServeBackgroundReadinessErrorRetainsLegacyLogsOutput(t *testing.T) {
	dir := runtimeTestDir(t)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	logPath := filepath.Join(dir, "serve.log")

	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestRunServeBackgroundReadinessErrorHelper$",
	)
	cmd.Env = append(
		os.Environ(),
		"AGENTSVIEW_BACKGROUND_READINESS_ERROR_HELPER=1",
		"AGENTSVIEW_BACKGROUND_READINESS_ERROR_DIR="+dir,
		"AGENTSVIEW_BACKGROUND_READINESS_ERROR_LOG="+logPath,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Equal(
		t,
		"fatal: serve background: server exited before becoming ready: "+
			"server process exited\nLogs: "+logPath+"\n",
		string(out),
	)
}

func TestRunServeBackgroundReadinessErrorHelper(t *testing.T) {
	if os.Getenv("AGENTSVIEW_BACKGROUND_READINESS_ERROR_HELPER") != "1" {
		return
	}
	dir := os.Getenv("AGENTSVIEW_BACKGROUND_READINESS_ERROR_DIR")
	logPath := os.Getenv("AGENTSVIEW_BACKGROUND_READINESS_ERROR_LOG")
	require.NotEmpty(t, dir)
	require.NotEmpty(t, logPath)

	startServeBackgroundProcessForRun = func(
		config.Config, []string,
	) (*exec.Cmd, string, error) {
		child := exec.Command(os.Args[0], "-test.run=^$")
		if err := child.Start(); err != nil {
			return nil, logPath, err
		}
		return child, logPath, nil
	}
	runServeBackground(
		config.Config{DataDir: dir},
		[]string{"serve", "--background"},
		serveReplacementOptions{},
	)
}

func TestStartServeBackgroundProcessOpenErrorReturnsLogPath(t *testing.T) {
	dir := t.TempDir()
	notDirectory := filepath.Join(dir, "not-a-directory")
	require.NoError(t, os.WriteFile(notDirectory, []byte("file"), 0o600))

	child, logPath, err := startServeBackgroundProcess(
		config.Config{DataDir: notDirectory}, []string{"serve"},
	)

	require.Error(t, err)
	assert.Nil(t, child)
	assert.Equal(t, filepath.Join(notDirectory, "serve.log"), logPath)
}

func TestRunServeBackgroundKeepsInvocationNoSyncWhenReplacingSyncingDaemon(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	oldHost, oldPort := testPingServer(t)
	_, err := WriteDaemonRuntimeWithAuthAndNoSync(
		dir, oldHost, oldPort, "1.0.0", false, false, false,
	)
	require.NoError(t, err)
	setTestVersion(t, "1.1.0")

	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		assert.False(t, rt.NoSync)
		RemoveDaemonRuntime(dir)
		return nil
	})

	newHost, newPort := testPingServer(t)
	oldStart := startServeBackgroundProcessForRun
	var gotArgs []string
	startServeBackgroundProcessForRun = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		gotArgs = append([]string(nil), arguments...)
		_, err := WriteDaemonRuntime(dir, newHost, newPort, "1.1.0", false)
		require.NoError(t, err)
		cmd := exec.Command("sleep", "2")
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() {
		startServeBackgroundProcessForRun = oldStart
		RemoveDaemonRuntime(dir)
	})

	runServeBackground(
		config.Config{DataDir: dir},
		[]string{"serve", "--background", "--no-sync"},
		serveReplacementOptions{},
	)

	assert.Equal(t, []string{"serve", "--no-sync"}, gotArgs)
}

func TestRefreshServeDaemonReplacementDecisionKeepsStopConfirmedOriginal(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	ln, oldPort := freeTCPListener(t)
	require.NoError(t, ln.Close())
	liveCreateTime, ok := processCreateTimeMillis(os.Getpid())
	if !ok {
		t.Skip("process create time is unavailable on this platform")
	}
	rec := daemonRuntimeRecord(
		"127.0.0.1", oldPort,
		withRuntimeVersion("1.0.0"),
		withRuntimeMetadata(
			runtimeCreateTime, strconv.FormatInt(liveCreateTime, 10),
		),
	)
	writeRuntimeRecordFixture(t, dir, rec)
	require.Nil(t, FindDaemonRuntime(dir),
		"precondition: runtime record must be live but unprobeable")
	original := daemonRuntimeFromRecord(rec)
	require.True(t, stopTargetConfirmed(original.Record, ""))

	got := refreshServeDaemonReplacementDecision(
		config.Config{DataDir: dir},
		serveReplacementOptions{},
		serveReplacementDecision{
			Action:  serveReplacementAuto,
			Runtime: original,
		},
		false,
		time.Time{},
	)

	assert.Equal(t, serveReplacementAuto, got.Action)
	require.NotNil(t, got.Runtime)
	assert.Equal(t, oldPort, got.Runtime.Port)
}

func TestRefreshServeDaemonReplacementDecisionKeepsStartupPublishedRuntime(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	setTestVersion(t, "dev")

	host, port := testPingServer(t)
	replacementCheckStarted := time.Date(
		2026, time.January, 1, 0, 0, 0, 0, time.UTC,
	)
	rec := daemonRuntimeRecord(
		host, port,
		withRuntimeVersion("dev"),
		withRuntimeStartedAt(replacementCheckStarted.Add(time.Minute)),
	)
	writeRuntimeRecordFixture(t, dir, rec)

	got := refreshServeDaemonReplacementDecision(
		config.Config{DataDir: dir},
		serveReplacementOptions{Replace: true},
		serveReplacementDecision{
			Action:  serveReplacementExplicit,
			Runtime: daemonRuntimeFromRecord(rec),
		},
		true,
		replacementCheckStarted,
	)

	assert.Equal(t, serveReplacementUseExisting, got.Action)
	require.NotNil(t, got.Runtime)
	assert.Equal(t, port, got.Runtime.Port)
}

func TestEnsureBackgroundServeConcurrentLaunchConvergesOnDaemon(t *testing.T) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := runtimeTestDir(t)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	launchLock, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	t.Cleanup(func() {
		UnmarkDaemonStarting(dir)
		_ = launchLock.Unlock()
		RemoveDaemonRuntime(dir)
	})

	host, port := testPingServer(t)
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(2 * startProbeTick())
		MarkDaemonStarting(dir)
		_, err := WriteDaemonRuntime(dir, host, port, "test", false)
		if err == nil {
			UnmarkDaemonStarting(dir)
			err = launchLock.Unlock()
		}
		errCh <- err
	}()

	cfg := config.Config{DataDir: dir}
	rt, err := ensureBackgroundServe(context.Background(), &cfg, time.Second)
	require.NoError(t, err)
	require.NotNil(t, rt)
	assert.Equal(t, port, rt.Port)
	require.NoError(t, <-errCh)
}

func TestEnsureTransportArchiveWriteRecoversStaleBackgroundRuntime(t *testing.T) {
	dir := runtimeTestDir(t)
	const deadPID = 99999999
	require.False(t, daemon.ProcessAlive(deadPID))
	_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		"127.0.0.1", 9,
		withRuntimePID(deadPID),
		withRuntimeVersion("stale"),
	))
	require.NoError(t, err)

	oldStart := startBackgroundServeForTransport
	var started bool
	startBackgroundServeForTransport = func(
		context.Context, *config.Config, time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		return &DaemonRuntime{Host: "127.0.0.1", Port: 12345}, nil
	}
	t.Cleanup(func() { startBackgroundServeForTransport = oldStart })

	cfg := config.Config{DataDir: dir}
	tr, err := ensureTransport(&cfg, transportIntentArchiveWrite, 100*time.Millisecond)
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://127.0.0.1:12345", tr.URL)
}

func TestConfigureServeBackgroundCommandSetsProcessAttributes(t *testing.T) {
	requireConfiguredServeBackgroundSysProcAttr(t)
}

func writeTooNewSQLiteDB(t *testing.T, dir string) string {
	t.Helper()
	dbPath := filepath.Join(dir, "sessions.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	futureVersion := db.CurrentDataVersion() + 10
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", futureVersion))
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	return dbPath
}

// requireConfiguredServeBackgroundSysProcAttr builds the background serve
// command, applies configureServeBackgroundCommand, and returns the resulting
// non-nil SysProcAttr for platform-specific assertions.
func requireConfiguredServeBackgroundSysProcAttr(t *testing.T) *syscall.SysProcAttr {
	t.Helper()
	cmd := exec.Command("agentsview")
	configureServeBackgroundCommand(cmd)
	require.NotNil(t, cmd.SysProcAttr)
	return cmd.SysProcAttr
}
