package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRooCodeSession(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-123")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	// Create history_item.json
	historyItem := rooCodeHistoryItem{
		ID:        "test-task-123",
		Number:    1,
		Timestamp: 1688836851000, // 2023-07-08T08:20:51.000Z
		Task:      "Test task description",
		TokensIn:  100,
		TokensOut: 200,
		TotalCost: 0.05,
		Workspace: "/Users/test/project",
		Mode:      "code",
		Status:    "completed",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// Create ui_messages.json matching real RooCode format:
	// [0] say=text: user's initial task
	// [1] say=text: assistant response
	// [2] ask=tool: assistant tool call
	// [3] say=text: assistant text after tool
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Test task description",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "text",
			Text:      "Let me help you with that.",
		},
		{
			Timestamp: 1688836870000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"readFile","path":"src/main.go","isOutsideWorkspace":false}`,
		},
		{
			Timestamp: 1688836880000,
			Type:      "say",
			Say:       "text",
			Text:      "Here is the result.",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	// Parse
	sess, msgs, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// Assertions
	assert.Contains(t, sess.ID, "roocode:test-task-123")
	assert.Equal(t, AgentRooCode, sess.Agent)

	// Messages: user task (1) + assistant text (1) + assistant tool call (1) + assistant text (1) = 4
	assert.Equal(t, 4, sess.MessageCount)

	// User messages: only the initial task (message [0])
	// tool calls and command_output are NOT user messages
	assert.Equal(t, 1, sess.UserMessageCount)

	// Verify roles
	assert.Equal(t, RoleUser, msgs[0].Role, "message [0] should be user (initial task)")
	assert.Equal(t, RoleAssistant, msgs[1].Role, "message [1] should be assistant")
	assert.Equal(t, RoleAssistant, msgs[2].Role, "message [2] should be assistant (tool call)")
	assert.True(t, msgs[2].HasToolUse, "message [2] should have tool use")

	assert.Equal(t, "/Users/test/project", sess.Cwd)
	assert.Equal(t, "Test task description", sess.SessionName)
	// PeakContextTokens is derived from api_req_started entries,
	// not from the cumulative tokensIn in history_item.json.
	// This test has no api_req_started messages, so peak is 0.
	assert.Equal(t, 0, sess.PeakContextTokens)
	assert.False(t, sess.HasPeakContextTokens)
	assert.Equal(t, 200, sess.TotalOutputTokens)
	assert.True(t, sess.HasTotalOutputTokens)
}

func TestParseRooCodeSessionWithPartialMessages(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-456")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	// Create history_item.json
	historyItem := rooCodeHistoryItem{
		ID:        "test-task-456",
		Number:    2,
		Timestamp: 1688836851000,
		Task:      "Task with partial messages",
		TokensIn:  50,
		TokensOut: 100,
		TotalCost: 0.02,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// Create ui_messages.json with partial messages
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Task with partial messages",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "text",
			Text:      "Partial message...",
			Partial:   true, // Should be skipped
		},
		{
			Timestamp: 1688836870000,
			Type:      "say",
			Say:       "text",
			Text:      "Complete message.",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	// Parse
	sess, msgs, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// Partial message should be skipped: user task + complete = 2
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
}

func TestParseRooCodeSessionWithReasoning(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-789")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-789",
		Number:    3,
		Timestamp: 1688836851000,
		Task:      "Task with reasoning",
		TokensIn:  30,
		TokensOut: 80,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// Message with reasoning text.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Task with reasoning",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "text",
			Text:      "Here is the answer.",
			Reasoning: "I need to think about this...",
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

	// User task + thinking message + assistant answer = 3
	assert.Equal(t, 3, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.True(t, msgs[1].HasThinking, "message [1] should be thinking")
	assert.Contains(t, msgs[1].Content, "I need to think about this...")
	assert.Equal(t, RoleAssistant, msgs[2].Role)
}

func TestParseRooCodeSessionWithAPIConfigModel(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-model")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:            "test-task-model",
		Number:        1,
		Timestamp:     1688836851000,
		Task:          "Model test",
		TokensIn:      100,
		TokensOut:     200,
		CacheReads:    50,
		CacheWrites:   30,
		TotalCost:     0.05,
		Workspace:     "/Users/test/project",
		APIConfigName: "anthropic/claude-sonnet-4",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Model test task",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "text",
			Text:      "Response with model",
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

	// Usage event should have the model, tokens, and cost.
	require.Len(t, sess.UsageEvents, 1)
	assert.Equal(t, "anthropic/claude-sonnet-4", sess.UsageEvents[0].Model)
	assert.Equal(t, "session", sess.UsageEvents[0].Source)
	assert.Equal(t, 100, sess.UsageEvents[0].InputTokens)
	assert.Equal(t, 200, sess.UsageEvents[0].OutputTokens)
	assert.Equal(t, 50, sess.UsageEvents[0].CacheReadInputTokens)
	assert.Equal(t, 30, sess.UsageEvents[0].CacheCreationInputTokens)
	require.NotNil(t, sess.UsageEvents[0].CostUSD)
	assert.Equal(t, 0.05, *sess.UsageEvents[0].CostUSD)

	// Model should be set on every parsed message.
	for _, msg := range msgs {
		assert.Equal(t, "anthropic/claude-sonnet-4", msg.Model,
			"every message should have the model set")
	}
}

func TestParseRooCodeSessionWithoutAPIConfigName(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-no-config")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-no-config",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "No config test",
		TokensIn:  500,
		TokensOut: 150,
		TotalCost: 0.02,
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Task with no config",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "text",
			Text:      "Response",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, _, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	require.Len(t, sess.UsageEvents, 1)
	assert.Equal(t, "", sess.UsageEvents[0].Model)
	assert.Equal(t, "session", sess.UsageEvents[0].Source)
	assert.Equal(t, 500, sess.UsageEvents[0].InputTokens)
	assert.Equal(t, 150, sess.UsageEvents[0].OutputTokens)
	require.NotNil(t, sess.UsageEvents[0].CostUSD)
	assert.Equal(t, 0.02, *sess.UsageEvents[0].CostUSD)
}

func TestParseRooCodeSessionWithProjectExtraction(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-proj")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	// Workspace points to a real git repo-like path.
	historyItem := rooCodeHistoryItem{
		ID:        "test-task-proj",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Project extraction test",
		TokensIn:  50,
		TokensOut: 100,
		Workspace: "/Users/test/my-awesome-project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Project extraction test",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, _, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// Project should be derived from workspace.
	assert.Equal(t, "my_awesome_project", sess.Project)
}

func TestParseRooCodeSessionWithoutMessages(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-empty")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-empty",
		Number:    5,
		Timestamp: 1688836851000,
		Task:      "Empty session",
		TokensIn:  0,
		TokensOut: 0,
		Workspace: "/Users/test/project",
		Status:    "active",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// No ui_messages.json - should still parse successfully.
	sess, _, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	assert.Equal(t, 0, sess.MessageCount)
	assert.Equal(t, "roocode:test-task-empty", sess.ID)
	assert.Equal(t, "Empty session", sess.SessionName)
}

func TestParseRooCodeSessionSkipsMetadataMessages(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-meta")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-meta",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Metadata test",
		TokensIn:  10,
		TokensOut: 20,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// Include api_req_started and checkpoint_saved which should be skipped.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Metadata test task",
		},
		{
			Timestamp: 1688836852000,
			Type:      "say",
			Say:       "api_req_started",
			Text:      "{}",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "text",
			Text:      "Assistant response.",
		},
		{
			Timestamp: 1688836865000,
			Type:      "say",
			Say:       "checkpoint_saved",
			Text:      "",
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

	// Metadata messages should be skipped: user task + assistant = 2
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
}

func TestParseRooCodeSessionToolCalls(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-tools")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:            "test-task-tools",
		Number:        1,
		Timestamp:     1688836851000,
		Task:          "Tool call test",
		TokensIn:      100,
		TokensOut:     200,
		Workspace:     "/Users/test/project",
		APIConfigName: "anthropic/claude-sonnet-4",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// Realistic RooCode interaction with tool calls.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Fix the bug in main.go",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "text",
			Text:      "Let me read the file first.",
		},
		{
			Timestamp: 1688836870000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"readFile","path":"src/main.go","isOutsideWorkspace":false,"content":"/Users/test/project/src/main.go"}`,
		},
		{
			Timestamp: 1688836880000,
			Type:      "say",
			Say:       "text",
			Text:      "I see the bug. Let me fix it.",
		},
		{
			Timestamp: 1688836890000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"appliedDiff","path":"src/main.go","diff":"...","isOutsideWorkspace":false}`,
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

	// User task (1) + assistant text (1) + readFile tool call (1) +
	// assistant text (1) + appliedDiff tool call (1) = 5
	assert.Equal(t, 5, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)

	// Verify tool calls.
	assert.Equal(t, RoleAssistant, msgs[2].Role)
	assert.True(t, msgs[2].HasToolUse)
	require.Len(t, msgs[2].ToolCalls, 1)
	assert.Equal(t, "readFile", msgs[2].ToolCalls[0].ToolName)

	assert.Equal(t, RoleAssistant, msgs[4].Role)
	assert.True(t, msgs[4].HasToolUse)
	require.Len(t, msgs[4].ToolCalls, 1)
	assert.Equal(t, "appliedDiff", msgs[4].ToolCalls[0].ToolName)

	// Model should be set on tool call messages too.
	assert.Equal(t, "anthropic/claude-sonnet-4", msgs[2].Model)
}

func TestParseRooCodeSessionCommandOutput(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-cmd")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-cmd",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Command output test",
		TokensIn:  50,
		TokensOut: 100,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Test command",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "command_output",
			Text:      "test output: all tests passed",
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

	// User task + command output (user/system with tool result) = 2
	assert.Equal(t, 2, sess.MessageCount)
	// command_output has IsSystem=true, so only the initial task counts
	// as a user message.
	assert.Equal(t, 1, sess.UserMessageCount)

	// Command output should be a user message with tool results.
	assert.Equal(t, RoleUser, msgs[1].Role)
	assert.True(t, msgs[1].IsSystem)
	require.Len(t, msgs[1].ToolResults, 1)
	assert.Equal(t, len("test output: all tests passed"),
		msgs[1].ToolResults[0].ContentLength)
	assert.Equal(t, "test output: all tests passed",
		msgs[1].ToolResults[0].ContentRaw)
}

func TestParseRooCodeSessionWithCommandAsk(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-askcmd")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-askcmd",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Command ask test",
		TokensIn:  20,
		TokensOut: 40,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

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
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, msgs, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// User task + command tool call = 2
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)

	// Command should be an assistant tool call.
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "execute_command", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "npm test", msgs[1].ToolCalls[0].InputJSON)
}

func TestParseRooCodeSessionReasoningSay(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-reasoning-say")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:            "test-task-reasoning-say",
		Number:        1,
		Timestamp:     1688836851000,
		Task:          "Reasoning say test",
		TokensIn:      30,
		TokensOut:     80,
		Workspace:     "/Users/test/project",
		APIConfigName: "openai/gpt-5",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// Real RooCode pattern: reasoning text is in the text field
	// when say="reasoning", not in a separate Reasoning field.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Fix the code",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "reasoning",
			Text:      "I need to understand the code structure first.",
		},
		{
			Timestamp: 1688836870000,
			Type:      "say",
			Say:       "text",
			Text:      "Let me fix that.",
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

	// User task + thinking message + assistant text = 3
	assert.Equal(t, 3, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)

	// say="reasoning" should produce a thinking block.
	assert.True(t, msgs[1].HasThinking,
		"say=reasoning message should be treated as thinking")
	assert.Contains(t, msgs[1].Content,
		"I need to understand the code structure first.")

	// Model should be set on the thinking message.
	assert.Equal(t, "openai/gpt-5", msgs[1].Model)
}

func TestParseRooCodeSessionSubtaskTree(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-child")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:           "test-task-child",
		ParentTaskID: "parent-task-uuid-1234",
		RootTaskID:   "root-task-uuid-5678",
		Number:       2,
		Timestamp:    1688836851000,
		Task:         "Subtask: fix the tests",
		TokensIn:     50,
		TokensOut:    30,
		Workspace:    "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Subtask: fix the tests",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, _, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// Should be wired as a subagent of the parent task.
	assert.Equal(t, "roocode:parent-task-uuid-1234", sess.ParentSessionID)
	assert.Equal(t, RelSubagent, sess.RelationshipType)
}

func TestParseRooCodeSessionWithoutParentTask(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-root")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-root",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Root task",
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

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Root task",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, _, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// No parent — should not have a relationship.
	assert.Equal(t, "", sess.ParentSessionID)
	assert.Equal(t, RelNone, sess.RelationshipType)
}

func TestParseRooCodeSessionNewSayTypes(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-new-says")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-new-says",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "New say types test",
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

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "New say types test",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "error",
			Text:      "Something went wrong",
		},
		{
			Timestamp: 1688836870000,
			Type:      "say",
			Say:       "subtask_result",
			Text:      "Subtask completed successfully",
		},
		{
			Timestamp: 1688836880000,
			Type:      "say",
			Say:       "condense_context",
			Text:      "Context condensed",
		},
		{
			Timestamp: 1688836890000,
			Type:      "say",
			Say:       "text",
			Text:      "Final response",
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

	// user task + error (system) + subtask_result (system) +
	// condense_context (system) + assistant text = 5
	assert.Equal(t, 5, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)

	// Verify error is a system message with content preserved.
	assert.Equal(t, RoleSystem, msgs[1].Role)
	assert.True(t, msgs[1].IsSystem)
	assert.Equal(t, "Something went wrong", msgs[1].Content,
		"error message content should be preserved")
	assert.Equal(t, RoleSystem, msgs[2].Role)
	assert.Equal(t, RoleSystem, msgs[3].Role)
	assert.Equal(t, RoleAssistant, msgs[4].Role)
}

func TestParseRooCodeSessionSkillTool(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-skill")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-skill",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Skill test",
		TokensIn:  20,
		TokensOut: 10,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Load the skill",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"skill","skill":"obsidian","description":"Adding new Command Palette actions"}`,
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

	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)

	// Verify skill tool call with SkillName extracted.
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "skill", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "obsidian", msgs[1].ToolCalls[0].SkillName)
}

func TestParseRooCodeSessionSkillToolFallbackName(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-skill-name")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-skill-name",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Skill name fallback test",
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

	// Uses "name" field instead of "skill".
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Load the skill",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"skill","name":"frontend-design"}`,
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

	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "frontend-design", msgs[1].ToolCalls[0].SkillName)
}

func TestParseRooCodeSessionSkillFromReadFile(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-skill-read")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-skill-read",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Skill from readFile test",
		TokensIn:  20,
		TokensOut: 10,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Load the skill",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"readFile","path":".roo/skills/obsidian/SKILL.md"}`,
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

	// readFile to SKILL.md should be detected as a skill via
	// inferToolSkillName → isCursorSkillReadTool (matches readFile).
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "readFile", msgs[1].ToolCalls[0].ToolName)
	assert.NotEmpty(t, msgs[1].ToolCalls[0].SkillName,
		"readFile to SKILL.md should infer skill name")
}

func TestParseRooCodeSessionMCPServerTool(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-mcp")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-mcp",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "MCP test",
		TokensIn:  20,
		TokensOut: 10,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "MCP test",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "use_mcp_server",
			Text:      `{"tool":"search","serverName":"my-server","query":"test"}`,
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
	assert.Equal(t, 1, sess.UserMessageCount)

	// Verify MCP tool call.
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "search", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "MCP", msgs[1].ToolCalls[0].Category)
}

func TestParseRooCodeSessionNewTaskSubagentLink(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-parent")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	childID1 := "child-task-uuid-1111"
	childID2 := "child-task-uuid-2222"

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-parent",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Orchestrator task",
		TokensIn:  100,
		TokensOut: 200,
		Workspace: "/Users/test/project",
		ChildIDs:  []string{childID1, childID2},
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// Messages: user task, assistant text, newTask tool call 1,
	// subtask_result 1, assistant text, newTask tool call 2,
	// subtask_result 2.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Do the thing",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "text",
			Text:      "I will delegate.",
		},
		{
			Timestamp: 1688836870000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"newTask","mode":"Code","content":"Implement feature A"}`,
		},
		{
			Timestamp: 1688836875000,
			Type:      "say",
			Say:       "subtask_result",
			Text:      "Feature A done",
		},
		{
			Timestamp: 1688836880000,
			Type:      "say",
			Say:       "text",
			Text:      "Now let me delegate the docs.",
		},
		{
			Timestamp: 1688836890000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"newTask","mode":"Documentation Writer","content":"Write docs for feature A"}`,
		},
		{
			Timestamp: 1688836895000,
			Type:      "say",
			Say:       "subtask_result",
			Text:      "Docs done",
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

	// No parent — this is the orchestrator.
	assert.Equal(t, "", sess.ParentSessionID)

	// Find the two newTask tool call messages.
	var newTaskCalls []ParsedToolCall
	for _, msg := range msgs {
		for _, tc := range msg.ToolCalls {
			if tc.ToolName == "newTask" {
				newTaskCalls = append(newTaskCalls, tc)
			}
		}
	}
	require.Len(t, newTaskCalls, 2, "should have two newTask tool calls")

	// Both newTask calls should have Category="Task" so the
	// frontend renders SubagentInline.
	assert.Equal(t, "Task", newTaskCalls[0].Category,
		"newTask should have Category=Task")
	assert.Equal(t, "Task", newTaskCalls[1].Category,
		"newTask should have Category=Task")

	// First newTask should link to first child.
	assert.Equal(t,
		"roocode:"+childID1,
		newTaskCalls[0].SubagentSessionID,
		"first newTask should link to first childId",
	)
	// Second newTask should link to second child.
	assert.Equal(t,
		"roocode:"+childID2,
		newTaskCalls[1].SubagentSessionID,
		"second newTask should link to second childId",
	)
}

func TestParseRooCodeSessionNewTaskNoChildren(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-no-children")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-no-children",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Task with newTask but no childIds",
		TokensIn:  10,
		TokensOut: 5,
		Workspace: "/Users/test/project",
		// No ChildIDs — older session or no delegation completed.
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Task with newTask but no childIds",
		},
		{
			Timestamp: 1688836870000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"newTask","mode":"Code","content":"Do something"}`,
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

	// newTask call should exist but have no SubagentSessionID.
	for _, msg := range msgs {
		for _, tc := range msg.ToolCalls {
			if tc.ToolName == "newTask" {
				assert.Equal(t, "", tc.SubagentSessionID,
					"no childIds means no subagent link")
			}
		}
	}
}

func TestParseRooCodeSessionNewTaskBoundsCheck(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-bounds")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	// Only one childId but two newTask calls — second should not crash.
	historyItem := rooCodeHistoryItem{
		ID:        "test-task-bounds",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Bounds check",
		TokensIn:  10,
		TokensOut: 5,
		Workspace: "/Users/test/project",
		ChildIDs:  []string{"only-child"},
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Bounds check",
		},
		{
			Timestamp: 1688836870000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"newTask","mode":"Code","content":"First"}`,
		},
		{
			Timestamp: 1688836880000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"newTask","mode":"Code","content":"Second"}`,
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

	// First newTask should link; second should not crash and have empty link.
	var newTaskCalls []ParsedToolCall
	for _, msg := range msgs {
		for _, tc := range msg.ToolCalls {
			if tc.ToolName == "newTask" {
				newTaskCalls = append(newTaskCalls, tc)
			}
		}
	}
	require.Len(t, newTaskCalls, 2)
	assert.Equal(t, "roocode:only-child", newTaskCalls[0].SubagentSessionID)
	assert.Equal(t, "", newTaskCalls[1].SubagentSessionID,
		"second newTask with no remaining childIds should have empty link")
}

func TestParseRooCodeSessionPeakContextFromApiReqStarted(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-peakctx")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	// history_item has cumulative tokensIn (sum across all requests).
	historyItem := rooCodeHistoryItem{
		ID:            "test-task-peakctx",
		Number:        1,
		Timestamp:     1688836851000,
		Task:          "Peak context test",
		TokensIn:      3130608, // cumulative, should NOT be used for peak
		TokensOut:     13958,
		Workspace:     "/Users/test/project",
		APIConfigName: "Z.ai glm-4.7",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// ui_messages.json with api_req_started entries that have
	// per-request tokensIn values (the actual context window).
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Peak context test",
		},
		{
			Timestamp: 1688836852000,
			Type:      "say",
			Say:       "api_req_started",
			Text:      `{"tokensIn":12997,"tokensOut":50,"cacheReads":10000,"cost":0.001}`,
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "text",
			Text:      "Working on it.",
		},
		{
			Timestamp: 1688836865000,
			Type:      "say",
			Say:       "api_req_started",
			Text:      `{"tokensIn":50000,"tokensOut":200,"cacheReads":48000,"cost":0.005}`,
		},
		{
			Timestamp: 1688836870000,
			Type:      "say",
			Say:       "text",
			Text:      "Done.",
		},
		{
			Timestamp: 1688836875000,
			Type:      "say",
			Say:       "api_req_started",
			Text:      `{"tokensIn":79569,"tokensOut":300,"cacheReads":78000,"cost":0.008}`,
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, _, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// PeakContextTokens should be the max of tokensIn + cacheReads
	// across api_req_started entries, NOT the cumulative history_item
	// tokensIn. The three entries sum to 22997, 98000, 157569.
	assert.Equal(t, 157569, sess.PeakContextTokens,
		"peak context should be tokensIn + cacheReads from api_req_started")
	assert.True(t, sess.HasPeakContextTokens)

	// Cumulative tokensIn still goes to usage event for cost.
	require.Len(t, sess.UsageEvents, 1)
	assert.Equal(t, 3130608, sess.UsageEvents[0].InputTokens,
		"usage event should carry cumulative tokensIn")
}

func TestParseRooCodeSessionPeakContextNoApiReqs(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-noapi")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-noapi",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "No api_req test",
		TokensIn:  5000,
		TokensOut: 1000,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// No api_req_started messages at all.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "No api_req test",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, _, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// No api_req_started = no peak context data available.
	assert.Equal(t, 0, sess.PeakContextTokens)
	assert.False(t, sess.HasPeakContextTokens)
}

func TestParseRooCodeSessionPeakContextIncludesCacheWrites(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-cachewrites")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-cachewrites",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Cache writes test",
		TokensIn:  1000,
		TokensOut: 500,
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Cache writes test",
		},
		{
			Timestamp: 1688836852000,
			Type:      "say",
			Say:       "api_req_started",
			Text:      `{"tokensIn":5000,"cacheReads":3000,"cacheWrites":2000,"tokensOut":100}`,
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, _, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// Peak context = tokensIn + cacheReads + cacheWrites = 5000 + 3000 + 2000.
	assert.Equal(t, 10000, sess.PeakContextTokens,
		"peak context should include cache reads and writes")
	assert.True(t, sess.HasPeakContextTokens)
}

func TestParseRooCodeSessionCommandOutputPairing(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-cmdpair")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-cmdpair",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Command pairing test",
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

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Run tests",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "command",
			Text:      "npm test",
		},
		{
			Timestamp: 1688836865000,
			Type:      "say",
			Say:       "api_req_started",
			Text:      `{"tokensIn":1000}`,
		},
		{
			Timestamp: 1688836870000,
			Type:      "say",
			Say:       "command_output",
			Text:      "All tests passed.",
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

	// user task + execute_command tool call = 2 (command_output
	// is paired into the tool call, not emitted separately).
	assert.Equal(t, 2, sess.MessageCount)

	// Verify the tool call has a ResultEvent with the output.
	require.Len(t, msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	assert.Equal(t, "execute_command", tc.ToolName)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "completed", tc.ResultEvents[0].Status)
	assert.Equal(t, "All tests passed.", tc.ResultEvents[0].Content)
}

func TestParseRooCodeSessionCommandOutputError(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-cmderr")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-cmderr",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Command error test",
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

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Run tests",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "command",
			Text:      "npm test",
		},
		{
			Timestamp: 1688836870000,
			Type:      "say",
			Say:       "command_output",
			Text:      "Error: Test suite failed. exit code 1",
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

	assert.Equal(t, 2, sess.MessageCount)

	// Error output should set status to "errored".
	require.Len(t, msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "errored", tc.ResultEvents[0].Status)
	assert.Contains(t, tc.ResultEvents[0].Content, "exit code 1")
}

func TestParseRooCodeSessionFileToolResult(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-fileresult")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-fileresult",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "File tool result test",
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

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Read the file",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"readFile","path":"src/main.go","content":"package main\nfunc main() {}"}`,
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

	// readFile should have ResultEvents with the embedded content.
	require.Len(t, msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	assert.Equal(t, "readFile", tc.ToolName)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "completed", tc.ResultEvents[0].Status)
	assert.Equal(t, "package main\nfunc main() {}", tc.ResultEvents[0].Content)
}

func TestParseRooCodeSessionWriteToolContentNotResult(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-writenoresult")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-writenoresult",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Write tool content not result",
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

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Write the file",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"writeToFile","path":"src/main.go","content":"package main"}`,
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

	// writeToFile content is input, not a result — no ResultEvents.
	require.Len(t, msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	assert.Equal(t, "writeToFile", tc.ToolName)
	assert.Empty(t, tc.ResultEvents,
		"writeToFile content should not be treated as a completed result")
}

func TestParseRooCodeSessionNewTaskContentNotResult(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-newtask-noresult")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-newtask-noresult",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "newTask content not result",
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

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Delegate task",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "tool",
			Text:      `{"tool":"newTask","mode":"Code","content":"Implement feature A"}`,
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

	// newTask should NOT have ResultEvents (content is task prompt,
	// not a tool result).
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "newTask", msgs[1].ToolCalls[0].ToolName)
	assert.Empty(t, msgs[1].ToolCalls[0].ResultEvents,
		"newTask content is a task prompt, not a result")
}

func TestRooCommandOutputIsError(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		// Basic non-errors.
		{"empty", "", false},
		{"zero", "0", false},
		{"exit code 0", "exit code 0", false},
		{"all passed", "All tests passed.", false},
		{"warning", "warning: unused variable", false},
		{"compiling", "Compiling... done.", false},

		// Exit code patterns (whitespace variant).
		{"exit code 1", "exit code 1", true},
		{"exit status 2", "exit status 2", true},
		{"Exit Code 127", "Exit Code 127", true},

		// Exit code patterns (colon variant — the main bug fix).
		{"exit status colon 1", "exit status: 1", true},
		{"exit code colon 1", "exit code: 1", true},
		{"exit status colon 2", "Exit Status: 2", true},
		{"parens exit status colon", "(exit status: 1)", true},
		{"multiline exit status colon",
			"Building project...\nError in src/main.ts\n(exit status: 1)", true},

		// Prefix patterns (first line).
		{"error colon", "Error: file not found", true},
		{"error space", "error: undefined variable", true},
		{"fatal colon", "fatal: not a git repository", true},
		{"failed colon", "Failed: build error", true},

		// Anywhere patterns (npm ERR! in multi-line output).
		{"npm ERR basic", "npm ERR! code ELIFECYCLE", true},
		{"npm ERR multiline",
			"> todoseq@0.10.0 test\n> jest\n\nnpm ERR! code ELIFECYCLE", true},

		// Error at start of line in multi-line output.
		{"error colon multiline",
			"Compiling project...\nError: Cannot find module 'foo'\nBuild failed.", true},
		{"fatal multiline",
			"Running setup...\nFatal: unable to access repo", true},
	}
	for _, tt := range tests {
		got := rooCommandOutputIsError(tt.output)
		assert.Equal(t, tt.want, got,
			"%s: rooCommandOutputIsError(\"%s\") = %v, want %v",
			tt.name, tt.output, got, tt.want)
	}
}

func TestParseRooCodeSessionErrorTypes(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-errors")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-errors",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Error types test",
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

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Error types test",
		},
		{
			Timestamp: 1688836852000,
			Type:      "say",
			Say:       "error",
			Text:      "File not found: src/missing.ts",
		},
		{
			Timestamp: 1688836853000,
			Type:      "say",
			Say:       "diff_error",
			Text:      "Search and replace resulted in identical content",
		},
		{
			Timestamp: 1688836854000,
			Type:      "say",
			Say:       "rooignore_error",
			Text:      "File is in .rooignore and cannot be accessed",
		},
		{
			Timestamp: 1688836855000,
			Type:      "say",
			Say:       "shell_integration_warning",
			Text:      "Shell integration not available",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "text",
			Text:      "Continuing despite errors.",
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

	// user task + 4 error/warning system messages + assistant text = 6
	assert.Equal(t, 6, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)

	// All error types should be system messages with content preserved.
	errorMsgs := []struct {
		say     string
		content string
	}{
		{"error", "File not found: src/missing.ts"},
		{"diff_error", "Search and replace resulted in identical content"},
		{"rooignore_error", "File is in .rooignore and cannot be accessed"},
		{"shell_integration_warning", "Shell integration not available"},
	}
	for i, want := range errorMsgs {
		idx := i + 1 // offset past user task
		assert.Equal(t, RoleSystem, msgs[idx].Role,
			"%s should be RoleSystem", want.say)
		assert.True(t, msgs[idx].IsSystem,
			"%s should be IsSystem", want.say)
		assert.Equal(t, want.content, msgs[idx].Content,
			"%s content should be preserved", want.say)
	}
	assert.Equal(t, RoleAssistant, msgs[5].Role)
}

func TestParseRooCodeSessionUserFeedbackIsUserRole(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-feedback")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-feedback",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Feedback test",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Initial task",
		},
		{
			Timestamp: 1688836852000,
			Type:      "say",
			Say:       "text",
			Text:      "Working on it.",
		},
		{
			Timestamp: 1688836853000,
			Type:      "say",
			Say:       "user_feedback",
			Text:      "Please also handle edge cases",
		},
		{
			Timestamp: 1688836854000,
			Type:      "say",
			Say:       "text",
			Text:      "Done with edge cases.",
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

	assert.Equal(t, 4, sess.MessageCount)
	// user_feedback should be classified as user, not system.
	assert.Equal(t, RoleUser, msgs[2].Role,
		"user_feedback should be a user message")
	assert.False(t, msgs[2].IsSystem,
		"user_feedback should not be marked as system")
	assert.Equal(t, "Please also handle edge cases", msgs[2].Content)
}

func TestParseRooCodeSessionCompletionResultAskIsAssistant(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-completion-ask")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-completion-ask",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Completion ask test",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Initial task",
		},
		{
			Timestamp: 1688836852000,
			Type:      "ask",
			Ask:       "completion_result",
			Text:      "Task completed successfully.",
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

	assert.Equal(t, 2, sess.MessageCount)
	// ask=completion_result should be assistant, not system.
	assert.Equal(t, RoleAssistant, msgs[1].Role,
		"completion_result ask should be an assistant message")
	assert.False(t, msgs[1].IsSystem,
		"completion_result ask should not be marked as system")
}

func TestParseRooCodeSessionErrorSayTypePairing(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-errsay")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-errsay",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Error say type pairing test",
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

	// Sequence: user task, command ask, error say (no command_output).
	// The error should pair with the pending execute_command.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Build the project",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "command",
			Text:      "npm run build",
		},
		{
			Timestamp: 1688836870000,
			Type:      "say",
			Say:       "error",
			Text:      "Build failed: missing dependency",
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
	// (error is paired as ResultEvent, not emitted standalone)
	assert.Equal(t, 2, sess.MessageCount)

	// Verify the error was paired with the execute_command as an
	// errored ResultEvent (not emitted as a standalone message).
	require.Len(t, msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	assert.Equal(t, "execute_command", tc.ToolName)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "errored", tc.ResultEvents[0].Status)
	assert.Equal(t, "Build failed: missing dependency",
		tc.ResultEvents[0].Content)
}

func TestParseRooCodeSessionErrorSayTypeNoPendingCommand(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-errsay-nopending")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-errsay-nopending",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Error with no pending command",
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

	// Error arrives with no preceding command — should be a
	// standalone system message with no tool call pairing.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Hello",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "error",
			Text:      "Internal error occurred",
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

	// user task + error system msg = 2
	assert.Equal(t, 2, sess.MessageCount)

	// Error should be a standalone system message, no tool calls.
	assert.Equal(t, RoleSystem, msgs[1].Role)
	assert.True(t, msgs[1].IsSystem)
	assert.Equal(t, "Internal error occurred", msgs[1].Content)
}

func TestParseRooCodeSessionErrorEmptyText(t *testing.T) {
	// shell_integration_warning with empty text should be skipped
	// (no content, no tool calls/results → filtered out).
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-err-empty")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-err-empty",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Empty error test",
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

	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Empty error test",
		},
		{
			Timestamp: 1688836852000,
			Type:      "say",
			Say:       "shell_integration_warning",
			Text:      "", // empty text
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "text",
			Text:      "Done.",
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

	// Empty error should be filtered: user task + assistant = 2
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
}

func TestClassifyRooCodeTermination(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		messages []ParsedMessage
		want     TerminationStatus
	}{
		{
			name:   "completed with normal assistant text",
			status: "completed",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "do something"},
				{Role: RoleAssistant, Content: "Done."},
			},
			want: TerminationClean,
		},
		{
			name:   "completed with tool call resolved",
			status: "completed",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "fix it"},
				{
					Role:       RoleAssistant,
					HasToolUse: true,
					ToolCalls: []ParsedToolCall{
						{ToolUseID: "t1", ToolName: "readFile"},
					},
				},
				{
					Role: RoleUser, IsSystem: true,
					ToolResults: []ParsedToolResult{
						{ToolUseID: "t1"},
					},
				},
				{Role: RoleAssistant, Content: "All done."},
			},
			want: TerminationClean,
		},
		{
			name:   "completed with tool call resolved via ResultEvents",
			status: "completed",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "fix it"},
				{
					Role:       RoleAssistant,
					HasToolUse: true,
					ToolCalls: []ParsedToolCall{
						{
							ToolUseID: "t1",
							ToolName:  "readFile",
							ResultEvents: []ParsedToolResultEvent{
								{Status: "completed", Content: "file contents"},
							},
						},
					},
				},
				{Role: RoleAssistant, Content: "All done."},
			},
			want: TerminationClean,
		},
		{
			status: "completed",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "fix it"},
				{
					Role:       RoleAssistant,
					HasToolUse: true, ToolCalls: []ParsedToolCall{
						{ToolUseID: "t1", ToolName: "readFile"},
					},
				},
			},
			want: TerminationToolCallPending,
		},
		{
			name:   "completed with thinking-only ending",
			status: "completed",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "do something"},
				{
					Role:        RoleAssistant,
					Content:     "[Thinking]\nAnalyzing the codebase...\n[/Thinking]",
					HasThinking: true,
				},
			},
			want: TerminationToolCallPending,
		},
		{
			name:   "active status",
			status: "active",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "do something"},
				{Role: RoleAssistant, Content: "Working on it."},
			},
			want: "",
		},
		{
			name:   "error status",
			status: "error",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "do something"},
				{Role: RoleAssistant, Content: "Trying..."},
			},
			want: TerminationTruncated,
		},
		{
			name:     "unknown status with no messages",
			status:   "something_else",
			messages: []ParsedMessage{},
			want:     "",
		},
		{
			name:   "empty status with orphaned tool call",
			status: "",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "fix it"},
				{
					Role:       RoleAssistant,
					HasToolUse: true, ToolCalls: []ParsedToolCall{
						{ToolUseID: "t1", ToolName: "readFile"},
					},
				},
			},
			want: TerminationToolCallPending,
		},
		{
			name:   "empty status with thinking-only ending",
			status: "",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "do something"},
				{
					Role:        RoleAssistant,
					Content:     "[Thinking]\nAnalyzing the codebase...\n[/Thinking]",
					HasThinking: true,
				},
			},
			want: TerminationToolCallPending,
		},
		{
			name:   "empty status with normal messages",
			status: "",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "do something"},
				{Role: RoleAssistant, Content: "Done."},
			},
			want: "",
		},
		{
			name:   "completed with thinking + real text",
			status: "completed",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "do something"},
				{
					Role:        RoleAssistant,
					Content:     "[Thinking]\nAnalyzing...\n[/Thinking]\nHere is the result.",
					HasThinking: true,
				},
			},
			want: TerminationClean,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyRooCodeTermination(tt.status, tt.messages)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRooCodeIsMetadataSay(t *testing.T) {
	metadataTypes := []string{
		"api_req_started",
		"api_req_deleted",
		"api_req_retried",
		"api_req_retry_delayed",
		"checkpoint_saved",
	}
	for _, say := range metadataTypes {
		assert.Truef(t, rooCodeIsMetadataSay(say),
			"%q should be metadata", say)
	}

	nonMetadata := []string{
		"text", "reasoning", "command_output",
		"error", "diff_error", "rooignore_error",
		"shell_integration_warning",
		"subtask_result", "completion_result", "mcp_server_response",
		"user_feedback", "condense_context",
	}
	for _, say := range nonMetadata {
		assert.Falsef(t, rooCodeIsMetadataSay(say),
			"%q should NOT be metadata", say)
	}
}
