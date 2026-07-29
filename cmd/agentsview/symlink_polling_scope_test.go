package main

// TestSymlinkPollingObligationsCarryProviderAgent: symlinkPollingObligations
// must preserve the agent from the watchScope when building PollingScope values.
// This test uses the post-fix type map[string][]watchScope; before the fix the
// parameter type is map[string][]string and the test fails to compile.
//
// The compile error is the pre-fix state:
//   cannot use map[string][]watchScope as map[string][]string

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSymlinkPollingObligationsCarryProviderAgent(t *testing.T) {
	parent := t.TempDir()
	symRoot := filepath.Join(parent, "sessions-symlink")
	dir := filepath.Join(parent, "provider-dir")

	gatedDirs := map[string][]watchScope{
		symRoot: {{agent: parser.AgentClaude, syncDir: dir}},
	}
	obligations := symlinkPollingObligations(gatedDirs)

	require.Len(t, obligations, 1)
	require.Len(t, obligations[0].Scopes, 1)
	assert.Equal(t, string(parser.AgentClaude), obligations[0].Scopes[0].Agent,
		"symlink gate obligation must carry the provider's agent")
	assert.Equal(t, filepath.Clean(dir), obligations[0].Scopes[0].Root,
		"symlink gate obligation scope must use the configured dir as Root")
	assert.Equal(t, filepath.Clean(symRoot), obligations[0].Probe,
		"symlink gate obligation probe must be the symlink root path")
}
