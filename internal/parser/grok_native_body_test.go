package parser

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGrokNativeMessageBodies(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "native-body")
	summary := filepath.Join(dir, "summary.json")
	writeGrokFixtureFile(t, summary, `{}`)
	writeGrokFixtureFile(t, filepath.Join(dir, "chat_history.jsonl"), `{"type":"user","content":"[Read: literal]"}
{"type":"backend_tool_call","kind":{"tool_type":"web_search","id":"web-1","action":{"type":"search","query":"input-needle"}}}
{"type":"reasoning","summary":[{"type":"summary_text","text":"plan"}]}
{"type":"assistant","content":"[Thinking]\nplan\n[/Thinking]\nis literal"}
{"type":"tool_result","content":"output-needle"}
{"type":"assistant","reasoning":{},"content":""}`)
	writeGrokFixtureFile(t, filepath.Join(dir, "updates.jsonl"), `{"timestamp":2000,"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"[Thinking]\nplan\n[/Thinking]\nis literal"}}}}`)
	result, err := ParseGrokSummary(summary, "project", "local")
	require.NoError(t, err)
	msgs := result.Messages
	require.Len(t, msgs, 5)
	assert.Equal(t, 1, result.Session.UserMessageCount)
	assert.Equal(t, "[Read: literal]", msgs[0].Content)
	assert.Empty(t, msgs[1].Content)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "[backend web_search] search: input-needle", msgs[1].ToolCalls[0].Rendering)
	assert.Equal(t, 41, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_call"}}}, msgs[1].ContentLayout)
	assert.Equal(t, "[Thinking]\nplan\n[/Thinking]\nis literal", msgs[2].Content)
	assert.Equal(t, "plan", msgs[2].ThinkingText)
	assert.Equal(t, 42, msgs[2].ContentLength)
	assert.Equal(t, time.Unix(2000, 0).UTC(), msgs[2].Timestamp)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 4}, {Kind: "text", End: 38}}}, msgs[2].ContentLayout)
	assert.Empty(t, msgs[3].Content)
	assert.Equal(t, "output-needle", msgs[3].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[3].ContentLayout)
	assert.True(t, msgs[4].HasThinking)
	assert.Empty(t, msgs[4].ThinkingText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[4].ContentLayout)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}

func TestGrokNativeSummaryBody(t *testing.T) {
	summary := filepath.Join(t.TempDir(), "native-summary", "summary.json")
	writeGrokFixtureFile(t, summary, `{"firstPrompt":"[Thinking] is literal"}`)
	result, err := ParseGrokSummary(summary, "project", "local")
	require.NoError(t, err)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, "[Thinking] is literal", result.Messages[0].Content)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 21}}}, result.Messages[0].ContentLayout)
}
