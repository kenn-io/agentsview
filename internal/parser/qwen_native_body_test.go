package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQwenNativeCoalescedMessageBody(t *testing.T) {
	for _, resultType := range []string{"user", "tool_result"} {
		t.Run(resultType, func(t *testing.T) {
			path := createTestFile(t, "native.jsonl", `{"uuid":"u1","sessionId":"native","type":"user","message":{"role":"user","parts":[{"text":"[Thinking] is literal"}]}}
{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"model","parts":[{"text":"first"},{"text":"plan","thought":true},{"functionCall":{"id":"call-1","name":"Read","args":{"path":"input-needle"}}},{"text":"after"}]},"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5,"cachedContentTokenCount":20}}
{"uuid":"r1","type":"`+resultType+`","message":{"role":"user","parts":[{"functionResponse":{"id":"call-1","name":"Read","response":{"output":"output-needle"}}}]}}
{"uuid":"a2","type":"assistant","message":{"role":"model","parts":[{"text":"next","thought":true},{"text":"done"}]},"usageMetadata":{"promptTokenCount":150,"candidatesTokenCount":7,"cachedContentTokenCount":30}}`)
			sess, msgs, err := parseQwenSession(path, "project", "local")
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.Len(t, msgs, 2)
			assert.Equal(t, 1, sess.UserMessageCount)
			assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
			require.NotNil(t, msgs[0].ContentLayout)
			body := msgs[1]
			assert.Equal(t, "first\nafter\ndone", body.Content)
			assert.Equal(t, "plan\nnext", body.ThinkingText)
			assert.Equal(t, "output-needle", body.ToolResultText)
			assert.Equal(t, 16, body.ContentLength)
			assert.Equal(t, 12, body.OutputTokens)
			assert.Equal(t, 150, body.ContextTokens)
			require.Len(t, body.ToolCalls, 1)
			require.Len(t, body.ToolResults, 1)
			assert.Equal(t, "call-1", body.ToolCalls[0].ToolUseID)
			assert.Equal(t, "call-1", body.ToolResults[0].ToolUseID)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
				{Kind: "text", End: 5},
				{Kind: "thinking", End: 4},
				{Kind: "tool_call"},
				{Kind: "text", Start: 6, End: 11},
				{Kind: "tool_result", End: 13},
				{Kind: "thinking", Start: 5, End: 9},
				{Kind: "text", Start: 12, End: 16},
			}}, body.ContentLayout)
		})
	}
}

func TestQwenNativeEmptyAndUsageBodies(t *testing.T) {
	for _, tc := range []struct {
		name, record string
		thinking     bool
		blocks       []ContentBlock
	}{
		{"empty-thinking", `{"message":{"role":"model","parts":[{"thought":true,"text":""}]},"type":"assistant"}`, true, []ContentBlock{{Kind: "thinking"}}},
		{"usage-only", `{"message":{"role":"model","parts":[]},"type":"assistant","usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`, false, []ContentBlock{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := createTestFile(t, "native.jsonl", tc.record)
			sess, msgs, err := parseQwenSession(path, "project", "local")
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.Len(t, msgs, 1)
			assert.Empty(t, msgs[0].Content)
			assert.Equal(t, tc.thinking, msgs[0].HasThinking)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: tc.blocks}, msgs[0].ContentLayout)
			if tc.name == "usage-only" {
				assert.Equal(t, 10, sess.PeakContextTokens)
				assert.Equal(t, 5, sess.TotalOutputTokens)
			}
		})
	}
}
