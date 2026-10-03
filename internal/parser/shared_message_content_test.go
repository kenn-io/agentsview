package parser

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSharedNativeMessageBodies(t *testing.T) {
	const blocks = `[{"type":"thinking","thinking":"thinking-needle"},{"type":"text","text":"dialogue-needle"},{"type":"tool_use","id":"call-one","name":"Bash","input":{"command":"command-needle"}}]`
	const user = `{"type":"user","uuid":"user-one","parentUuid":null,"sessionId":"session-one","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"literal [Thinking]"}}`
	assistant := `{"type":"assistant","uuid":"answer-one","parentUuid":"user-one","sessionId":"session-one","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":` + blocks + `}}`
	transcript := user + "\n" + assistant + "\n"
	tests := []struct {
		name  string
		parse func(*testing.T) ParsedMessage
	}{
		{"Claude full", func(t *testing.T) ParsedMessage {
			t.Helper()
			_, msgs := runClaudeParserTest(t, "session-one.jsonl", transcript)
			require.Len(t, msgs, 2)
			return msgs[1]
		}},
		{"Claude incremental", func(t *testing.T) ParsedMessage {
			t.Helper()
			path := createTestFile(t, "session-one.jsonl", transcript)
			msgs, _, _, err := callParseClaudeSessionFrom(path, int64(len(user)+1), 1, "user-one")
			require.NoError(t, err)
			require.Len(t, msgs, 1)
			return msgs[0]
		}},
		{"Amp", func(t *testing.T) ParsedMessage {
			t.Helper()
			_, msgs, err := runAmpParserTest(t, `{"v":1,"id":"T-content-session","created":1704067200000,"messages":[{"role":"assistant","content":`+blocks+`}]}`)
			require.NoError(t, err)
			require.Len(t, msgs, 1)
			return msgs[0]
		}},
		{"OpenClaude", func(t *testing.T) ParsedMessage {
			t.Helper()
			path := createTestFile(t, "session-one.jsonl", transcript)
			_, msgs, err := parseOpenClaudeSession(path, "project", "local")
			require.NoError(t, err)
			require.Len(t, msgs, 2)
			return msgs[1]
		}},
		{"OpenClaw", func(t *testing.T) ParsedMessage {
			t.Helper()
			native := strings.ReplaceAll(strings.ReplaceAll(blocks, `"tool_use"`, `"toolCall"`), `"input":`, `"arguments":`)
			path, _ := writeOpenClawTestFile(t, "main", `{"type":"session","id":"session-one","version":3}`, `{"type":"message","message":{"role":"assistant","content":`+native+`}}`)
			_, msgs, err := parseOpenClawSessionForTest(t, path, "project", "local")
			require.NoError(t, err)
			require.Len(t, msgs, 1)
			return msgs[0]
		}},
		{"QClaw", func(t *testing.T) ParsedMessage {
			t.Helper()
			native := strings.ReplaceAll(strings.ReplaceAll(blocks, `"tool_use"`, `"toolCall"`), `"input":`, `"arguments":`)
			path, _ := writeQClawTestFile(t, "main", `{"type":"session","id":"session-one","version":3}`, `{"type":"message","message":{"role":"assistant","content":`+native+`}}`)
			_, msgs, err := parseQClawSessionForTest(t, path, "project", "local")
			require.NoError(t, err)
			require.Len(t, msgs, 1)
			return msgs[0]
		}},
		{"Cursor JSONL", func(t *testing.T) ParsedMessage {
			t.Helper()
			msgs := parseCursorJSONL(`{"role":"assistant","message":{"content":` + blocks + `}}`)
			require.Len(t, msgs, 1)
			return msgs[0]
		}},
		{"iFlow", func(t *testing.T) ParsedMessage {
			t.Helper()
			path := createTestFile(t, "session-one.jsonl", transcript)
			results, err := parseIflowSessionForTest(t, path, "project", "local")
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.Len(t, results[0].Messages, 2)
			return results[0].Messages[1]
		}},
		{"Devin transcript", func(t *testing.T) ParsedMessage {
			t.Helper()
			msg, ok := parseDevinStep("session-one", gjson.Parse(`{"source":"agent","step_id":1,"message":`+blocks+`}`), 0, "demo-model")
			require.True(t, ok)
			return msg
		}},
		{"Devin database", func(t *testing.T) ParsedMessage {
			t.Helper()
			msg, ok, err := parseDevinDBMessageNode("session-one", devinMessageNodeRow{NodeID: 1, ChatMessage: `{"role":"assistant","content":` + blocks + `}`}, 0, "demo-model")
			require.NoError(t, err)
			require.True(t, ok)
			return msg
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := tt.parse(t)
			assert.Equal(t, "dialogue-needle", msg.Content)
			assert.Equal(t, "thinking-needle", msg.ThinkingText)
			assert.True(t, msg.HasThinking)
			assert.True(t, msg.HasToolUse)
			require.Len(t, msg.ToolCalls, 1)
			assert.JSONEq(t, `{"command":"command-needle"}`, msg.ToolCalls[0].InputJSON)
			require.NotNil(t, msg.ContentLayout)
			assert.Equal(t, []ContentBlock{{Kind: "thinking", End: 15}, {Kind: "text", End: 15}, {Kind: "tool_call"}}, msg.ContentLayout.Blocks)
		})
	}
}

func TestOpenHandsNativeThinkingBody(t *testing.T) {
	ev := gjson.Parse(`{"kind":"MessageEvent","llm_message":{"role":"assistant","content":[{"type":"text","text":"dialogue-needle"}],"thinking_blocks":[{"type":"thinking","thinking":"thinking-needle","signature":"opaque-signature"}]}}`)
	msg, ok, _ := parseOpenHandsMessageEvent(ev, 0, "demo-model", time.Time{})
	require.True(t, ok)
	assert.Equal(t, "dialogue-needle", msg.Content)
	assert.Equal(t, "thinking-needle", msg.ThinkingText)
	assert.True(t, msg.HasThinking)
	require.NotNil(t, msg.ContentLayout)
	assert.Equal(t, []ContentBlock{{Kind: "text", End: 15}, {Kind: "thinking", End: 15}}, msg.ContentLayout.Blocks)
}

func TestClaudeNativeRedactedAndUsageOnly(t *testing.T) {
	const user = `{"type":"user","uuid":"user-one","parentUuid":null,"sessionId":"session-one","message":{"role":"user","content":"literal [Thinking]"}}`
	const redacted = `{"type":"assistant","uuid":"thought-one","parentUuid":"user-one","sessionId":"session-one","message":{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque-signature"}]}}`
	const usage = `{"type":"assistant","uuid":"usage-one","parentUuid":"thought-one","sessionId":"session-one","message":{"role":"assistant","model":"demo-model","content":[],"usage":{"input_tokens":2,"output_tokens":7}}}`
	transcript := user + "\n" + redacted + "\n" + usage + "\n"
	_, messages := runClaudeParserTest(t, "session-one.jsonl", transcript)
	require.Len(t, messages, 3)
	assert.Equal(t, "literal [Thinking]", messages[0].Content)
	assert.Equal(t, "thought-one", messages[1].SourceUUID)
	assert.Empty(t, messages[1].Content)
	assert.Empty(t, messages[1].ThinkingText)
	assert.True(t, messages[1].HasThinking)
	require.NotNil(t, messages[1].ContentLayout)
	assert.Equal(t, []ContentBlock{{Kind: "thinking"}}, messages[1].ContentLayout.Blocks)
	assert.Equal(t, "usage-one", messages[2].SourceUUID)
	assert.Empty(t, messages[2].Content)
	assert.Equal(t, 7, messages[2].OutputTokens)
	assert.True(t, messages[2].HasOutputTokens)
	require.NotNil(t, messages[2].ContentLayout)
	assert.Equal(t, 1, messages[2].ContentLayout.Version)
	assert.Empty(t, messages[2].ContentLayout.Blocks)
	path := createTestFile(t, "session-one.jsonl", transcript)
	incremental, _, _, err := callParseClaudeSessionFrom(path, int64(len(user)+1), 1, "user-one")
	require.NoError(t, err)
	require.Len(t, incremental, 2)
	assert.Equal(t, messages[1:], incremental)
}

func TestPiNativeWorkLengthAfterSanitization(t *testing.T) {
	fixture := `{"type":"session","id":"control-session","version":3}
{"type":"message","id":"answer-one","parentId":null,"message":{"role":"assistant","content":[{"type":"text","text":"A\u0000\u0085\uD801\uDC00B"},{"type":"thinking","thinking":"\u0000\u0085"},{"type":"toolCall","id":"call-one","name":"bash","arguments":{"command":"\u0000"}}]}}
`
	_, messages := runPiParserTest(t, fixture)
	require.Len(t, messages, 1)
	assert.Equal(t, "A\U00010400B", messages[0].Content)
	assert.Empty(t, messages[0].ThinkingText)
	// Sanitized work retains 6 text bytes, 23 thinking bytes (the marker),
	// 9 tool bytes, and two separating newlines: 40 bytes in total.
	assert.Equal(t, 40, messages[0].ContentLength)
}

func TestOpenHandsNativeActionAndObservationBodies(t *testing.T) {
	event := gjson.Parse(`{"kind":"ActionEvent","tool_name":"terminal","tool_call_id":"call-one","thought":[{"type":"text","text":"dialogue-needle"}],"thinking_blocks":[{"type":"thinking","thinking":"thinking-needle"}],"action":{"command":"command-needle"},"tool_call":{"arguments":"{\"command\":\"command-needle\"}"}}`)
	msg, ok, _ := parseOpenHandsActionEvent(event, 3, "demo-model", time.Time{})
	require.True(t, ok)
	assert.Equal(t, "dialogue-needle", msg.Content)
	assert.Equal(t, "thinking-needle", msg.ThinkingText)
	require.NotNil(t, msg.ContentLayout)
	assert.Equal(t, []ContentBlock{{Kind: "text", End: 15}, {Kind: "tool_call"}, {Kind: "thinking", End: 15}}, msg.ContentLayout.Blocks)
	for _, id := range []string{"call-one", ""} {
		t.Run("observation "+id, func(t *testing.T) {
			ev := gjson.Parse(`{"kind":"ObservationEvent","tool_call_id":"` + id + `","observation":{"content":[{"type":"text","text":"result-needle"}]}}`)
			msg, ok, _ := parseOpenHandsObservationEvent(ev, 4, time.Time{})
			require.True(t, ok)
			assert.Empty(t, msg.Content)
			assert.Equal(t, "result-needle", msg.ToolResultText)
			require.NotNil(t, msg.ContentLayout)
			assert.Equal(t, []ContentBlock{{Kind: "tool_result", End: 13}}, msg.ContentLayout.Blocks)
		})
	}
}

func TestClawNativeStandaloneOutputBodies(t *testing.T) {
	for _, qclaw := range []bool{false, true} {
		for _, id := range []string{"call-one", ""} {
			t.Run(fmt.Sprintf("qclaw=%t id=%s", qclaw, id), func(t *testing.T) {
				line := `{"type":"message","message":{"role":"toolResult","toolCallId":"` + id + `","content":[{"type":"text","text":"result-needle"}]}}`
				var messages []ParsedMessage
				if qclaw {
					path, _ := writeQClawTestFile(t, "main", `{"type":"session","id":"session-one","version":3}`, line)
					_, msgs, err := parseQClawSessionForTest(t, path, "project", "local")
					require.NoError(t, err)
					messages = msgs
				} else {
					path, _ := writeOpenClawTestFile(t, "main", `{"type":"session","id":"session-one","version":3}`, line)
					_, msgs, err := parseOpenClawSessionForTest(t, path, "project", "local")
					require.NoError(t, err)
					messages = msgs
				}
				require.Len(t, messages, 1)
				assert.Empty(t, messages[0].Content)
				assert.Equal(t, "result-needle", messages[0].ToolResultText)
				assert.Equal(t, 13, messages[0].ContentLength)
				require.NotNil(t, messages[0].ContentLayout)
				assert.Equal(t, []ContentBlock{{Kind: "tool_result", End: 13}}, messages[0].ContentLayout.Blocks)
			})
		}
	}
}

func TestSharedNativeResultWithoutCallIdentity(t *testing.T) {
	msg := ExtractMessageContent(t.Context(), gjson.Parse(`[{"type":"tool_result","content":"result-needle"}]`))
	assert.Empty(t, msg.Content)
	assert.Equal(t, "result-needle", msg.ToolResultText)
	require.NotNil(t, msg.ContentLayout)
	assert.Equal(t, []ContentBlock{{Kind: "tool_result", End: 13}}, msg.ContentLayout.Blocks)
}

func TestOpenHandsNativeResponsesReasoning(t *testing.T) {
	for _, tt := range []struct {
		name, item, thinking string
		end                  int
	}{
		{"visible", `{"summary":["thinking-needle"],"content":["detail"],"encrypted_content":"opaque-payload"}`, "thinking-needle\n\ndetail", 23},
		{"encrypted only", `{"summary":[],"encrypted_content":"opaque-payload"}`, "", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ev := gjson.Parse(`{"kind":"MessageEvent","llm_message":{"role":"assistant","content":[],"responses_reasoning_item":` + tt.item + `}}`)
			msg, ok, _ := parseOpenHandsMessageEvent(ev, 0, "demo-model", time.Time{})
			require.True(t, ok)
			assert.Empty(t, msg.Content)
			assert.Equal(t, tt.thinking, msg.ThinkingText)
			assert.True(t, msg.HasThinking)
			require.NotNil(t, msg.ContentLayout)
			assert.Equal(t, []ContentBlock{{Kind: "thinking", End: tt.end}}, msg.ContentLayout.Blocks)
		})
	}
}

func TestOpenHandsGroupedThinkingPreservesWorkLength(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested=%t", nested), func(t *testing.T) {
			const blocks = `[{"type":"thinking","thinking":"first"},{"type":"thinking","thinking":"second"}]`
			line := `{"kind":"MessageEvent","llm_message":{"role":"assistant","content":[{"type":"text","text":"dialogue-needle"}]},"thinking_blocks":` + blocks + `}`
			if nested {
				line = `{"kind":"MessageEvent","llm_message":{"role":"assistant","content":[{"type":"text","text":"dialogue-needle"}],"thinking_blocks":` + blocks + `}}`
			}
			msg, ok, _ := parseOpenHandsMessageEvent(gjson.Parse(line), 0, "demo-model", time.Time{})
			require.True(t, ok)
			assert.Equal(t, "dialogue-needle", msg.Content)
			assert.Equal(t, "first\n\nsecond", msg.ThinkingText)
			// This format historically rendered one reasoning envelope around
			// both blocks: 15 dialogue bytes + newline + 13 reasoning + 23 markers.
			assert.Equal(t, 52, msg.ContentLength)
			require.NotNil(t, msg.ContentLayout)
			assert.Equal(t, []ContentBlock{{Kind: "text", End: 15}, {Kind: "thinking", End: 5}, {Kind: "thinking", Start: 7, End: 13}}, msg.ContentLayout.Blocks)
		})
	}
}

func TestOpenHandsRedactedBlocksRetainSeparateReasoning(t *testing.T) {
	ev := gjson.Parse(`{"kind":"MessageEvent","llm_message":{"role":"assistant","content":[],"thinking_blocks":[{"type":"redacted_thinking","data":"opaque-payload"}],"reasoning_content":"thinking-needle"}}`)
	msg, ok, _ := parseOpenHandsMessageEvent(ev, 0, "demo-model", time.Time{})
	require.True(t, ok)
	assert.Empty(t, msg.Content)
	assert.Equal(t, "thinking-needle", msg.ThinkingText)
	require.NotNil(t, msg.ContentLayout)
	assert.Equal(t, []ContentBlock{{Kind: "thinking"}, {Kind: "thinking", End: 15}}, msg.ContentLayout.Blocks)
}
