package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
}

func TestRootCommandIncludesTUI(t *testing.T) {
	command, _, err := newRootCommand().Find([]string{"tui"})

	require.NoError(t, err)
	assert.Equal(t, "tui", command.Name())
}
