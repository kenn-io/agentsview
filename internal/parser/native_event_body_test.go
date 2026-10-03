package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopilotNativeMessageBodies(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"id":"start","type":"session.start","data":{"sessionId":"native"}}`,
		`{"id":"user","parentId":"start","type":"user.message","data":{"content":"[Thinking] is literal"}}`,
		`{"id":"answer","parentId":"user","type":"assistant.message","data":{"content":"answer","reasoningText":" plan ","toolRequests":[{"toolCallId":"call-1","name":"read_file","arguments":{"path":"input-needle"}}]}}`,
		`{"type":"assistant.reasoning","ephemeral":true,"data":{"reasoningId":"reason-1","content":" plan "}}`,
		`{"id":"output","type":"tool.execution_complete","data":{"toolCallId":"call-1","success":true,"result":"output-needle"}}`,
		`{"id":"orphan","type":"tool.execution_complete","data":{"toolCallId":"","success":true,"result":"unmatched-output"}}`,
		`{"id":"opaque","type":"assistant.message","data":{"content":"","reasoningOpaque":"opaque-needle","encryptedContent":"encrypted-needle"}}`,
		`{"id":"call-only","type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"call-2","name":"read_file","arguments":{"path":"next-input"}}]}}`,
	)
	sess, msgs := parseAndValidateHelper(t, path, "local", 6)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.False(t, msgs[0].HasThinking)
	assert.Equal(t, "answer", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 35, msgs[1].ContentLength)
	assert.Equal(t, "answer", msgs[1].SourceUUID)
	assert.Equal(t, "user", msgs[1].SourceParentUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "thinking", End: 4}, {Kind: "text", End: 6}, {Kind: "tool_call"},
	}}, msgs[1].ContentLayout)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Empty(t, msgs[1].ToolCalls[0].Rendering)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "output-needle", msgs[1].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, "unmatched-output", msgs[3].ToolResultText)
	assert.Equal(t, "orphan", msgs[3].SourceUUID)
	assert.Empty(t, msgs[3].Content)
	assert.True(t, msgs[4].HasThinking)
	assert.Empty(t, msgs[4].ThinkingText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[4].ContentLayout)
	assert.Empty(t, msgs[5].Content)
	require.Len(t, msgs[5].ToolCalls, 1)
	assert.Equal(t, "[Read: read_file]", msgs[5].ToolCalls[0].Rendering)
	assert.Equal(t, 17, msgs[5].ContentLength)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}

func TestCopilotNativeReasoningEventBody(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"native-reason"}}`,
		`{"type":"assistant.message","data":{"content":"answer"}}`,
		`{"type":"assistant.reasoning","data":{"reasoningId":"reason-1","content":"event-plan"}}`,
		`{"type":"assistant.reasoning","data":{"reasoningId":"reason-1","content":"event-plan"}}`,
	)
	_, msgs := parseAndValidateHelper(t, path, "local", 1)
	assert.Equal(t, "answer", msgs[0].Content)
	assert.Equal(t, "event-plan", msgs[0].ThinkingText)
	assert.Equal(t, 6, msgs[0].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "text", End: 6}, {Kind: "thinking", End: 10},
	}}, msgs[0].ContentLayout)
}

func TestCopilotNativeToolWorkLength(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"native-work"}}`,
		`{"type":"assistant.message","data":{"content":"","reasoningText":"plan","toolRequests":[{"toolCallId":"call-1","name":"read_file","arguments":{}},{"toolCallId":"call-2","name":"read_file","arguments":{}}]}}`,
	)
	_, msgs := parseAndValidateHelper(t, path, "local", 1)
	assert.Empty(t, msgs[0].Content)
	assert.Equal(t, "plan", msgs[0].ThinkingText)
	assert.Equal(t, 64, msgs[0].ContentLength)
	require.Len(t, msgs[0].ToolCalls, 2)
	assert.Equal(t, "[Read: read_file]", msgs[0].ToolCalls[0].Rendering)
	assert.Equal(t, "[Read: read_file]", msgs[0].ToolCalls[1].Rendering)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "tool_call", CallIndex: 1},
	}}, msgs[0].ContentLayout)
}

func TestDeepSeekTUINativeMessageBodies(t *testing.T) {
	path := createTestFile(t, "native.json", `{"metadata":{"id":"native"},"messages":[
{"role":"user","content":[{"type":"text","text":"[Thinking] is literal"}]},
{"role":"assistant","content":[{"type":"text","text":"first"},{"type":"thinking","thinking":"plan"},{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"input-needle"}},{"type":"text","text":"after"}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"output-needle"}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"","content":"unmatched-output"}]},
{"role":"assistant","content":[{"type":"thinking","thinking":""}]}]}`)
	sess, msgs, err := parseDeepSeekTUITestSession(t, path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 5)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.False(t, msgs[0].HasThinking)
	assert.Equal(t, "first\nafter", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 60, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", Start: 6, End: 11},
	}}, msgs[1].ContentLayout)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, "unmatched-output", msgs[3].ToolResultText)
	assert.Empty(t, msgs[3].Content)
	assert.True(t, msgs[4].HasThinking)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[4].ContentLayout)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}

func TestTauNativeMessageBodies(t *testing.T) {
	result := parseTauTestSource(t, "native.jsonl", tauLines(
		`{"id":"info","type":"session_info","cwd":"/repo"}`,
		`{"id":"u","parent_id":"info","type":"message","message":{"role":"user","content":"[Thinking] is literal"}}`,
		`{"id":"a","parent_id":"u","type":"message","message":{"role":"assistant","content":[{"type":"text","text":"first"},{"type":"thinking","thinking":"plan"},{"type":"toolCall","id":"call-1","name":"read","arguments":{"path":"input-needle"}},{"type":"text","text":"after"}]}}`,
		`{"id":"r","parent_id":"a","type":"message","message":{"role":"toolResult","toolCallId":"call-1","content":[{"type":"text","text":"output-needle"}]}}`,
		`{"id":"orphan","parent_id":"r","type":"message","message":{"role":"toolResult","toolCallId":"","content":[{"type":"text","text":"unmatched-output"}]}}`,
		`{"id":"redacted","parent_id":"orphan","type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"opaque-needle","redacted":true}]}}`,
		`{"id":"compact","parent_id":"redacted","type":"compaction","summary":"summary"}`,
		`{"id":"branch","parent_id":"compact","type":"branch_summary","summary":"branch-text"}`,
		`{"id":"leaf","type":"leaf","entry_id":"branch"}`,
	))
	msgs := result.Messages
	require.Len(t, msgs, 7)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.False(t, msgs[0].HasThinking)
	assert.Equal(t, "first\nafter", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 60, msgs[1].ContentLength)
	assert.Equal(t, "a", msgs[1].SourceUUID)
	assert.Equal(t, "u", msgs[1].SourceParentUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", Start: 6, End: 11},
	}}, msgs[1].ContentLayout)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, "unmatched-output", msgs[3].ToolResultText)
	assert.True(t, msgs[4].HasThinking)
	assert.Empty(t, msgs[4].ThinkingText)
	assert.Empty(t, msgs[4].Content)
	assert.Equal(t, 36, msgs[4].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[4].ContentLayout)
	assert.True(t, msgs[5].IsCompactBoundary)
	assert.True(t, msgs[6].IsSystem)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}
