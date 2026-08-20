package parser

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRooCodeSessionMistakeLimitPairing(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-mistake")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-mistake",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Mistake limit test",
		TokensIn:  10,
		TokensOut: 5,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// Sequence: user task, command ask, mistake_limit_reached ask
	// mistake_limit_reached appears as an ask type in real RooCode data.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Run the tests",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "command",
			Text:      "npm test",
		},
		{
			Timestamp: 1688836870000,
			Type:      "ask",
			Ask:       "mistake_limit_reached",
			Text:      "Too many mistakes, stopping",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, msgs, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// user task + execute_command tool call = 2
	assert.Equal(t, 2, sess.MessageCount)

	// Verify the tool call has an errored ResultEvent.
	require.Len(t, msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	assert.Equal(t, "execute_command", tc.ToolName)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "errored", tc.ResultEvents[0].Status)
	assert.Equal(t, "Too many mistakes, stopping", tc.ResultEvents[0].Content)
}

func TestParseRooCodeSessionAPIReqFailedPairing(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-apifail")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-apifail",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "API req failed test",
		TokensIn:  10,
		TokensOut: 5,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// Sequence: user task, MCP tool call, api_req_failed ask
	// api_req_failed appears as an ask type in real RooCode data.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Search for docs",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "use_mcp_server",
			Text:      `{"tool":"brave-search","serverName":"brave","query":"React"}`,
		},
		{
			Timestamp: 1688836870000,
			Type:      "ask",
			Ask:       "api_req_failed",
			Text:      "API request failed: 401 Unauthorized",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, msgs, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// user task + MCP tool call = 2
	assert.Equal(t, 2, sess.MessageCount)

	// Verify the MCP tool call has an errored ResultEvent.
	require.Len(t, msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	assert.Equal(t, "brave-search", tc.ToolName)
	assert.Equal(t, "MCP", tc.Category)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "errored", tc.ResultEvents[0].Status)
	assert.Equal(t, "API request failed: 401 Unauthorized", tc.ResultEvents[0].Content)
}

func TestRooCodeIsToolErrorEvent(t *testing.T) {
	tests := []struct {
		say  string
		ask  string
		want bool
	}{
		{"mistake_limit_reached", "", true},
		{"api_req_failed", "", true},
		{"error", "", false},
		{"text", "", false},
		{"", "command", false},
		{"", "tool", false},
	}

	for _, tt := range tests {
		got := rooCodeIsToolErrorEvent(tt.say, tt.ask)
		assert.Equal(t, tt.want, got,
			"rooCodeIsToolErrorEvent(%q, %q) = %v, want %v",
			tt.say, tt.ask, got, tt.want)
	}
}

func TestParseRooCodeSessionErrorNotPairedToCompletedRead(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-err-completed")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-err-completed",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Error after completed read",
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// The readFile call completes via its embedded content, so the
	// later unrelated error must not attach to it as a failure.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Read the config",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"readFile","path":"config.json","content":"{}"}`,
		},
		{
			Timestamp: 1688836870000,
			Type:      "say",
			Say:       "error",
			Text:      "Unrelated provider error",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, msgs, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// user task + readFile + standalone error message = 3.
	assert.Equal(t, 3, sess.MessageCount)

	// The completed readFile keeps exactly its embedded result.
	require.Len(t, msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "completed", tc.ResultEvents[0].Status)

	// The error surfaces as a standalone system message.
	assert.Equal(t, RoleSystem, msgs[2].Role)
	assert.Equal(t, "Unrelated provider error", msgs[2].Content)
}

func TestParseRooCodeSessionErrorNotPairedAcrossNormalTurn(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-err-stale")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-err-stale",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Error after intervening turn",
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// A normal assistant message after the appliedDiff call ends
	// that call's turn; the later diff_error must not reach back
	// across it and mark the call as failed.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Do the edit",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"appliedDiff","path":"src/main.go","diff":"..."}`,
		},
		{
			Timestamp: 1688836865000,
			Type:      "say",
			Say:       "text",
			Text:      "The edit is applied, moving on.",
		},
		{
			Timestamp: 1688836870000,
			Type:      "say",
			Say:       "diff_error",
			Text:      "Stale diff error from a later attempt",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, msgs, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// task + appliedDiff + assistant text + standalone error = 4.
	assert.Equal(t, 4, sess.MessageCount)

	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Empty(t, msgs[1].ToolCalls[0].ResultEvents,
		"error across a normal turn must not attach to the tool call")

	assert.Equal(t, RoleSystem, msgs[3].Role)
	assert.Equal(t, "Stale diff error from a later attempt", msgs[3].Content)
}

func TestParseRooCodeSessionErrorPairsMostRecentTool(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-err-recent")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-err-recent",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Error pairs most recent tool",
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// A command is still pending (no output yet) when a later
	// appliedDiff fails. The diff_error belongs to the edit, not to
	// the older command.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Run the server then edit",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "command",
			Text:      "npm run dev",
		},
		{
			Timestamp: 1688836865000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"appliedDiff","path":"src/main.go","diff":"..."}`,
		},
		{
			Timestamp: 1688836870000,
			Type:      "say",
			Say:       "diff_error",
			Text:      "Search block not found in file",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	_, msgs, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// The pending command stays unresolved.
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "execute_command", msgs[1].ToolCalls[0].ToolName)
	assert.Empty(t, msgs[1].ToolCalls[0].ResultEvents,
		"stale command must not absorb the later diff_error")

	// The diff_error pairs with the most recent tool, appliedDiff.
	require.Len(t, msgs[2].ToolCalls, 1)
	tc := msgs[2].ToolCalls[0]
	assert.Equal(t, "appliedDiff", tc.ToolName)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "errored", tc.ResultEvents[0].Status)
	assert.Equal(t, "Search block not found in file", tc.ResultEvents[0].Content)
}
