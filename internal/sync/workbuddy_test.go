package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestWorkBuddyRegistryUsesRecursiveWatch(t *testing.T) {
	def, ok := parser.AgentByType(parser.AgentWorkBuddy)
	require.True(t, ok, "AgentWorkBuddy missing from Registry")
	require.False(t, def.ShallowWatch, "WorkBuddy should use recursive watch for nested sessions")
}

func TestEngineClassifyWorkBuddyPaths(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	db := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentWorkBuddy: {root},
		},
		Machine: "local",
	})

	mainPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	subPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	toolPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "tool-results", "tool_123.txt")
	for _, path := range []string{mainPath, subPath, toolPath} {
		require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(os.WriteFile(path, []byte("{}\n"), 0o644))
	}

	files := requireClassifyPaths(t, engine, []string{mainPath})
	require.Len(files, 1, "main path did not classify")
	got := files[0]
	assert.Equal(mainPath, got.Path)
	assert.Equal("proj", got.Project)
	assert.Equal(parser.AgentWorkBuddy, got.Agent)

	files = requireClassifyPaths(t, engine, []string{subPath})
	require.Len(files, 1, "subagent path did not classify")
	got = files[0]
	assert.Equal(subPath, got.Path)
	assert.Equal("proj", got.Project)
	assert.Equal(parser.AgentWorkBuddy, got.Agent)

	files = requireClassifyPaths(t, engine, []string{toolPath})
	assert.Empty(files, "tool result classified as %+v", files)
}

func TestEngineClassifyWorkBuddyProjectNamedSubagentsAsMainSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	db := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentWorkBuddy: {root},
		},
		Machine: "local",
	})

	path := filepath.Join(root, "subagents", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(os.WriteFile(path, []byte("{}\n"), 0o644))

	files := requireClassifyPaths(t, engine, []string{path})
	require.Len(files, 1, "path did not classify")
	got := files[0]
	assert.Equal(path, got.Path)
	assert.Equal("subagents", got.Project)
	assert.Equal(parser.AgentWorkBuddy, got.Agent)
}
