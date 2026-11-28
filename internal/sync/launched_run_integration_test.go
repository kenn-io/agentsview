package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func codexStartup(id string) string {
	return "OpenAI Codex v0.1.0\n--------\nworkdir: /workspace/project\nsession id: " + id + "\n--------\nuser\nReview\n"
}

func launchedRunFixture(t *testing.T, id string) (claudeRoot, codexRoot, launcher, content string) {
	t.Helper()
	root := t.TempDir()
	claudeRoot, codexRoot = filepath.Join(root, "claude"), filepath.Join(root, "codex")
	launcher = filepath.Join(claudeRoot, "project", "launcher.jsonl")
	rollout := filepath.Join(codexRoot, "rollout-2026-10-01T10-00-00-"+id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(launcher), 0o700))
	require.NoError(t, os.MkdirAll(codexRoot, 0o700))
	content = `{"type":"user","uuid":"u1","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"Review"}}` + "\n" +
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-10-01T10:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":"codex exec review"}}]}}` + "\n"
	require.NoError(t, os.WriteFile(launcher, []byte(content), 0o600))
	require.NoError(t, os.WriteFile(rollout, []byte(testjsonl.CodexSessionMetaJSON(id, "/workspace/project", "codex_exec", "2026-10-01T10:00:00Z")+"\n"+testjsonl.CodexMsgJSON("user", "Review", "2026-10-01T10:00:00Z")+"\n"), 0o600))
	return claudeRoot, codexRoot, launcher, content
}

func launchedRunResult(id string) string {
	return `{"type":"user","uuid":"r1","parentUuid":"a1","timestamp":"2026-10-01T10:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":` + strconv.Quote(codexStartup(id)) + `}]}}` + "\n"
}

func TestLaunchedRunLinksAfterIncrementalResult(t *testing.T) {
	const id = "019eb791-cf7d-75c1-8439-9ed74c122c80"
	claudeRoot, codexRoot, launcher, content := launchedRunFixture(t, id)
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeRoot}, parser.AgentCodex: {codexRoot}}, Machine: "local"})
	t.Cleanup(func() { engine.Close() })
	engine.SyncAll(t.Context(), nil)
	children, err := database.GetChildSessions(t.Context(), "launcher")
	require.NoError(t, err)
	assert.Empty(t, children)
	content += launchedRunResult(id)
	require.NoError(t, os.WriteFile(launcher, []byte(content), 0o600))
	stats := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 1, stats.LinksUpdated)
	children, err = database.GetChildSessions(t.Context(), "launcher")
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, "codex:"+id, children[0].ID)
	count, err := database.ToolCallCount(t.Context(), "launcher")
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	messages, err := database.GetAllMessages(t.Context(), "launcher")
	require.NoError(t, err)
	var calls []db.ToolCall
	for _, msg := range messages {
		calls = append(calls, msg.ToolCalls...)
	}
	require.Len(t, calls, 1)
	assert.Equal(t, "codex:"+id, calls[0].SubagentSessionID)
}

func TestCodexLaunchesClaudeOnRemoteMachine(t *testing.T) {
	const launcherID = "019eb791-cf7d-75c1-8439-9ed74c122c80"
	const childID = "12345678-1234-1234-1234-123456789abc"
	root := t.TempDir()
	claudeRoot, codexRoot := filepath.Join(root, "claude"), filepath.Join(root, "codex")
	require.NoError(t, os.MkdirAll(filepath.Join(claudeRoot, "project"), 0o700))
	require.NoError(t, os.MkdirAll(codexRoot, 0o700))
	child := `{"type":"user","uuid":"u1","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"Review"}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(claudeRoot, "project", childID+".jsonl"), []byte(child), 0o600))
	content := testjsonl.CodexSessionMetaJSON(launcherID, "/workspace/project", "cli", "2026-10-01T10:00:00Z") + "\n" +
		testjsonl.CodexMsgJSON("user", "Review", "2026-10-01T10:00:00Z") + "\n" +
		testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call-1", map[string]any{"cmd": "claude -p --session-id " + childID}, "2026-10-01T10:00:01Z") + "\n"
	rollout := filepath.Join(codexRoot, "rollout-2026-10-01T10-00-00-"+launcherID+".jsonl")
	require.NoError(t, os.WriteFile(rollout, []byte(content), 0o600))
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeRoot}, parser.AgentCodex: {codexRoot}}, Machine: "remote", IDPrefix: "remote~"})
	t.Cleanup(func() { engine.Close() })
	engine.SyncAll(t.Context(), nil)
	children, err := database.GetChildSessions(t.Context(), "remote~codex:"+launcherID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, "remote~"+childID, children[0].ID)
}

func TestLaunchedRunScopedReparse(t *testing.T) {
	ctx := t.Context()
	const id = "019eb791-cf7d-75c1-8439-9ed74c122c80"
	claudeRoot, codexRoot, launcher, content := launchedRunFixture(t, id)
	require.NoError(t, os.WriteFile(launcher, []byte(content+launchedRunResult(id)), 0o600))
	archive := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.OpenIsolated(ctx, archive)
	require.NoError(t, err)
	cfg := sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeRoot}, parser.AgentCodex: {codexRoot}}, Machine: "local"}
	engine := sync.NewEngine(ctx, database, cfg)
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	engine.Close()
	require.NoError(t, database.Close())
	raw, err := sql.Open("sqlite3", archive)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `UPDATE tool_calls SET subagent_session_id=NULL; UPDATE sessions SET parent_session_id=NULL, spawn_linked=0, data_version=193; PRAGMA user_version=193`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	assert.ElementsMatch(t, []parser.AgentType{parser.AgentClaude, parser.AgentCodex}, db.ScopedDataVersionProviders(193, 194))
	database, err = db.OpenIsolated(ctx, archive)
	require.NoError(t, err)
	defer database.Close()
	engine = sync.NewEngine(ctx, database, cfg)
	defer engine.Close()
	stats := engine.SyncAll(ctx, nil)
	require.Zero(t, stats.Failed)
	assert.Positive(t, stats.Synced)
	children, err := database.GetChildSessions(ctx, "launcher")
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, "codex:"+id, children[0].ID)
}

func TestCodexLaunchedRunLateResult(t *testing.T) {
	for _, mode := range []string{"all", "staged"} {
		t.Run(mode, func(t *testing.T) {
			const parent = "019eb791-cf7d-75c1-8439-9ed74c122c80"
			const child = "12345678-1234-1234-1234-123456789abc"
			root := t.TempDir()
			launcher := filepath.Join(root, "rollout-2026-10-01T10-00-00-"+parent+".jsonl")
			run := filepath.Join(root, "rollout-2026-10-01T10-00-01-"+child+".jsonl")
			content := testjsonl.CodexSessionMetaJSON(parent, "/workspace/project", "cli", "2026-10-01T10:00:00Z") + "\n" + testjsonl.CodexMsgJSON("user", "Review", "2026-10-01T10:00:00Z") + "\n" + testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call-1", map[string]any{"cmd": "codex exec review"}, "2026-10-01T10:00:01Z") + "\n"
			require.NoError(t, os.WriteFile(launcher, []byte(content), 0o600))
			require.NoError(t, os.WriteFile(run, []byte(testjsonl.CodexSessionMetaJSON(child, "/workspace/project", "codex_exec", "2026-10-01T10:00:01Z")+"\n"+testjsonl.CodexMsgJSON("user", "Review", "2026-10-01T10:00:02Z")+"\n"), 0o600))
			database := dbtest.OpenTestDB(t)
			cfg := sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local"}
			if mode == "staged" {
				cfg.StagedCodexParseMinBytes = 1
			}
			engine := sync.NewEngine(t.Context(), database, cfg)
			t.Cleanup(func() { engine.Close() })
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			children, err := database.GetChildSessions(t.Context(), "codex:"+parent)
			require.NoError(t, err)
			assert.Empty(t, children)
			content += testjsonl.CodexFunctionCallOutputJSON("call-1", map[string]any{"output": codexStartup(child)}, "2026-10-01T10:00:03Z") + "\n"
			require.NoError(t, os.WriteFile(launcher, []byte(content), 0o600))
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			children, err = database.GetChildSessions(t.Context(), "codex:"+parent)
			require.NoError(t, err)
			require.Len(t, children, 1)
			assert.Equal(t, "codex:"+child, children[0].ID)
			count, err := database.ToolCallCount(t.Context(), "codex:"+parent)
			require.NoError(t, err)
			assert.Equal(t, 1, count)
		})
	}
}
