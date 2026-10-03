package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNativeArrayMessageBodies(t *testing.T) {
	tests := []struct {
		name  string
		parse func(*testing.T) ParsedMessage
		kinds []string
		work  int
	}{
		{"Gemini", func(t *testing.T) ParsedMessage {
			t.Helper()
			m, ok := parseGeminiMessage(gjson.Parse(`{"type":"gemini","thoughts":[{"description":"thinking-needle"}],"content":"dialogue-needle","toolCalls":[{"id":"call-one","name":"run_shell_command","args":{"command":"command-needle"},"result":[{"functionResponse":{"response":{"output":"result-needle"}}}]}],"model":"demo-model","tokens":{"input":2,"output":3}}`), 7)
			require.True(t, ok)
			assert.Equal(t, "demo-model", m.Model)
			assert.Equal(t, 3, m.OutputTokens)
			return m
		}, []string{"thinking", "text", "tool_call", "tool_result"}, len("[Thinking]\nthinking-needle\n[/Thinking]\n\ndialogue-needle\n\n[Bash]\n$ command-needle")},
		{"OpenCode", func(t *testing.T) ParsedMessage {
			t.Helper()
			return buildOpenCodeMessage(7, "answer-one", RoleAssistant, 1700000000000, []openCodePartRow{
				{data: `{"type":"reasoning","text":"thinking-needle"}`},
				{data: `{"type":"text","text":"dialogue-needle"}`},
				{data: `{"type":"tool","callID":"call-one","tool":"bash","state":{"status":"completed","input":{"command":"command-needle"},"output":"result-needle"}}`},
			}, "/workspace/project")
		}, []string{"thinking", "text", "tool_call"}, 54},
		{"Goose", func(t *testing.T) ParsedMessage {
			t.Helper()
			m, ok, err := buildGooseMessage(t.Context(), 7, gooseMessageRow{role: "assistant", contentJSON: `[{"type":"thinking","thinking":"thinking-needle"},{"type":"text","text":"dialogue-needle"},{"type":"toolRequest","id":"call-one","toolCall":{"status":"success","value":{"name":"bash","arguments":{"command":"command-needle"}}}},{"type":"toolResponse","id":"call-one","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"result-needle"}]}}}]`}, "demo-model")
			require.NoError(t, err)
			require.True(t, ok)
			return m
		}, []string{"thinking", "text", "tool_call", "tool_result"}, 54},
		{"Crush", func(t *testing.T) ParsedMessage {
			t.Helper()
			m, ok, err := buildCrushMessage(t.Context(), 7, "answer-one", "assistant", `[{"type":"reasoning","data":{"thinking":"thinking-needle"}},{"type":"text","data":{"text":"dialogue-needle"}},{"type":"tool_call","data":{"id":"call-one","name":"bash","input":"{\"command\":\"command-needle\"}"}},{"type":"tool_result","data":{"tool_call_id":"call-one","content":"result-needle"}},{"type":"finish","data":{"reason":"stop"}}]`, "demo-model", "demo-provider", 1700000000, false)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, "stop", m.StopReason)
			assert.Equal(t, "demo-provider", m.ProviderID)
			return m
		}, []string{"thinking", "text", "tool_call", "tool_result"}, 54},
		{"Z Code", func(t *testing.T) ParsedMessage {
			t.Helper()
			m, ok := buildZCodeMessage(t.Context(), 7, zcodeMessageRow{id: "answer-one", data: `{"role":"assistant","model":"demo-model"}`}, []zcodePartRow{
				{data: `{"type":"thinking","thinking":"thinking-needle"}`},
				{data: `{"type":"text","text":"dialogue-needle"}`},
				{data: `{"type":"tool_use","id":"call-one","name":"bash","input":{"command":"command-needle"}}`},
				{data: `{"type":"tool_result","tool_use_id":"call-one","content":"result-needle"}`},
			})
			require.True(t, ok)
			return m
		}, []string{"thinking", "text", "tool_call", "tool_result"}, 54},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.parse(t)
			assert.Equal(t, 7, m.Ordinal)
			assert.Equal(t, "dialogue-needle", m.Content)
			assert.Equal(t, "thinking-needle", m.ThinkingText)
			assert.True(t, m.HasThinking)
			assert.True(t, m.HasToolUse)
			assert.Equal(t, tt.work, m.ContentLength)
			require.Len(t, m.ToolCalls, 1)
			assert.JSONEq(t, `{"command":"command-needle"}`, m.ToolCalls[0].InputJSON)
			require.NotNil(t, m.ContentLayout)
			assert.Equal(t, 1, m.ContentLayout.Version)
			var kinds []string
			for _, block := range m.ContentLayout.Blocks {
				kinds = append(kinds, block.Kind)
			}
			assert.Equal(t, tt.kinds, kinds)
			assert.Equal(t, ContentBlock{Kind: "thinking", End: 15}, m.ContentLayout.Blocks[0])
			assert.Equal(t, ContentBlock{Kind: "text", End: 15}, m.ContentLayout.Blocks[1])
			if tt.name == "OpenCode" {
				assert.Equal(t, "answer-one", m.SourceUUID)
			} else {
				assert.Equal(t, "result-needle", m.ToolResultText)
				require.Len(t, m.ToolResults, 1)
				assert.NotEmpty(t, m.ToolResults[0].ToolUseID)
				assert.Equal(t, ContentBlock{Kind: "tool_result", End: 13}, m.ContentLayout.Blocks[3])
			}
		})
	}
}

func TestOpenCodeV2NativeBodyOrder(t *testing.T) {
	path, seed, writer := newTestDB(t)
	seed.AddProject("project-a", "/workspace/project-a")
	seed.AddSession("ses_a", "project-a", "", "", 1700000000000, 1700000001000)
	_, err := writer.ExecContext(t.Context(), openCodeV2TestSchema)
	require.NoError(t, err)
	_, err = writer.ExecContext(t.Context(), `INSERT INTO session_message VALUES ('answer-one', 'ses_a', 'assistant', 1, 1700000000000, 1700000001000, ?)`, `{"model":{"id":"demo-model"},"content":[{"type":"reasoning","text":"thinking-needle"},{"type":"text","text":"dialogue-needle"},{"type":"tool","id":"call-one","name":"bash","state":{"status":"completed","input":{"command":"command-needle"},"content":[{"type":"text","text":"result-needle"}]}},{"type":"text","text":"after tool"}]}`)
	require.NoError(t, err)
	_, msgs, err := parseOpenCodeDBSession(path, "ses_a", "host-a")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	m := msgs[0]
	assert.Equal(t, "dialogue-needle\nafter tool", m.Content)
	assert.Equal(t, "thinking-needle", m.ThinkingText)
	assert.Equal(t, "answer-one", m.SourceUUID)
	assert.Equal(t, 65, m.ContentLength)
	require.Len(t, m.ToolCalls, 1)
	assert.JSONEq(t, `{"command":"command-needle"}`, m.ToolCalls[0].InputJSON)
	require.Len(t, m.ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "result-needle", m.ToolCalls[0].ResultEvents[0].Content)
	require.NotNil(t, m.ContentLayout)
	assert.Equal(t, []ContentBlock{{Kind: "thinking", End: 15}, {Kind: "text", End: 15}, {Kind: "tool_call"}, {Kind: "text", Start: 16, End: 26}}, m.ContentLayout.Blocks)
}

func TestGeminiNativeEmptyAndReasoningBodies(t *testing.T) {
	for _, tt := range []struct {
		name, raw, thinking string
		work, blocks        int
	}{
		{"reasoning", `{"type":"gemini","thoughts":[{"subject":"Planning","description":"thinking-needle"}]}`, "Planning\nthinking-needle", 47, 1},
		{"redacted", `{"type":"gemini","thoughts":[{"description":""}]}`, "", 0, 1},
		{"usage", `{"type":"gemini","content":"","tokens":{"input":0,"output":0}}`, "", 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := parseGeminiMessage(gjson.Parse(tt.raw), 0)
			require.True(t, ok)
			assert.Empty(t, m.Content)
			assert.Equal(t, tt.thinking, m.ThinkingText)
			assert.Equal(t, tt.work, m.ContentLength)
			require.NotNil(t, m.ContentLayout)
			assert.Len(t, m.ContentLayout.Blocks, tt.blocks)
		})
	}
}

func TestGeminiNativeTextSpacing(t *testing.T) {
	m, ok := parseGeminiMessage(gjson.Parse(`{"id":"answer-one","type":"gemini","content":[{"text":"dialogue-needle"},{"text":"[Thinking]\nliteral\n[/Thinking]"}]}`), 0)
	require.True(t, ok)
	assert.Equal(t, "dialogue-needle\n\n[Thinking]\nliteral\n[/Thinking]", m.Content)
	assert.Empty(t, m.ThinkingText)
	assert.False(t, m.HasThinking)
	assert.Equal(t, "answer-one", m.SourceUUID)
	assert.Equal(t, 47, m.ContentLength)
	require.NotNil(t, m.ContentLayout)
	assert.Equal(t, []ContentBlock{{Kind: "text", End: 15}, {Kind: "text", Start: 17, End: 47}}, m.ContentLayout.Blocks)
}

func TestOpenCodeNativeReasoningAndShellCarriers(t *testing.T) {
	for _, generation := range []string{"v1", "v2"} {
		t.Run(generation, func(t *testing.T) {
			path, seed, writer := newTestDB(t)
			seed.AddProject("project-a", "/workspace/project-a")
			seed.AddSession("ses_a", "project-a", "", "", 1700000000000, 1700000001000)
			if generation == "v1" {
				seed.AddMessage("answer-one", "ses_a", 1700000000000, 1700000000000, `{"role":"assistant"}`)
				seed.AddPart("reason-one", "answer-one", "ses_a", 1700000000000, 1700000000000, `{"type":"reasoning","text":"thinking-needle"}`)
			} else {
				_, err := writer.ExecContext(t.Context(), openCodeV2TestSchema)
				require.NoError(t, err)
				_, err = writer.ExecContext(t.Context(), `INSERT INTO session_message VALUES ('answer-one', 'ses_a', 'assistant', 1, 1700000000000, 1700000000000, ?)`, `{"content":[{"type":"reasoning","text":"thinking-needle"}]}`)
				require.NoError(t, err)
				_, err = writer.ExecContext(t.Context(), `INSERT INTO session_message VALUES ('shell-one', 'ses_a', 'shell', 2, 1700000001000, 1700000001000, ?)`, `{"command":"command-needle","callID":"call-one","output":"result-needle","time":{"created":1700000001000,"completed":1700000001000}}`)
				require.NoError(t, err)
			}
			sess, msgs, err := parseOpenCodeDBSession(path, "ses_a", "host-a")
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.NotEmpty(t, msgs)
			assert.Empty(t, msgs[0].Content)
			assert.Equal(t, "thinking-needle", msgs[0].ThinkingText)
			require.NotNil(t, msgs[0].ContentLayout)
			assert.Equal(t, []ContentBlock{{Kind: "thinking", End: 15}}, msgs[0].ContentLayout.Blocks)
			if generation == "v2" {
				require.Len(t, msgs, 2)
				assert.Equal(t, 1, sess.UserMessageCount, "native user shell activity retains its existing count")
				assert.Equal(t, "command-needle", sess.FirstMessage)
				assert.Empty(t, msgs[1].Content)
				require.Len(t, msgs[1].ToolCalls, 1)
				assert.Equal(t, "command-needle", msgs[1].ToolCalls[0].Rendering)
				assert.Equal(t, 14, msgs[1].ContentLength)
			}
		})
	}
}
