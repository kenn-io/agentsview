package parser

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// A native thinking or tool block must not make its payload searchable as
// dialogue. Literal marker-looking text must remain dialogue.
func TestPiMessageContentSeparation(t *testing.T) {
	fixture := `{"type":"session","id":"content-session","version":3,"cwd":"/workspace/project"}
{"type":"message","id":"user-one","parentId":null,"message":{"role":"user","content":"literal [Thinking] and [Bash]"}}
{"type":"message","id":"answer-one","parentId":"user-one","message":{"role":"assistant","model":"demo-model","usage":{"input":2,"output":3},"content":[{"type":"thinking","thinking":"thinking-needle"},{"type":"text","text":"dialogue-needle"},{"type":"toolCall","id":"read-one","name":"read","arguments":{"path":"command-needle.go"}},{"type":"text","text":"after tool"},{"type":"toolCall","id":"bash-one","name":"bash","arguments":{"command":"echo command-needle"}},{"type":"thinking","thinking":"second thought"}]}}
`
	_, messages := runPiParserTest(t, fixture)
	require.Len(t, messages, 2)
	assert.Equal(t, "literal [Thinking] and [Bash]", messages[0].Content)
	m := messages[1]
	assert.Equal(t, "dialogue-needle\nafter tool", m.Content)
	assert.Equal(t, "thinking-needle\n\nsecond thought", m.ThinkingText)
	assert.True(t, m.HasThinking)
	assert.True(t, m.HasToolUse)
	assert.Equal(t, "answer-one", m.SourceUUID)
	assert.Equal(t, "user-one", m.SourceParentUUID)
	assert.Equal(t, 1, m.Ordinal)
	assert.Equal(t, "demo-model", m.Model)
	assert.Equal(t, 3, m.OutputTokens)
	require.Len(t, m.ToolCalls, 2)
	assert.Equal(t, "read", m.ToolCalls[0].ToolName)
	assert.Equal(t, `{"path":"command-needle.go"}`, m.ToolCalls[0].InputJSON)
	assert.Equal(t, "bash", m.ToolCalls[1].ToolName)
	assert.Equal(t, `{"command":"echo command-needle"}`, m.ToolCalls[1].InputJSON)
	assert.Equal(t, len("[Thinking]\nthinking-needle\n[/Thinking]\ndialogue-needle\n[Read: command-needle.go]\nafter tool\n[Bash]\n$ echo command-needle\n[Thinking]\nsecond thought\n[/Thinking]"), m.ContentLength)

	encoded, err := json.Marshal(m)
	require.NoError(t, err)
	layout := gjson.GetBytes(encoded, "content_layout")
	assert.Equal(t, int64(1), layout.Get("version").Int())
	blocks := layout.Get("blocks").Array()
	require.Len(t, blocks, 6)
	assert.Equal(t, "thinking", blocks[0].Get("kind").Str)
	assert.Equal(t, int64(0), blocks[0].Get("start").Int())
	assert.Equal(t, int64(15), blocks[0].Get("end").Int())
	assert.Equal(t, "text", blocks[1].Get("kind").Str)
	assert.Equal(t, int64(15), blocks[1].Get("end").Int())
	assert.Equal(t, "tool_call", blocks[2].Get("kind").Str)
	assert.Equal(t, int64(0), blocks[2].Get("call_index").Int())
	assert.Equal(t, "text", blocks[3].Get("kind").Str)
	assert.Equal(t, int64(16), blocks[3].Get("start").Int())
	assert.Equal(t, int64(26), blocks[3].Get("end").Int())
	assert.Equal(t, "tool_call", blocks[4].Get("kind").Str)
	assert.Equal(t, int64(1), blocks[4].Get("call_index").Int())
	assert.Equal(t, "thinking", blocks[5].Get("kind").Str)
	assert.Equal(t, int64(17), blocks[5].Get("start").Int())
	assert.Equal(t, int64(31), blocks[5].Get("end").Int())
}

func TestPiMessageContentBodyKinds(t *testing.T) {
	tests := []struct {
		name, blocks, dialogue, thinking, kind string
		hasThinking, hasTools                  bool
	}{
		{"thinking only", `[{"type":"thinking","thinking":"thinking-needle"}]`, "", "thinking-needle", "thinking", true, false},
		{"redacted thinking", `[{"type":"thinking","thinking":"","thinkingSignature":"redacted"}]`, "", "", "thinking", true, false},
		{"tool only", `[{"type":"toolCall","id":"call-one","name":"bash","arguments":{"command":"command-needle"}}]`, "", "", "tool_call", false, true},
		{"literal markers", `[{"type":"text","text":"[Thinking]\nliteral\n[/Thinking]\n[Read: literal.go]"}]`, "[Thinking]\nliteral\n[/Thinking]\n[Read: literal.go]", "", "text", false, false},
		{"Unicode and controls", `[{"type":"text","text":"A\u0000\u0085\uD801\uDC00B"}]`, "A\U00010400B", "", "text", false, false},
		{"plain string", `"plain dialogue"`, "plain dialogue", "", "text", false, false},
		{"empty", `[]`, "", "", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := `{"type":"session","id":"body-session","version":3}` + "\n" +
				`{"type":"message","id":"answer-one","parentId":null,"message":{"role":"assistant","content":` + tt.blocks + `}}` + "\n"
			_, messages := runPiParserTest(t, fixture)
			require.Len(t, messages, 1)
			m := messages[0]
			assert.Equal(t, tt.dialogue, m.Content)
			assert.Equal(t, tt.thinking, m.ThinkingText)
			assert.Equal(t, tt.hasThinking, m.HasThinking)
			assert.Equal(t, tt.hasTools, m.HasToolUse)
			encoded, err := json.Marshal(m)
			require.NoError(t, err)
			layout := gjson.GetBytes(encoded, "content_layout")
			assert.Equal(t, int64(1), layout.Get("version").Int())
			blocks := layout.Get("blocks").Array()
			if tt.kind == "" {
				assert.Empty(t, blocks)
			} else {
				require.Len(t, blocks, 1)
				assert.Equal(t, tt.kind, blocks[0].Get("kind").Str)
				if tt.kind == "text" {
					assert.Equal(t, int64(len(tt.dialogue)), blocks[0].Get("end").Int())
				}
			}
		})
	}
}

func TestMessageContentSanitizedWorkLengths(t *testing.T) {
	tests := []struct {
		name                                     string
		build                                    func(*MessageContentBuilder)
		wantContent, wantThinking, wantRendering string
		wantWork                                 int
	}{
		{"text", func(b *MessageContentBuilder) { b.AddText("A\x00\u0085\U00010400B") }, "A\U00010400B", "", "", 6},
		{"thinking", func(b *MessageContentBuilder) { b.AddThinking("a\x00b") }, "", "ab", "", 25},
		{"tool", func(b *MessageContentBuilder) {
			b.AddToolCall(ParsedToolCall{ToolName: "Bash", Rendering: "[Bash]\n$ a\x00b"})
		}, "", "", "[Bash]\n$ ab", 11},
		{"mixed", func(b *MessageContentBuilder) {
			b.AddText("a\x00")
			b.AddThinking("b\x00")
			b.AddToolCall(ParsedToolCall{ToolName: "Read", Rendering: "[Read: x\x00]"})
		}, "a", "b", "[Read: x]", 36},
		{"invalid UTF-8", func(b *MessageContentBuilder) { b.AddText("é\xffx") }, "éx", "", "", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var builder MessageContentBuilder
			tt.build(&builder)
			message := builder.Message()
			assert.Equal(t, tt.wantContent, message.Content)
			assert.Equal(t, tt.wantThinking, message.ThinkingText)
			assert.Equal(t, tt.wantWork, message.ContentLength)
			if tt.wantRendering != "" {
				require.Len(t, message.ToolCalls, 1)
				assert.Equal(t, tt.wantRendering, message.ToolCalls[0].Rendering)
			}
		})
	}
}

func TestPlainBodySanitizationPreservesSemanticWorkLength(t *testing.T) {
	message := (ParsedMessage{Content: "abc\x00", ContentLength: 50}).withPlainBody()
	assert.Equal(t, "abc", message.Content)
	assert.Equal(t, 49, message.ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 3}}}, message.ContentLayout)
}

func TestNativeHistoricalWorkOverridesExcludeSanitizedBytes(t *testing.T) {
	t.Run("UI thinking-only work", func(t *testing.T) {
		message := composeNativeUIMessage(ParsedMessage{
			HasThinking: true, ThinkingText: "think\x00", ContentLength: 6,
		})
		assert.Equal(t, "think", message.ThinkingText)
		assert.Equal(t, 5, message.ContentLength)
	})
	t.Run("Grok dialogue and reasoning without markers", func(t *testing.T) {
		message := ParsedMessage{Content: "say\x00", HasThinking: true, ThinkingText: "plan\x00", ContentLength: 9}
		composeGrokMessageBody(&message)
		assert.Equal(t, "say", message.Content)
		assert.Equal(t, "plan", message.ThinkingText)
		assert.Equal(t, 7, message.ContentLength)
	})
	t.Run("Claude export double-newline convention", func(t *testing.T) {
		var input claudeAIMessage
		require.NoError(t, json.Unmarshal([]byte(`{"content":[{"type":"thinking","thinking":"a\u0000b"},{"type":"text","text":"c\u0000d"}]}`), &input))
		message := assembleClaudeAIBody(input)
		assert.Equal(t, "ab", message.ThinkingText)
		assert.Equal(t, "cd", message.Content)
		assert.Equal(t, 29, message.ContentLength)
	})
}

func TestPiStandaloneToolResultContent(t *testing.T) {
	fixture := `{"type":"session","id":"output-session","version":3}
{"type":"message","id":"result-one","parentId":null,"message":{"role":"toolResult","toolCallId":"missing-call","toolName":"bash","content":[{"type":"text","text":"result-needle"}]}}
`
	_, messages := runPiParserTest(t, fixture)
	require.Len(t, messages, 1)
	assert.Empty(t, messages[0].Content)
	assert.Equal(t, "result-one", messages[0].SourceUUID)
	require.Len(t, messages[0].ToolResults, 1)
	assert.Equal(t, "missing-call", messages[0].ToolResults[0].ToolUseID)
	encoded, err := json.Marshal(messages[0])
	require.NoError(t, err)
	assert.Equal(t, "result-needle", gjson.GetBytes(encoded, "tool_result_text").Str)
	assert.Equal(t, "Bash", gjson.GetBytes(encoded, "content_layout.blocks.0.category").Str)
	assert.Equal(t, "tool_result", gjson.GetBytes(encoded, "content_layout.blocks.0.kind").Str)
}

func TestPiFamilyNativeRedactedThinking(t *testing.T) {
	fixture := `{"type":"session","id":"redacted-session","version":3}
{"type":"message","id":"answer-one","parentId":null,"message":{"role":"assistant","content":[{"type":"redactedThinking","data":"opaque-signature"},{"type":"text","text":"Visible answer"}]}}
`
	_, messages := parsePiLikeTestSession(t, AgentOMP, fixture)
	require.Len(t, messages, 1)
	assert.Equal(t, "Visible answer", messages[0].Content)
	assert.Empty(t, messages[0].ThinkingText)
	assert.True(t, messages[0].HasThinking)
	require.NotNil(t, messages[0].ContentLayout)
	assert.Equal(t, []ContentBlock{{Kind: "thinking"}, {Kind: "text", End: 14}}, messages[0].ContentLayout.Blocks)
}

func TestPiFamilyMessageContent(t *testing.T) {
	fixture := `{"type":"session","id":"family-session","version":3}
{"type":"message","id":"user-one","parentId":null,"message":{"role":"user","content":"literal [Bash]"}}
{"type":"message","id":"answer-one","parentId":"user-one","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Private reasoning"},{"type":"text","text":"Visible answer"},{"type":"toolCall","id":"call-one","name":"bash","arguments":{"command":"command-needle"}}]}}
{"type":"message","id":"result-one","parentId":"answer-one","message":{"role":"toolResult","toolCallId":"call-one","toolName":"bash","content":[{"type":"text","text":"result-needle"}]}}
`
	for _, agent := range []AgentType{AgentPi, AgentOMP, AgentPrimeAgent} {
		t.Run(string(agent), func(t *testing.T) {
			_, messages := parsePiLikeTestSession(t, agent, fixture)
			require.Len(t, messages, 3)
			assert.Equal(t, "literal [Bash]", messages[0].Content)
			assert.Equal(t, "Visible answer", messages[1].Content)
			assert.Equal(t, "Private reasoning", messages[1].ThinkingText)
			require.Len(t, messages[1].ToolCalls, 1)
			assert.JSONEq(t, `{"command":"command-needle"}`, messages[1].ToolCalls[0].InputJSON)
			assert.Equal(t, "result-needle", messages[2].ToolResultText)
			for _, message := range messages {
				require.NotNil(t, message.ContentLayout)
				assert.Equal(t, 1, message.ContentLayout.Version)
			}
		})
	}
}

func FuzzPiMessageDialogue(f *testing.F) {
	for _, seed := range []string{"", "ordinary dialogue", "[Thinking]\nliteral\n[/Thinking]", "\U00010400 text [Bash]\n$ literal", "[Read: literal.go]"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, dialogue string) {
		if !utf8.ValidString(dialogue) || strings.IndexFunc(dialogue, func(r rune) bool {
			return unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r'
		}) >= 0 {
			t.Skip("this property covers valid display text; sanitizer cases are separate")
		}
		input := map[string]any{"message": map[string]any{"content": []map[string]any{
			{"type": "thinking", "thinking": "thinking-needle"},
			{"type": "text", "text": dialogue},
			{"type": "toolCall", "name": "bash", "id": "call-one", "arguments": map[string]string{"command": "command-needle"}},
		}}}
		encoded, err := json.Marshal(input)
		require.NoError(t, err)
		m := parsePiAssistantMessage(string(encoded), 0, "", "", "", "")
		require.NotNil(t, m)
		assert.Equal(t, dialogue, m.Content)
		assert.Equal(t, "thinking-needle", m.ThinkingText)
		require.Len(t, m.ToolCalls, 1)
		assert.Equal(t, `{"command":"command-needle"}`, m.ToolCalls[0].InputJSON)
		require.NotNil(t, m.ContentLayout)
		for _, block := range m.ContentLayout.Blocks {
			body := m.Content
			if block.Kind == "thinking" {
				body = m.ThinkingText
			}
			if block.Kind == "text" || block.Kind == "thinking" {
				require.GreaterOrEqual(t, block.Start, 0)
				require.GreaterOrEqual(t, block.End, block.Start)
				require.LessOrEqual(t, block.End, len(body))
				assert.True(t, utf8.ValidString(body[block.Start:block.End]))
			}
		}
	})
}

func TestSanitizedHistoricalWorkConventions(t *testing.T) {
	for _, tt := range []struct {
		name    string
		message ParsedMessage
		want    int
	}{
		{"semantic excess", ParsedMessage{Content: "a\x00", ThinkingText: "b\x00", ContentLength: 30}, 28},
		{"dialogue only", ParsedMessage{Content: "a\x00", ThinkingText: "b\x00", ContentLength: 2}, 1},
		{"standalone output", ParsedMessage{ToolResultText: "a\x00", ContentLength: 2}, 1},
		{"raw result work", ParsedMessage{ToolResults: []ParsedToolResult{{ContentRaw: `"a\u0000b"`, ContentLength: 3}}, ContentLength: 3}, 2},
		{"raw result zero work", ParsedMessage{ToolResults: []ParsedToolResult{{ContentRaw: `"a\u0000b"`, ContentLength: 3}}}, 0},
		{"zero work", ParsedMessage{Content: "a\x00", ThinkingText: "b\x00"}, 0},
		{"already clean", ParsedMessage{Content: "a", ThinkingText: "b", ContentLength: 28}, 28},
	} {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, tt.message.sanitizedHistoricalWorkLength()) })
	}
	result := gjson.Parse(`[{"text":"a\u0000"},{"result":"b\u0085"}]`)
	assert.Equal(t, 2, sanitizedToolResultWorkLength(result), "array separators are not work bytes")
	assert.Equal(t, 4, toolResultContentLength(gjson.Parse(`[{"text":"a\u0000"},{"result":"b\u0000"}]`)), "raw result metadata retains its own sanitation boundary")
}
