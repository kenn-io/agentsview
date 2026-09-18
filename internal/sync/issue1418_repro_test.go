package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestIssue1418WorkspaceAppearsWithoutTranscriptChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	workspaceRoot := cursorWorkspaceTempDir(t)
	workspace := filepath.Join(workspaceRoot, "Code", "app")
	projectDir := encodeCursorProjectDir(workspace)
	sessionID := "dddddddd-eeee-4fff-8000-111111111111"
	path := filepath.Join(root, projectDir, "agent-transcripts", sessionID+".jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"issue 1418"}}`+"\n",
	), 0o644))

	d := dbtest.OpenTestDB(t)
	e := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
	})
	e.SyncAll(t.Context(), nil)
	first, err := d.GetSession(t.Context(), "cursor:"+sessionID)
	require.NoError(err)
	require.NotNil(first)
	assert.Empty(first.Cwd)
	e.Close()

	require.NoError(os.MkdirAll(workspace, 0o755))
	filtered := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs:          map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:            "local",
		IncludeCwdPrefixes: []string{workspaceRoot},
	})
	t.Cleanup(func() { filtered.Close() })
	stats := filtered.SyncAll(t.Context(), nil)
	assert.Zero(stats.Failed)
	assert.False(stats.Aborted)

	second, err := d.GetSession(t.Context(), "cursor:"+sessionID)
	require.NoError(err)
	require.NotNil(second)
	assert.Equal(workspace, second.Cwd)
}
