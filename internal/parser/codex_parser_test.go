package parser

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wesm/agentsview/internal/testjsonl"
)

func runCodexParserTest(t *testing.T, fileName, content string, includeExec bool) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	if fileName == "" {
		fileName = "test.jsonl"
	}
	path := createTestFile(t, fileName, content)
	sess, msgs, err := ParseCodexSession(path, "local", includeExec)
	require.NoError(t, err)
	return sess, msgs
}

func assertToolResultEvents(
	t *testing.T,
	got []ParsedToolResultEvent,
	want []ParsedToolResultEvent,
) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		assert.Equal(t, want[i].ToolUseID, got[i].ToolUseID, "event %d tool_use_id", i)
		assert.Equal(t, want[i].AgentID, got[i].AgentID, "event %d agent_id", i)
		assert.Equal(t, want[i].SubagentSessionID, got[i].SubagentSessionID, "event %d subagent_session_id", i)
		assert.Equal(t, want[i].Source, got[i].Source, "event %d source", i)
		assert.Equal(t, want[i].Status, got[i].Status, "event %d status", i)
		assert.Equal(t, want[i].Content, got[i].Content, "event %d content", i)
	}
}

func TestParseCodexSession_Basic(t *testing.T) {
	content := loadFixture(t, "codex/standard_session.jsonl")
	sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

	require.NotNil(t, sess)
	assert.Equal(t, "codex:abc-123", sess.ID)
	assert.Equal(t, 2, len(msgs))
	assertSessionMeta(t, sess, "codex:abc-123", "my_api", AgentCodex)
}

func TestParseCodexSession_PreservesAssistantBlockquotes(t *testing.T) {
	content := loadFixture(t, "codex/blockquotes_session.jsonl")
	sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

	require.NotNil(t, sess)
	assert.Equal(t, "codex:quote-123", sess.ID)
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "blablabla?", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t,
		"blabla1\n\n> blabla2\n\nblabla3\n\n> blabla4\n\nblabla5",
		msgs[1].Content,
	)
}

func TestParseCodexSession_ExecOriginator(t *testing.T) {
	execContent := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("abc", "/tmp", "codex_exec", tsEarly),
		testjsonl.CodexMsgJSON("user", "test", tsEarlyS1),
	)

	t.Run("includes exec originator by default", func(t *testing.T) {
		sess, msgs := runCodexParserTest(t, "test.jsonl", execContent, false)
		require.NotNil(t, sess)
		assert.Equal(t, "codex:abc", sess.ID)
		assert.Equal(t, 1, len(msgs))
	})

	t.Run("includes exec when requested", func(t *testing.T) {
		sess, msgs := runCodexParserTest(t, "test.jsonl", execContent, true)
		require.NotNil(t, sess)
		assert.Equal(t, "codex:abc", sess.ID)
		assert.Equal(t, 1, len(msgs))
	})
}

func TestCodexInsertMessage_PreservesChronologyOnSameOrdinal(t *testing.T) {
	b := newCodexSessionBuilder(false)
	b.messages = []ParsedMessage{{
		Ordinal:   2,
		Role:      RoleAssistant,
		Content:   "later assistant message",
		Timestamp: parseTimestamp("2024-01-01T10:01:06Z"),
	}}

	idx := b.insertMessage(ParsedMessage{
		Ordinal:   2,
		Role:      RoleUser,
		Content:   "earlier orphan notification",
		Timestamp: parseTimestamp("2024-01-01T10:01:05Z"),
	})

	assert.Equal(t, 0, idx)
	b.normalizeOrdinals()
	require.Len(t, b.messages, 2)
	assert.Equal(t, "earlier orphan notification", b.messages[0].Content)
	assert.Equal(t, "later assistant message", b.messages[1].Content)
	assert.Equal(t, 0, b.messages[0].Ordinal)
	assert.Equal(t, 1, b.messages[1].Ordinal)
}

func TestParseCodexSession_FunctionCalls(t *testing.T) {
	t.Run("function calls", func(t *testing.T) {
		content := loadFixture(t, "codex/function_calls.jsonl")
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, "codex:fc-1", sess.ID)
		assert.Equal(t, 3, len(msgs))

		assert.Equal(t, RoleUser, msgs[0].Role)
		assert.False(t, msgs[0].HasToolUse)

		assert.Equal(t, RoleAssistant, msgs[1].Role)
		assert.True(t, msgs[1].HasToolUse)
		assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{ToolName: "shell_command", Category: "Bash"}})
		assert.Equal(t, "[Bash: Running tests]", msgs[1].Content)

		assert.True(t, msgs[2].HasToolUse)
		assertToolCalls(t, msgs[2].ToolCalls, []ParsedToolCall{{ToolName: "apply_patch", Category: "Edit"}})

		for i, m := range msgs {
			assert.Equal(t, i, m.Ordinal)
		}
	})

	t.Run("exec_command arguments include command detail", func(t *testing.T) {
		content := loadFixture(t, "codex/fc_args_1.jsonl")
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, "[Bash]\n$ rg --files", msgs[1].Content)
		assert.Equal(t, `{"cmd":"rg --files","workdir":"/tmp"}`, msgs[1].ToolCalls[0].InputJSON)
	})

	t.Run("multi-line command truncated to first line", func(t *testing.T) {
		multiLineCmd := "cat > file.toml <<'EOF'\n[package]\nname = \"foo\"\nEOF"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-ml", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "create file", tsEarlyS1),
			testjsonl.CodexFunctionCallArgsJSON("exec_command", map[string]any{
				"cmd": multiLineCmd,
			}, tsEarlyS5),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, "[Bash]\n$ cat > file.toml <<'EOF'", msgs[1].Content)
		assert.Contains(t, msgs[1].ToolCalls[0].InputJSON, "cmd")
		assert.Contains(t, msgs[1].ToolCalls[0].InputJSON, "[package]")
	})

	t.Run("apply_patch arguments summarize edited files", func(t *testing.T) {
		content := loadFixture(t, "codex/fc_args_2.jsonl")
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		want := "[Edit: internal/parser/codex.go (+1 more)]\ninternal/parser/codex.go\ninternal/parser/parser_test.go"
		assert.Equal(t, want, msgs[1].Content)
		assert.NotEmpty(t, msgs[1].ToolCalls[0].InputJSON)
		assert.Contains(t, msgs[1].ToolCalls[0].InputJSON, "Begin Patch")
	})

	t.Run("write_stdin formats with session and chars", func(t *testing.T) {
		content := loadFixture(t, "codex/fc_stdin.jsonl")
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		want := "[Bash: stdin -> sess-42]\nyes\\n"
		assert.Equal(t, want, msgs[1].Content)
		assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{ToolName: "write_stdin", Category: "Bash"}})
	})

	t.Run("Agent function call normalizes to Task category", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-agent", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "explore code", tsEarlyS1),
			testjsonl.CodexFunctionCallArgsJSON("Agent", map[string]any{
				"description":   "explore codebase",
				"subagent_type": "Explore",
			}, tsEarlyS5),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, "codex:fc-agent", sess.ID)
		assert.Equal(t, 2, len(msgs))
		assert.Contains(t, msgs[1].Content, "[Task: explore codebase (Explore)]")
		assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{ToolName: "Agent", Category: "Task"}})
	})

	t.Run("spawn_agent links child session and wait output becomes tool result", func(t *testing.T) {
		childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		waitSummary := "Exit code: `1`\n\nFull output:\n```text\nTraceback...\n```"
		notification := "<subagent_notification>\n" +
			"{\"agent_id\":\"" + childID + "\",\"status\":{\"completed\":\"Exit code: `1`\\n\\nFull output:\\n```text\\nTraceback...\\n```\"}}\n" +
			"</subagent_notification>"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run a child agent", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn", map[string]any{
				"agent_type": "awaiter",
				"message":    "Run the compile smoke test",
			}, tsEarlyS5),
			testjsonl.CodexFunctionCallOutputJSON("call_spawn", `{"agent_id":"`+childID+`","nickname":"Fennel"}`, tsLate),
			testjsonl.CodexFunctionCallWithCallIDJSON("wait", "call_wait", map[string]any{
				"ids":        []string{childID},
				"timeout_ms": 600000,
			}, tsLateS5),
			testjsonl.CodexFunctionCallOutputJSON("call_wait", "{\"status\":{\""+childID+"\":{\"completed\":\"Exit code: `1`\\n\\nFull output:\\n```text\\nTraceback...\\n```\"}}}", "2024-01-01T10:01:06Z"),
			testjsonl.CodexMsgJSON("user", notification, "2024-01-01T10:01:07Z"),
			testjsonl.CodexMsgJSON("assistant", "continuing", "2024-01-01T10:01:08Z"),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 4, len(msgs))
		assert.Equal(t, RoleAssistant, msgs[1].Role)
		assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{
			ToolUseID: "call_spawn",
			ToolName:  "spawn_agent",
			Category:  "Task",
		}})
		assert.Equal(t, RoleAssistant, msgs[2].Role)
		assertToolCalls(t, msgs[2].ToolCalls, []ParsedToolCall{{
			ToolUseID: "call_wait",
			ToolName:  "wait",
			Category:  "Other",
		}})
		assertToolResultEvents(t, msgs[2].ToolCalls[0].ResultEvents, []ParsedToolResultEvent{{
			ToolUseID:         "call_wait",
			AgentID:           childID,
			SubagentSessionID: "codex:" + childID,
			Source:            "wait_output",
			Status:            "completed",
			Content:           waitSummary,
		}})
		assert.Equal(t, RoleAssistant, msgs[3].Role)
		assert.Equal(t, "continuing", msgs[3].Content)
	})

	t.Run("subagent notification without wait result falls back to spawn_agent output", func(t *testing.T) {
		childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		summary := "Exit code: `1`\n\nFull output:\n```text\nTraceback...\n```"
		notification := "<subagent_notification>\n" +
			"{\"agent_id\":\"" + childID + "\",\"status\":{\"completed\":\"Exit code: `1`\\n\\nFull output:\\n```text\\nTraceback...\\n```\"}}\n" +
			"</subagent_notification>"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent-notify", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run a child agent", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn", map[string]any{
				"agent_type": "awaiter",
				"message":    "Run the compile smoke test",
			}, tsEarlyS5),
			testjsonl.CodexFunctionCallOutputJSON("call_spawn", `{"agent_id":"`+childID+`","nickname":"Fennel"}`, tsLate),
			testjsonl.CodexMsgJSON("user", notification, tsLateS5),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 2, len(msgs))
		assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{
			ToolUseID: "call_spawn",
			ToolName:  "spawn_agent",
			Category:  "Task",
		}})
		assertToolResultEvents(t, msgs[1].ToolCalls[0].ResultEvents, []ParsedToolResultEvent{{
			ToolUseID:         "call_spawn",
			AgentID:           childID,
			SubagentSessionID: "codex:" + childID,
			Source:            "subagent_notification",
			Status:            "completed",
			Content:           summary,
		}})
	})

	t.Run("no-wait fallback preserves chronology before later messages", func(t *testing.T) {
		childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		summary := "Exit code: `1`\n\nFull output:\n```text\nTraceback...\n```"
		notification := "<subagent_notification>\n" +
			"{\"agent_id\":\"" + childID + "\",\"status\":{\"completed\":\"Exit code: `1`\\n\\nFull output:\\n```text\\nTraceback...\\n```\"}}\n" +
			"</subagent_notification>"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent-notify-order", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run a child agent", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn", map[string]any{
				"agent_type": "awaiter",
				"message":    "Run the compile smoke test",
			}, tsEarlyS5),
			testjsonl.CodexFunctionCallOutputJSON("call_spawn", `{"agent_id":"`+childID+`","nickname":"Fennel"}`, tsLate),
			testjsonl.CodexMsgJSON("user", notification, tsLateS5),
			testjsonl.CodexMsgJSON("assistant", "continuing", "2024-01-01T10:01:06Z"),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 3, len(msgs))
		assertToolResultEvents(t, msgs[1].ToolCalls[0].ResultEvents, []ParsedToolResultEvent{{
			ToolUseID:         "call_spawn",
			AgentID:           childID,
			SubagentSessionID: "codex:" + childID,
			Source:            "subagent_notification",
			Status:            "completed",
			Content:           summary,
		}})
		assert.Equal(t, RoleAssistant, msgs[2].Role)
		assert.Equal(t, "continuing", msgs[2].Content)
	})

	t.Run("duplicate pending notification preserves earliest chronology", func(t *testing.T) {
		childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		summary := "Exit code: `1`\n\nFull output:\n```text\nTraceback...\n```"
		notification := "<subagent_notification>\n" +
			"{\"agent_id\":\"" + childID + "\",\"status\":{\"completed\":\"Exit code: `1`\\n\\nFull output:\\n```text\\nTraceback...\\n```\"}}\n" +
			"</subagent_notification>"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent-notify-dupe-order", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run a child agent", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn", map[string]any{
				"agent_type": "awaiter",
				"message":    "Run the compile smoke test",
			}, tsEarlyS5),
			testjsonl.CodexFunctionCallOutputJSON("call_spawn", `{"agent_id":"`+childID+`","nickname":"Fennel"}`, tsLate),
			testjsonl.CodexMsgJSON("user", notification, tsLateS5),
			testjsonl.CodexMsgJSON("assistant", "continuing", "2024-01-01T10:01:06Z"),
			testjsonl.CodexMsgJSON("user", notification, "2024-01-01T10:01:07Z"),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 3, len(msgs))
		assertToolResultEvents(t, msgs[1].ToolCalls[0].ResultEvents, []ParsedToolResultEvent{{
			ToolUseID:         "call_spawn",
			AgentID:           childID,
			SubagentSessionID: "codex:" + childID,
			Source:            "subagent_notification",
			Status:            "completed",
			Content:           summary,
		}})
		assert.Equal(t, RoleAssistant, msgs[2].Role)
		assert.Equal(t, "continuing", msgs[2].Content)
	})

	t.Run("running subagent notification does not suppress later completion", func(t *testing.T) {
		childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		running := "<subagent_notification>\n" +
			"{\"agent_id\":\"" + childID + "\",\"status\":{\"running\":\"Still working\"}}\n" +
			"</subagent_notification>"
		completed := "<subagent_notification>\n" +
			"{\"agent_id\":\"" + childID + "\",\"status\":{\"completed\":\"Finished successfully\"}}\n" +
			"</subagent_notification>"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent-running", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run a child agent", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn", map[string]any{
				"agent_type": "awaiter",
				"message":    "Run the compile smoke test",
			}, tsEarlyS5),
			testjsonl.CodexFunctionCallOutputJSON("call_spawn", `{"agent_id":"`+childID+`","nickname":"Fennel"}`, tsLate),
			testjsonl.CodexMsgJSON("user", running, tsLateS5),
			testjsonl.CodexMsgJSON("user", completed, "2024-01-01T10:01:06Z"),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 2, len(msgs))
		assertToolResultEvents(t, msgs[1].ToolCalls[0].ResultEvents, []ParsedToolResultEvent{
			{
				ToolUseID:         "call_spawn",
				AgentID:           childID,
				SubagentSessionID: "codex:" + childID,
				Source:            "subagent_notification",
				Status:            "running",
				Content:           "Still working",
			},
			{
				ToolUseID:         "call_spawn",
				AgentID:           childID,
				SubagentSessionID: "codex:" + childID,
				Source:            "subagent_notification",
				Status:            "completed",
				Content:           "Finished successfully",
			},
		})
	})

	t.Run("notification after wait binds to wait call", func(t *testing.T) {
		childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		completed := "<subagent_notification>\n" +
			"{\"agent_id\":\"" + childID + "\",\"status\":{\"completed\":\"Finished successfully\"}}\n" +
			"</subagent_notification>"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent-wait-bind", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run a child agent", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn", map[string]any{
				"agent_type": "awaiter",
				"message":    "Run the compile smoke test",
			}, tsEarlyS5),
			testjsonl.CodexFunctionCallOutputJSON("call_spawn", `{"agent_id":"`+childID+`","nickname":"Fennel"}`, tsLate),
			testjsonl.CodexFunctionCallWithCallIDJSON("wait", "call_wait", map[string]any{
				"ids": []string{childID},
			}, tsLateS5),
			testjsonl.CodexMsgJSON("user", completed, "2024-01-01T10:01:06Z"),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 3, len(msgs))
		assertToolCalls(t, msgs[2].ToolCalls, []ParsedToolCall{{
			ToolUseID: "call_wait",
			ToolName:  "wait",
			Category:  "Other",
		}})
		assertToolResultEvents(t, msgs[2].ToolCalls[0].ResultEvents, []ParsedToolResultEvent{{
			ToolUseID:         "call_wait",
			AgentID:           childID,
			SubagentSessionID: "codex:" + childID,
			Source:            "subagent_notification",
			Status:            "completed",
			Content:           "Finished successfully",
		}})
	})

	t.Run("notification before wait binds to later wait call", func(t *testing.T) {
		childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		completed := "<subagent_notification>\n" +
			"{\"agent_id\":\"" + childID + "\",\"status\":{\"completed\":\"Finished successfully\"}}\n" +
			"</subagent_notification>"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent-wait-rebind", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run a child agent", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn", map[string]any{
				"agent_type": "awaiter",
				"message":    "Run the compile smoke test",
			}, tsEarlyS5),
			testjsonl.CodexFunctionCallOutputJSON("call_spawn", `{"agent_id":"`+childID+`","nickname":"Fennel"}`, tsLate),
			testjsonl.CodexMsgJSON("user", completed, tsLateS5),
			testjsonl.CodexFunctionCallWithCallIDJSON("wait", "call_wait", map[string]any{
				"ids": []string{childID},
			}, "2024-01-01T10:01:06Z"),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 3, len(msgs))
		assertToolResultEvents(t, msgs[2].ToolCalls[0].ResultEvents, []ParsedToolResultEvent{{
			ToolUseID:         "call_wait",
			AgentID:           childID,
			SubagentSessionID: "codex:" + childID,
			Source:            "subagent_notification",
			Status:            "completed",
			Content:           "Finished successfully",
		}})
	})

	t.Run("late spawn output does not override wait binding", func(t *testing.T) {
		childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		completed := "<subagent_notification>\n" +
			"{\"agent_id\":\"" + childID + "\",\"status\":{\"completed\":\"Finished successfully\"}}\n" +
			"</subagent_notification>"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent-late-spawn-output", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run a child agent", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn", map[string]any{
				"agent_type": "awaiter",
				"message":    "Run the compile smoke test",
			}, tsEarlyS5),
			testjsonl.CodexFunctionCallWithCallIDJSON("wait", "call_wait", map[string]any{
				"ids": []string{childID},
			}, tsLate),
			testjsonl.CodexFunctionCallOutputJSON("call_spawn", `{"agent_id":"`+childID+`","nickname":"Fennel"}`, tsLateS5),
			testjsonl.CodexMsgJSON("user", completed, "2024-01-01T10:01:06Z"),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 3, len(msgs))
		assertToolResultEvents(t, msgs[2].ToolCalls[0].ResultEvents, []ParsedToolResultEvent{{
			ToolUseID:         "call_wait",
			AgentID:           childID,
			SubagentSessionID: "codex:" + childID,
			Source:            "subagent_notification",
			Status:            "completed",
			Content:           "Finished successfully",
		}})
	})

	t.Run("wait output does not duplicate terminal notification result", func(t *testing.T) {
		childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		completed := "<subagent_notification>\n" +
			"{\"agent_id\":\"" + childID + "\",\"status\":{\"completed\":\"Finished successfully\"}}\n" +
			"</subagent_notification>"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent-wait-dedupe", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run a child agent", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn", map[string]any{
				"agent_type": "awaiter",
				"message":    "Run the compile smoke test",
			}, tsEarlyS5),
			testjsonl.CodexFunctionCallOutputJSON("call_spawn", `{"agent_id":"`+childID+`","nickname":"Fennel"}`, tsLate),
			testjsonl.CodexFunctionCallWithCallIDJSON("wait", "call_wait", map[string]any{
				"ids": []string{childID},
			}, tsLateS5),
			testjsonl.CodexMsgJSON("user", completed, "2024-01-01T10:01:06Z"),
			testjsonl.CodexFunctionCallOutputJSON("call_wait",
				"{\"status\":{\""+childID+"\":{\"completed\":\"Finished successfully\"}}}",
				"2024-01-01T10:01:07Z",
			),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 3, len(msgs))
		assertToolResultEvents(t, msgs[2].ToolCalls[0].ResultEvents, []ParsedToolResultEvent{{
			ToolUseID:         "call_wait",
			AgentID:           childID,
			SubagentSessionID: "codex:" + childID,
			Source:            "subagent_notification",
			Status:            "completed",
			Content:           "Finished successfully",
		}})
	})

	t.Run("mixed wait status preserves later completion for running agent", func(t *testing.T) {
		completedID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		runningID := "019c9c96-6ee7-77c0-ba4c-380f844289d6"
		laterCompleted := "<subagent_notification>\n" +
			"{\"agent_id\":\"" + runningID + "\",\"status\":{\"completed\":\"Second agent finished\"}}\n" +
			"</subagent_notification>"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent-mixed-wait", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run child agents", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("wait", "call_wait", map[string]any{
				"ids": []string{completedID, runningID},
			}, tsEarlyS5),
			testjsonl.CodexFunctionCallOutputJSON("call_wait",
				"{\"status\":{\""+completedID+"\":{\"completed\":\"First agent finished\"},\""+runningID+"\":{\"running\":\"Still working\"}}}",
				tsLate,
			),
			testjsonl.CodexMsgJSON("user", laterCompleted, tsLateS5),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 2, len(msgs))
		assertToolResultEvents(t, msgs[1].ToolCalls[0].ResultEvents, []ParsedToolResultEvent{
			{
				ToolUseID:         "call_wait",
				AgentID:           completedID,
				SubagentSessionID: "codex:" + completedID,
				Source:            "wait_output",
				Status:            "completed",
				Content:           "First agent finished",
			},
			{
				ToolUseID:         "call_wait",
				AgentID:           runningID,
				SubagentSessionID: "codex:" + runningID,
				Source:            "wait_output",
				Status:            "running",
				Content:           "Still working",
			},
			{
				ToolUseID:         "call_wait",
				AgentID:           runningID,
				SubagentSessionID: "codex:" + runningID,
				Source:            "subagent_notification",
				Status:            "completed",
				Content:           "Second agent finished",
			},
		})
	})

	t.Run("running-only wait output is preserved as a result event", func(t *testing.T) {
		childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent-running-wait", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run child agent", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("wait", "call_wait", map[string]any{
				"ids": []string{childID},
			}, tsEarlyS5),
			testjsonl.CodexFunctionCallOutputJSON("call_wait",
				"{\"status\":{\""+childID+"\":{\"running\":\"Still working\"}}}",
				tsLate,
			),
		)

		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 2, len(msgs))
		assertToolResultEvents(t, msgs[1].ToolCalls[0].ResultEvents, []ParsedToolResultEvent{{
			ToolUseID:         "call_wait",
			AgentID:           childID,
			SubagentSessionID: "codex:" + childID,
			Source:            "wait_output",
			Status:            "running",
			Content:           "Still working",
		}})
	})

	t.Run("wait result events preserve JSON order for multiple agents", func(t *testing.T) {
		firstID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		secondID := "019c9c96-6ee7-77c0-ba4c-380f844289d6"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent-order", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run child agents", tsEarlyS1),
			testjsonl.CodexFunctionCallWithCallIDJSON("wait", "call_wait", map[string]any{
				"ids": []string{firstID, secondID},
			}, tsEarlyS5),
			testjsonl.CodexFunctionCallOutputJSON("call_wait",
				"{\"status\":{\""+secondID+"\":{\"completed\":\"Second agent finished\"},\""+firstID+"\":{\"completed\":\"First agent finished\"}}}",
				tsLate,
			),
		)

		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 2, len(msgs))
		assertToolResultEvents(t, msgs[1].ToolCalls[0].ResultEvents, []ParsedToolResultEvent{
			{
				ToolUseID:         "call_wait",
				AgentID:           secondID,
				SubagentSessionID: "codex:" + secondID,
				Source:            "wait_output",
				Status:            "completed",
				Content:           "Second agent finished",
			},
			{
				ToolUseID:         "call_wait",
				AgentID:           firstID,
				SubagentSessionID: "codex:" + firstID,
				Source:            "wait_output",
				Status:            "completed",
				Content:           "First agent finished",
			},
		})
	})

	t.Run("orphaned terminal notifications dedupe", func(t *testing.T) {
		childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
		completed := "<subagent_notification>\n" +
			"{\"agent_id\":\"" + childID + "\",\"status\":{\"completed\":\"Finished successfully\"}}\n" +
			"</subagent_notification>"
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-subagent-orphan", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", completed, tsEarlyS1),
			testjsonl.CodexMsgJSON("user", completed, tsEarlyS5),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		require.NotNil(t, sess)
		assert.Equal(t, 1, len(msgs))
		assert.Equal(t, "Finished successfully", msgs[0].Content)
	})

	t.Run("function call no name skipped", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-2", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
			testjsonl.CodexFunctionCallJSON("", "", tsEarlyS5),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, "codex:fc-2", sess.ID)
		assert.Equal(t, 1, len(msgs))
	})

	t.Run("mixed content and function calls", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-3", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "Fix it", tsEarlyS1),
			testjsonl.CodexMsgJSON("assistant", "Looking at it", tsEarlyS5),
			testjsonl.CodexFunctionCallJSON("shell_command", "Running rg", tsLate),
			testjsonl.CodexMsgJSON("assistant", "Found the issue", tsLateS5),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, "codex:fc-3", sess.ID)
		assert.Equal(t, 4, len(msgs))
		for i, m := range msgs {
			assert.Equal(t, i, m.Ordinal)
			assert.Equal(t, i == 2, m.HasToolUse)
		}
	})

	t.Run("function call without summary", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-4", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "do it", tsEarlyS1),
			testjsonl.CodexFunctionCallJSON("exec_command", "", tsEarlyS5),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, "codex:fc-4", sess.ID)
		assert.Equal(t, 2, len(msgs))
		assert.Equal(t, "[Bash]", msgs[1].Content)
	})

	t.Run("empty arguments falls through to input", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-empty-args", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run command", tsEarlyS1),
			testjsonl.CodexFunctionCallFieldsJSON("exec_command", map[string]any{}, `{"cmd":"ls -la"}`, tsEarlyS5),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, "codex:fc-empty-args", sess.ID)
		assert.Equal(t, 2, len(msgs))
		assert.Equal(t, "[Bash]\n$ ls -la", msgs[1].Content)
	})

	t.Run("empty array arguments falls through to input", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("fc-empty-arr", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run command", tsEarlyS1),
			testjsonl.CodexFunctionCallFieldsJSON("exec_command", []any{}, `{"cmd":"echo hello"}`, tsEarlyS5),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, "codex:fc-empty-arr", sess.ID)
		assert.Equal(t, 2, len(msgs))
		assert.Equal(t, "[Bash]\n$ echo hello", msgs[1].Content)
	})
}

func TestParseCodexSession_InputJSON(t *testing.T) {
	t.Run("object arguments populates InputJSON", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("ij-1", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "do it", tsEarlyS1),
			testjsonl.CodexFunctionCallArgsJSON("shell_command", map[string]any{
				"cmd": "ls -la",
			}, tsEarlyS5),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{
			ToolName:  "shell_command",
			Category:  "Bash",
			InputJSON: `{"cmd":"ls -la"}`,
		}})
	})

	t.Run("string-encoded JSON arguments", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("ij-2", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "do it", tsEarlyS1),
			testjsonl.CodexFunctionCallArgsJSON("exec_command",
				`{"cmd":"rg foo","workdir":"/tmp"}`, tsEarlyS5),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{
			ToolName:  "exec_command",
			Category:  "Bash",
			InputJSON: `{"cmd":"rg foo","workdir":"/tmp"}`,
		}})
	})

	t.Run("non-JSON string arguments preserved", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("ij-3", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "do it", tsEarlyS1),
			testjsonl.CodexFunctionCallArgsJSON("shell_command",
				"echo hello world", tsEarlyS5),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, "echo hello world", msgs[1].ToolCalls[0].InputJSON)
	})

	t.Run("input field used when arguments empty", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("ij-4", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run", tsEarlyS1),
			testjsonl.CodexFunctionCallFieldsJSON("exec_command",
				map[string]any{}, `{"cmd":"echo hi"}`, tsEarlyS5),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{
			ToolName:  "exec_command",
			Category:  "Bash",
			InputJSON: `{"cmd":"echo hi"}`,
		}})
	})

	t.Run("string-encoded empty JSON falls through to input", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("ij-str-empty", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "run", tsEarlyS1),
			testjsonl.CodexFunctionCallFieldsJSON("exec_command",
				`{}`, `{"cmd":"echo fallback"}`, tsEarlyS5),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{
			ToolName:  "exec_command",
			Category:  "Bash",
			InputJSON: `{"cmd":"echo fallback"}`,
		}})
	})

	t.Run("no arguments yields empty InputJSON", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("ij-5", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "do it", tsEarlyS1),
			testjsonl.CodexFunctionCallJSON("exec_command", "", tsEarlyS5),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Empty(t, msgs[1].ToolCalls[0].InputJSON)
	})
}

func TestParseCodexSession_TurnContextModel(t *testing.T) {
	t.Run("model from turn_context applied to subsequent messages", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("m-1", "/tmp", "user", tsEarly),
			testjsonl.CodexTurnContextJSON("gpt-5-codex", tsEarlyS1),
			testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
			testjsonl.CodexMsgJSON("assistant", "hi there", tsEarlyS5),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		require.NotNil(t, sess)
		assert.Equal(t, 2, len(msgs))
		assert.Equal(t, "gpt-5-codex", msgs[0].Model)
		assert.Equal(t, "gpt-5-codex", msgs[1].Model)
	})

	t.Run("model changes across turns", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("m-2", "/tmp", "user", tsEarly),
			testjsonl.CodexTurnContextJSON("gpt-5-codex", tsEarlyS1),
			testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
			testjsonl.CodexMsgJSON("assistant", "hi", tsEarlyS5),
			testjsonl.CodexTurnContextJSON("o3-pro", tsLate),
			testjsonl.CodexMsgJSON("user", "think harder", tsLate),
			testjsonl.CodexMsgJSON("assistant", "deep thought", tsLateS5),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, 4, len(msgs))
		assert.Equal(t, "gpt-5-codex", msgs[0].Model)
		assert.Equal(t, "gpt-5-codex", msgs[1].Model)
		assert.Equal(t, "o3-pro", msgs[2].Model)
		assert.Equal(t, "o3-pro", msgs[3].Model)
	})

	t.Run("empty model in turn_context clears previous model", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("m-4", "/tmp", "user", tsEarly),
			testjsonl.CodexTurnContextJSON("gpt-5-codex", tsEarlyS1),
			testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
			testjsonl.CodexMsgJSON("assistant", "hi", tsEarlyS5),
			testjsonl.CodexTurnContextJSON("", tsLate),
			testjsonl.CodexMsgJSON("user", "follow up", tsLate),
			testjsonl.CodexMsgJSON("assistant", "reply", tsLateS5),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, 4, len(msgs))
		assert.Equal(t, "gpt-5-codex", msgs[0].Model)
		assert.Equal(t, "gpt-5-codex", msgs[1].Model)
		assert.Empty(t, msgs[2].Model)
		assert.Empty(t, msgs[3].Model)
	})

	t.Run("no turn_context leaves model empty", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("m-3", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
			testjsonl.CodexMsgJSON("assistant", "hi", tsEarlyS5),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, 2, len(msgs))
		assert.Empty(t, msgs[0].Model)
		assert.Empty(t, msgs[1].Model)
	})
}

func TestParseCodexSession_TokenUsage(t *testing.T) {
	t.Run("token_count attached to assistant message", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("tu-1", "/tmp", "user", tsEarly),
			testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
			testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
			testjsonl.CodexMsgJSON("assistant", "hi there", tsEarlyS5),
			testjsonl.CodexTokenCountJSON(tsEarlyS5, 10000, 500, 6000),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		require.NotNil(t, sess)
		require.Len(t, msgs, 2)

		// User message has no usage.
		assert.Empty(t, msgs[0].TokenUsage)

		// Assistant message has normalized usage. Codex reports
		// input_tokens=10000 as the full input (cached included);
		// after normalization the stored input_tokens is the
		// uncached remainder (10000-6000=4000).
		assert.NotEmpty(t, msgs[1].TokenUsage)
		assert.Contains(t, string(msgs[1].TokenUsage), `"input_tokens":4000`)
		assert.Contains(t, string(msgs[1].TokenUsage), `"output_tokens":500`)
		assert.Contains(t, string(msgs[1].TokenUsage), `"cache_read_input_tokens":6000`)
		assert.Equal(t, 500, msgs[1].OutputTokens)
		assert.Equal(t, 10000, msgs[1].ContextTokens) // 4000+6000
		assert.True(t, msgs[1].HasOutputTokens)
		assert.True(t, msgs[1].HasContextTokens)

		// Session-level accumulation.
		assert.True(t, sess.HasTotalOutputTokens)
		assert.Equal(t, 500, sess.TotalOutputTokens)
		assert.True(t, sess.HasPeakContextTokens)
		assert.Equal(t, 10000, sess.PeakContextTokens)
	})

	t.Run("duplicate token_count events deduplicated", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("tu-2", "/tmp", "user", tsEarly),
			testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
			testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
			testjsonl.CodexMsgJSON("assistant", "hi", tsEarlyS5),
			testjsonl.CodexTokenCountJSON(tsEarlyS5, 10000, 500, 6000),
			// Streaming duplicates.
			testjsonl.CodexTokenCountJSON(tsEarlyS5, 10000, 500, 6000),
			testjsonl.CodexTokenCountJSON(tsEarlyS5, 10000, 500, 6000),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		require.Len(t, msgs, 2)
		assert.NotEmpty(t, msgs[1].TokenUsage)
		assert.Equal(t, 500, msgs[1].OutputTokens)
	})

	t.Run("multiple turns get separate usage", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("tu-3", "/tmp", "user", tsEarly),
			testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
			testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
			testjsonl.CodexMsgJSON("assistant", "hi", tsEarlyS5),
			testjsonl.CodexTokenCountJSON(tsEarlyS5, 10000, 500, 6000),
			testjsonl.CodexMsgJSON("user", "think more", tsLate),
			testjsonl.CodexMsgJSON("assistant", "deep thought", tsLateS5),
			testjsonl.CodexTokenCountJSON(tsLateS5, 20000, 800, 12000),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		require.Len(t, msgs, 4)

		// First assistant msg (10000 total, 6000 cached).
		assert.Equal(t, 500, msgs[1].OutputTokens)
		assert.Equal(t, 10000, msgs[1].ContextTokens)

		// Second assistant msg (20000 total, 12000 cached).
		assert.Equal(t, 800, msgs[3].OutputTokens)
		assert.Equal(t, 20000, msgs[3].ContextTokens)

		// Session totals.
		assert.Equal(t, 1300, sess.TotalOutputTokens)
		assert.Equal(t, 20000, sess.PeakContextTokens)
	})

	t.Run("multiple API calls in one turn", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("tu-5", "/tmp", "user", tsEarly),
			testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
			testjsonl.CodexMsgJSON("user", "do stuff", tsEarlyS1),
			// First API call: assistant + function call.
			testjsonl.CodexMsgJSON("assistant", "let me check", tsEarlyS5),
			testjsonl.CodexFunctionCallJSON("exec_command", "ls", tsEarlyS5),
			testjsonl.CodexTokenCountJSON(tsEarlyS5, 10000, 300, 6000),
			// Second API call after tool output.
			testjsonl.CodexMsgJSON("assistant", "here is the result", tsLate),
			testjsonl.CodexTokenCountJSON(tsLate, 15000, 400, 10000),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)

		// First token_count attaches to function_call (last
		// assistant msg before it).
		assert.Equal(t, 300, msgs[2].OutputTokens)
		assert.Empty(t, msgs[1].TokenUsage)

		// Second token_count attaches to second assistant msg.
		assert.Equal(t, 400, msgs[3].OutputTokens)
	})

	t.Run("no token_count leaves usage empty", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("tu-4", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
			testjsonl.CodexMsgJSON("assistant", "hi", tsEarlyS5),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		require.Len(t, msgs, 2)
		assert.Empty(t, msgs[1].TokenUsage)
		assert.Equal(t, 0, msgs[1].OutputTokens)
	})
}

func TestParseCodexSession_EdgeCases(t *testing.T) {
	t.Run("skips system messages", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("abc", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "# AGENTS.md\nsome instructions", tsEarlyS1),
			testjsonl.CodexMsgJSON("user", "<environment_context>stuff</environment_context>", "2024-01-01T10:00:02Z"),
			testjsonl.CodexMsgJSON("user", "<INSTRUCTIONS>ignore</INSTRUCTIONS>", "2024-01-01T10:00:03Z"),
			testjsonl.CodexMsgJSON("user", "Actual user message", "2024-01-01T10:00:04Z"),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		require.NotNil(t, sess)
		assert.Equal(t, 1, len(msgs))
		assert.Equal(t, "Actual user message", msgs[0].Content)
	})

	// Codex injects skill template content as role=user JSONL
	// entries when the model invokes a skill. These look like
	// follow-up user turns to a naive count, which inflates
	// user_message_count past the single-turn classifier gate
	// and prevents automated sessions from being recognized.
	// Treat them as system content and drop from the message
	// list, the same way <environment_context> and similar
	// envelopes are handled.
	t.Run("skips skill template injections", func(t *testing.T) {
		skill := "<skill>\n  <name>roborev:fix</name>\n  <path>" +
			"/Users/wesm/.codex/skills/roborev-fix/SKILL.md</path>\n" +
			"---\nname: roborev:fix\n..."
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("abc", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "You are a code reviewer.", tsEarlyS1),
			testjsonl.CodexMsgJSON("user", skill, "2024-01-01T10:00:02Z"),
			testjsonl.CodexMsgJSON("assistant", "OK", "2024-01-01T10:00:03Z"),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		require.NotNil(t, sess)
		require.Len(t, msgs, 2)
		assert.Equal(t, "You are a code reviewer.", msgs[0].Content)
		assert.Equal(t, "OK", msgs[1].Content)
		assert.Equal(t, 1, sess.UserMessageCount,
			"skill injection must not count as a user turn")
	})

	t.Run("fallback ID from filename", func(t *testing.T) {
		content := testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1)
		sess, _ := runCodexParserTest(t, "test.jsonl", content, false)
		require.NotNil(t, sess)
		assert.Equal(t, "codex:test", sess.ID)
	})

	t.Run("fallback ID from hyphenated filename", func(t *testing.T) {
		content := testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1)
		sess, _ := runCodexParserTest(t, "my-codex-session.jsonl", content, false)
		require.NotNil(t, sess)
		assert.Equal(t, "codex:my-codex-session", sess.ID)
	})

	t.Run("large message within scanner limit", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("big", "/tmp", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", generateLargeString(1024*1024), tsEarlyS1),
		)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, 1024*1024, msgs[0].ContentLength)
	})

	t.Run("second session_meta with unparsable cwd resets project", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON("multi", "/Users/alice/code/my-api", "user", tsEarly),
			testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
			testjsonl.CodexSessionMetaJSON("multi", "/", "user", "2024-01-01T10:00:02Z"),
		)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		assert.Equal(t, "codex:multi", sess.ID)
		assert.Equal(t, 1, len(msgs))
		assert.Equal(t, "unknown", sess.Project)
	})
}

func TestParseCodexSessionFrom_Incremental(t *testing.T) {
	t.Parallel()

	// Build initial content with session_meta + one message.
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"inc-1", "/projects/api",
			"codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
	)

	path := createTestFile(t, "incremental.jsonl", initial)

	// Full parse to get baseline.
	sess, msgs, err := ParseCodexSession(path, "local", false)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "codex:inc-1", sess.ID)
	assert.Equal(t, 1, len(msgs))
	assert.Equal(t, 0, msgs[0].Ordinal)

	// Record the file size as the incremental offset.
	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	// Append new messages.
	appended := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON(
			"assistant", "world", tsEarlyS5,
		),
		testjsonl.CodexMsgJSON(
			"user", "thanks", tsLate,
		),
	)
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Incremental parse from the offset.
	newMsgs, endedAt, _, err := ParseCodexSessionFrom(
		path, offset, 1, false,
	)
	require.NoError(t, err)
	assert.Equal(t, 2, len(newMsgs))

	// Ordinals start from startOrdinal=1.
	assert.Equal(t, 1, newMsgs[0].Ordinal)
	assert.Equal(t, RoleAssistant, newMsgs[0].Role)
	assert.Contains(t, newMsgs[0].Content, "world")

	assert.Equal(t, 2, newMsgs[1].Ordinal)
	assert.Equal(t, RoleUser, newMsgs[1].Role)

	// endedAt reflects the latest timestamp.
	assert.False(t, endedAt.IsZero())
}

func TestParseCodexSessionFrom_SkipsSessionMeta(t *testing.T) {
	t.Parallel()

	// File where session_meta appears after the offset
	// (shouldn't happen in practice but should be skipped).
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"meta-2", "/tmp", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "first", tsEarlyS1),
	)
	path := createTestFile(t, "meta-skip.jsonl", initial)

	info, _ := os.Stat(path)
	offset := info.Size()

	// Append a duplicate session_meta + a message.
	extra := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"meta-2", "/tmp", "codex_cli_rs", tsEarlyS5,
		),
		testjsonl.CodexMsgJSON(
			"assistant", "reply", tsLate,
		),
	)
	f, _ := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	f.WriteString(extra)
	f.Close()

	newMsgs, _, _, err := ParseCodexSessionFrom(
		path, offset, 5, false,
	)
	require.NoError(t, err)
	// Only the assistant message, not the session_meta.
	assert.Equal(t, 1, len(newMsgs))
	assert.Equal(t, 5, newMsgs[0].Ordinal)
}

func TestParseCodexSessionFrom_NoNewData(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"empty-1", "/tmp", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "hi", tsEarlyS1),
	)
	path := createTestFile(t, "no-new.jsonl", content)

	info, _ := os.Stat(path)
	offset := info.Size()

	// Parse from end of file — no new data.
	newMsgs, endedAt, _, err := ParseCodexSessionFrom(
		path, offset, 10, false,
	)
	require.NoError(t, err)
	assert.Equal(t, 0, len(newMsgs))
	assert.True(t, endedAt.IsZero())
}

func TestParseCodexSessionFrom_SubagentOutputRequiresFullParse(t *testing.T) {
	t.Parallel()

	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("inc-sub", "/tmp", "codex_cli_rs", tsEarly),
		testjsonl.CodexMsgJSON("user", "run child", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn", map[string]any{
			"agent_type": "awaiter",
			"message":    "run it",
		}, tsEarlyS5),
	)
	path := createTestFile(t, "codex-subagent-inc.jsonl", initial)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallOutputJSON("call_spawn", `{"agent_id":"019c9c96-6ee7-77c0-ba4c-380f844289d5","nickname":"Fennel"}`, tsLate),
	))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, _, _, err = ParseCodexSessionFrom(path, offset, 2, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "full parse")
}

func TestParseCodexSessionFrom_WaitCallRequiresFullParse(t *testing.T) {
	t.Parallel()

	childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
	notification := "<subagent_notification>\n" +
		"{\"agent_id\":\"" + childID + "\",\"status\":{\"completed\":\"Finished successfully\"}}\n" +
		"</subagent_notification>"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("inc-wait", "/tmp", "codex_cli_rs", tsEarly),
		testjsonl.CodexMsgJSON("user", "run child", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "call_spawn", map[string]any{
			"agent_type": "awaiter",
			"message":    "run it",
		}, tsEarlyS5),
		testjsonl.CodexFunctionCallOutputJSON("call_spawn", `{"agent_id":"`+childID+`","nickname":"Fennel"}`, tsLate),
		testjsonl.CodexMsgJSON("user", notification, tsLateS5),
	)
	path := createTestFile(t, "codex-wait-inc.jsonl", initial)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallWithCallIDJSON("wait", "call_wait", map[string]any{
			"ids": []string{childID},
		}, "2024-01-01T10:01:06Z"),
	))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, _, _, err = ParseCodexSessionFrom(path, offset, 4, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "full parse")
}

func TestParseCodexSessionFrom_SystemMessageDoesNotRequireFullParse(t *testing.T) {
	t.Parallel()

	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("inc-system", "/tmp", "codex_cli_rs", tsEarly),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
	)
	path := createTestFile(t, "codex-system-inc.jsonl", initial)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("user", "# AGENTS.md\nsome instructions", tsLate),
	))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	newMsgs, endedAt, _, err := ParseCodexSessionFrom(path, offset, 1, false)
	require.NoError(t, err)
	assert.Equal(t, 0, len(newMsgs))
	assert.False(t, endedAt.IsZero())
}

func TestParseCodexSessionFrom_RunningNotificationRequiresFullParse(t *testing.T) {
	t.Parallel()

	childID := "019c9c96-6ee7-77c0-ba4c-380f844289d5"
	running := "<subagent_notification>\n" +
		"{\"agent_id\":\"" + childID + "\",\"status\":{\"running\":\"Still working\"}}\n" +
		"</subagent_notification>"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("inc-running", "/tmp", "codex_cli_rs", tsEarly),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
	)
	path := createTestFile(t, "codex-running-inc.jsonl", initial)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("user", running, tsLate),
	))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, _, _, err = ParseCodexSessionFrom(path, offset, 1, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "full parse")
}

func TestParseCodexSessionFrom_NonSubagentFunctionOutputDoesNotRequireFullParse(t *testing.T) {
	t.Parallel()

	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("inc-nonsubagent-output", "/tmp", "codex_cli_rs", tsEarly),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
	)
	path := createTestFile(t, "codex-nonsubagent-output-inc.jsonl", initial)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallOutputJSON("call_other", `{"status":"ok"}`, tsLate),
	))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	newMsgs, endedAt, _, err := ParseCodexSessionFrom(path, offset, 1, false)
	require.NoError(t, err)
	assert.Equal(t, 0, len(newMsgs))
	assert.False(t, endedAt.IsZero())
}

func TestParseCodexSessionFrom_SeedsModelFromTurnContext(
	t *testing.T,
) {
	t.Parallel()

	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"model-seed", "/tmp", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON(
			"gpt-5.4", tsEarlyS1,
		),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS5),
		testjsonl.CodexMsgJSON(
			"assistant", "hi there", tsLate,
		),
	)
	path := createTestFile(t, "model-seed.jsonl", initial)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON(
			"assistant", "second reply", tsLateS5,
		),
	)
	f2, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err)
	_, err = f2.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f2.Close())

	newMsgs2, _, _, err := ParseCodexSessionFrom(
		path, offset, 2, false,
	)
	require.NoError(t, err)
	require.Equal(t, 1, len(newMsgs2))
	assert.Equal(t, "gpt-5.4", newMsgs2[0].Model,
		"incremental parse should seed model from "+
			"prior turn_context via file scan")
}

func TestParseCodexSessionFrom_SeedsBoundaryAfterTurnContext(
	t *testing.T,
) {
	t.Parallel()

	// Offset lands immediately after a turn_context with no
	// following message — the exact sync boundary edge case.
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"tc-boundary", "/tmp", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON(
			"gpt-5.4", tsEarlyS1,
		),
	)
	path := createTestFile(
		t, "tc-boundary.jsonl", initial,
	)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS5),
		testjsonl.CodexMsgJSON(
			"assistant", "world", tsLate,
		),
	)
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	newMsgs, _, _, err := ParseCodexSessionFrom(
		path, offset, 0, false,
	)
	require.NoError(t, err)
	require.Equal(t, 2, len(newMsgs))
	assert.Equal(t, "gpt-5.4", newMsgs[0].Model,
		"user message after turn_context boundary")
	assert.Equal(t, "gpt-5.4", newMsgs[1].Model,
		"assistant message after turn_context boundary")
}

func TestParseCodexSessionFrom_EmptyModelReset(
	t *testing.T,
) {
	t.Parallel()

	// turn_context clears model to "" — incremental parse
	// must honor the reset, not retain the old model.
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"model-reset", "/tmp", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON(
			"gpt-5.4", tsEarlyS1,
		),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS5),
		testjsonl.CodexTurnContextJSON("", tsLate),
	)
	path := createTestFile(
		t, "model-reset.jsonl", initial,
	)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON(
			"assistant", "after reset", tsLateS5,
		),
	)
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	newMsgs, _, _, err := ParseCodexSessionFrom(
		path, offset, 2, false,
	)
	require.NoError(t, err)
	require.Equal(t, 1, len(newMsgs))
	assert.Equal(t, "", newMsgs[0].Model,
		"empty-model turn_context should reset model")
}

func TestReadCodexModelAtOffset_SkipsInvalidJSON(
	t *testing.T,
) {
	t.Parallel()

	// Truncated turn_context between a valid one and the
	// offset — must not override the valid model.
	validTC := testjsonl.CodexTurnContextJSON(
		"gpt-5.4", tsEarlyS1,
	)
	truncated := `{"type":"turn_context","payload":{"model":"wrong`
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"invalid-json", "/tmp",
			"codex_cli_rs", tsEarly,
		),
	) + validTC + "\n" + truncated + "\n"

	path := createTestFile(
		t, "invalid-tc.jsonl", content,
	)

	info, err := os.Stat(path)
	require.NoError(t, err)
	got := readCodexModelAtOffset(path, info.Size())
	assert.Equal(t, "gpt-5.4", got,
		"truncated turn_context should be skipped")
}

func TestReadCodexModelAtOffset(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"model-at-offset", "/tmp",
			"codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON(
			"gpt-5", tsEarlyS1,
		),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS5),
		testjsonl.CodexTurnContextJSON(
			"gpt-5.4", tsLate,
		),
		testjsonl.CodexMsgJSON("user", "bye", tsLateS5),
	)
	path := createTestFile(
		t, "model-at-offset.jsonl", content,
	)

	t.Run("full file returns last model", func(t *testing.T) {
		info, err := os.Stat(path)
		require.NoError(t, err)
		got := readCodexModelAtOffset(path, info.Size())
		assert.Equal(t, "gpt-5.4", got)
	})

	t.Run("zero offset returns empty", func(t *testing.T) {
		got := readCodexModelAtOffset(path, 0)
		assert.Equal(t, "", got)
	})

	t.Run("nonexistent file returns empty", func(t *testing.T) {
		got := readCodexModelAtOffset("/no/such/file", 100)
		assert.Equal(t, "", got)
	})
}

// TestParseCodexSession_TurnAbortedNotCountedAsUser pins the
// behavior that Codex's synthetic <turn_aborted> "user" message
// (emitted when codex exec is interrupted) is filtered like other
// system messages and does not inflate UserMessageCount. Without
// this, a single-turn roborev review session whose codex process
// was killed during shutdown gets UserMessageCount=2 and falls
// through the IsAutomatedSession single-turn gate.
func TestParseCodexSession_TurnAbortedNotCountedAsUser(t *testing.T) {
	turnAborted := "<turn_aborted>\nThe user interrupted the previous turn on purpose. " +
		"Any running unified exec processes may still be running in the background. " +
		"If any tools/commands were aborted, they may have partially executed.\n</turn_aborted>"
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("abc", "/tmp", "codex_exec", tsEarly),
		testjsonl.CodexMsgJSON("user", "You are a code reviewer. Review the diff.", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", turnAborted, tsEarlyS5),
	)
	sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

	require.NotNil(t, sess)
	assert.Equal(t, 1, sess.UserMessageCount,
		"<turn_aborted> synthetic must not be counted as a user message")
	for _, m := range msgs {
		assert.NotContains(t, m.Content, "<turn_aborted>",
			"<turn_aborted> synthetic must be filtered from message list")
	}
}
