package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVisualStudioCopilotNativeTraceBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl")
	const conv = "1c4ff921-fa0c-46f6-a043-c282c49761da"
	chat := vsCopilotTraceLineJSONWithSpanID(conv, "chat-native", "chat fixture-model", "1781293600000000000", "1781293610000000000", map[string]string{
		"gen_ai.operation.name":      "chat",
		"gen_ai.input.messages":      `[{"role":"user","parts":[{"type":"text","content":"[Thinking] is literal"}]}]`,
		"gen_ai.output.messages":     `[{"role":"assistant","parts":[{"type":"text","content":"first"},{"type":"tool_call","id":"call-chat","name":"get_file","arguments":{"filename":"input-needle"}},{"type":"text","content":"second"}]}]`,
		"gen_ai.usage.output_tokens": "3",
	})
	tool := vsCopilotTraceLineJSONWithSpanID(conv, "tool-native", "execute_tool get_file", "1781293620000000000", "1781293630000000000", map[string]string{
		"gen_ai.tool.name": "get_file", "gen_ai.tool.call.id": "call-exec",
		"gen_ai.tool.call.arguments": `{"filename":"input-needle"}`,
		"gen_ai.tool.call.result":    `{"content":"output-needle"}`,
	})
	usage := vsCopilotTraceLineJSONWithSpanID(conv, "usage-native", "chat fixture-model", "1781293640000000000", "1781293650000000000", map[string]string{
		"gen_ai.operation.name": "chat", "gen_ai.request.model": "fixture-model",
		"gen_ai.input.messages":     `[{"role":"user","parts":[{"type":"text","content":"next"}]}]`,
		"gen_ai.usage.input_tokens": "10", "gen_ai.usage.output_tokens": "2",
	})
	writeSourceFile(t, path, chat+"\n"+tool+"\n"+usage+"\n")
	sess, msgs, err := parseVisualStudioCopilotTestSession(t, path, "visualstudio", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 5)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "first\n\nsecond", msgs[1].Content)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "call-chat", msgs[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "[Read: get_file]\ninput-needle", msgs[1].ToolCalls[0].Rendering)
	assert.Equal(t, 44, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 5}, {Kind: "tool_call"}, {Kind: "text", Start: 7, End: 13}}}, msgs[1].ContentLayout)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, 29, msgs[2].ContentLength)
	require.Len(t, msgs[2].ToolCalls, 1)
	require.Len(t, msgs[2].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "output-needle", msgs[2].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_call"}}}, msgs[2].ContentLayout)
	assert.Empty(t, msgs[4].Content)
	assert.Equal(t, 42, msgs[4].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{}}, msgs[4].ContentLayout)
	assert.Equal(t, 10, msgs[4].ContextTokens)
	assert.Equal(t, 2, msgs[4].OutputTokens)
	assert.Equal(t, 5, sess.TotalOutputTokens)
}

func TestVisualStudioCopilotNativeDiagnosticBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl")
	writeSourceFile(t, path, vsCopilotTraceLineJSON("1c4ff921-fa0c-46f6-a043-c282c49761da", "invoke_agent GitHub Copilot", "1781293600000000000", "1781293610000000000", map[string]string{"copilot_chat.mode": "agent"})+"\n")
	_, msgs, err := parseVisualStudioCopilotTestSession(t, path, "visualstudio", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "GitHub Copilot turn | mode: agent", msgs[0].Content)
	assert.True(t, msgs[0].IsSystem)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 33}}}, msgs[0].ContentLayout)
}

func TestVisualStudioCopilotSanitizedChatKeepsRichestSpanOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl")
	const conv = "1c4ff921-fa0c-46f6-a043-c282c49761da"
	lean := vsCopilotTraceLineJSONWithSpanID(conv, "shared-chat", "chat fixture-model", "1781293600000000000", "1781293610000000000", map[string]string{
		"gen_ai.operation.name":  "chat",
		"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"a much longer lean answer"}]}]`,
	})
	middle := vsCopilotTraceLineJSONWithSpanID(conv, "middle-chat", "chat fixture-model", "1781293620000000000", "1781293630000000000", map[string]string{
		"gen_ai.operation.name":  "chat",
		"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"middle"}]}]`,
	})
	rich := vsCopilotTraceLineJSONWithSpanID(conv, "shared-chat", "chat fixture-model", "1781293640000000000", "1781293650000000000", map[string]string{
		"gen_ai.operation.name":      "chat",
		"gen_ai.output.messages":     `[{"role":"user","parts":[{"type":"text","content":"ignore this"}]},{"role":"assistant","parts":[{"type":"text","content":"a\u0000"},{"type":"tool_call","id":"read-one","name":"get_file","arguments":{"filename":"x\u0000"}},{"type":"text","content":"b"}]}]`,
		"gen_ai.usage.output_tokens": "3",
	})
	writeSourceFile(t, path, lean+"\n"+middle+"\n"+rich+"\n")
	_, messages, err := parseVisualStudioCopilotTestSession(t, path, "visualstudio", "local")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "a\n\nb", messages[0].Content)
	assert.Equal(t, "middle", messages[1].Content)
	assert.Equal(t, int64(1781293610000000000), messages[0].Timestamp.UnixNano())
	assert.Equal(t, 24, messages[0].ContentLength)
	assert.Equal(t, 3, messages[0].OutputTokens)
	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, "[Read: get_file]\nx", messages[0].ToolCalls[0].Rendering)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 1}, {Kind: "tool_call"}, {Kind: "text", Start: 3, End: 4}}}, messages[0].ContentLayout)
}

func TestDeepSeekHarnessNativeOrderedBodies(t *testing.T) {
	records := deepSeekHarnessCompleteFixture("native-body", nil)
	for _, record := range records {
		event, ok := record.(map[string]any)
		if !ok {
			continue
		}
		seq, ok := event["seq"].(int)
		if !ok {
			continue
		}
		if seq != 22 && seq != 31 {
			continue
		}
		msg := event["data"].(map[string]any)["message"].(map[string]any)
		if seq == 31 {
			msg["content"] = []any{map[string]any{"type": "reasoning", "text": ""}}
			continue
		}
		msg["content"] = []any{
			map[string]any{"type": "text", "text": "first"},
			map[string]any{"type": "reasoning", "text": "plan"},
			map[string]any{"type": "tool-call", "id": "call-1", "name": "read_file", "arguments": `{"path":"x"}`},
			map[string]any{"type": "text", "text": "second"},
		}
	}
	path := writeDeepSeekHarnessFixture(t, t.TempDir(), "native-body", deepSeekHarnessFixtureCwd, "plain", records)
	result, err := parseDeepSeekHarnessSession(t.Context(), path, "local")
	require.NoError(t, err)
	require.Len(t, result.Messages, 5)
	msgs := result.Messages
	require.NotNil(t, msgs[0].ContentLayout)
	require.NotNil(t, msgs[1].ContentLayout)
	assert.Equal(t, "first\nsecond", msgs[2].Content)
	assert.Equal(t, "plan", msgs[2].ThinkingText)
	assert.Equal(t, 12, msgs[2].ContentLength)
	assert.Equal(t, "a1", msgs[2].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", Start: 6, End: 12}}}, msgs[2].ContentLayout)
	assert.Equal(t, "file data\n[image]", msgs[3].ToolResultText)
	assert.Equal(t, "tr1", msgs[3].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 17}}}, msgs[3].ContentLayout)
	assert.True(t, msgs[4].HasThinking)
	assert.Empty(t, msgs[4].ThinkingText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[4].ContentLayout)
	assert.Equal(t, 1, msgs[4].ContextTokens)
}

func TestVisualStudioCopilotControlOnlyPartRetainsHistoricalSeparators(t *testing.T) {
	span := vsCopilotSpan{attrMap: map[string]string{
		"gen_ai.operation.name":  "chat",
		"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"a"},{"type":"text","content":"\u0000"},{"type":"text","content":"b"}]}]`,
	}}
	historical, message := visualStudioCopilotChatOutput(span, nil)
	assert.Equal(t, "a\n\n\x00\n\nb", historical)
	assert.Equal(t, "a\n\nb", message.Content)
	assert.Equal(t, 6, message.ContentLength)
}
