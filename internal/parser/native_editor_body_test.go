package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVSCodeCopilotNativeMessageBodies(t *testing.T) {
	fixture := `{"version":3,"sessionId":"native","requests":[{"requestId":"u1","responseId":"a1","message":{"text":"[Thinking] is literal"},"response":[{"value":"first"},{"kind":"thinking","value":"plan"},{"kind":"toolInvocationSerialized","toolId":"copilot_runInTerminal","toolCallId":"call-1","toolSpecificData":{"kind":"terminal","commandLine":{"original":"input-needle"}}},{"kind":"thinking","value":["ne","xt"]},{"value":"after"}],"result":{"metadata":{"promptTokens":10,"outputTokens":5,"resolvedModel":"gpt-5"}}},{"requestId":"u2","responseId":"a2","message":{"text":""},"response":[{"kind":"thinking","value":[]}]}]}`
	for _, agent := range []AgentType{AgentVSCodeCopilot, AgentPositron} {
		t.Run(string(agent), func(t *testing.T) {
			path := createTestFile(t, "native.json", fixture)
			var sess *ParsedSession
			var msgs []ParsedMessage
			var err error
			if agent == AgentPositron {
				sess, msgs, err = (&positronProvider{}).parseSession(path, "project", "local")
			} else {
				sess, msgs, err = (&vscodeCopilotProvider{}).parseSession(path, "project", "local")
			}
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.Len(t, msgs, 3)
			assert.Equal(t, 1, sess.UserMessageCount)
			assert.Equal(t, 10, sess.PeakContextTokens)
			assert.Equal(t, 5, sess.TotalOutputTokens)
			assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
			assert.Equal(t, "u1", msgs[0].SourceUUID)
			require.NotNil(t, msgs[0].ContentLayout)
			assert.Equal(t, "firstafter", msgs[1].Content)
			assert.Equal(t, "plan\n\nnext", msgs[1].ThinkingText)
			assert.Equal(t, 60, msgs[1].ContentLength)
			assert.Equal(t, "gpt-5", msgs[1].Model)
			assert.Equal(t, "a1", msgs[1].SourceUUID)
			require.Len(t, msgs[1].ToolCalls, 1)
			assert.Equal(t, "[Bash: copilot_runInTerminal]\n$ input-needle", msgs[1].ToolCalls[0].Rendering)
			assert.JSONEq(t, `{"command":"input-needle"}`, msgs[1].ToolCalls[0].InputJSON)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
				{Kind: "text", End: 5},
				{Kind: "thinking", End: 4},
				{Kind: "tool_call"},
				{Kind: "thinking", Start: 6, End: 10},
				{Kind: "text", Start: 5, End: 10},
			}}, msgs[1].ContentLayout)
			assert.True(t, msgs[2].HasThinking)
			assert.Equal(t, "a2", msgs[2].SourceUUID)
			assert.Empty(t, msgs[2].Content)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[2].ContentLayout)
		})
	}
}

func TestCortexNativeMessageBodies(t *testing.T) {
	path := createTestFile(t, cortexTestUUID+".json", `{"session_id":"`+cortexTestUUID+`","history":[{"role":"user","id":"u1","content":[{"type":"text","text":"[Thinking] is literal"}]},{"role":"assistant","id":"a1","content":[{"type":"text","text":"first"},{"type":"tool_use","tool_use":{"tool_use_id":"call-1","name":"read","input":{"file_path":"input-needle"}}},{"type":"text","text":"after"}]},{"role":"user","id":"r1","content":[{"type":"tool_result","tool_result":{"tool_use_id":"","name":"read","content":[{"type":"text","text":"output-needle"}]}}]}]}`)
	sess, msgs, err := parseCortexSessionForTest(t, path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 3)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "first\nafter", msgs[1].Content)
	assert.Equal(t, 11, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 5}, {Kind: "tool_call"}, {Kind: "text", Start: 6, End: 11}}}, msgs[1].ContentLayout)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "[Read: input-needle]", msgs[1].ToolCalls[0].Rendering)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, 18, msgs[2].ContentLength)
	assert.Equal(t, SourceSubtypeToolResult, msgs[2].SourceSubtype)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
}
