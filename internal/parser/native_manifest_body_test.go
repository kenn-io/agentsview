package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandCodeNativeMessageBodies(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project", "native-body.jsonl")
	writeSourceFile(t, path, `{"id":"literal","sessionId":"native-body","role":"user","content":"[Thinking] is literal"}
{"id":"ordered","sessionId":"native-body","role":"assistant","content":[{"type":"text","text":"first"},{"type":"thinking","thinking":"plan"},{"type":"tool-call","toolCallId":"call-1","toolName":"Read","input":{"file_path":"input-needle"}},{"type":"text","text":"after"}]}
{"id":"result","sessionId":"native-body","role":"tool","content":[{"type":"tool-result","toolCallId":"","output":{"type":"text","value":"output-needle"}}]}
{"id":"empty","sessionId":"native-body","role":"assistant","content":[{"type":"thinking","thinking":""}]}
{"id":"controls","sessionId":"native-body","role":"tool","content":[{"type":"tool-result","toolCallId":"orphan","output":{"type":"text","value":"NUL\u0000END"}}]}`)
	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{Roots: []string{root}, Machine: "local"})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "native-body"})
	require.NoError(t, err)
	require.True(t, found)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source, Machine: "local"})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 5)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.Equal(t, "first\nafter", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 11, msgs[1].ContentLength)
	assert.Equal(t, "ordered", msgs[1].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", Start: 6, End: 11},
	}}, msgs[1].ContentLayout)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, "result", msgs[2].SourceUUID)
	assert.True(t, msgs[3].HasThinking)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[3].ContentLayout)
	assert.Equal(t, "NULEND", msgs[4].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 6}}}, msgs[4].ContentLayout)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}

func TestCodeBuddyNativeMessageBodies(t *testing.T) {
	index := writeCodeBuddyFixture(t, t.TempDir(), "workspace", "native-body")
	dir := filepath.Join(filepath.Dir(index), "messages")
	require.NoError(t, os.WriteFile(index, []byte(`{"messages":[{"id":"literal"},{"id":"ordered"},{"id":"result"},{"id":"empty"}]}`), 0o600))
	for id, body := range map[string]string{
		"literal": `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>[Thinking] is literal</user_query>"}]}}`,
		"ordered": `{"role":"assistant","message":{"content":[{"type":"text","text":"first"},{"type":"reasoning","text":"plan"},{"type":"tool-call","toolCallId":"call-1","toolName":"Read","args":{"file_path":"input-needle"}},{"type":"text","text":"after"}]},"extra":{"lastStepInputTokens":10,"lastStepCachedInputTokens":2,"lastStepOutputTokens":5}}`,
		"result":  `{"role":"tool","message":{"content":[{"type":"tool-result","toolCallId":"","result":{"result":{"stdout":"output-needle"}}}]}}`,
		"empty":   `{"role":"assistant","message":{"content":[{"type":"thinking","text":""}]}}`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o600))
	}
	sess, msgs, err := parseCodeBuddySession(index, "project", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 4)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.Equal(t, "first\nafter", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 15, msgs[1].ContentLength)
	assert.Equal(t, 10, msgs[1].ContextTokens)
	assert.Equal(t, 5, msgs[1].OutputTokens)
	assert.Equal(t, "ordered", msgs[1].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", Start: 6, End: 11},
	}}, msgs[1].ContentLayout)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, "result", msgs[2].SourceUUID)
	assert.True(t, msgs[3].HasThinking)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[3].ContentLayout)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}

func TestCodeBuddyNativeMissingResultUsesToolStatus(t *testing.T) {
	index := writeCodeBuddyFixture(t, t.TempDir(), "workspace", "fallback-body")
	message := filepath.Join(filepath.Dir(index), "messages", "u1.json")
	require.NoError(t, os.WriteFile(message, []byte(`{"role":"tool","message":{"content":[{"type":"tool-result"}]},"extra":{"toolStatus":{"call-1":{"result":{"result":{"stdout":"fallback-output"}}}}}}`), 0o600))
	sess, msgs, err := parseCodeBuddySession(index, "project", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 1)
	assert.Equal(t, "fallback-output", msgs[0].ToolResultText)
	require.Len(t, msgs[0].ToolResults, 1)
	assert.Equal(t, "call-1", msgs[0].ToolResults[0].ToolUseID)
}
