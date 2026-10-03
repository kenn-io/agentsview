package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForgeNativeMessageBodies(t *testing.T) {
	path, seeder, database := newForgeTestDB(t)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	seeder.AddConversation(t.Context(), "native-body", "Native bodies", 1, `{"messages":[
{"message":{"text":{"role":"User","content":"[Thinking] is literal","timestamp":"2026-01-01T00:00:00Z"}}},
{"message":{"text":{"role":"Assistant","content":"answer","reasoning_details":[{"text":"plan","type_of":"reasoning.text"},{"data":"encrypted-needle","type_of":"reasoning.encrypted"}],"tool_calls":[{"name":"read","call_id":"call-1","arguments":{"path":"input-needle"}}],"timestamp":"2026-01-01T00:00:01Z"}},"usage":{"prompt_tokens":{"actual":10},"completion_tokens":{"actual":5}}},
{"message":{"tool":{"name":"read","call_id":"","output":{"values":[{"text":"output-needle"}]}}}},
{"message":{"text":{"role":"Assistant","content":"","reasoning_details":[{"signature":"signature-needle"}],"timestamp":"2026-01-01T00:00:03Z"}}}]}`, "2026-01-01 00:00:00", "2026-01-01 00:00:03", "")
	sessions, err := parseForgeAll(path, "local")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	msgs := sessions[0].Messages
	require.Len(t, msgs, 4)
	assert.Equal(t, 1, sessions[0].Session.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.Equal(t, "answer", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 10, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "thinking", End: 4}, {Kind: "thinking", Start: 4, End: 4}, {Kind: "text", End: 6}, {Kind: "tool_call"},
	}}, msgs[1].ContentLayout)
	assert.Equal(t, 10, msgs[1].ContextTokens)
	assert.Equal(t, 5, msgs[1].OutputTokens)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.True(t, msgs[3].HasThinking)
	assert.Empty(t, msgs[3].ThinkingText)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}

func TestEvenerNativeMessageBodies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "native-body.transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"kind":"header","format_version":2,"session_id":"native-body","created_at":"2026-01-01T00:00:00Z","model":"model","system_prompt":"system context"}
{"kind":"entry","seq":0,"turn":{"kind":"USER_INPUT","stable_turn_id":"user","timestamp":"2026-01-01T00:00:01Z","message":{"content":[{"kind":"text","text":"[Thinking] is literal"}]}}}
{"kind":"entry","seq":1,"turn":{"kind":"ASSISTANT","stable_turn_id":"assistant","timestamp":"2026-01-01T00:00:02Z","message":{"content":[{"kind":"text","text":"first"},{"kind":"thinking","thinking":{"text":"plan"}},{"kind":"tool_call","tool_call":{"id":"call-1","name":"read","arguments":{"path":"input-needle"}}},{"kind":"text","text":"after"}]}}}
{"kind":"entry","seq":2,"turn":{"kind":"TOOL_RESULTS","stable_turn_id":"result","timestamp":"2026-01-01T00:00:03Z","message":{"content":[{"kind":"tool_result","tool_result":{"tool_call_id":"call-1","name":"read","content":"output-needle"}}]}}}
{"kind":"entry","seq":3,"turn":{"kind":"ASSISTANT","stable_turn_id":"opaque","timestamp":"2026-01-01T00:00:04Z","message":{"content":[{"kind":"redacted_thinking","thinking":{"redacted":true,"encrypted_content":"encrypted-needle"}}]}}}
`), 0o600))
	sess, msgs, err := parseEvenerSession(t.Context(), path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 5)
	assert.Equal(t, "system context", msgs[0].Content)
	assert.True(t, msgs[0].IsSystem)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", msgs[1].Content)
	assert.Equal(t, "first\nafter", msgs[2].Content)
	assert.Equal(t, "plan", msgs[2].ThinkingText)
	assert.Equal(t, "assistant", msgs[2].SourceUUID)
	assert.Equal(t, 53, msgs[2].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", Start: 6, End: 11},
	}}, msgs[2].ContentLayout)
	require.Len(t, msgs[2].ToolCalls, 1)
	require.Len(t, msgs[2].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "output-needle", msgs[2].ToolCalls[0].ResultEvents[0].Content)
	assert.Empty(t, msgs[3].Content)
	assert.Equal(t, "output-needle", msgs[3].ToolResultText)
	assert.Equal(t, "result", msgs[3].SourceUUID)
	assert.True(t, msgs[4].HasThinking)
	assert.Empty(t, msgs[4].ThinkingText)
	assert.Equal(t, "opaque", msgs[4].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[4].ContentLayout)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}

func TestEvenerNativeUsageOnlyBody(t *testing.T) {
	turn := evenerTestTurn("ASSISTANT", "")
	turn["usage"] = map[string]any{"input_tokens": 10, "output_tokens": 5}
	path := writeEvenerFixture(t, t.TempDir(), "usage-body", nil, turn)
	sess, msgs, err := parseEvenerSession(t.Context(), path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 1)
	assert.Empty(t, msgs[0].Content)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{}}, msgs[0].ContentLayout)
	assert.Equal(t, 10, msgs[0].ContextTokens)
	assert.Equal(t, 5, msgs[0].OutputTokens)
	assert.Equal(t, 5, sess.TotalOutputTokens)
}

func TestEvenerNativeWebSearchStaysWork(t *testing.T) {
	turn := evenerTestTurn("ASSISTANT", "")
	turn["message"] = map[string]any{"content": []any{
		map[string]any{"kind": "web_search", "web_search": map[string]any{"query": "input-needle"}},
	}}
	path := writeEvenerFixture(t, t.TempDir(), "web-body", nil, turn)
	_, msgs, err := parseEvenerSession(t.Context(), path, "local")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Empty(t, msgs[0].Content)
	assert.Equal(t, 25, msgs[0].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_call"}}}, msgs[0].ContentLayout)
	require.Len(t, msgs[0].ToolCalls, 1)
	assert.Equal(t, "web_search", msgs[0].ToolCalls[0].ToolName)
	assert.Equal(t, "[web search] input-needle", msgs[0].ToolCalls[0].Rendering)
	assert.JSONEq(t, `{"query":"input-needle"}`, msgs[0].ToolCalls[0].InputJSON)
}
