package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
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
				Persistent:       true,
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
			assert.True(t, stop.Persistent)
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
			_, err = WriteDaemonRuntimeWithAuthAndNoSync(nil,
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

func TestRestartDaemonAfterUpdatePreservesLifetime(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(strconv.FormatBool(persistent), func(t *testing.T) {
			dir := testDataDir(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			t.Setenv("AGENTSVIEW_TEST_REAL_CLI", "1")
			t.Setenv("DO_NOT_TRACK", "1")
			t.Setenv("AGENTSVIEW_DAEMON_IDLE_TIMEOUT", "0s")
			var disabled []string
			for _, def := range parser.Registry {
				if def.FileBased || def.EnvVar != "" {
					disabled = append(disabled, strconv.Quote(string(def.Type)))
				}
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("disabled_agents = ["+strings.Join(disabled, ",")+"]\n"+"host = \"127.0.0.1\"\nport = 0\nno_browser = true\nno_sync = true\ndisable_update_check = true\n[vector]\nenabled = false\n"), 0o600))
			cfg := config.Config{DataDir: dir}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
				defer cancel()
				_, err := stopWritableDaemonsForUpdate(cleanupCtx, cfg)
				assert.NoError(t, err)
			})
			args := []string{"serve", "--background", "--no-sync"}
			out, err := exec.CommandContext(ctx, os.Args[0], args...).CombinedOutput()
			require.NoError(t, err, string(out))
			if persistent {
				out, err = exec.CommandContext(ctx, os.Args[0], "daemon", "start").CombinedOutput()
				require.NoError(t, err, string(out))
			}
			rt := FindWritableDaemonRuntime(dir)
			require.NotNil(t, rt)
			stopped, err := stopWritableDaemonsForUpdate(ctx, cfg)
			require.NoError(t, err)
			t.Setenv("AGENTSVIEW_DAEMON_IDLE_TIMEOUT", "2s")
			require.NoError(t, restartDaemonAfterUpdate(ctx, cfg, stopped))
			rt = FindWritableDaemonRuntime(dir)
			require.NotNil(t, rt)
			assert.Equal(t, persistent, rt.Persistent)
			require.NotNil(t, rt.IdleTimeout)
			if persistent {
				assert.Zero(t, *rt.IdleTimeout)
			} else {
				assert.Equal(t, 2*time.Second, *rt.IdleTimeout)
			}
			time.Sleep(5 * time.Second) //nolint:kennlint // sleeptest: the child process uses the real clock for its two-second idle exit.
			assert.Equal(t, persistent, FindWritableDaemonRuntime(dir) != nil)
		})
	}
}

func TestRestartDaemonAfterUpdateAcrossAPIVersion(t *testing.T) {
	if os.Getenv("AGENTSVIEW_TEST_UPDATE_RESIDENT") == "1" {
		_, err := io.ReadFull(os.Stdin, make([]byte, 1))
		require.NoError(t, err)
		require.NoError(t, restartDaemonAfterUpdate(t.Context(), config.Config{DataDir: os.Getenv("AGENTSVIEW_DATA_DIR")}, updateDaemonStopResult{Persistent: true, NoSync: true}))
		return
	}
	requirePOSIXSignals(t, "replaces the running updater executable")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	dir := testDataDir(t)
	t.Setenv("DO_NOT_TRACK", "1")
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("AGENTSVIEW_DAEMON_IDLE_TIMEOUT", "2s")
	var disabled []string
	for _, def := range parser.Registry {
		if def.FileBased || def.EnvVar != "" {
			disabled = append(disabled, strconv.Quote(string(def.Type)))
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("disabled_agents = ["+strings.Join(disabled, ",")+"]\nhost = \"127.0.0.1\"\nport = 0\nno_browser = true\ndisable_update_check = true\n[vector]\nenabled = false\n"), 0o600))
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	source := filepath.Join(root, "internal", "server", "server.go")
	data, err := os.ReadFile(source)
	require.NoError(t, err)
	futureAPI := daemonAPIVersion + 1
	changed := bytes.Replace(data, []byte(fmt.Sprintf("APIVersion = %d", daemonAPIVersion)), []byte(fmt.Sprintf("APIVersion = %d", futureAPI)), 1)
	require.NotEqual(t, data, changed)
	overlaySource := filepath.Join(dir, "server.go")
	require.NoError(t, os.WriteFile(overlaySource, changed, 0o600))
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{source: overlaySource}})
	require.NoError(t, err)
	overlayPath := filepath.Join(dir, "overlay.json")
	require.NoError(t, os.WriteFile(overlayPath, overlay, 0o600))
	installed := filepath.Join(dir, "installed")
	build := exec.CommandContext(ctx, "make", "--eval=future-cli: pricing-snapshot ensure-embed-dir; CGO_ENABLED=1 go build -tags fts5 -overlay \"$(OVERLAY)\" -o \"$(OUTPUT)\" ./cmd/agentsview", "future-cli", "OVERLAY="+overlayPath, "OUTPUT="+installed)
	build.Dir = root
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	updater := filepath.Join(dir, "updater")
	src, err := os.Open(os.Args[0])
	require.NoError(t, err)
	defer src.Close()
	dst, err := os.OpenFile(updater, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	require.NoError(t, err)
	_, err = io.Copy(dst, src)
	closeErr := dst.Close()
	require.NoError(t, err)
	require.NoError(t, closeErr)
	resident := exec.CommandContext(ctx, updater, "-test.run=^TestRestartDaemonAfterUpdateAcrossAPIVersion$")
	resident.Env = append(os.Environ(), "AGENTSVIEW_TEST_UPDATE_RESIDENT=1")
	var output bytes.Buffer
	resident.Stdout, resident.Stderr = &output, &output
	input, err := resident.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, resident.Start())
	t.Cleanup(func() {
		if resident.ProcessState == nil {
			_ = resident.Process.Kill()
			_ = resident.Wait()
		}
	})
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		stop := exec.CommandContext(cleanupCtx, updater, "daemon", "stop")
		out, err := stop.CombinedOutput()
		assert.NoError(t, err, string(out))
	})
	require.NoError(t, os.Rename(installed, updater))
	_, err = input.Write([]byte{1})
	require.NoError(t, err)
	require.NoError(t, input.Close())
	err = resident.Wait()
	require.NoError(t, err, output.String())
	records := liveDaemonRecords(dir)
	require.Len(t, records, 1)
	rt := daemonRuntimeFromRecord(records[0])
	assert.Equal(t, futureAPI, rt.API)
	assert.True(t, rt.Persistent)
	time.Sleep(5 * time.Second) //nolint:kennlint // sleeptest: the replacement is a real process with a two-second idle timeout.
	status := exec.CommandContext(ctx, updater, "daemon", "status")
	out, err = status.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "Stays running until agentsview daemon stop")
}
