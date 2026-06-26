package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
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
		context.Background(), &cfg, 100*time.Millisecond,
	)
	require.Error(t, err)
	assert.Nil(t, rt)
	assert.Contains(t, err.Error(), "incompatible daemon")
	assert.Contains(t, err.Error(), "serve stop")
}

func TestEnsureBackgroundServeLaunchLoserWaitsThroughReplacementGap(
	t *testing.T,
) {
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
		time.Sleep(2 * startProbeTick)
		_, err := WriteDaemonRuntime(dir, newHost, newPort, version, false)
		published <- err
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

func TestEnsureBackgroundServeLaunchLoserIgnoresReadOnlyRuntimeDuringReplacement(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	launchLock, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	t.Cleanup(func() { _ = launchLock.Unlock() })
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })

	readOnlyHost, readOnlyPort := testPingServer(t)
	_, err := WriteDaemonRuntime(
		dir, readOnlyHost, readOnlyPort, version, true,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	writableHost, writablePort := testPingServer(t)
	published := make(chan error, 1)
	go func() {
		time.Sleep(2 * startProbeTick)
		_ = launchLock.Unlock()
		time.Sleep(2 * startProbeTick)
		_, err := WriteDaemonRuntime(
			dir, writableHost, writablePort, version, false,
		)
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
		time.Sleep(50 * time.Millisecond)
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
			)

			assert.Equal(t, []string{"serve", "--no-sync"}, gotArgs)
		})
	}
}

func TestEnsureBackgroundServeConcurrentLaunchConvergesOnDaemon(t *testing.T) {
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
		time.Sleep(50 * time.Millisecond)
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
