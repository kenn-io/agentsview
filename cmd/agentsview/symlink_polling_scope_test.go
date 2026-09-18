package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// TestSymlinkPollingObligationsCarryProviderAgent asserts that
// symlinkPollingObligations preserves the agent from the watchScope when
// building PollingScope values.
func TestSymlinkPollingObligationsCarryProviderAgent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	parent := t.TempDir()
	symRoot := filepath.Join(parent, "sessions-symlink")
	dir := filepath.Join(parent, "provider-dir")

	gatedDirs := map[string][]watchScope{
		symRoot: {{agent: parser.AgentClaude, syncDir: dir}},
	}
	obligations := symlinkPollingObligations(gatedDirs)

	require.Len(obligations, 1)
	require.Len(obligations[0].Scopes, 1)
	assert.Equal(string(parser.AgentClaude), obligations[0].Scopes[0].Agent,
		"symlink gate obligation must carry the provider's agent")
	assert.Equal(filepath.Clean(dir), obligations[0].Scopes[0].Root,
		"symlink gate obligation scope must use the configured dir as Root")
	assert.Equal(filepath.Clean(symRoot), obligations[0].Probe,
		"symlink gate obligation probe must be the symlink root path")
}
