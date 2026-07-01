package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	oldHost, oldPort := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		oldHost, oldPort, withRuntimeVersion("1.0.0"),
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
		time.Sleep(2 * startProbeTick())
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
		unlockStart()
		_ = launchLock.Unlock()
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
	assert.Contains(t, err.Error(), "serve stop")
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
	assert.Contains(t, err.Error(), "serve stop")
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

	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
	oldHost, oldPort := testPingServer(t)
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(2 * startProbeTick())
		_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
			oldHost, oldPort,
			withRuntimeVersion("1.0.0"),
			withRuntimeAPIVersion(0),
		))
		UnmarkDaemonStarting(dir)
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

	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
	oldHost, oldPort := testPingServer(t)
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(2 * startProbeTick())
		_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
			oldHost, oldPort,
			withRuntimeVersion("1.0.0"),
			withRuntimeAPIVersion(0),
		))
		UnmarkDaemonStarting(dir)
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
	)

	assert.Equal(t, serveReplacementAuto, got.Action)
	require.NotNil(t, got.Runtime)
	assert.Equal(t, oldPort, got.Runtime.Port)
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
