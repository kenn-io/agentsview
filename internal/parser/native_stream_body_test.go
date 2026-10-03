package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGptmeNativeRoleBodies(t *testing.T) {
	root := t.TempDir()
	const id = "2026-10-01-native-body"
	path := filepath.Join(root, id, "conversation.jsonl")
	writeSourceFile(t, path, "{\"role\":\"user\",\"content\":\"[Thinking] is literal\"}\n{\"role\":\"assistant\",\"content\":\"\",\"metadata\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":3}}}\n{\"role\":\"tool\",\"call_id\":\"unknown-call\",\"content\":\"output-needle\"}\n")
	provider, ok := NewProvider(AgentGptme, ProviderConfig{Roots: []string{root}, Machine: "local"})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: id})
	require.NoError(t, err)
	require.True(t, found)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 3)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Empty(t, msgs[1].Content)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{}}, msgs[1].ContentLayout)
	assert.Equal(t, 10, msgs[1].ContextTokens)
	assert.Equal(t, 3, msgs[1].OutputTokens)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, 13, msgs[2].ContentLength)
	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "unknown-call", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
	assert.Equal(t, 1, outcome.Results[0].Result.Session.UserMessageCount)
}

func TestZedNativeOrderedBodies(t *testing.T) {
	path := createZedThreadsDB(t, []zedTestThread{{id: "native-body", dataType: "json", data: []byte(`{"messages":[{"User":{"content":[{"Text":"[Thinking] is literal"}]}},{"Agent":{"content":[{"Thinking":{"text":"plan"}},{"Text":"first"},{"ToolUse":{"id":"call-x","name":"terminal","input":{"command":"input-needle"}}},{"Text":"second"},{"RedactedThinking":"opaque-only"}],"tool_results":{"call-x":{"output":"output-needle"}}}},{"Agent":{"content":[{"RedactedThinking":"opaque-only"}]}}]}`)}})
	results, err := parseZedAll(path, "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	msgs := results[0].Messages
	require.Len(t, msgs, 3)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "first\nsecond", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, "output-needle", msgs[1].ToolResultText)
	assert.Equal(t, 12, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 4}, {Kind: "text", End: 5}, {Kind: "tool_call"}, {Kind: "text", Start: 6, End: 12}, {Kind: "thinking", Start: 4, End: 4}, {Kind: "tool_result", End: 13}}}, msgs[1].ContentLayout)
	assert.True(t, msgs[2].HasThinking)
	assert.Empty(t, msgs[2].ThinkingText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[2].ContentLayout)
}

func TestPoolsideNativeEventBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trajectory-standalone_native-body.ndjson")
	writeSourceFile(t, path, `{"type":"session.input","session_input":{"prompt":"[Thinking] is literal"}}
{"type":"assistant_message.start"}
{"type":"thought.end","thought_end":{"thought":"plan"}}
{"type":"tool_call.parsed","tool_call_parsed":{"id":"call-x","name":"read","args":{"path":"input-needle"}}}
{"type":"tool_call.result","tool_call_result":{"id":"call-x","tool_name":"read","observation":"paired-output"}}
{"type":"thought.end","thought_end":{"thought":""}}
{"type":"tool_call.result","tool_call_result":{"id":"","tool_name":"read","observation":"output-needle"}}
{"type":"assistant_message.end","assistant_message_end":{"assistant_message":"answer"}}
`)
	sess, msgs, _, err := parsePoolsideSession(path, "project", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "answer", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, "output-needle", msgs[1].ToolResultText)
	assert.Equal(t, 6, msgs[1].ContentLength)
	require.Len(t, msgs[1].ToolCalls, 1)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "paired-output", msgs[1].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "thinking", Start: 4, End: 4}, {Kind: "tool_result", End: 13, Category: "Read"}, {Kind: "text", End: 6}}}, msgs[1].ContentLayout)
}

func TestPoolsideNativeOutputBeforeAssistant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trajectory-standalone_output-only.ndjson")
	writeSourceFile(t, path, "{\"type\":\"tool_call.result\",\"tool_call_result\":{\"id\":\"unknown-call\",\"tool_name\":\"read\",\"observation\":\"output-needle\"}}\n")
	sess, msgs, _, err := parsePoolsideSession(path, "project", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, 0, sess.UserMessageCount)
	assert.Equal(t, RoleTool, msgs[0].Role)
	assert.Empty(t, msgs[0].Content)
	assert.Equal(t, "output-needle", msgs[0].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13, Category: "Read"}}}, msgs[0].ContentLayout)
	require.Len(t, msgs[0].ToolResults, 1)
	assert.Equal(t, "unknown-call", msgs[0].ToolResults[0].ToolUseID)
}
