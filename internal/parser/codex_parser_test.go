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

func TestParseCodexSession_Basic(t *testing.T) {
	content := loadFixture(t, "codex/standard_session.jsonl")
	sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)

	require.NotNil(t, sess)
	assert.Equal(t, "codex:abc-123", sess.ID)
	assert.Equal(t, 2, len(msgs))
	assertSessionMeta(t, sess, "codex:abc-123", "my_api", AgentCodex)
}

func TestParseCodexSession_ExecOriginator(t *testing.T) {
	execContent := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("abc", "/tmp", "codex_exec", tsEarly),
		testjsonl.CodexMsgJSON("user", "test", tsEarlyS1),
	)

	t.Run("skips exec originator", func(t *testing.T) {
		sess, _ := runCodexParserTest(t, "test.jsonl", execContent, false)
		assert.Nil(t, sess)
	})

	t.Run("includes exec when requested", func(t *testing.T) {
		sess, msgs := runCodexParserTest(t, "test.jsonl", execContent, true)
		require.NotNil(t, sess)
		assert.Equal(t, "codex:abc", sess.ID)
		assert.Equal(t, 1, len(msgs))
	})
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
