package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestZencoderNativeMessageBodies(t *testing.T) {
	sess, msgs, err := runZencoderParserTest(t, strings.Join([]string{
		`{"id":"native-body","createdAt":"2026-01-01T00:00:00Z"}`,
		`{"role":"user","content":[{"type":"text","text":"[Thinking] is literal"}]}`,
		`{"role":"assistant","content":[{"type":"text","text":"first"},{"type":"reasoning","text":"plan"},{"type":"tool-call","toolCallId":"call-1","toolName":"Read","input":{"file_path":"input-needle"}},{"type":"text","text":"after"}]}`,
		`{"role":"tool","content":[{"type":"tool-result","toolCallId":"","content":[{"type":"text","text":"output-needle"},{"type":"text","tag":"system-reminder","text":"system-output"}]}]}`,
		`{"role":"assistant","content":[{"type":"reasoning","text":""}]}`,
		`{"role":"finish","reason":"endTurn"}`,
	}, "\n"))
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 6)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.False(t, msgs[0].HasThinking)
	assert.Equal(t, "first\nafter", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 60, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", Start: 6, End: 11},
	}}, msgs[1].ContentLayout)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "[Read: input-needle]", msgs[1].ToolCalls[0].Rendering)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Empty(t, msgs[2].Content)
	assert.True(t, msgs[3].IsSystem)
	assert.Equal(t, SourceSubtypeToolResult, msgs[3].SourceSubtype)
	assert.Equal(t, "system-output", msgs[3].ToolResultText)
	assert.Empty(t, msgs[3].Content)
	assert.True(t, msgs[4].HasThinking)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[4].ContentLayout)
	assert.True(t, msgs[5].IsSystem)
	assert.Equal(t, "[Turn finished: endTurn]", msgs[5].Content)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}

func TestZencoderNativeOnlyWorkSession(t *testing.T) {
	sess, msgs, err := runZencoderParserTest(t, `{"id":"work-only"}
{"role":"assistant","content":[{"type":"reasoning","text":""}]}`)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 1)
	assert.True(t, msgs[0].HasThinking)
	assert.Empty(t, msgs[0].Content)
}

func TestCodebuffNativeMessageBodies(t *testing.T) {
	dir := codebuffTestSession(t, `[
{"variant":"user","content":"[Thinking] is literal","timestamp":"2026-01-01T00:00:00Z"},
{"variant":"ai","timestamp":"2026-01-01T00:00:01Z","blocks":[
{"type":"text","content":"first"},{"type":"text","textType":"reasoning","content":"plan"},
{"type":"tool","toolCallId":"","toolName":"read_file","input":{"path":"input-needle"},"output":"output-needle"},
{"type":"text","content":"after"},{"type":"text","textType":"reasoning","content":""},
{"type":"mode-divider","mode":"plan"}]}]`, "", "")
	sess, msgs, err := parseCodebuffSession(dir, "project", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 8)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.Equal(t, "first", msgs[1].Content)
	assert.Equal(t, "plan", msgs[2].ThinkingText)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, 4, msgs[2].ContentLength)
	require.Len(t, msgs[3].ToolCalls, 1)
	assert.Equal(t, "read_file", msgs[3].ToolCalls[0].ToolName)
	assert.Empty(t, msgs[4].Content)
	assert.Equal(t, "output-needle", msgs[4].ToolResultText)
	assert.Equal(t, "after", msgs[5].Content)
	assert.True(t, msgs[6].HasThinking)
	assert.Empty(t, msgs[6].ThinkingText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[6].ContentLayout)
	assert.True(t, msgs[7].IsSystem)
	assert.Equal(t, "[Mode: plan]", msgs[7].Content)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}
