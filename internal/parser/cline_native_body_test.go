package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseClineNativeFixture(t *testing.T, messages string) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "native-array")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	metadata := filepath.Join(dir, "native-array.json")
	require.NoError(t, os.WriteFile(metadata,
		[]byte(`{"session_id":"native-array","status":"completed","prompt":"request"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "native-array.messages.json"), []byte(messages), 0o644))
	sess, msgs, err := parseClineSession(metadata, "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	return sess, msgs
}

func TestClineNativeMessageBodies(t *testing.T) {
	sess, msgs := parseClineNativeFixture(t, `{"messages":[
{"id":"literal","role":"user","ts":1000,"content":[{"type":"text","text":"<user_input mode=\"act\">[Thinking] is literal</user_input>"}]},
{"id":"ordered","role":"assistant","ts":2000,"content":[{"type":"text","text":"first"},{"type":"thinking","thinking":"plan"},{"type":"tool_use","id":"call-1","name":"read_file","input":{"path":"input-needle"}},{"type":"text","text":"after"}],"metrics":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":2,"cacheWriteTokens":3}},
{"id":"result","role":"user","ts":3000,"content":[{"type":"tool_result","tool_use_id":"call-1","content":"output-needle"}]},
{"id":"opaque","role":"assistant","ts":4000,"content":[{"type":"redacted_thinking","data":"encrypted-needle"}]}]}`)
	require.Len(t, msgs, 4)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.False(t, msgs[0].HasThinking)
	assert.Equal(t, "first\n\nafter", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, "ordered", msgs[1].SourceUUID)
	assert.Equal(t, 16, msgs[1].ContentLength)
	assert.Equal(t, 15, msgs[1].ContextTokens)
	assert.Equal(t, 5, msgs[1].OutputTokens)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", Start: 7, End: 12},
	}}, msgs[1].ContentLayout)
	require.Len(t, msgs[1].ToolCalls, 1)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "output-needle", msgs[1].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "completed", msgs[1].ToolCalls[0].ResultEvents[0].Status)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, "result", msgs[2].SourceUUID)
	assert.True(t, msgs[2].IsSystem)
	assert.True(t, msgs[3].HasThinking)
	assert.Empty(t, msgs[3].ThinkingText)
	assert.Equal(t, "opaque", msgs[3].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[3].ContentLayout)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}

func TestClineNativeThinkingDoesNotReclassifyLiteralDialogue(t *testing.T) {
	sess, msgs := parseClineNativeFixture(t, `{"messages":[
{"id":"user","role":"user","ts":1000,"content":[{"type":"text","text":"request"}]},
{"id":"assistant","role":"assistant","ts":2000,"content":[{"type":"thinking","thinking":"plan"},{"type":"text","text":"[Thinking]\nliteral\n[/Thinking]"}]}]}`)
	require.Len(t, msgs, 2)
	assert.Equal(t, "[Thinking]\nliteral\n[/Thinking]", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 59, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "thinking", End: 4}, {Kind: "text", End: 30},
	}}, msgs[1].ContentLayout)
	assert.Equal(t, TerminationClean, sess.TerminationStatus)
}
