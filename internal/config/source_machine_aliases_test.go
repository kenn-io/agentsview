package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSourceMachineAliasesSurviveRootResolution(t *testing.T) {
	root := t.TempDir()
	localRoot, foreignRoot := filepath.Join(root, "local"), filepath.Join(root, "foreign")
	cfg := Config{
		InstallationID: "installation-a", LocalMachineName: "foreign-host",
		AgentDirs:      make(map[parser.AgentType][]string),
		agentDirSource: make(map[parser.AgentType]dirSource),
		sessionSourceConfigs: []sessionSourceConfig{
			{Agent: "claude", Dir: localRoot, Machine: new("old-host")},
			{Agent: "claude", Dir: foreignRoot, Machine: new("foreign-host")},
		},
	}
	require.NoError(t, cfg.resolveSessionSources())
	before := cfg
	cfg.ApplyMachineAliases(map[string]string{
		"old-host": "installation-a", "foreign-host": "installation-b",
	})
	for range 2 {
		assert.Equal(t, "installation-a", cfg.SourceMachines[parser.AgentClaude][localRoot])
		assert.Equal(t, "foreign-host", cfg.SourceMachines[parser.AgentClaude][foreignRoot])
		assert.Equal(t, "installation-a", cfg.SessionSources[0].Machine)
		require.NoError(t, cfg.resolveSessionSources())
	}
	assert.Equal(t, "old-host", before.SourceMachines[parser.AgentClaude][localRoot],
		"normalizing a config snapshot must not mutate the original")
	assert.Equal(t, "old-host", before.SessionSources[0].Machine)
}
