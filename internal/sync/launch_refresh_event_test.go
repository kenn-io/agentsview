package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestLaunchRefreshEmitsSessions(t *testing.T) {
	ctx := t.Context()
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	parent := "00000000-0000-4000-8000-000000000001"
	path := filepath.Join(root, "project", parent+".jsonl")
	dbtest.WriteTestFile(t, path, []byte(`{"type":"user","uuid":"u1","sessionId":"`+parent+`","cwd":"/repo","timestamp":"2026-01-02T03:03:59Z","message":{"role":"user","content":"run it"}}
`))
	emitter := &fakeEmitter{}
	engine := sync.NewEngine(ctx, database, sync.EngineConfig{Machine: "local", Emitter: emitter, AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(ctx, nil).Synced)
	started := "2026-01-02T03:04:03Z"
	require.NoError(t, database.UpsertSession(ctx, db.Session{ID: "child", Project: "project", Machine: "local", Agent: "codex", Cwd: "/repo", StartedAt: &started, SessionKind: "non-interactive"}))
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString(`{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"` + parent + `","cwd":"/repo","timestamp":"2026-01-02T03:04:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"codex exec hi"}}]}}
`)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	before := len(emitter.got())
	require.NoError(t, engine.SyncSingleSessionContext(ctx, parent))
	link, err := database.GetSessionExternalParent(ctx, "child")
	require.NoError(t, err)
	require.Equal(t, parent, link.ParentSessionID)
	require.Contains(t, emitter.got()[before:], "sessions")
}
