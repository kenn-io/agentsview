package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/wesm/agentsview/internal/testjsonl"
)

func runClaudeParserTest(t *testing.T, fileName, content string) (ParsedSession, []ParsedMessage) {
	t.Helper()
	if fileName == "" {
		fileName = "test.jsonl"
	}
	path := createTestFile(t, fileName, content)
	results, err := ParseClaudeSession(path, "my_app", "local")
	require.NoError(t, err)
	require.NotEmpty(t, results)
	return results[0].Session, results[0].Messages
}

func TestParseClaudeSession_Basic(t *testing.T) {
	content := loadFixture(t, "claude/valid_session.jsonl")
	sess, msgs := runClaudeParserTest(t, "test.jsonl", content)

	assertMessageCount(t, len(msgs), 4)
	assertMessageCount(t, sess.MessageCount, 4)
	assertSessionMeta(t, &sess, "test", "my_app", AgentClaude)
	assert.Equal(t, "Fix the login bug", sess.FirstMessage)

	assertMessage(t, msgs[0], RoleUser, "")
	assertMessage(t, msgs[1], RoleAssistant, "")
	assert.True(t, msgs[1].HasToolUse)
	assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{ToolUseID: "toolu_1", ToolName: "Read", Category: "Read", InputJSON: `{"file_path":"src/auth.ts"}`}})
	assert.Equal(t, 0, msgs[0].Ordinal)
	assert.Equal(t, 1, msgs[1].Ordinal)
}

func TestParseClaudeSession_HyphenatedFilename(t *testing.T) {
	content := loadFixture(t, "claude/valid_session.jsonl")
	sess, _ := runClaudeParserTest(t, "my-test-session.jsonl", content)
	assert.Equal(t, "my-test-session", sess.ID)
}

func TestParseClaudeSession_EdgeCases(t *testing.T) {
	t.Run("empty file", func(t *testing.T) {
		sess, msgs := runClaudeParserTest(t, "test.jsonl", "")
		assert.Empty(t, msgs)
		assert.Equal(t, 0, sess.MessageCount)
	})

	t.Run("skips blank content", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("", tsZero),
			testjsonl.ClaudeUserJSON("  ", tsZeroS1),
			testjsonl.ClaudeUserJSON("actual message", tsZeroS2),
		)
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 1, sess.MessageCount)
	})

	t.Run("truncates long first message", func(t *testing.T) {
		content := testjsonl.ClaudeUserJSON(generateLargeString(400), tsZero) + "\n"
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 303, len(sess.FirstMessage))
	})

	t.Run("skips invalid JSON lines", func(t *testing.T) {
		content := "not valid json\n" +
			testjsonl.ClaudeUserJSON("hello", tsZero) + "\n" +
			"also not valid\n"
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 1, sess.MessageCount)
	})

	t.Run("malformed UTF-8", func(t *testing.T) {
		badUTF8 := `{"type":"user","timestamp":"` + tsZeroS1 + `","message":{"content":"bad ` + string([]byte{0xff, 0xfe}) + `"}}` + "\n"
		content := testjsonl.ClaudeUserJSON("valid message", tsZero) + "\n" + badUTF8
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.GreaterOrEqual(t, sess.MessageCount, 1)
	})

	t.Run("very large message", func(t *testing.T) {
		content := testjsonl.ClaudeUserJSON(generateLargeString(1024*1024), tsZero) + "\n"
		_, msgs := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 1024*1024, msgs[0].ContentLength)
	})

	t.Run("skips empty lines in file", func(t *testing.T) {
		content := "\n\n" +
			testjsonl.ClaudeUserJSON("msg1", tsZero) +
			"\n   \n\t\n" +
			testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "text", "text": "reply"}}, tsZeroS1) +
			"\n\n"
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 2, sess.MessageCount)
	})

	t.Run("skips partial/truncated JSON", func(t *testing.T) {
		content := testjsonl.ClaudeUserJSON("first", tsZero) + "\n" +
			`{"type":"user","truncated` + "\n" +
			testjsonl.ClaudeAssistantJSON([]map[string]any{{"type": "text", "text": "last"}}, tsZeroS2) + "\n"
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 2, sess.MessageCount)
	})
}

func TestParseClaudeSession_SkippedMessages(t *testing.T) {
	t.Run("skips isMeta user messages", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeMetaUserJSON("meta context", tsZero, true, false),
			testjsonl.ClaudeUserJSON("real question", tsZeroS1),
		)
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 1, sess.MessageCount)
		assert.Equal(t, "real question", sess.FirstMessage)
	})

	t.Run("persists isCompactSummary as system message", func(t *testing.T) {
		compactLine := `{"type":"user","isCompactSummary":true,` +
			`"message":{"role":"user","content":[` +
			`{"type":"text","text":"Summary of conversation so far..."}` +
			`]},"uuid":"compact-uuid","parentUuid":"parent-uuid",` +
			`"isSidechain":false,"timestamp":"` + tsZero + `"}`
		content := testjsonl.JoinJSONL(
			compactLine,
			testjsonl.ClaudeUserJSON("actual prompt", tsZeroS1),
		)
		sess, msgs := runClaudeParserTest(t, "test.jsonl", content)
		// Compact boundary + real user message = 2 messages.
		assert.Equal(t, 2, sess.MessageCount)
		// Only real user messages count toward UserMessageCount.
		assert.Equal(t, 1, sess.UserMessageCount)
		// FirstMessage is from the first real user message.
		assert.Equal(t, "actual prompt", sess.FirstMessage)

		// Verify compact boundary message fields.
		cb := msgs[0]
		assert.Equal(t, RoleAssistant, cb.Role)
		assert.True(t, cb.IsSystem)
		assert.True(t, cb.IsCompactBoundary)
		assert.Equal(t, "system", cb.SourceType)
		assert.Equal(t, "compact_boundary", cb.SourceSubtype)
		assert.Equal(t, "compact-uuid", cb.SourceUUID)
		assert.Equal(t, "parent-uuid", cb.SourceParentUUID)
		assert.False(t, cb.IsSidechain)
		assert.Contains(t, cb.Content, "Summary of conversation so far...")
		assert.Equal(t, 0, cb.Ordinal)

		// Real user message follows with next ordinal.
		assert.Equal(t, 1, msgs[1].Ordinal)
		assert.Equal(t, RoleUser, msgs[1].Role)
	})

	t.Run("promotes and skips system-injected patterns", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("This session is being continued from a previous conversation.", tsZero),
			testjsonl.ClaudeUserJSON("[Request interrupted by user]", tsZeroS1),
			testjsonl.ClaudeUserJSON("<local-command-caveat>Caveat: resumed</local-command-caveat>", tsZeroS2),
			testjsonl.ClaudeUserJSON("<task-notification>data</task-notification>", "2024-01-01T00:00:04Z"),
			// Non-caveat local-command is pure noise and stays skipped.
			testjsonl.ClaudeUserJSON("<local-command-result>ok</local-command-result>", "2024-01-01T00:00:05Z"),
			testjsonl.ClaudeUserJSON("Stop hook feedback: rejected", "2024-01-01T00:00:06Z"),
			testjsonl.ClaudeUserJSON("real user message", "2024-01-01T00:00:07Z"),
		)
		sess, msgs := runClaudeParserTest(t, "test.jsonl", content)
		// 5 promoted system + 1 real user; <local-command-result>
		// is still skipped.
		assert.Equal(t, 6, sess.MessageCount)
		assert.Equal(t, 1, sess.UserMessageCount)
		assert.Equal(t, "real user message", sess.FirstMessage)

		wantSubtypes := []string{
			"continuation", "interrupted", "resume",
			"task_notification", "stop_hook",
		}
		require.Len(t, msgs, 6)
		for i, want := range wantSubtypes {
			assert.True(t, msgs[i].IsSystem,
				"msgs[%d] should be system", i)
			// Promoted system markers keep Role=user so
			// role-keyed analytics don't count them as
			// assistant replies.
			assert.Equal(t, RoleUser, msgs[i].Role)
			assert.Equal(t, "system", msgs[i].SourceType)
			assert.Equal(t, want, msgs[i].SourceSubtype)
		}
		// Final message is the real user message.
		assert.False(t, msgs[5].IsSystem)
		assert.Equal(t, RoleUser, msgs[5].Role)
		assert.Equal(t, "real user message", msgs[5].Content)
	})

	t.Run("skill invocation shown as user message", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON(
				"<command-message>roborev-fix</command-message>\n<command-name>/roborev-fix</command-name>\n<command-args>450</command-args>",
				tsZero,
			),
			testjsonl.ClaudeAssistantJSON([]map[string]any{
				{"type": "text", "text": "Looking at issue 450..."},
			}, tsZeroS1),
		)
		sess, msgs := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 2, sess.MessageCount)
		assert.Equal(t, 1, sess.UserMessageCount)
		assert.Equal(t, "/roborev-fix 450", sess.FirstMessage)
		assert.Equal(t, RoleUser, msgs[0].Role)
		assert.Equal(t, "/roborev-fix 450", msgs[0].Content)
	})

	t.Run("skill invocation without args shown as user message", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON(
				"<command-message>superpowers:brainstorming</command-message>\n<command-name>/superpowers:brainstorming</command-name>",
				tsZero,
			),
			testjsonl.ClaudeAssistantJSON([]map[string]any{
				{"type": "text", "text": "Starting brainstorming..."},
			}, tsZeroS1),
		)
		sess, msgs := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 2, sess.MessageCount)
		assert.Equal(t, "/superpowers:brainstorming", sess.FirstMessage)
		assert.Equal(t, RoleUser, msgs[0].Role)
		assert.Equal(t, "/superpowers:brainstorming", msgs[0].Content)
	})

	t.Run("assistant with system-like content not filtered", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("hello", tsZero),
			testjsonl.ClaudeAssistantJSON([]map[string]any{
				{"type": "text", "text": "This session is being continued from a previous conversation."},
			}, tsZeroS1),
		)
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 2, sess.MessageCount)
	})

	t.Run("firstMsg from first non-system user message", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeMetaUserJSON("context data", tsZero, true, false),
			testjsonl.ClaudeUserJSON("This session is being continued from a previous conversation.", tsZeroS1),
			testjsonl.ClaudeUserJSON("Fix the auth bug", tsZeroS2),
		)
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		// Meta user is skipped, continuation is promoted to a
		// system message, real user is kept.
		assert.Equal(t, 2, sess.MessageCount)
		assert.Equal(t, 1, sess.UserMessageCount)
		assert.Equal(t, "Fix the auth bug", sess.FirstMessage)
	})
}

func TestParseClaudeSession_QueuedCommand(t *testing.T) {
	t.Run("surfaces as user message between turns", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("first request", tsZero),
			testjsonl.ClaudeAssistantJSON([]map[string]any{
				{"type": "text", "text": "starting work"},
			}, tsZeroS1),
			testjsonl.ClaudeQueuedCommandJSON(
				"hold on, also do X", tsZeroS2,
			),
			testjsonl.ClaudeAssistantJSON([]map[string]any{
				{"type": "text", "text": "OK doing X too"},
			}, "2024-01-01T00:00:03Z"),
		)
		sess, msgs := runClaudeParserTest(t, "test.jsonl", content)
		require.Len(t, msgs, 4)
		assert.Equal(t, 4, sess.MessageCount)
		// Original first user + queued command both count.
		assert.Equal(t, 2, sess.UserMessageCount)
		assert.Equal(t, "first request", sess.FirstMessage)

		assert.Equal(t, RoleUser, msgs[0].Role)
		assert.Equal(t, "first request", msgs[0].Content)

		assert.Equal(t, RoleAssistant, msgs[1].Role)

		assert.Equal(t, RoleUser, msgs[2].Role)
		assert.Equal(t, "hold on, also do X", msgs[2].Content)
		assert.False(t, msgs[2].IsSystem)
		assert.Equal(t, "user", msgs[2].SourceType)
		assert.Equal(t, "queued_command", msgs[2].SourceSubtype)

		assert.Equal(t, RoleAssistant, msgs[3].Role)

		for i, m := range msgs {
			assert.Equal(t, i, m.Ordinal,
				"ordinal mismatch at %d", i)
		}
	})

	t.Run("becomes first message when session opens with one", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeQueuedCommandJSON(
				"queued opener", tsZero,
			),
			testjsonl.ClaudeAssistantJSON([]map[string]any{
				{"type": "text", "text": "ok"},
			}, tsZeroS1),
		)
		sess, msgs := runClaudeParserTest(t, "test.jsonl", content)
		require.Len(t, msgs, 2)
		assert.Equal(t, 1, sess.UserMessageCount)
		assert.Equal(t, "queued opener", sess.FirstMessage)
		assert.Equal(t, "queued_command", msgs[0].SourceSubtype)
	})

	t.Run("empty prompt is skipped", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("hi", tsZero),
			testjsonl.ClaudeQueuedCommandJSON("   ", tsZeroS1),
			testjsonl.ClaudeAssistantJSON([]map[string]any{
				{"type": "text", "text": "ok"},
			}, tsZeroS2),
		)
		sess, msgs := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 2, sess.MessageCount)
		require.Len(t, msgs, 2)
	})

	t.Run("non-queued attachment types are dropped", func(t *testing.T) {
		taskReminder := `{"type":"attachment","timestamp":"` +
			tsZeroS1 + `","attachment":{"type":"task_reminder",` +
			`"content":"reminder"}}`
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("hi", tsZero),
			taskReminder,
			testjsonl.ClaudeAssistantJSON([]map[string]any{
				{"type": "text", "text": "ok"},
			}, tsZeroS2),
		)
		sess, msgs := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 2, sess.MessageCount)
		require.Len(t, msgs, 2)
	})

	t.Run("works in DAG sessions with uuids on real entries", func(t *testing.T) {
		// Real entries form a uuid chain; the attachment has
		// no uuid but must still surface as a user message.
		userLine := `{"type":"user","uuid":"u1","timestamp":"` +
			tsZero + `","message":{"content":"hi"}}`
		assistant1 := `{"type":"assistant","uuid":"a1",` +
			`"parentUuid":"u1","timestamp":"` + tsZeroS1 +
			`","message":{"content":[{"type":"text",` +
			`"text":"work"}]}}`
		attachment := testjsonl.ClaudeQueuedCommandJSON(
			"also do X", tsZeroS2,
		)
		assistant2 := `{"type":"assistant","uuid":"a2",` +
			`"parentUuid":"a1","timestamp":` +
			`"2024-01-01T00:00:03Z","message":{"content":[` +
			`{"type":"text","text":"done"}]}}`
		content := testjsonl.JoinJSONL(
			userLine, assistant1, attachment, assistant2,
		)
		sess, msgs := runClaudeParserTest(t, "test.jsonl", content)
		require.Len(t, msgs, 4)
		assert.Equal(t, "also do X", msgs[2].Content)
		assert.Equal(t, "queued_command", msgs[2].SourceSubtype)
		assert.Equal(t, 2, sess.UserMessageCount)
	})
}

func TestParseClaudeSession_ParentSessionID(t *testing.T) {
	t.Run("sessionId != fileId sets ParentSessionID", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserWithSessionIDJSON("hello", tsZero, "parent-uuid"),
			testjsonl.ClaudeAssistantJSON([]map[string]any{
				{"type": "text", "text": "hi"},
			}, tsZeroS1),
		)
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, "parent-uuid", sess.ParentSessionID)
	})

	t.Run("sessionId == fileId yields empty ParentSessionID", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserWithSessionIDJSON("hello", tsZero, "test"),
		)
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Empty(t, sess.ParentSessionID)
	})

	t.Run("no sessionId field yields empty ParentSessionID", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("hello", tsZero),
		)
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Empty(t, sess.ParentSessionID)
	})
}

func TestParseClaudeSessionFrom_Incremental(t *testing.T) {
	t.Parallel()

	// Build initial content: user + assistant.
	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello world", tsEarly),
		testjsonl.ClaudeAssistantJSON("hi there", tsEarlyS1),
	)

	path := createTestFile(t, "inc-claude.jsonl", initial)

	// Full parse to get baseline.
	results, err := ParseClaudeSession(path, "proj", "local")
	require.NoError(t, err)
	require.NotEmpty(t, results)
	assert.Equal(t, 2, len(results[0].Messages))
	assert.Equal(t, 0, results[0].Messages[0].Ordinal)
	assert.Equal(t, 1, results[0].Messages[1].Ordinal)

	// Record file size as the incremental offset.
	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	// Append new messages.
	appended := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("follow up", tsEarlyS5),
		testjsonl.ClaudeAssistantJSON("got it", tsLate),
	)
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Incremental parse from offset.
	newMsgs, endedAt, _, err := ParseClaudeSessionFrom(
		path, offset, 2,
	)
	require.NoError(t, err)
	assert.Equal(t, 2, len(newMsgs))

	// Ordinals continue from startOrdinal=2.
	assert.Equal(t, 2, newMsgs[0].Ordinal)
	assert.Equal(t, RoleUser, newMsgs[0].Role)
	assert.Contains(t, newMsgs[0].Content, "follow up")

	assert.Equal(t, 3, newMsgs[1].Ordinal)
	assert.Equal(t, RoleAssistant, newMsgs[1].Role)
	assert.Contains(t, newMsgs[1].Content, "got it")

	assert.False(t, endedAt.IsZero())
}

func TestParseClaudeSessionFrom_QueuedCommand(t *testing.T) {
	t.Parallel()

	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", tsEarly),
		testjsonl.ClaudeAssistantJSON("hi", tsEarlyS1),
	)
	path := createTestFile(t, "inc-queued.jsonl", initial)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	// Append: queued command + assistant turn.
	appended := testjsonl.JoinJSONL(
		testjsonl.ClaudeQueuedCommandJSON("plus do X", tsEarlyS5),
		testjsonl.ClaudeAssistantJSON("done", tsLate),
	)
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	newMsgs, _, _, err := ParseClaudeSessionFrom(path, offset, 2)
	require.NoError(t, err)
	require.Len(t, newMsgs, 2)

	// queued_command first (earlier timestamp), then assistant.
	assert.Equal(t, RoleUser, newMsgs[0].Role)
	assert.Equal(t, "plus do X", newMsgs[0].Content)
	assert.Equal(t, "queued_command", newMsgs[0].SourceSubtype)
	assert.Equal(t, 2, newMsgs[0].Ordinal)

	assert.Equal(t, RoleAssistant, newMsgs[1].Role)
	assert.Equal(t, 3, newMsgs[1].Ordinal)
}

func TestParseClaudeSessionFrom_SkipsNonMessages(
	t *testing.T,
) {
	t.Parallel()

	// Initial content with a "system" type line mixed in.
	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("first", tsEarly),
	)
	path := createTestFile(
		t, "inc-claude-skip.jsonl", initial,
	)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	// Append a system line followed by a real message.
	appended := `{"type":"system","timestamp":"` +
		tsEarlyS5 + `","message":{}}` + "\n" +
		testjsonl.ClaudeAssistantJSON("response", tsLate) +
		"\n"
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	newMsgs, _, _, err := ParseClaudeSessionFrom(
		path, offset, 1,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, len(newMsgs))
	assert.Equal(t, RoleAssistant, newMsgs[0].Role)
	assert.Equal(t, 1, newMsgs[0].Ordinal)
}

func TestParseClaudeSessionFrom_NoNewData(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("only msg", tsEarly),
	)
	path := createTestFile(
		t, "inc-claude-empty.jsonl", content,
	)

	info, err := os.Stat(path)
	require.NoError(t, err)

	// Parse from EOF — should return empty.
	newMsgs, endedAt, _, err := ParseClaudeSessionFrom(
		path, info.Size(), 1,
	)
	require.NoError(t, err)
	assert.Empty(t, newMsgs)
	assert.True(t, endedAt.IsZero())
}

func TestParseClaudeSessionFrom_PartialLineAtEOF(
	t *testing.T,
) {
	t.Parallel()

	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", tsEarly),
	)
	path := createTestFile(
		t, "inc-partial.jsonl", initial,
	)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	// Append a complete line + a partial (truncated) line.
	complete := testjsonl.ClaudeAssistantJSON(
		"complete", tsEarlyS5,
	) + "\n"
	partial := `{"type":"user","timestamp":"` + tsLate
	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err)
	_, err = f.WriteString(complete + partial)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	newMsgs, _, consumed, err := ParseClaudeSessionFrom(
		path, offset, 1,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, len(newMsgs))
	assert.Equal(t, RoleAssistant, newMsgs[0].Role)

	// consumed should cover only the complete line, not
	// the partial one.
	assert.Equal(t, int64(len(complete)), consumed)
}

func TestParseClaudeSessionFrom_DAGDetected(
	t *testing.T,
) {
	t.Parallel()

	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", tsEarly),
	)
	path := createTestFile(
		t, "inc-dag.jsonl", initial,
	)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	// Append two entries that form a fork: both have the
	// same parentUuid but different uuids.
	fork1 := `{"type":"user","uuid":"child-1",` +
		`"parentUuid":"root-1",` +
		`"timestamp":"` + tsEarlyS5 +
		`","message":{"content":"branch A"}}` + "\n"
	fork2 := `{"type":"assistant","uuid":"child-2",` +
		`"parentUuid":"root-1",` +
		`"timestamp":"` + tsLate +
		`","message":{"content":[` +
		`{"type":"text","text":"branch B"}]}}` + "\n"

	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err)
	_, err = f.WriteString(fork1 + fork2)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, _, _, err = ParseClaudeSessionFrom(
		path, offset, 1,
	)
	assert.ErrorIs(t, err, ErrDAGDetected)
}

func TestParseClaudeSessionFrom_DAGAcrossNonUUID(
	t *testing.T,
) {
	t.Parallel()

	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", tsEarly),
	)
	path := createTestFile(
		t, "inc-dag-gap.jsonl", initial,
	)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	// Append: UUID entry, then a non-UUID entry (no uuid
	// field), then another UUID entry whose parentUuid
	// doesn't match the first UUID entry. The non-UUID gap
	// must not prevent fork detection.
	line1 := `{"type":"user","uuid":"u1",` +
		`"parentUuid":"pre",` +
		`"timestamp":"` + tsEarlyS5 +
		`","message":{"content":"a"}}` + "\n"
	noUUID := `{"type":"user",` +
		`"timestamp":"` + tsLate +
		`","message":{"content":"gap"}}` + "\n"
	line2 := `{"type":"assistant","uuid":"u2",` +
		`"parentUuid":"other",` +
		`"timestamp":"` + tsLate +
		`","message":{"content":[` +
		`{"type":"text","text":"b"}]}}` + "\n"

	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err)
	_, err = f.WriteString(line1 + noUUID + line2)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, _, _, err = ParseClaudeSessionFrom(
		path, offset, 1,
	)
	assert.ErrorIs(t, err, ErrDAGDetected)
}

func TestParseClaudeSessionFrom_LinearUUID(
	t *testing.T,
) {
	t.Parallel()

	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", tsEarly),
	)
	path := createTestFile(
		t, "inc-linear-uuid.jsonl", initial,
	)

	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	// Append UUID-bearing entries that form a linear chain
	// (each entry's parentUuid == previous entry's uuid).
	// This should NOT trigger ErrDAGDetected.
	line1 := `{"type":"user","uuid":"u1",` +
		`"parentUuid":"pre-existing",` +
		`"timestamp":"` + tsEarlyS5 +
		`","message":{"content":"msg1"}}` + "\n"
	line2 := `{"type":"assistant","uuid":"u2",` +
		`"parentUuid":"u1",` +
		`"timestamp":"` + tsLate +
		`","message":{"content":[` +
		`{"type":"text","text":"reply"}]}}` + "\n"

	f, err := os.OpenFile(
		path, os.O_APPEND|os.O_WRONLY, 0o644,
	)
	require.NoError(t, err)
	_, err = f.WriteString(line1 + line2)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	newMsgs, endedAt, _, err := ParseClaudeSessionFrom(
		path, offset, 1,
	)
	require.NoError(t, err)
	assert.Equal(t, 2, len(newMsgs))
	assert.Equal(t, 1, newMsgs[0].Ordinal)
	assert.Equal(t, 2, newMsgs[1].Ordinal)
	assert.False(t, endedAt.IsZero())
}

func TestParseClaudeSession_TerminationStatus(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		content := loadFixture(t, "claude/valid_session.jsonl")
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, TerminationClean, sess.TerminationStatus)
	})

	t.Run("tool_call_pending", func(t *testing.T) {
		content := loadFixture(t, "claude/tool_call_pending.jsonl")
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, TerminationToolCallPending, sess.TerminationStatus)
	})

	t.Run("truncated", func(t *testing.T) {
		content := loadFixture(t, "claude/truncated.jsonl")
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, TerminationTruncated, sess.TerminationStatus)
	})
}

func TestParseClaudeSession_TokenUsage(t *testing.T) {
	t.Run("explicit parser presence beats fallback inference", func(t *testing.T) {
		msg := ParsedMessage{
			TokenUsage:         json.RawMessage(`{"input_tokens":100,"output_tokens":50}`),
			tokenPresenceKnown: true,
		}
		msgHasCtx, msgHasOut := msg.TokenPresence()
		assert.False(t, msgHasCtx)
		assert.False(t, msgHasOut)

		sess := ParsedSession{
			TotalOutputTokens:           50,
			PeakContextTokens:           100,
			aggregateTokenPresenceKnown: true,
		}
		sessHasTotal, sessHasPeak := sess.AggregateTokenPresence()
		assert.False(t, sessHasTotal)
		assert.False(t, sessHasPeak)
	})

	t.Run("per-message token fields from fixture", func(t *testing.T) {
		content := loadFixture(t, "claude/valid_session.jsonl")
		_, msgs := runClaudeParserTest(t, "test.jsonl", content)

		// msgs[0] is user (no usage), msgs[1] is assistant (has usage),
		// msgs[2] is user (no usage), msgs[3] is assistant (has usage).
		assert.Equal(t, 0, msgs[0].ContextTokens)
		assert.Equal(t, 0, msgs[0].OutputTokens)
		assert.False(t, msgs[0].HasContextTokens)
		assert.False(t, msgs[0].HasOutputTokens)
		assert.Empty(t, msgs[0].Model)
		assert.Empty(t, msgs[0].TokenUsage)

		// input=100, cache_creation=200, cache_read=300 -> context=600
		assert.Equal(t, 600, msgs[1].ContextTokens)
		assert.Equal(t, 50, msgs[1].OutputTokens)
		assert.True(t, msgs[1].HasContextTokens)
		assert.True(t, msgs[1].HasOutputTokens)
		assert.Equal(t, "claude-sonnet-4-20250514", msgs[1].Model)
		assert.Contains(t, string(msgs[1].TokenUsage), `"input_tokens":100`)

		assert.Equal(t, 0, msgs[2].ContextTokens)
		assert.Equal(t, 0, msgs[2].OutputTokens)
		assert.False(t, msgs[2].HasContextTokens)
		assert.False(t, msgs[2].HasOutputTokens)

		// input=150, cache_creation=0, cache_read=500 -> context=650
		assert.Equal(t, 650, msgs[3].ContextTokens)
		assert.Equal(t, 75, msgs[3].OutputTokens)
		assert.True(t, msgs[3].HasContextTokens)
		assert.True(t, msgs[3].HasOutputTokens)
		assert.Equal(t, "claude-sonnet-4-20250514", msgs[3].Model)
		assert.Contains(t, string(msgs[3].TokenUsage), `"input_tokens":150`)
	})

	t.Run("session totals from fixture", func(t *testing.T) {
		content := loadFixture(t, "claude/valid_session.jsonl")
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)

		assert.Equal(t, 125, sess.TotalOutputTokens)
		assert.Equal(t, 650, sess.PeakContextTokens)
		assert.True(t, sess.HasTotalOutputTokens)
		assert.True(t, sess.HasPeakContextTokens)
	})

	t.Run("messages without usage get zero values", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("hello", tsZero),
			testjsonl.ClaudeAssistantJSON([]map[string]any{
				{"type": "text", "text": "hi there"},
			}, tsZeroS1),
		)
		sess, msgs := runClaudeParserTest(t, "test.jsonl", content)

		assert.Equal(t, 0, msgs[0].ContextTokens)
		assert.Equal(t, 0, msgs[1].ContextTokens)
		assert.Equal(t, 0, msgs[1].OutputTokens)
		assert.False(t, msgs[0].HasContextTokens)
		assert.False(t, msgs[0].HasOutputTokens)
		assert.False(t, msgs[1].HasContextTokens)
		assert.False(t, msgs[1].HasOutputTokens)
		assert.Empty(t, msgs[1].TokenUsage)

		assert.Equal(t, 0, sess.TotalOutputTokens)
		assert.Equal(t, 0, sess.PeakContextTokens)
		assert.False(t, sess.HasTotalOutputTokens)
		assert.False(t, sess.HasPeakContextTokens)
	})

	t.Run("zero-valued usage keys preserve coverage", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("hello", tsZero),
			`{"type":"assistant","timestamp":"`+tsZeroS1+`","message":{"model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"still counted"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`,
		)
		sess, msgs := runClaudeParserTest(t, "test.jsonl", content)

		require.Equal(t, 2, len(msgs))
		assert.Equal(t, 0, msgs[1].ContextTokens)
		assert.Equal(t, 0, msgs[1].OutputTokens)
		assert.True(t, msgs[1].HasContextTokens)
		assert.True(t, msgs[1].HasOutputTokens)
		msgHasCtx, msgHasOut := msgs[1].TokenPresence()
		assert.True(t, msgHasCtx)
		assert.True(t, msgHasOut)

		assert.Equal(t, 0, sess.TotalOutputTokens)
		assert.Equal(t, 0, sess.PeakContextTokens)
		assert.True(t, sess.HasTotalOutputTokens)
		assert.True(t, sess.HasPeakContextTokens)
		sessHasTotal, sessHasPeak := sess.AggregateTokenPresence()
		assert.True(t, sessHasTotal)
		assert.True(t, sessHasPeak)
		coverageTotal, coveragePeak := sess.TokenCoverage(msgs)
		assert.True(t, coverageTotal)
		assert.True(t, coveragePeak)
	})
}

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func TestTruncateRespectsRuneBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{
			name:   "ASCII within limit",
			input:  "hello",
			maxLen: 10,
			want:   "hello",
		},
		{
			name:   "ASCII truncated",
			input:  "hello world",
			maxLen: 5,
			want:   "hello...",
		},
		{
			name:   "multibyte within limit",
			input:  "café",
			maxLen: 10,
			want:   "café",
		},
		{
			name: "multibyte at boundary",
			// 4 runes: c, a, f, é — truncate at 3 runes
			input:  "café",
			maxLen: 3,
			want:   "caf...",
		},
		{
			name: "CJK characters",
			// 3 runes, each 3 bytes
			input:  "你好世界",
			maxLen: 2,
			want:   "你好...",
		},
		{
			name: "ellipsis character preserved",
			// U+2026 is 3 bytes but 1 rune
			input:  "abc\u2026def",
			maxLen: 4,
			want:   "abc\u2026...",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.input, tc.maxLen)
			if got != tc.want {
				t.Errorf(
					"truncate(%q, %d) = %q, want %q",
					tc.input, tc.maxLen, got, tc.want,
				)
			}
			// Verify result is valid UTF-8.
			if !utf8.ValidString(got) {
				t.Errorf(
					"truncate produced invalid UTF-8: %q",
					got,
				)
			}
		})
	}
}

func TestParseClaudeSession_ExtractsMessageIDAndRequestID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-1.jsonl")
	// Single assistant line with usage + id + requestId.
	line := `{"type":"assistant","uuid":"u1","parentUuid":"",` +
		`"timestamp":"2026-04-10T10:00:00.000Z",` +
		`"requestId":"req_01ABC",` +
		`"message":{"id":"msg_01XYZ","model":"claude-opus-4-6",` +
		`"content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":10,"output_tokens":20,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	results, err := ParseClaudeSession(path, "proj", "m")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	msgs := results[0].Messages
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	m := msgs[0]
	if m.ClaudeMessageID != "msg_01XYZ" {
		t.Errorf("ClaudeMessageID = %q, want msg_01XYZ", m.ClaudeMessageID)
	}
	if m.ClaudeRequestID != "req_01ABC" {
		t.Errorf("ClaudeRequestID = %q, want req_01ABC", m.ClaudeRequestID)
	}
	if m.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20", m.OutputTokens)
	}
}

func TestParseClaudeSession_CompactBoundary(t *testing.T) {
	t.Parallel()

	compactEntry := `{"type":"user","isCompactSummary":true,` +
		`"message":{"role":"user","content":[` +
		`{"type":"text","text":"Summary of conversation so far..."},` +
		`{"type":"text","text":"Additional context."}` +
		`]},"uuid":"compact-uuid","parentUuid":"parent-uuid",` +
		`"isSidechain":true,"timestamp":"` + tsZeroS1 + `"}`

	t.Run("linear path", func(t *testing.T) {
		t.Parallel()
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("hello", tsZero),
			compactEntry,
			testjsonl.ClaudeUserJSON("after compact", tsZeroS2),
		)
		sess, msgs := runClaudeParserTest(
			t, "test.jsonl", content,
		)

		require.Len(t, msgs, 3)
		assert.Equal(t, 3, sess.MessageCount)
		// Only real user messages count.
		assert.Equal(t, 2, sess.UserMessageCount)
		assert.Equal(t, "hello", sess.FirstMessage)

		// Verify compact boundary at ordinal 1.
		cb := msgs[1]
		assert.Equal(t, 1, cb.Ordinal)
		assert.Equal(t, RoleAssistant, cb.Role)
		assert.True(t, cb.IsSystem)
		assert.True(t, cb.IsCompactBoundary)
		assert.Equal(t, "system", cb.SourceType)
		assert.Equal(t, "compact_boundary", cb.SourceSubtype)
		assert.Equal(t, "compact-uuid", cb.SourceUUID)
		assert.Equal(t, "parent-uuid", cb.SourceParentUUID)
		assert.True(t, cb.IsSidechain)
		assert.Equal(
			t,
			"Summary of conversation so far...\n"+
				"Additional context.",
			cb.Content,
		)
		assert.Equal(t, len(cb.Content), cb.ContentLength)

		// Following message has ordinal 2.
		assert.Equal(t, 2, msgs[2].Ordinal)
		assert.Equal(t, RoleUser, msgs[2].Role)
	})

	t.Run("DAG path", func(t *testing.T) {
		t.Parallel()
		content := testjsonl.JoinJSONL(
			`{"type":"user","uuid":"u1","parentUuid":"",`+
				`"timestamp":"`+tsZero+`",`+
				`"message":{"content":"hello"}}`,
			`{"type":"user","isCompactSummary":true,`+
				`"uuid":"u2","parentUuid":"u1",`+
				`"timestamp":"`+tsZeroS1+`",`+
				`"isSidechain":false,`+
				`"message":{"role":"user","content":[`+
				`{"type":"text","text":"DAG compact summary"}`+
				`]}}`,
			`{"type":"user","uuid":"u3","parentUuid":"u2",`+
				`"timestamp":"`+tsZeroS2+`",`+
				`"message":{"content":"after compact"}}`,
		)
		sess, msgs := runClaudeParserTest(
			t, "test.jsonl", content,
		)

		require.Len(t, msgs, 3)
		assert.Equal(t, 2, sess.UserMessageCount)

		cb := msgs[1]
		assert.Equal(t, RoleAssistant, cb.Role)
		assert.True(t, cb.IsSystem)
		assert.True(t, cb.IsCompactBoundary)
		assert.Equal(t, "DAG compact summary", cb.Content)
		assert.Equal(t, "u2", cb.SourceUUID)
		assert.Equal(t, "u1", cb.SourceParentUUID)
		assert.False(t, cb.IsSidechain)
	})

	t.Run("incremental path", func(t *testing.T) {
		t.Parallel()
		initial := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("first", tsEarly),
		)
		path := createTestFile(
			t, "inc-compact.jsonl", initial,
		)

		info, err := os.Stat(path)
		require.NoError(t, err)
		offset := info.Size()

		// Append compact boundary + real message.
		appended := compactEntry + "\n" +
			testjsonl.ClaudeUserJSON(
				"after compact", tsEarlyS5,
			) + "\n"
		f, err := os.OpenFile(
			path, os.O_APPEND|os.O_WRONLY, 0o644,
		)
		require.NoError(t, err)
		_, err = f.WriteString(appended)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		newMsgs, _, _, err := ParseClaudeSessionFrom(
			path, offset, 1,
		)
		require.NoError(t, err)
		require.Len(t, newMsgs, 2)

		cb := newMsgs[0]
		assert.Equal(t, 1, cb.Ordinal)
		assert.Equal(t, RoleAssistant, cb.Role)
		assert.True(t, cb.IsSystem)
		assert.True(t, cb.IsCompactBoundary)
		assert.Equal(t, "system", cb.SourceType)
		assert.Equal(t, "compact_boundary", cb.SourceSubtype)

		assert.Equal(t, 2, newMsgs[1].Ordinal)
		assert.Equal(t, RoleUser, newMsgs[1].Role)
	})
}

func TestExtractTextContent_ReturnsThinkingText(t *testing.T) {
	t.Parallel()

	t.Run("joins multiple thinking blocks", func(t *testing.T) {
		content := gjson.Parse(`[
			{"type":"thinking","thinking":"first thought"},
			{"type":"text","text":"reply A"},
			{"type":"thinking","thinking":"second thought"},
			{"type":"text","text":"reply B"}
		]`)
		text, thinking, hasThinking, _, _, _ := ExtractTextContent(content)
		assert.True(t, hasThinking)
		assert.Equal(t, "first thought\n\nsecond thought", thinking)
		assert.Contains(t, text, "[Thinking]\nfirst thought\n[/Thinking]")
		assert.Contains(t, text, "reply A")
	})

	t.Run("skips empty thinking blocks", func(t *testing.T) {
		content := gjson.Parse(`[
			{"type":"thinking","thinking":""},
			{"type":"thinking","thinking":"real thought"}
		]`)
		_, thinking, _, _, _, _ := ExtractTextContent(content)
		assert.Equal(t, "real thought", thinking)
	})
}

func TestClassifyClaudeSystemMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		content  string
		expected string
	}{
		{"continuation", "This session is being continued from a previous conversation...", "continuation"},
		{"resume caveat", "<local-command-caveat>Caveat: ...</local-command-caveat>", "resume"},
		{"interrupted", "[Request interrupted by user]", "interrupted"},
		{"task notification", "<task-notification>done</task-notification>", "task_notification"},
		{"stop hook", "Stop hook feedback: ...", "stop_hook"},
		{"bom prefix", "\uFEFF  This session is being continued", "continuation"},
		{"non-caveat local-command", "<local-command-stdout>foo</local-command-stdout>", ""},
		{"regular text", "what do you think?", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyClaudeSystemMessage(c.content)
			assert.Equal(t, c.expected, got)
		})
	}
}

func TestParseClaudeSession_PromotesSystemSubtypes(t *testing.T) {
	t.Parallel()
	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("what time is it?", tsZero),
		testjsonl.ClaudeAssistantJSON([]map[string]any{
			{"type": "text", "text": "it is 3pm"},
		}, tsZeroS1),
		// Promoted: continuation marker on resume.
		testjsonl.ClaudeUserJSON(
			"This session is being continued from a previous conversation.",
			tsZeroS2,
		),
		testjsonl.ClaudeAssistantJSON([]map[string]any{
			{"type": "text", "text": "welcome back"},
		}, "2024-01-01T00:00:03Z"),
		// Promoted: interrupt marker.
		testjsonl.ClaudeUserJSON(
			"[Request interrupted by user]",
			"2024-01-01T00:00:04Z",
		),
	)
	_, msgs := runClaudeParserTest(t, "test.jsonl", content)

	var systems []ParsedMessage
	for _, m := range msgs {
		if m.IsSystem {
			systems = append(systems, m)
		}
	}
	require.Len(t, systems, 2)

	assert.Equal(t, "continuation", systems[0].SourceSubtype)
	assert.Equal(t, "system", systems[0].SourceType)
	// Role stays "user" so analytics that key off role alone
	// don't count these markers as assistant replies. The UI uses
	// is_system + source_subtype for routing.
	assert.Equal(t, RoleUser, systems[0].Role)
	assert.Contains(t, systems[0].Content, "This session is being continued")

	assert.Equal(t, "interrupted", systems[1].SourceSubtype)
	assert.Equal(t, "system", systems[1].SourceType)
	assert.Equal(t, RoleUser, systems[1].Role)
}

func TestIsSkippablePreviewCommand(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"bare /clear", "/clear", true},
		{"bare /effort", "/effort", true},
		{"/clear with trailing space", "/clear ", true},
		{"/clear with args", "/clear foo", true},
		{"/effort with args", "/effort max", true},
		{"surrounded by whitespace", "  /clear  ", true},
		{"/clear with tab", "/clear\tfoo", true},
		{"/clear with newline", "/clear\nfoo", true},
		{"empty string", "", false},
		{"/clearcache (no word boundary)", "/clearcache", false},
		{"/effortless (no word boundary)", "/effortless", false},
		{"/cleareffort", "/cleareffort", false},
		{"unrelated command", "/unrelated", false},
		{"prose containing /clear", "hello /clear", false},
		{"/clear-xyz (dash not whitespace)", "/clear-xyz", false},
		{"plain text", "Fix the login bug", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isSkippablePreviewCommand(tc.content)
			assert.Equal(t, tc.want, got,
				"content=%q", tc.content)
		})
	}
}

func TestParseClaudeSession_SkipClearEffortFirstMessage(t *testing.T) {
	t.Run("single /clear followed by real message", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON(
				"<command-name>/clear</command-name>",
				tsZero,
			),
			testjsonl.ClaudeUserJSON("Fix the login bug", tsZeroS1),
			testjsonl.ClaudeAssistantJSON([]map[string]any{
				{"type": "text", "text": "ok"},
			}, tsZeroS2),
		)
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, "Fix the login bug", sess.FirstMessage)
		assert.Equal(t, 2, sess.UserMessageCount,
			"skipped commands still count as user turns")
	})

	t.Run("cascade /effort then /clear then real", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON(
				"<command-name>/effort</command-name>\n<command-args>max</command-args>",
				tsZero,
			),
			testjsonl.ClaudeUserJSON(
				"<command-name>/clear</command-name>",
				tsZeroS1,
			),
			testjsonl.ClaudeUserJSON("Real question", tsZeroS2),
		)
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, "Real question", sess.FirstMessage)
		assert.Equal(t, 3, sess.UserMessageCount)
	})

	t.Run("all messages are skipped commands", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON(
				"<command-name>/clear</command-name>",
				tsZero,
			),
			testjsonl.ClaudeUserJSON(
				"<command-name>/effort</command-name>",
				tsZeroS1,
			),
		)
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, "", sess.FirstMessage)
		assert.Equal(t, 2, sess.UserMessageCount)
	})

	t.Run("non-skipped command still becomes first_message", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON(
				"<command-message>roborev-fix</command-message>\n<command-name>/roborev-fix</command-name>\n<command-args>450</command-args>",
				tsZero,
			),
			testjsonl.ClaudeUserJSON("follow-up", tsZeroS1),
		)
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, "/roborev-fix 450", sess.FirstMessage)
	})
}
