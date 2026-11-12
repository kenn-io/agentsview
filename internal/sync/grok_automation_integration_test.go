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

func TestGrokSyncMidTurnThenFinishedFillsToolOutput(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "%2Fworkspace%2Ffetchkit", "019f6000-0000-7000-8000-000000000025")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(sessionDir, name), []byte(body), 0o600))
	}
	write("summary.json", `{"info":{"id":"019f6000-0000-7000-8000-000000000025","cwd":"/workspace/fetchkit"},"created_at":"2026-07-23T09:46:40Z"}`)
	update := func(ts, body string) string {
		return `{"timestamp":` + ts + `,"method":"session/update","params":{"sessionId":"019f6000-0000-7000-8000-000000000025","update":` + body + `}}` + "\n"
	}
	started := update("1784800001", `{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Run the tests"}}`) +
		update("1784800002", `{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"run_terminal_command","rawInput":{"command":"go test ./..."}}`)
	chatStarted := `{"type":"user","content":"Run the tests"}` + "\n" +
		`{"type":"assistant","content":"","tool_calls":[{"id":"call-1","name":"run_terminal_command","arguments":"{\"command\":\"go test ./...\"}"}]}` + "\n"
	write("updates.jsonl", started)
	write("chat_history.jsonl", chatStarted)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGrok: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	// The command finishes after the first sync stored its call.
	write("updates.jsonl", started+
		update("1784800005", `{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"ok  fetchkit"}}]}`)+
		update("1784800006", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Tests pass."}}`))
	write("chat_history.jsonl", chatStarted+
		`{"type":"tool_result","tool_call_id":"call-1","content":"ok  fetchkit"}`+"\n"+
		`{"type":"assistant","content":"Tests pass."}`+"\n")
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	messages, err := database.GetAllMessages(t.Context(), "grok:019f6000-0000-7000-8000-000000000025")
	require.NoError(t, err)
	var outputs, dialogue []string
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			outputs = append(outputs, call.ResultContent)
		}
		if msg.Content != "" {
			dialogue = append(dialogue, msg.Content)
		}
	}
	// The result pairs onto the call the first sync stored.
	assert.Equal(t, []string{"ok  fetchkit"}, outputs)
	assert.Equal(t, []string{"Run the tests", "[Bash]\n$ go test ./...", "Tests pass."}, dialogue)
}

func TestGrokMissingCheckpointKeepsArchivedSummary(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.CopyFS(root, os.DirFS(filepath.Join("..", "parser", "testdata", "grok-build", "compacted"))))
	sessionDir := filepath.Join(root, "%2Fworkspace%2Ffetchkit", "019f6000-0000-7000-8000-000000000020")
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGrok: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	const id = "grok:019f6000-0000-7000-8000-000000000020"
	before, err := database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)

	// The first compaction's summary lives only in its checkpoint file. The
	// file goes away and the session goes on.
	require.NoError(t, os.Remove(filepath.Join(sessionDir, "compaction_checkpoints", "0a7c0000-0000-4000-8000-0000000000c1.json")))
	chat, err := os.OpenFile(filepath.Join(sessionDir, "chat_history.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = chat.WriteString(`{"type":"user","content":"Add a jitter test too."}` + "\n")
	require.NoError(t, err)
	require.NoError(t, chat.Close())
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	after, err := database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, after, len(before)+1, "the new turn is added")
	for i := range before {
		assert.Equal(t, before[i].Content, after[i].Content, "archived row %d keeps its content", i)
	}
	assert.Equal(t, "Add a jitter test too.", after[len(after)-1].Content)
}
