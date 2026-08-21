package sync_test

import (
	"context"
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
	root := t.TempDir()
	workspaceRoot := cursorWorkspaceTempDir(t)
	workspace := filepath.Join(workspaceRoot, "Code", "app")
	projectDir := encodeCursorProjectDir(workspace)
	sessionID := "dddddddd-eeee-4fff-8000-111111111111"
	path := filepath.Join(root, projectDir, "agent-transcripts", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"role":"user","message":{"content":"issue 1418"}}`+"\n",
	), 0o644))

	d := dbtest.OpenTestDB(t)
	e := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:   "local",
	})
	e.SyncAll(context.Background(), nil)
	first, err := d.GetSession(context.Background(), "cursor:"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Empty(t, first.Cwd)
	e.Close()

	require.NoError(t, os.MkdirAll(workspace, 0o755))
	filtered := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs:          map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Machine:            "local",
		IncludeCwdPrefixes: []string{workspaceRoot},
	})
	t.Cleanup(func() { filtered.Close() })
	stats := filtered.SyncAll(context.Background(), nil)
	assert.Zero(t, stats.Failed)
	assert.False(t, stats.Aborted)

	second, err := d.GetSession(context.Background(), "cursor:"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, workspace, second.Cwd)
}
