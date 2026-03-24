package parser

import (
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	t.Run("skips isCompactSummary user messages", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeMetaUserJSON("summary of prior turns", tsZero, false, true),
			testjsonl.ClaudeUserJSON("actual prompt", tsZeroS1),
		)
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 1, sess.MessageCount)
		assert.Equal(t, "actual prompt", sess.FirstMessage)
	})

	t.Run("skips content-heuristic system messages", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON("This session is being continued from a previous conversation.", tsZero),
			testjsonl.ClaudeUserJSON("[Request interrupted by user]", tsZeroS1),
			testjsonl.ClaudeUserJSON("<task-notification>data</task-notification>", tsZeroS2),
			testjsonl.ClaudeUserJSON("<local-command-result>ok</local-command-result>", "2024-01-01T00:00:05Z"),
			testjsonl.ClaudeUserJSON("Stop hook feedback: rejected", "2024-01-01T00:00:06Z"),
			testjsonl.ClaudeUserJSON("real user message", "2024-01-01T00:00:07Z"),
		)
		sess, msgs := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, 1, sess.MessageCount)
		assert.Equal(t, "real user message", msgs[0].Content)
		assert.Equal(t, "real user message", sess.FirstMessage)
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
		assert.Equal(t, 1, sess.MessageCount)
		assert.Equal(t, "Fix the auth bug", sess.FirstMessage)
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

func TestParseClaudeSession_TokenUsage(t *testing.T) {
	t.Run("per-message token fields from fixture", func(t *testing.T) {
		content := loadFixture(t, "claude/valid_session.jsonl")
		_, msgs := runClaudeParserTest(t, "test.jsonl", content)

		// msgs[0] is user (no usage), msgs[1] is assistant (has usage),
		// msgs[2] is user (no usage), msgs[3] is assistant (has usage).
		assert.Equal(t, 0, msgs[0].ContextTokens)
		assert.Equal(t, 0, msgs[0].OutputTokens)
		assert.Empty(t, msgs[0].Model)
		assert.Empty(t, msgs[0].TokenUsage)

		// input=100, cache_creation=200, cache_read=300 -> context=600
		assert.Equal(t, 600, msgs[1].ContextTokens)
		assert.Equal(t, 50, msgs[1].OutputTokens)
		assert.Equal(t, "claude-sonnet-4-20250514", msgs[1].Model)
		assert.Contains(t, string(msgs[1].TokenUsage), `"input_tokens":100`)

		assert.Equal(t, 0, msgs[2].ContextTokens)
		assert.Equal(t, 0, msgs[2].OutputTokens)

		// input=150, cache_creation=0, cache_read=500 -> context=650
		assert.Equal(t, 650, msgs[3].ContextTokens)
		assert.Equal(t, 75, msgs[3].OutputTokens)
		assert.Equal(t, "claude-sonnet-4-20250514", msgs[3].Model)
		assert.Contains(t, string(msgs[3].TokenUsage), `"input_tokens":150`)
	})

	t.Run("session totals from fixture", func(t *testing.T) {
		content := loadFixture(t, "claude/valid_session.jsonl")
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)

		assert.Equal(t, 125, sess.TotalOutputTokens)
		assert.Equal(t, 650, sess.PeakContextTokens)
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
		assert.Empty(t, msgs[1].TokenUsage)

		assert.Equal(t, 0, sess.TotalOutputTokens)
		assert.Equal(t, 0, sess.PeakContextTokens)
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
