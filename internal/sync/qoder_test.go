package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestEngineClassifyQoderPaths(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentQoder: {root},
		},
		Machine: "local",
	})

	mainPath := filepath.Join(root, "-Users-alice-project", "11111111-1111-4111-8111-111111111111.jsonl")
	subPath := filepath.Join(root, "-Users-alice-project", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	sidecarPath := filepath.Join(root, "-Users-alice-project", "11111111-1111-4111-8111-111111111111-session.json")
	statsPath := filepath.Join(root, "ai-stats", "usage.json")
	rootAgentPath := filepath.Join(root, "-Users-alice-project", "agent-123.jsonl")
	nestedPath := filepath.Join(root, "-Users-alice-project", "notes", "stray.jsonl")
	for _, path := range []string{mainPath, subPath, sidecarPath, statsPath, rootAgentPath, nestedPath} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))
	}

	files := engine.classifyPaths([]string{mainPath})
	require.Len(t, files, 1, "main path did not classify")
	got := files[0]
	assert.Equal(t, mainPath, got.Path)
	assert.Equal(t, "project", got.Project)
	assert.Equal(t, parser.AgentQoder, got.Agent)

	files = engine.classifyPaths([]string{subPath})
	require.Len(t, files, 1, "subagent path did not classify")
	got = files[0]
	assert.Equal(t, subPath, got.Path)
	assert.Equal(t, "project", got.Project)
	assert.Equal(t, parser.AgentQoder, got.Agent)

	files = engine.classifyPaths([]string{sidecarPath})
	require.Len(t, files, 1, "sidecar path did not map to transcript")
	got = files[0]
	assert.Equal(t, mainPath, got.Path)
	assert.Equal(t, "project", got.Project)
	assert.Equal(t, parser.AgentQoder, got.Agent)

	for _, path := range []string{statsPath, rootAgentPath, nestedPath} {
		files = engine.classifyPaths([]string{path})
		assert.Emptyf(t, files, "%s classified as %+v", path, files)
	}
}

func TestEngineClassifyQoderProjectNamedSubagentsAsMainSession(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentQoder: {root},
		},
		Machine: "local",
	})

	path := filepath.Join(root, "subagents", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))

	files := engine.classifyPaths([]string{path})
	require.Len(t, files, 1, "path did not classify")
	got := files[0]
	assert.Equal(t, path, got.Path)
	assert.Equal(t, "subagents", got.Project)
	assert.Equal(t, parser.AgentQoder, got.Agent)
}
