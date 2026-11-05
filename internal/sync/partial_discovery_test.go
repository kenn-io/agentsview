package sync

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestPartialDiscoveryImportsHealthyLocalHistory(t *testing.T) {
	const id = "22222222-2222-4222-8222-222222222222"
	const stamp = "2026-01-02T00:00:00Z"
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentCodex, parser.AgentCursor, parser.AgentIcodemate} {
		t.Run(string(agent), func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
			}))
			t.Cleanup(remote.Close)
			t.Setenv("AWS_S3_ENDPOINT", remote.URL)
			t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
			t.Setenv("AWS_REGION", "us-east-1")
			local := t.TempDir()
			relative := "project/" + id + ".jsonl"
			content := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID(stamp, "healthy local history", id).String()
			storedID := id
			if agent == parser.AgentIcodemate {
				storedID = "icodemate:" + id
			}
			if agent == parser.AgentCodex {
				storedID = "codex:" + id
				relative = "rollout-2026-01-02T00-00-00-" + id + ".jsonl"
				content = testjsonl.CodexSessionMetaJSON(id, "/demo/project", "codex_cli_rs", stamp) + "\n" + testjsonl.CodexMsgJSON("user", "healthy local history", stamp) + "\n"
			}
			if agent == parser.AgentCursor {
				relative = "project/agent-transcripts/" + id + ".jsonl"
				content = "{\"role\":\"user\",\"message\":{\"content\":\"healthy local history\"}}\n"
				storedID = "cursor:" + id
			}
			dbtest.WriteTestFile(t, filepath.Join(local, filepath.FromSlash(relative)), []byte(content))
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {"s3://bucket/sessions", local}}, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			assert.False(t, stats.ProcessingComplete())
			messages, err := database.GetAllMessages(t.Context(), storedID)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "healthy local history", messages[0].Content)
		})
	}
}

func TestClaudeSyncIgnoresToolOutput(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("fixture needs enforced POSIX directory permissions")
	}
	root := t.TempDir()
	content := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "readable history", "session").String()
	dbtest.WriteTestFile(t, filepath.Join(root, "project", "session.jsonl"), []byte(content))
	tools := filepath.Join(root, "project", "session", "tool-results")
	require.NoError(t, os.MkdirAll(tools, 0o755))
	require.NoError(t, os.Chmod(tools, 0))
	t.Cleanup(func() { _ = os.Chmod(tools, 0o755) })
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	assert.True(t, stats.ProcessingComplete(), "%+v", stats)
	assert.Zero(t, stats.Failed)
	messages, err := database.GetAllMessages(t.Context(), "session")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "readable history", messages[0].Content)
}

func TestCursorSyncKeepsReadableChildren(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("fixture needs enforced POSIX directory permissions")
	}
	root := t.TempDir()
	for _, child := range []struct{ parent, id, message string }{{"A", "11111111-1111-4111-8111-111111111111", "morning child-a"}, {"C", "33333333-3333-4333-8333-333333333333", "morning child-c"}} {
		dbtest.WriteTestFile(t, filepath.Join(root, "project", "agent-transcripts", child.parent, "subagents", child.id+".jsonl"), []byte(`{"role":"user","message":{"content":"`+child.message+`"}}`+"\n"))
	}
	blocked := filepath.Join(root, "project", "agent-transcripts", "B", "subagents")
	require.NoError(t, os.MkdirAll(blocked, 0o755))
	require.NoError(t, os.Chmod(blocked, 0))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCursor: {root}}, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	assert.False(t, stats.ProcessingComplete(), "the unreadable transcript scope must remain incomplete")
	for _, expected := range []struct{ id, message string }{{"cursor:11111111-1111-4111-8111-111111111111", "morning child-a"}, {"cursor:33333333-3333-4333-8333-333333333333", "morning child-c"}} {
		messages, err := database.GetAllMessages(t.Context(), expected.id)
		require.NoError(t, err)
		if assert.Len(t, messages, 1, expected.id) {
			assert.Equal(t, expected.message, messages[0].Content)
		}
	}
}
