package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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

func TestScopedConfigUpdateUsesDeploymentAuthForStartupFallback(t *testing.T) {
	seedScopedConfigArchive(t)
	dir := os.Getenv("AGENTSVIEW_DATA_DIR")
	tokenFile := filepath.Join(dir, "deployment-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("update-secret\n"), 0o600))
	t.Setenv("AGENTSVIEW_MODE", "serve")
	t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", tokenFile)
	t.Setenv("AGENTSVIEW_HOST", "")
	t.Setenv("AGENTSVIEW_EMBEDDINGS_ENDPOINT", "http://127.0.0.1:1/v1")
	writeTestConfig(t, dir, "[vector]\nenabled=false\n[insights]\nmodel='incomplete'\n")
	host, port := testAuthenticatedPingServer(t, "update-secret")
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10))
	cfg, err := loadDaemonConfigForUpdate()
	require.NoError(t, err)

	// Exercise real discovery without signaling the test process or replacing
	// a binary. A daemon without a written runtime record needs its token.
	orig := stopDaemonRuntimeForUpgrade
	calls := 0
	stopDaemonRuntimeForUpgrade = func(_ context.Context, cfg config.Config, rt *DaemonRuntime) error {
		calls++
		assert.Equal(t, port, rt.Port)
		assert.True(t, daemonRecordPingConfirmed(rt.Record, cfg.AuthToken))
		return nil
	}
	t.Cleanup(func() { stopDaemonRuntimeForUpgrade = orig })
	result, err := stopWritableDaemonsForUpdate(t.Context(), cfg)
	require.NoError(t, err)
	assert.True(t, result.Stopped)
	assert.Equal(t, 1, calls)
}

func TestScopedConfigUpdateRejectsInvalidDeploymentAuth(t *testing.T) {
	seedScopedConfigArchive(t)
	t.Setenv("AGENTSVIEW_MODE", "serve")
	t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", filepath.Join(os.Getenv("HOME"), "missing-token"))
	_, err := loadDaemonConfigForUpdate()
	require.ErrorContains(t, err, "AGENTSVIEW_AUTH_TOKEN_FILE")
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

func stubDistribution(t *testing.T, value string) {
	t.Helper()
	orig := distribution
	distribution = value
	t.Cleanup(func() { distribution = orig })
}

func TestUpdateRefusesInstallForPackageManagedBuild(t *testing.T) {
	tests := []struct {
		name         string
		distribution string
		wantErr      string
	}{
		{
			name:         "homebrew",
			distribution: "homebrew",
			wantErr:      "Homebrew manages this installation; run `brew upgrade agentsview` instead",
		},
		{
			name:         "other package manager",
			distribution: "nix",
			wantErr:      "nix manages this installation; upgrade agentsview through it instead",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubDistribution(t, tt.distribution)
			dataDir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

			_, err := executeCommand(newRootCommand(), "update", "--yes")

			require.Error(t, err)
			assert.Equal(t, tt.wantErr, err.Error())
			// The refusal happens before any update check runs.
			assert.NoFileExists(t, filepath.Join(dataDir, "update_check.json"))
		})
	}
}

func TestUpdateInstallRunsForSelfUpdatingBuilds(t *testing.T) {
	tests := []struct {
		name         string
		distribution string
	}{
		{name: "source build", distribution: ""},
		{name: "official direct download", distribution: "kenn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubDistribution(t, tt.distribution)
			origVersion := version
			version = "1.0.0"
			t.Cleanup(func() { version = origVersion })
			dataDir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
			// A fresh cached check for the running version lets the install
			// path finish without the network once it is past the refusal.
			cache := `{"checked_at":"` + time.Now().UTC().Format(time.RFC3339) +
				`","version":"v1.0.0"}`
			require.NoError(t, os.WriteFile(
				filepath.Join(dataDir, "update_check.json"), []byte(cache), 0o600,
			))

			out, err := executeCommand(newRootCommand(), "update", "--yes")

			require.NoError(t, err)
			assert.Contains(t, out, "agentsview 1.0.0 is up to date.")
			assert.NotContains(t, out, "manages this installation")
		})
	}
}

func TestUpdateCheckGivesPackageManagerGuidance(t *testing.T) {
	tests := []struct {
		name         string
		distribution string
		wantGuidance bool
	}{
		{name: "homebrew", distribution: "homebrew", wantGuidance: true},
		{name: "source build", distribution: "", wantGuidance: false},
		{name: "official direct download", distribution: "kenn", wantGuidance: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubDistribution(t, tt.distribution)
			origVersion := version
			version = "dev"
			t.Cleanup(func() { version = origVersion })
			dataDir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
			// A fresh cached check lets --check answer without the network.
			cache := `{"checked_at":"` + time.Now().UTC().Format(time.RFC3339) +
				`","version":"v9.9.9"}`
			require.NoError(t, os.WriteFile(
				filepath.Join(dataDir, "update_check.json"), []byte(cache), 0o600,
			))

			out, err := executeCommand(newRootCommand(), "update", "--check")

			require.NoError(t, err)
			assert.Contains(t, out, "Latest release: v9.9.9")
			if tt.wantGuidance {
				assert.Contains(t, out, "run `brew upgrade agentsview` instead")
			} else {
				assert.NotContains(t, out, "manages this installation")
			}
		})
	}
}
