package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

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

func TestRunningAsBackgroundChild(t *testing.T) {
	assert.False(t, runningAsBackgroundChild())
	t.Setenv(backgroundChildEnvVar, "1")
	assert.True(t, runningAsBackgroundChild())
}
