package parser

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// codexReasoningLine builds a native reasoning response item. Codex writes the
// readable summary as summary_text parts, optional raw reasoning as content
// parts, and opaque reasoning as encrypted_content.
func codexReasoningLine(t *testing.T, ts string, summary, content []string, encrypted bool) string {
	t.Helper()
	parts := func(kind string, texts []string) []map[string]string {
		out := []map[string]string{}
		for _, text := range texts {
			out = append(out, map[string]string{"type": kind, "text": text})
		}
		return out
	}
	payload := map[string]any{"type": "reasoning", "summary": parts("summary_text", summary)}
	if content != nil {
		payload["content"] = parts("reasoning_text", content)
	}
	if encrypted {
		payload["encrypted_content"] = "gAAAAopaque"
	} else {
		payload["encrypted_content"] = nil
	}
	return mustCodexLine(t, map[string]any{"type": "response_item", "timestamp": ts, "payload": payload})
}

func codexCompactedLine(t *testing.T, ts, message string) string {
	t.Helper()
	return mustCodexLine(t, map[string]any{"type": "compacted", "timestamp": ts, "payload": map[string]any{
		"message":             message,
		"replacement_history": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "earlier prompt"}}}},
	}})
}

// codexTokenCountTotalsLine builds a token_count whose info carries both the
// per-call last_token_usage and the cumulative total_token_usage.
func codexTokenCountTotalsLine(t *testing.T, ts string, input, output, cached, totalInput, totalOutput, totalCached int) string {
	t.Helper()
	usage := func(in, out, cache int) map[string]int {
		return map[string]int{
			"input_tokens": in, "cached_input_tokens": cache, "output_tokens": out,
			"reasoning_output_tokens": 0, "total_tokens": in + out,
		}
	}
	return mustCodexLine(t, map[string]any{"type": "event_msg", "timestamp": ts, "payload": map[string]any{
		"type": "token_count",
		"info": map[string]any{
			"last_token_usage":     usage(input, output, cached),
			"total_token_usage":    usage(totalInput, totalOutput, totalCached),
			"model_context_window": 272000,
		},
	}})
}

func mustCodexLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestCodexReasoningKeptAsThinking(t *testing.T) {
	for _, tc := range []struct {
		name      string
		summary   []string
		content   []string
		encrypted bool
		want      string
	}{
		{name: "summary", summary: []string{"**Planning**\n\nRead the parser first.", "Then the tests."}, want: "**Planning**\n\nRead the parser first.\n\nThen the tests."},
		{name: "summary with encrypted reasoning", summary: []string{"Checking the schema."}, encrypted: true, want: "Checking the schema."},
		{name: "summary wins over raw content", summary: []string{"Short summary."}, content: []string{"Full chain."}, want: "Short summary."},
		{name: "raw content without summary", content: []string{"Raw reasoning text."}, want: "Raw reasoning text."},
		{name: "encrypted only", encrypted: true},
		{name: "blank summary", summary: []string{"  "}, encrypted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON("reasoning-1", "/tmp", "user", tsEarly),
				testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
				testjsonl.CodexMsgJSON("user", "fix it", tsEarlyS1),
				codexReasoningLine(t, tsEarlyS5, tc.summary, tc.content, tc.encrypted),
				testjsonl.CodexMsgJSON("assistant", "done", tsLate),
			)
			sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
			require.NotNil(t, sess)
			if tc.want == "" {
				require.Len(t, msgs, 2, "unreadable reasoning adds no row")
				assert.False(t, msgs[1].HasThinking)
				return
			}
			require.Len(t, msgs, 3)
			thinking := msgs[1]
			assert.Equal(t, RoleAssistant, thinking.Role)
			assert.True(t, thinking.HasThinking)
			assert.False(t, thinking.HasToolUse)
			assert.Equal(t, tc.want, thinking.ThinkingText)
			assert.Equal(t, "[Thinking]\n"+tc.want+"\n[/Thinking]", thinking.Content)
			require.NotNil(t, thinking.DialogueText)
			assert.Empty(t, *thinking.DialogueText, "reasoning is not dialogue")
			assert.Equal(t, "gpt-5.4", thinking.Model)
			assert.Equal(t, "done", msgs[2].Content)
			assert.Equal(t, 2, sess.MessageCount-sess.UserMessageCount)
		})
	}
}

func TestCodexReasoningUsageAttachment(t *testing.T) {
	head := func(id string) []string {
		return []string{
			testjsonl.CodexSessionMetaJSON(id, "/tmp", "user", tsEarly),
			testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
			testjsonl.CodexMsgJSON("user", "fix it", tsEarlyS1),
		}
	}

	t.Run("message after reasoning takes the response usage", func(t *testing.T) {
		content := testjsonl.JoinJSONL(append(head("reasoning-usage-1"),
			codexReasoningLine(t, tsEarlyS5, []string{"Looking."}, nil, true),
			testjsonl.CodexMsgJSON("assistant", "done", tsEarlyS5),
			codexTokenCountTotalsLine(t, tsEarlyS5, 1000, 50, 400, 1000, 50, 400),
		)...)
		_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		require.Len(t, msgs, 3)
		assert.Empty(t, msgs[1].TokenUsage)
		assert.Equal(t, 50, msgs[2].OutputTokens)
	})

	t.Run("reasoning-only response keeps its usage", func(t *testing.T) {
		content := testjsonl.JoinJSONL(append(head("reasoning-usage-2"),
			testjsonl.CodexMsgJSON("assistant", "first answer", tsEarlyS5),
			codexTokenCountTotalsLine(t, tsEarlyS5, 1000, 50, 400, 1000, 50, 400),
			codexReasoningLine(t, tsLate, []string{"Nothing more to do."}, nil, true),
			codexTokenCountTotalsLine(t, tsLate, 1200, 30, 1000, 2200, 80, 1400),
		)...)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		require.Len(t, msgs, 3)
		assert.Equal(t, 50, msgs[1].OutputTokens)
		assert.True(t, msgs[2].HasThinking)
		assert.Equal(t, `{"cache_read_input_tokens":1000,"input_tokens":200,"output_tokens":30}`, string(msgs[2].TokenUsage))
		assert.Equal(t, 80, sess.TotalOutputTokens)
	})

	t.Run("older reasoning row does not absorb a later call", func(t *testing.T) {
		content := testjsonl.JoinJSONL(append(head("reasoning-usage-3"),
			codexReasoningLine(t, tsEarlyS5, []string{"Looking."}, nil, true),
			testjsonl.CodexMsgJSON("assistant", "done", tsEarlyS5),
			codexTokenCountTotalsLine(t, tsEarlyS5, 1000, 50, 400, 1000, 50, 400),
			// A later call with no row of its own becomes a hidden
			// usage-only row instead of landing on the older reasoning row.
			codexTokenCountTotalsLine(t, tsLate, 1100, 20, 900, 2100, 70, 1300),
		)...)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		require.Len(t, msgs, 4)
		assert.Empty(t, msgs[1].TokenUsage)
		assert.Equal(t, 50, msgs[2].OutputTokens)
		assert.True(t, msgs[3].IsSystem)
		assert.Empty(t, msgs[3].Content)
		assert.Equal(t, 20, msgs[3].OutputTokens)
		assert.Equal(t, 70, sess.TotalOutputTokens)
	})
}

func TestCodexCompactedRecordBecomesBoundary(t *testing.T) {
	const summary = "Another language model started to solve this problem and produced a summary of its thinking process.\n## Progress\nParser read."
	for _, tc := range []struct {
		name, message, want string
	}{
		{name: "local compaction summary", message: summary, want: summary},
		{name: "remote compaction without summary", message: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON("compacted-1", "/tmp", "user", tsEarly),
				testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
				testjsonl.CodexMsgJSON("user", "keep going", tsEarlyS1),
				testjsonl.CodexMsgJSON("assistant", "summary reply", tsEarlyS5),
				codexTokenCountTotalsLine(t, tsEarlyS5, 1000, 50, 400, 1000, 50, 400),
				codexCompactedLine(t, tsLate, tc.message),
				// The compaction call has no row of its own. Its usage must
				// not land on the boundary; it becomes a hidden usage-only row.
				codexTokenCountTotalsLine(t, tsLate, 1100, 20, 900, 2100, 70, 1300),
				testjsonl.CodexMsgJSON("user", "next", tsLateS5),
			)
			sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
			require.Len(t, msgs, 5, "replacement_history is not replayed")
			boundary := msgs[2]
			assert.True(t, boundary.IsCompactBoundary)
			assert.True(t, boundary.IsSystem)
			assert.Equal(t, RoleAssistant, boundary.Role)
			assert.Equal(t, "system", boundary.SourceType)
			assert.Equal(t, "compact_boundary", boundary.SourceSubtype)
			assert.Equal(t, tc.want, boundary.Content)
			assert.Empty(t, boundary.TokenUsage)
			assert.Equal(t, 50, msgs[1].OutputTokens)
			usageOnly := msgs[3]
			assert.True(t, usageOnly.IsSystem)
			assert.False(t, usageOnly.IsCompactBoundary)
			assert.Empty(t, usageOnly.Content)
			assert.Equal(t, 20, usageOnly.OutputTokens)
			assert.Equal(t, 70, sess.TotalOutputTokens)
			assert.Equal(t, "next", msgs[4].Content)
		})
	}
}

func TestCodexTokenCountDedupUsesCumulativeTotal(t *testing.T) {
	head := []string{
		testjsonl.CodexSessionMetaJSON("dedup-total", "/tmp", "user", tsEarly),
		testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "run both", tsEarlyS1),
	}
	t.Run("equal per-call counts with a rising total are separate calls", func(t *testing.T) {
		content := testjsonl.JoinJSONL(append(head,
			testjsonl.CodexFunctionCallJSON("exec_command", "ls", tsEarlyS5),
			codexTokenCountTotalsLine(t, tsEarlyS5, 1000, 50, 400, 1000, 50, 400),
			testjsonl.CodexMsgJSON("assistant", "listed", tsLate),
			codexTokenCountTotalsLine(t, tsLate, 1000, 50, 400, 2000, 100, 800),
		)...)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		require.Len(t, msgs, 3)
		assert.Equal(t, 50, msgs[1].OutputTokens)
		assert.Equal(t, 50, msgs[2].OutputTokens)
		assert.Equal(t, 100, sess.TotalOutputTokens)
	})
	t.Run("a repeated token_count counts once", func(t *testing.T) {
		repeat := codexTokenCountTotalsLine(t, tsEarlyS5, 1000, 50, 400, 1000, 50, 400)
		content := testjsonl.JoinJSONL(append(head,
			testjsonl.CodexFunctionCallJSON("exec_command", "ls", tsEarlyS5),
			repeat,
			testjsonl.CodexMsgJSON("assistant", "listed", tsLate),
			repeat,
		)...)
		sess, msgs := runCodexParserTest(t, "test.jsonl", content, false)
		require.Len(t, msgs, 3)
		assert.Equal(t, 50, msgs[1].OutputTokens)
		assert.Empty(t, msgs[2].TokenUsage)
		assert.Equal(t, 50, sess.TotalOutputTokens)
	})
}
