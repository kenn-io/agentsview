package parser

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wesm/agentsview/internal/testjsonl"
)

func runGeminiParserTest(t *testing.T, content string) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	path := createTestFile(t, "session.json", content)
	sess, msgs, err := ParseGeminiSession(path, "my_project", "local")
	require.NoError(t, err)
	return sess, msgs
}

func TestParseGeminiSession_Basic(t *testing.T) {
	content := loadFixture(t, "gemini/standard_session.json")
	sess, msgs := runGeminiParserTest(t, content)

	require.NotNil(t, sess)
	assertMessageCount(t, len(msgs), 4)
	assertMessageCount(t, sess.MessageCount, 4)
	assertSessionMeta(t, sess, "gemini:sess-uuid-1", "my_project", AgentGemini)
	assert.Equal(t, "Fix the login bug", sess.FirstMessage)

	assertMessage(t, msgs[0], RoleUser, "Fix the login bug")
	assertMessage(t, msgs[1], RoleAssistant, "Looking at")
	assert.Equal(t, 0, msgs[0].Ordinal)
	assert.Equal(t, 1, msgs[1].Ordinal)
}

func TestParseGeminiSession_JSONLStream(t *testing.T) {
	content := strings.Join([]string{
		`{"sessionId":"sess-jsonl-1","projectHash":"hash","startTime":"2026-04-23T16:12:42.783Z","lastUpdated":"2026-04-23T16:12:42.783Z","kind":"main"}`,
		`{"id":"u1","timestamp":"2026-04-23T16:12:43.085Z","type":"user","content":[{"text":"Fix the import path"}]}`,
		`{"$set":{"lastUpdated":"2026-04-23T16:12:43.085Z"}}`,
		`{"id":"a1","timestamp":"2026-04-23T16:12:50.158Z","type":"gemini","content":"","thoughts":[{"subject":"Planning","description":"Looking for the failure.","timestamp":"2026-04-23T16:12:46.795Z"}],"tokens":{"input":9184,"output":26,"cached":0},"model":"gemini-3.1-pro-preview"}`,
		`{"id":"a1","timestamp":"2026-04-23T16:12:50.158Z","type":"gemini","content":"I found the issue.","thoughts":[{"subject":"Planning","description":"Looking for the failure.","timestamp":"2026-04-23T16:12:46.795Z"}],"tokens":{"input":9184,"output":26,"cached":0},"model":"gemini-3.1-pro-preview","toolCalls":[{"id":"read_file_1","name":"read_file","args":{"file_path":"main.go"},"result":[{"functionResponse":{"id":"read_file_1","name":"read_file","response":{"output":"package main"}}}],"displayName":"ReadFile"}]}`,
		`{"$set":{"lastUpdated":"2026-04-23T16:12:50.158Z"}}`,
	}, "\n")
	path := createTestFile(t, "session.jsonl", content)
	sess, msgs, err := ParseGeminiSession(path, "my_project", "local")
	require.NoError(t, err)

	require.NotNil(t, sess)
	require.Equal(t, 2, len(msgs))
	assertSessionMeta(t, sess, "gemini:sess-jsonl-1", "my_project", AgentGemini)
	assert.Equal(t, "Fix the import path", sess.FirstMessage)
	assertMessage(t, msgs[0], RoleUser, "Fix the import path")
	assertMessage(t, msgs[1], RoleAssistant, "I found the issue.")
	assert.True(t, msgs[1].HasThinking)
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "read_file_1", msgs[1].ToolCalls[0].ToolUseID)
	require.Len(t, msgs[1].ToolResults, 1)
	assert.Equal(t, "package main", DecodeContent(msgs[1].ToolResults[0].ContentRaw))
	assert.Equal(
		t,
		parseTimestamp("2026-04-23T16:12:50.158Z"),
		sess.EndedAt,
	)
}

func TestParseGeminiSession_JSONLStreamLargeRecord(t *testing.T) {
	largeContent := strings.Repeat("x", 16*1024*1024+1)
	content := strings.Join([]string{
		`{"sessionId":"sess-jsonl-large","projectHash":"hash","startTime":"2026-04-23T16:12:42.783Z","lastUpdated":"2026-04-23T16:12:42.783Z","kind":"main"}`,
		`{"id":"u1","timestamp":"2026-04-23T16:12:43.085Z","type":"user","content":[{"text":"` + largeContent + `"}]}`,
	}, "\n")
	path := createTestFile(t, "large-session.jsonl", content)
	sess, msgs, err := ParseGeminiSession(path, "my_project", "local")
	require.NoError(t, err)

	require.NotNil(t, sess)
	require.Len(t, msgs, 1)
	assertSessionMeta(t, sess, "gemini:sess-jsonl-large", "my_project", AgentGemini)
	assert.Equal(t, len(largeContent), len(msgs[0].Content))
}

func TestParseGeminiSession_JSONLStreamTolerantOfPartialLines(t *testing.T) {
	t.Run("partial trailing write", func(t *testing.T) {
		content := strings.Join([]string{
			`{"sessionId":"sess-jsonl-partial","projectHash":"hash","startTime":"2026-04-23T16:12:42.783Z","lastUpdated":"2026-04-23T16:12:42.783Z","kind":"main"}`,
			`{"id":"u1","timestamp":"2026-04-23T16:12:43.085Z","type":"user","content":[{"text":"first"}]}`,
			`{"id":"a1","timestamp":"2026-04-23T16:12:50.158Z","type":"gemini","content":"reply"`,
		}, "\n")
		path := createTestFile(t, "session.jsonl", content)
		sess, msgs, err := ParseGeminiSession(path, "my_project", "local")
		require.NoError(t, err)

		require.NotNil(t, sess)
		require.Equal(t, 1, len(msgs))
		assertMessage(t, msgs[0], RoleUser, "first")
	})

	t.Run("malformed line mid-stream", func(t *testing.T) {
		content := strings.Join([]string{
			`{"sessionId":"sess-jsonl-mid","projectHash":"hash","startTime":"2026-04-23T16:12:42.783Z","lastUpdated":"2026-04-23T16:12:42.783Z","kind":"main"}`,
			`{"id":"u1","timestamp":"2026-04-23T16:12:43.085Z","type":"user","content":[{"text":"first"}]}`,
			`{not valid json`,
			`{"id":"a1","timestamp":"2026-04-23T16:12:50.158Z","type":"gemini","content":"reply"}`,
			"",
		}, "\n")
		path := createTestFile(t, "session.jsonl", content)
		sess, msgs, err := ParseGeminiSession(path, "my_project", "local")
		require.NoError(t, err)

		require.NotNil(t, sess)
		require.Equal(t, 2, len(msgs))
		assertMessage(t, msgs[0], RoleUser, "first")
		assertMessage(t, msgs[1], RoleAssistant, "reply")
	})
}

func TestParseGeminiSession_ToolCalls(t *testing.T) {
	t.Run("basic tool calls", func(t *testing.T) {
		content := loadFixture(t, "gemini/tool_calls.json")
		_, msgs := runGeminiParserTest(t, content)

		assert.Equal(t, 2, len(msgs))
		assert.True(t, msgs[1].HasToolUse)
		assert.True(t, msgs[1].HasThinking)
		assert.True(t, strings.Contains(msgs[1].Content, "[Thinking]\nPlanning\n"))
		assert.True(t, strings.Contains(msgs[1].Content, "[/Thinking]"))
		assert.True(t, strings.Contains(msgs[1].Content, "[Read: main.go]"))
		// Chronological: thinking before content before tool calls
		thinkIdx := strings.Index(msgs[1].Content, "[Thinking]")
		contentIdx := strings.Index(msgs[1].Content, "Let me read it.")
		toolIdx := strings.Index(msgs[1].Content, "[Read:")
		assert.Less(t, thinkIdx, contentIdx)
		assert.Less(t, contentIdx, toolIdx)
		assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{ToolName: "read_file", Category: "Read"}})
	})

	t.Run("tool calls with results", func(t *testing.T) {
		content := loadFixture(t, "gemini/tool_calls_with_results.json")
		_, msgs := runGeminiParserTest(t, content)

		require.Equal(t, 2, len(msgs))
		assistantMsg := msgs[1]
		assert.True(t, assistantMsg.HasToolUse)

		// Verify ToolUseID and InputJSON are extracted
		require.Equal(t, 2, len(assistantMsg.ToolCalls))
		assertToolCalls(t, assistantMsg.ToolCalls, []ParsedToolCall{
			{
				ToolName:  "read_file",
				Category:  "Read",
				ToolUseID: "read_file_1772747340739_0",
				InputJSON: `{"file_path":".planning/ONE-PAGER.md"}`,
			},
			{
				ToolName:  "run_command",
				Category:  "Bash",
				ToolUseID: "run_command_1772747340739_1",
				InputJSON: `{"command":"ls -la"}`,
			},
		})

		// Verify tool results are extracted
		require.Equal(t, 2, len(assistantMsg.ToolResults))
		assert.Equal(t, "read_file_1772747340739_0", assistantMsg.ToolResults[0].ToolUseID)
		assert.Equal(t, len("# Agentstrove -- One-Pager\n\nDraft: 2026-03-04"), assistantMsg.ToolResults[0].ContentLength)
		// Verify DecodeContent works on the raw content
		assert.Equal(t, "# Agentstrove -- One-Pager\n\nDraft: 2026-03-04", DecodeContent(assistantMsg.ToolResults[0].ContentRaw))

		assert.Equal(t, "run_command_1772747340739_1", assistantMsg.ToolResults[1].ToolUseID)
		assert.Equal(t, "total 42\ndrwxr-xr-x  5 user user 160 Mar  4 10:00 .", DecodeContent(assistantMsg.ToolResults[1].ContentRaw))
	})

	t.Run("programmatic tool call with result", func(t *testing.T) {
		content := testjsonl.GeminiSessionJSON("sess-tc-result", "hash", tsEarly, tsEarlyS5, []map[string]any{
			testjsonl.GeminiUserMsg("u1", tsEarly, "list files"),
			testjsonl.GeminiAssistantMsg("a1", tsEarlyS5, "Running command.", &testjsonl.GeminiMsgOpts{
				ToolCalls: []testjsonl.GeminiToolCall{
					{
						ID:           "run_cmd_1",
						Name:         "run_command",
						DisplayName:  "RunCommand",
						Args:         map[string]string{"command": "ls"},
						ResultOutput: "file1.go\nfile2.go",
					},
				},
			}),
		})
		_, msgs := runGeminiParserTest(t, content)
		require.Equal(t, 2, len(msgs))
		require.Equal(t, 1, len(msgs[1].ToolCalls))
		assert.Equal(t, "run_cmd_1", msgs[1].ToolCalls[0].ToolUseID)

		require.Equal(t, 1, len(msgs[1].ToolResults))
		assert.Equal(t, "run_cmd_1", msgs[1].ToolResults[0].ToolUseID)
		assert.Equal(t, len("file1.go\nfile2.go"), msgs[1].ToolResults[0].ContentLength)
		assert.Equal(t, "file1.go\nfile2.go", DecodeContent(msgs[1].ToolResults[0].ContentRaw))
	})

	t.Run("tool call without result", func(t *testing.T) {
		content := testjsonl.GeminiSessionJSON("sess-tc-no-result", "hash", tsEarly, tsEarlyS5, []map[string]any{
			testjsonl.GeminiUserMsg("u1", tsEarly, "read it"),
			testjsonl.GeminiAssistantMsg("a1", tsEarlyS5, "Reading.", &testjsonl.GeminiMsgOpts{
				ToolCalls: []testjsonl.GeminiToolCall{
					{
						ID:          "read_1",
						Name:        "read_file",
						DisplayName: "ReadFile",
						Args:        map[string]string{"file_path": "main.go"},
					},
				},
			}),
		})
		_, msgs := runGeminiParserTest(t, content)
		require.Equal(t, 2, len(msgs))
		require.Equal(t, 1, len(msgs[1].ToolCalls))
		assert.Equal(t, "read_1", msgs[1].ToolCalls[0].ToolUseID)
		assert.Equal(t, 0, len(msgs[1].ToolResults))
	})

	t.Run("empty tool name skipped", func(t *testing.T) {
		content := testjsonl.GeminiSessionJSON("sess-uuid-empty-tc", "hash", tsEarly, tsEarlyS5, []map[string]any{
			testjsonl.GeminiUserMsg("u1", tsEarly, "do it"),
			testjsonl.GeminiAssistantMsg("a1", tsEarlyS5, "Using tool.", &testjsonl.GeminiMsgOpts{
				ToolCalls: []testjsonl.GeminiToolCall{{Name: "", DisplayName: "", Args: nil}},
			}),
		})
		_, msgs := runGeminiParserTest(t, content)
		assert.Equal(t, 2, len(msgs))
		assert.True(t, msgs[1].HasToolUse)
		assertToolCalls(t, msgs[1].ToolCalls, nil)
	})
}

func TestParseGeminiSession_ThinkingWithText(t *testing.T) {
	content := loadFixture(t, "gemini/thinking_only.json")
	_, msgs := runGeminiParserTest(t, content)

	require.Equal(t, 2, len(msgs))

	msg := msgs[1]
	assert.True(t, msg.HasThinking)
	assert.False(t, msg.HasToolUse)

	// Thinking and content should be separated by blank lines
	assert.Contains(t, msg.Content, "[Thinking]")
	assert.Contains(t, msg.Content, "Here is how it works")

	// Verify blank-line separation between thinking blocks
	// and between thinking and content
	thinkIdx := strings.LastIndex(
		msg.Content, "[Thinking]",
	)
	contentIdx := strings.Index(
		msg.Content,
		"Here is how it works",
	)
	assert.Less(t, thinkIdx, contentIdx)

	// The text between last thinking block and response
	// should contain a blank line
	between := msg.Content[thinkIdx:contentIdx]
	assert.Contains(t, between, "\n\n")
}

func TestParseGeminiSession_TokenUsage(t *testing.T) {
	t.Run("per-message tokens from fixture", func(t *testing.T) {
		content := loadFixture(t, "gemini/standard_session.json")
		sess, msgs := runGeminiParserTest(t, content)

		require.Equal(t, 4, len(msgs))

		// User messages have no tokens
		assert.Equal(t, 0, msgs[0].ContextTokens)
		assert.Equal(t, 0, msgs[0].OutputTokens)
		assert.False(t, msgs[0].HasContextTokens)
		assert.False(t, msgs[0].HasOutputTokens)
		assert.Empty(t, msgs[0].TokenUsage)

		// First assistant message (a1): input=1500, cached=100, output=200
		assert.Equal(t, 1600, msgs[1].ContextTokens)
		assert.Equal(t, 200, msgs[1].OutputTokens)
		assert.True(t, msgs[1].HasContextTokens)
		assert.True(t, msgs[1].HasOutputTokens)
		assert.NotEmpty(t, msgs[1].TokenUsage)

		// Second user message has no tokens
		assert.Equal(t, 0, msgs[2].ContextTokens)
		assert.Equal(t, 0, msgs[2].OutputTokens)
		assert.False(t, msgs[2].HasContextTokens)
		assert.False(t, msgs[2].HasOutputTokens)

		// Second assistant message (a2): input=2000, cached=50, output=300
		assert.Equal(t, 2050, msgs[3].ContextTokens)
		assert.Equal(t, 300, msgs[3].OutputTokens)
		assert.True(t, msgs[3].HasContextTokens)
		assert.True(t, msgs[3].HasOutputTokens)
		assert.NotEmpty(t, msgs[3].TokenUsage)

		// Session totals
		assert.Equal(t, 500, sess.TotalOutputTokens)
		assert.Equal(t, 2050, sess.PeakContextTokens)
		assert.True(t, sess.HasTotalOutputTokens)
		assert.True(t, sess.HasPeakContextTokens)
	})

	t.Run("messages without tokens get zero values", func(t *testing.T) {
		content := testjsonl.GeminiSessionJSON(
			"sess-no-tokens", "hash", tsEarly, tsEarlyS5,
			[]map[string]any{
				testjsonl.GeminiUserMsg("u1", tsEarly, "hello"),
				testjsonl.GeminiAssistantMsg("a1", tsEarlyS5, "hi there", nil),
			},
		)
		sess, msgs := runGeminiParserTest(t, content)

		require.Equal(t, 2, len(msgs))
		assert.Equal(t, 0, msgs[0].ContextTokens)
		assert.Equal(t, 0, msgs[1].ContextTokens)
		assert.Equal(t, 0, msgs[1].OutputTokens)
		assert.False(t, msgs[0].HasContextTokens)
		assert.False(t, msgs[0].HasOutputTokens)
		assert.False(t, msgs[1].HasContextTokens)
		assert.False(t, msgs[1].HasOutputTokens)
		assert.Equal(t, 0, sess.TotalOutputTokens)
		assert.Equal(t, 0, sess.PeakContextTokens)
		assert.False(t, sess.HasTotalOutputTokens)
		assert.False(t, sess.HasPeakContextTokens)
	})

	t.Run("tokens with programmatic fixture", func(t *testing.T) {
		content := testjsonl.GeminiSessionJSON(
			"sess-tokens-prog", "hash", tsEarly, tsEarlyS5,
			[]map[string]any{
				testjsonl.GeminiUserMsg("u1", tsEarly, "explain"),
				{
					"id":        "a1",
					"timestamp": tsEarlyS5,
					"type":      "gemini",
					"content":   "Here is the explanation.",
					"tokens": map[string]int{
						"input":    5000,
						"output":   800,
						"cached":   200,
						"thoughts": 100,
						"tool":     0,
						"total":    6100,
					},
				},
			},
		)
		sess, msgs := runGeminiParserTest(t, content)

		require.Equal(t, 2, len(msgs))
		assert.Equal(t, 5200, msgs[1].ContextTokens)
		assert.Equal(t, 800, msgs[1].OutputTokens)
		assert.True(t, msgs[1].HasContextTokens)
		assert.True(t, msgs[1].HasOutputTokens)
		assert.NotEmpty(t, msgs[1].TokenUsage)
		assert.Equal(t, 800, sess.TotalOutputTokens)
		assert.Equal(t, 5200, sess.PeakContextTokens)
		assert.True(t, sess.HasTotalOutputTokens)
		assert.True(t, sess.HasPeakContextTokens)
	})

	t.Run("zero-valued token keys preserve coverage", func(t *testing.T) {
		content := testjsonl.GeminiSessionJSON(
			"sess-zero-explicit", "hash", tsEarly, tsEarlyS5,
			[]map[string]any{
				testjsonl.GeminiUserMsg("u1", tsEarly, "hello"),
				{
					"id":        "a1",
					"timestamp": tsEarlyS5,
					"type":      "gemini",
					"content":   "still counted",
					"tokens": map[string]int{
						"input":  0,
						"output": 0,
						"cached": 0,
					},
				},
			},
		)
		sess, msgs := runGeminiParserTest(t, content)

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

func TestParseGeminiSession_EdgeCases(t *testing.T) {
	t.Run("only system messages", func(t *testing.T) {
		content := loadFixture(t, "gemini/system_messages.json")
		sess, msgs := runGeminiParserTest(t, content)
		require.NotNil(t, sess)
		assert.Equal(t, 0, len(msgs))
	})

	t.Run("first message truncation", func(t *testing.T) {
		content := testjsonl.GeminiSessionJSON(
			"sess-uuid-6", "hash", tsEarly, tsEarlyS5,
			[]map[string]any{
				testjsonl.GeminiUserMsg("u1", tsEarly, generateLargeString(400)),
			},
		)
		sess, _ := runGeminiParserTest(t, content)
		require.NotNil(t, sess)
		assert.Equal(t, 303, len(sess.FirstMessage))
	})

	t.Run("malformed JSON", func(t *testing.T) {
		path := createTestFile(t, "session.json", "not valid json {{{")
		_, _, err := ParseGeminiSession(path, "my_project", "local")
		assert.Error(t, err)
	})

	t.Run("missing file", func(t *testing.T) {
		_, _, err := ParseGeminiSession("/nonexistent.json", "my_project", "local")
		assert.Error(t, err)
	})

	t.Run("empty messages array", func(t *testing.T) {
		content := testjsonl.GeminiSessionJSON("sess-uuid-4", "hash", tsEarly, tsEarlyS5, []map[string]any{})
		sess, msgs := runGeminiParserTest(t, content)
		assert.Equal(t, 0, sess.MessageCount)
		assert.Equal(t, 0, len(msgs))
	})

	t.Run("content as Part array", func(t *testing.T) {
		content := testjsonl.GeminiSessionJSON("sess-uuid-5", "hash", tsEarly, tsEarlyS5, []map[string]any{
			{
				"id":        "u1",
				"timestamp": tsEarly,
				"type":      "user",
				"content": []map[string]string{
					{"text": "part one"},
					{"text": "part two"},
				},
			},
		})
		_, msgs := runGeminiParserTest(t, content)
		assert.Equal(t, 1, len(msgs))
		assert.True(t, strings.Contains(msgs[0].Content, "part one"))
		assert.True(t, strings.Contains(msgs[0].Content, "part two"))
	})

	t.Run("timestamps from startTime and lastUpdated", func(t *testing.T) {
		content := testjsonl.GeminiSessionJSON("sess-uuid-7", "hash", "2024-06-15T10:00:00Z", "2024-06-15T11:30:00Z", []map[string]any{
			testjsonl.GeminiUserMsg("u1", "2024-06-15T10:00:00Z", "hello"),
		})
		sess, _ := runGeminiParserTest(t, content)
		wantStart := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
		wantEnd := time.Date(2024, 6, 15, 11, 30, 0, 0, time.UTC)
		assertTimestamp(t, sess.StartedAt, wantStart)
		assertTimestamp(t, sess.EndedAt, wantEnd)
	})

	t.Run("missing sessionId", func(t *testing.T) {
		content := `{"projectHash":"abc","startTime":"2024-01-01T00:00:00Z","lastUpdated":"2024-01-01T00:00:00Z","messages":[]}`
		path := createTestFile(t, "session.json", content)
		_, _, err := ParseGeminiSession(path, "my_project", "local")
		assert.Error(t, err)
	})
}
