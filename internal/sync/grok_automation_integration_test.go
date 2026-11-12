package sync_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestGrokNativeSubagentsSurviveParentLoss(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.CopyFS(root, os.DirFS(filepath.Join("..", "parser", "testdata", "grok-build", "subagents"))))
	parentDir := filepath.Join(root, "%2Fworkspace%2Fproject", "019f6000-0000-7000-8000-000000000010")
	children := []struct{ cwd, id, kind string }{
		{"%2Fworkspace%2Fproject", "019f6000-0000-7000-8000-000000000011", "subagent"},
		{"%2Fworkspace%2Fproject", "019f6000-0000-7000-8000-000000000012", "subagent_resume"},
		{"%2Fworkspace%2Fworktree-child", "019f6000-0000-7000-8000-000000000015", "subagent_fork"},
	}
	for _, child := range children {
		dir := filepath.Join(root, child.cwd, child.id)
		if child.kind == "subagent_fork" {
			path := filepath.Join(dir, "summary.json")
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(string(body), `"subagent"`, `"subagent_fork"`)), 0o600))
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "chat_history.jsonl"), []byte(`{"type":"user","content":"You are a code reviewer. Review this change."}`+"\n"), 0o600))
	}
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGrok: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	assertChildren := func() {
		t.Helper()
		for _, child := range children {
			stored, err := database.GetSession(t.Context(), "grok:"+child.id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, "subagent", stored.RelationshipType, child.kind)
			assert.Empty(t, stored.SessionKind, child.kind)
			assert.True(t, stored.IsAutomated, child.kind)
		}
	}
	require.Equal(t, 6, engine.SyncAll(t.Context(), nil).Synced)
	assertChildren()
	// All removal targets are fixture directories beneath this test's temporary root.
	require.NoError(t, os.RemoveAll(filepath.Join(parentDir, "subagents")))
	require.GreaterOrEqual(t, engine.SyncAll(t.Context(), nil).Synced, 3)
	assertChildren()
	require.NoError(t, database.ForceBackfillIsAutomated(t.Context()))
	assertChildren()
	require.NoError(t, database.DeleteSession(t.Context(), "grok:019f6000-0000-7000-8000-000000000010"))
	require.NoError(t, os.RemoveAll(parentDir))
	engine.SyncAll(t.Context(), nil)
	require.NoError(t, database.ForceBackfillIsAutomated(t.Context()))
	assertChildren()
	require.False(t, engine.ResyncAll(t.Context(), nil).Aborted)
	require.NoError(t, database.ForceBackfillIsAutomated(t.Context()))
	assertChildren()
}

func TestGrokPromptContextSubagentMatchesParseDiff(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "cwd-key", "sess-1")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "summary.json"),
		[]byte(`{
			"summary":"Inspect a function",
			"firstPrompt":"Explain this function",
			"createdAt":"2026-07-08T10:00:00Z"
		}`),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "prompt_context.json"),
		[]byte(`{"is_non_interactive":false}`),
		0o644,
	))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGrok: {root}},
		Machine:   "local",
	})

	stats := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)
	before, err := database.GetSession(t.Context(), "grok:sess-1")
	require.NoError(t, err)
	require.NotNil(t, before)
	require.False(t, before.IsAutomated)

	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "prompt_context.json"),
		[]byte(`{"is_non_interactive":true}`),
		0o644,
	))
	stats = engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)

	after, err := database.GetSession(t.Context(), "grok:sess-1")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, "non-interactive", after.SessionKind)
	assert.False(t, after.IsAutomated)
	assert.Equal(t, "subagent", after.RelationshipType)

	diff := sync.NewDiffEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGrok: {root}}, Machine: "local",
	})
	t.Cleanup(diff.Close)
	report, err := diff.ParseDiff(t.Context(), sync.ParseDiffOptions{Agents: []parser.AgentType{parser.AgentGrok}})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Totals.Identical)
	assert.False(t, report.HasFailures())
}

func TestGrokCompactedSessionKeepsHistoryAndToolOutput(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.CopyFS(root, os.DirFS(filepath.Join("..", "parser", "testdata", "grok-build", "compacted"))))
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGrok: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	const id = "grok:019f6000-0000-7000-8000-000000000020"
	session, err := database.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 2, session.CompactionCount)

	messages, err := database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	outputs := map[string]string{}
	for _, msg := range messages {
		assert.False(t, msg.Role == "user" && msg.Content == "", "result carriers are paired away")
		for _, call := range msg.ToolCalls {
			outputs[call.ToolUseID] = call.ResultContent
		}
	}
	// Two calls ran before the first compaction, one between, one after;
	// each serves its own output from the messages read.
	assert.Equal(t, map[string]string{
		"call-0001-a-0": "1|package fetchkit\n2|\n3|func (c *Client) Get(url string) (*Response, error) {",
		"call-0002-b-0": `{"type":"EditsApplied","EditsApplied":{"path":"/workspace/fetchkit/fetch.go","count":1}}`,
		"call-0002-b-1": "--- FAIL: TestGetRetries (0.00s)\n    fetch_test.go:18: doWithRetry undefined\nFAIL",
		"call-0003-c-0": "No matches found.",
		"call-0004-d-0": "1|package fetchkit\n2|\n3|func (c *Client) doWithRetry(req *Request, n int) (*Response, error) {",
	}, outputs)
}

func TestGrokInterruptedCallKeepsChatHistoryResult(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "%2Fworkspace%2Ffetchkit", "019f6000-0000-7000-8000-000000000023")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sessionDir, "summary.json"), []byte(
		`{"info":{"id":"019f6000-0000-7000-8000-000000000023","cwd":"/workspace/fetchkit"},"created_at":"2026-07-23T09:46:40Z"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sessionDir, "chat_history.jsonl"), []byte(
		`{"type":"user","content":"Run the tests"}`+"\n"+
			`{"type":"assistant","content":"","tool_calls":[{"id":"call-9","name":"run_terminal_command","arguments":"{\"command\":\"go test ./...\"}"}]}`+"\n"+
			`{"type":"tool_result","tool_call_id":"call-9","content":"Command interrupted by the user."}`+"\n"), 0o600))
	// The stream recorded the call starting and nothing after it.
	require.NoError(t, os.WriteFile(filepath.Join(sessionDir, "updates.jsonl"), []byte(
		`{"timestamp":1784800001,"method":"session/update","params":{"sessionId":"019f6000-0000-7000-8000-000000000023","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Run the tests"}}}}`+"\n"+
			`{"timestamp":1784800003,"method":"session/update","params":{"sessionId":"019f6000-0000-7000-8000-000000000023","update":{"sessionUpdate":"tool_call","toolCallId":"call-9","title":"run_terminal_command","rawInput":{"command":"go test ./..."}}}}`+"\n"), 0o600))
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGrok: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	messages, err := database.GetAllMessages(t.Context(), "grok:019f6000-0000-7000-8000-000000000023")
	require.NoError(t, err)
	var outputs []string
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			outputs = append(outputs, call.ResultContent)
		}
	}
	assert.Equal(t, []string{"Command interrupted by the user."}, outputs)
}
