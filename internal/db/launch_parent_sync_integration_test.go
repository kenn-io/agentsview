package db_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestLaunchParentClaudeChildInLaterSync(t *testing.T) {
	ctx := t.Context()
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	project := filepath.Join(root, "project")
	parent := "00000000-0000-4000-8000-000000000001"
	child := "00000000-0000-4000-8000-000000000002"
	dbtest.WriteTestFile(t, filepath.Join(project, parent+".jsonl"), []byte(`{"type":"assistant","uuid":"a1","sessionId":"`+parent+`","cwd":"/repo","timestamp":"2026-01-02T03:04:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"claude -p hi"}}]}}
`))
	engine := sync.NewEngine(ctx, database, sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}})
	t.Cleanup(engine.Close)
	first := engine.SyncAll(ctx, nil)
	require.Zero(t, first.Failed)
	require.Equal(t, 1, first.Synced)
	dbtest.WriteTestFile(t, filepath.Join(project, child+".jsonl"), []byte(`{"type":"user","uuid":"u1","sessionId":"`+child+`","entrypoint":"sdk-cli","cwd":"/repo","timestamp":"2026-01-02T03:04:03Z","message":{"role":"user","content":"hi"}}
`))
	second := engine.SyncAll(ctx, nil)
	require.Zero(t, second.Failed)
	require.Equal(t, 1, second.Synced)
	snapshots, err := database.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snapshots, 2)
	stored, err := database.GetSession(ctx, child)
	require.NoError(t, err)
	require.Equal(t, "sdk-cli", stored.Entrypoint)
	require.Equal(t, "/repo", stored.Cwd)
	link, linkErr := database.GetSessionExternalParent(ctx, child)
	_, err = database.LinkSubagentSessionsForSessions(ctx, []string{child})
	require.NoError(t, err)
	repaired, err := database.GetSessionExternalParent(ctx, child)
	require.NoError(t, err)
	require.Equal(t, parent, repaired.ParentSessionID)
	require.NoError(t, linkErr)
	require.Equal(t, parent, link.ParentSessionID)
}
