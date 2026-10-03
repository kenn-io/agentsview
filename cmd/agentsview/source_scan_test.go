package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

func TestCollectedSourceScansKeepProviderOwnership(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{}
	cfg.AgentDirs = map[parser.AgentType][]string{parser.AgentClaude: {root}, parser.AgentCodex: {root}, parser.AgentGemini: {root}}
	roots, _, _, _ := collectWatchRoots(cfg)
	physical, ok := findCollectedWatchRoot(roots, root)
	require.True(t, ok)
	registered := physical.registeredRoot()
	assert.Contains(t, registered.SourceFileGlobs, "*.jsonl")
	var agents []string
	for _, scope := range registered.SourceScanScopes {
		agents = append(agents, scope.Agent)
	}
	assert.ElementsMatch(t, []string{string(parser.AgentClaude), string(parser.AgentCodex)}, agents)
	home, ok := findCollectedWatchRoot(roots, filepath.Dir(root))
	require.True(t, ok)
	assert.Contains(t, home.sourceFileGlobs, parser.CodexSessionIndexFilename)
}
