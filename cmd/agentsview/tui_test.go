package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	tuiterm "go.kenn.io/agentsview/internal/tui"
)

func TestTUICommandPassesExplicitDaemonConfiguration(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-token\n"), 0o600))
	original := runTUI
	t.Cleanup(func() { runTUI = original })
	var got tuiterm.Options
	runTUI = func(_ context.Context, opts tuiterm.Options) error { got = opts; return nil }

	cmd := newTUICommand()
	cmd.SetArgs([]string{"--server", "http://127.0.0.1:9090/", "--server-token-file", tokenPath})
	err := cmd.Execute()

	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9090", got.BaseURL)
	assert.Equal(t, "test-token", got.Token)
	assert.Equal(t, filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "tui-state.json"), got.StatePath)
	assert.True(t, got.ReadOnly)
	assert.True(t, got.ResolveReadOnly)
	assert.False(t, got.StartupSync)
}

func TestTUIAutostartServesBeforeForegroundSync(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	originalStart := startBackgroundServeForTransport
	originalRun := runTUI
	t.Cleanup(func() {
		startBackgroundServeForTransport = originalStart
		runTUI = originalRun
	})

	var skippedInitialSync bool
	startBackgroundServeForTransport = func(
		_ context.Context, cfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		skippedInitialSync = cfg.SkipInitialSync
		return &DaemonRuntime{Host: "127.0.0.1", Port: 9090}, nil
	}
	var got tuiterm.Options
	runTUI = func(_ context.Context, opts tuiterm.Options) error {
		got = opts
		return nil
	}

	cmd := newTUICommand()
	err := cmd.Execute()

	require.NoError(t, err)
	assert.True(t, skippedInitialSync,
		"daemon readiness must not wait for the archive-wide startup sync")
	assert.True(t, got.StartupSync,
		"the live TUI must drive the deferred sync after it starts")
	assert.False(t, got.ResolveReadOnly)
}

func TestRootCommandIncludesTUI(t *testing.T) {
	command, _, err := newRootCommand().Find([]string{"tui"})

	require.NoError(t, err)
	assert.Equal(t, "tui", command.Name())
}
