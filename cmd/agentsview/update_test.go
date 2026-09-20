package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/update"
)

func TestPerformUpdateWithDaemonLifecycleRestartsStoppedDaemon(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	var calls []string

	err := performUpdateWithDaemonLifecycle(t.Context(),
		&update.UpdateInfo{},
		nil,
		func() (config.Config, error) {
			calls = append(calls, "load")
			return cfg, nil
		},
		func(ctx context.Context, got config.Config) (updateDaemonStopResult, error) {
			calls = append(calls, "stop")
			assert.Equal(t, cfg.DataDir, got.DataDir)
			return updateDaemonStopResult{
				Stopped:          true,
				Host:             "127.0.0.1",
				Port:             18080,
				RequireAuth:      true,
				RequireAuthKnown: true,
			}, nil
		},
		func(ctx context.Context, _ *update.UpdateInfo, _ func(int64, int64)) error {
			calls = append(calls, "perform")
			return nil
		},
		func(ctx context.Context, got config.Config, stop updateDaemonStopResult) error {
			calls = append(calls, "restart")
			assert.Equal(t, cfg.DataDir, got.DataDir)
			assert.Equal(t, "127.0.0.1", stop.Host)
			assert.Equal(t, 18080, stop.Port)
			assert.True(t, stop.RequireAuth)
			assert.True(t, stop.RequireAuthKnown)
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t, []string{"load", "stop", "perform", "restart"}, calls)
}

func TestPerformUpdateWithDaemonLifecycleRestartsAfterInstallFailure(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	installErr := errors.New("install failed")
	var calls []string

	err := performUpdateWithDaemonLifecycle(t.Context(),
		&update.UpdateInfo{},
		nil,
		func() (config.Config, error) {
			calls = append(calls, "load")
			return cfg, nil
		},
		func(context.Context, config.Config) (updateDaemonStopResult, error) {
			calls = append(calls, "stop")
			return updateDaemonStopResult{Stopped: true}, nil
		},
		func(ctx context.Context, _ *update.UpdateInfo, _ func(int64, int64)) error {
			calls = append(calls, "perform")
			return installErr
		},
		func(context.Context, config.Config, updateDaemonStopResult) error {
			calls = append(calls, "restart")
			return nil
		},
	)

	require.Error(t, err)
	require.ErrorIs(t, err, installErr)
	assert.Equal(t, []string{"load", "stop", "perform", "restart"}, calls)
}

func TestPerformUpdateWithDaemonLifecycleRestartsAfterPartialStopFailure(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	stopErr := errors.New("second daemon failed to stop")
	var calls []string

	err := performUpdateWithDaemonLifecycle(t.Context(),
		&update.UpdateInfo{},
		nil,
		func() (config.Config, error) {
			calls = append(calls, "load")
			return cfg, nil
		},
		func(context.Context, config.Config) (updateDaemonStopResult, error) {
			calls = append(calls, "stop")
			return updateDaemonStopResult{Stopped: true}, stopErr
		},
		func(ctx context.Context, _ *update.UpdateInfo, _ func(int64, int64)) error {
			return errors.New("install must not run after stop failure")
		},
		func(context.Context, config.Config, updateDaemonStopResult) error {
			calls = append(calls, "restart")
			return nil
		},
	)

	require.Error(t, err)
	require.ErrorIs(t, err, stopErr)
	assert.Equal(t, []string{"load", "stop", "restart"}, calls)
}

func TestPerformUpdateWithDaemonLifecycleDoesNotRestartWhenNoneStopped(t *testing.T) {
	var calls []string

	err := performUpdateWithDaemonLifecycle(t.Context(),
		&update.UpdateInfo{},
		nil,
		func() (config.Config, error) {
			calls = append(calls, "load")
			return config.Config{DataDir: t.TempDir()}, nil
		},
		func(context.Context, config.Config) (updateDaemonStopResult, error) {
			calls = append(calls, "stop")
			return updateDaemonStopResult{}, nil
		},
		func(ctx context.Context, _ *update.UpdateInfo, _ func(int64, int64)) error {
			calls = append(calls, "perform")
			return nil
		},
		func(context.Context, config.Config, updateDaemonStopResult) error {
			return errors.New("restart must not run when no daemon was stopped")
		},
	)

	require.NoError(t, err)
	assert.Equal(t, []string{"load", "stop", "perform"}, calls)
}

func TestRestartDaemonAfterUpdateArgsPreserveRuntimeBind(t *testing.T) {
	args := restartDaemonAfterUpdateArgs(config.Config{}, updateDaemonStopResult{
		Host:             "0.0.0.0",
		Port:             18080,
		RequireAuth:      true,
		RequireAuthKnown: true,
		NoSync:           true,
	})

	assert.Equal(t, []string{
		"serve", "--background", "--host", "0.0.0.0", "--restart-port", "18080",
		"--require-auth", "--no-sync",
	}, args)
}

func TestUpdateRestartPreservesPortChoice(t *testing.T) {
	for _, tt := range []struct {
		name      string
		ephemeral bool
		occupied  bool
	}{
		{name: "explicit port preserves forwarded URL"},
		{name: "explicit port rejects collision", occupied: true},
		{name: "explicit zero keeps automatic selection", ephemeral: true, occupied: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := testDataDir(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(
				"port = 8080\npublic_url = \"http://viewer.example.test:8080\"\n"+
					"public_origins = [\"http://viewer.example.test:8080\"]\n",
			), 0o600))
			listener, port := heldLoopbackPort(t)
			require.NoError(t, listener.Close())
			if tt.ephemeral {
				port = 0
			}
			cmd := newServeCommand()
			require.NoError(t, cmd.Flags().Parse([]string{"--port", strconv.Itoa(port)}))
			cfg, err := config.LoadPFlags(cmd.Flags())
			require.NoError(t, err)
			first, _, err := prepareRunServeRuntimeConfig(cmd.Context(), cfg, 0, nil)
			require.NoError(t, err)
			require.Equal(t, "http://viewer.example.test:8080", first.PublicURL)
			_, err = WriteDaemonRuntimeWithAuthAndNoSync(
				dir, first.Host, first.Port, "test", first.PublicURL, false, false, false, new(port),
			)
			require.NoError(t, err)
			oldStop := stopDaemonRuntimeForUpgrade
			stopDaemonRuntimeForUpgrade = func(ctx context.Context, _ config.Config, rt *DaemonRuntime) error {
				require.Equal(t, first.Port, rt.Port)
				return nil
			}
			t.Cleanup(func() { stopDaemonRuntimeForUpgrade = oldStop })
			stopped, err := stopWritableDaemonsForUpdate(cmd.Context(), config.Config{DataDir: dir})
			require.NoError(t, err)
			require.True(t, stopped.Stopped)
			if tt.occupied {
				listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(first.Host, strconv.Itoa(first.Port)))
				require.NoError(t, err)
				t.Cleanup(func() { listener.Close() })
			}
			args := serveBackgroundChildArgs(restartDaemonAfterUpdateArgs(config.Config{}, stopped))
			cmd = newServeCommand()
			require.NoError(t, cmd.Flags().Parse(args[1:]))
			cfg, err = config.LoadPFlags(cmd.Flags())
			require.NoError(t, err)
			restartPort, err := cmd.Flags().GetInt("restart-port")
			require.NoError(t, err)
			restarted, _, err := prepareRunServeRuntimeConfig(cmd.Context(), cfg, restartPort, nil)
			if tt.occupied && !tt.ephemeral {
				require.ErrorContains(t, err, "requested port")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "http://viewer.example.test:8080", restarted.PublicURL)
			assert.Equal(t, []string{"http://viewer.example.test:8080"}, restarted.PublicOrigins)
			if tt.ephemeral {
				assert.Positive(t, restarted.Port)
				assert.NotEqual(t, first.Port, restarted.Port)
			} else {
				assert.Equal(t, first.Port, restarted.Port)
			}
		})
	}
}

func TestRestartDaemonAfterUpdateArgsDropsLegacyNonLoopbackWithoutAuthConfig(t *testing.T) {
	args := restartDaemonAfterUpdateArgs(config.Config{}, updateDaemonStopResult{
		Host: "0.0.0.0",
		Port: 18080,
	})

	assert.Equal(t, []string{
		"serve", "--background", "--host", "127.0.0.1", "--restart-port", "18080",
	}, args)
}

func TestRestartDaemonAfterUpdateArgsDropsKnownUnauthenticatedNonLoopback(t *testing.T) {
	args := restartDaemonAfterUpdateArgs(config.Config{}, updateDaemonStopResult{
		Host:             "0.0.0.0",
		Port:             18080,
		RequireAuth:      false,
		RequireAuthKnown: true,
	})

	assert.Equal(t, []string{
		"serve", "--background", "--host", "127.0.0.1", "--restart-port", "18080",
	}, args)
}

func TestRestartDaemonAfterUpdateArgsKeepsLegacyNonLoopbackWithAuthConfig(t *testing.T) {
	args := restartDaemonAfterUpdateArgs(
		config.Config{RequireAuth: true},
		updateDaemonStopResult{Host: "0.0.0.0", Port: 18080},
	)

	assert.Equal(t, []string{
		"serve", "--background", "--host", "0.0.0.0", "--restart-port", "18080",
		"--require-auth",
	}, args)
}
