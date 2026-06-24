package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/update"
)

func TestPerformUpdateWithDaemonLifecycleRestartsStoppedDaemon(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	var calls []string

	err := performUpdateWithDaemonLifecycle(
		&update.UpdateInfo{},
		nil,
		func() (config.Config, error) {
			calls = append(calls, "load")
			return cfg, nil
		},
		func(got config.Config) (updateDaemonStopResult, error) {
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
		func(_ *update.UpdateInfo, _ func(int64, int64)) error {
			calls = append(calls, "perform")
			return nil
		},
		func(got config.Config, stop updateDaemonStopResult) error {
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

	err := performUpdateWithDaemonLifecycle(
		&update.UpdateInfo{},
		nil,
		func() (config.Config, error) {
			calls = append(calls, "load")
			return cfg, nil
		},
		func(config.Config) (updateDaemonStopResult, error) {
			calls = append(calls, "stop")
			return updateDaemonStopResult{Stopped: true}, nil
		},
		func(_ *update.UpdateInfo, _ func(int64, int64)) error {
			calls = append(calls, "perform")
			return installErr
		},
		func(config.Config, updateDaemonStopResult) error {
			calls = append(calls, "restart")
			return nil
		},
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, installErr)
	assert.Equal(t, []string{"load", "stop", "perform", "restart"}, calls)
}

func TestPerformUpdateWithDaemonLifecycleRestartsAfterPartialStopFailure(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	stopErr := errors.New("second daemon failed to stop")
	var calls []string

	err := performUpdateWithDaemonLifecycle(
		&update.UpdateInfo{},
		nil,
		func() (config.Config, error) {
			calls = append(calls, "load")
			return cfg, nil
		},
		func(config.Config) (updateDaemonStopResult, error) {
			calls = append(calls, "stop")
			return updateDaemonStopResult{Stopped: true}, stopErr
		},
		func(_ *update.UpdateInfo, _ func(int64, int64)) error {
			t.Fatal("install must not run after stop failure")
			return nil
		},
		func(config.Config, updateDaemonStopResult) error {
			calls = append(calls, "restart")
			return nil
		},
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, stopErr)
	assert.Equal(t, []string{"load", "stop", "restart"}, calls)
}

func TestPerformUpdateWithDaemonLifecycleDoesNotRestartWhenNoneStopped(t *testing.T) {
	var calls []string

	err := performUpdateWithDaemonLifecycle(
		&update.UpdateInfo{},
		nil,
		func() (config.Config, error) {
			calls = append(calls, "load")
			return config.Config{DataDir: t.TempDir()}, nil
		},
		func(config.Config) (updateDaemonStopResult, error) {
			calls = append(calls, "stop")
			return updateDaemonStopResult{}, nil
		},
		func(_ *update.UpdateInfo, _ func(int64, int64)) error {
			calls = append(calls, "perform")
			return nil
		},
		func(config.Config, updateDaemonStopResult) error {
			t.Fatal("restart must not run when no daemon was stopped")
			return nil
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
		"serve", "--background", "--host", "0.0.0.0", "--port", "18080",
		"--require-auth", "--no-sync",
	}, args)
}

func TestRestartDaemonAfterUpdateArgsDropsLegacyNonLoopbackWithoutAuthConfig(t *testing.T) {
	args := restartDaemonAfterUpdateArgs(config.Config{}, updateDaemonStopResult{
		Host: "0.0.0.0",
		Port: 18080,
	})

	assert.Equal(t, []string{
		"serve", "--background", "--host", "127.0.0.1", "--port", "18080",
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
		"serve", "--background", "--host", "127.0.0.1", "--port", "18080",
	}, args)
}

func TestRestartDaemonAfterUpdateArgsKeepsLegacyNonLoopbackWithAuthConfig(t *testing.T) {
	args := restartDaemonAfterUpdateArgs(
		config.Config{RequireAuth: true},
		updateDaemonStopResult{Host: "0.0.0.0", Port: 18080},
	)

	assert.Equal(t, []string{
		"serve", "--background", "--host", "0.0.0.0", "--port", "18080",
		"--require-auth",
	}, args)
}
