package parser

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOmnigentNativeEventStoreBodies(t *testing.T) {
	path := writeOmnigentDB(t, omnigentDBOptions{seed: func(t *testing.T, conn *sql.DB) {
		t.Helper()
		_, err := conn.ExecContext(t.Context(), `INSERT INTO conversations (id,created_at,updated_at,title,root_conversation_id) VALUES ('native',1783716327,1783718231,'native','native')`)
		require.NoError(t, err)
		_, err = conn.ExecContext(t.Context(), `INSERT INTO omnigent_conversation_metadata (id,kind) VALUES ('native',1)`)
		require.NoError(t, err)
		for i, row := range []struct {
			id   string
			kind int
			data string
		}{
			{"user-native", 1, `{"role":"user","content":[{"type":"input_text","text":"[Thinking] is literal"}]}`},
			{"call-native", 2, `{"call_id":"call-x","name":"read","arguments":"{\"path\":\"input-needle\"}"}`},
			{"result-native", 3, `{"call_id":"call-x","output":"output-needle"}`},
			{"empty-native", 4, `{"summary":[],"encrypted_content":"opaque-signature"}`},
			{"orphan-native", 3, `{"call_id":"","output":"orphan-output"}`},
		} {
			_, err := conn.ExecContext(t.Context(), `INSERT INTO conversation_items (conversation_id,id,position,type,data,search_text) VALUES ('native',?,?,?,?, '')`, row.id, i, row.kind, row.data)
			require.NoError(t, err)
		}
	}})
	results, err := ParseOmnigentDB(t.Context(), path, "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	msgs := results[0].Messages
	require.Len(t, msgs, 4)
	assert.Equal(t, "user-native", msgs[0].SourceUUID)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.Equal(t, "call-native", msgs[1].SourceUUID)
	assert.Empty(t, msgs[1].Content)
	assert.Equal(t, "output-needle", msgs[1].ToolResultText)
	assert.Equal(t, 0, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_call"}, {Kind: "tool_result", End: 13}}}, msgs[1].ContentLayout)
	assert.True(t, msgs[2].HasThinking)
	assert.Empty(t, msgs[2].ThinkingText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[2].ContentLayout)
	assert.Equal(t, "orphan-native", msgs[3].SourceUUID)
	assert.Empty(t, msgs[3].Content)
	assert.Equal(t, "orphan-output", msgs[3].ToolResultText)
	assert.Equal(t, 13, msgs[3].ContentLength)
	assert.Equal(t, 1, results[0].Session.UserMessageCount)
}

func TestWorkBuddyNativeEventStoreBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.jsonl")
	writeSourceFile(t, path, `{"type":"message","role":"user","content":"[Thinking] is literal"}
{"type":"function_call","name":"read","callId":"call-x","arguments":{"path":"input-needle"}}
{"type":"function_call_result","callId":"","output":"output-needle"}
{"type":"message","role":"assistant","content":"","providerData":{"usage":{"inputTokens":10,"outputTokens":2}}}
`)
	sess, msgs, err := parseWorkBuddyTestSession(t, path, "project", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 4)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Empty(t, msgs[1].Content)
	assert.Equal(t, 4, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_call"}}}, msgs[1].ContentLayout)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, 13, msgs[2].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
	assert.True(t, msgs[3].HasContextTokens)
	assert.Equal(t, 10, msgs[3].ContextTokens)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{}}, msgs[3].ContentLayout)
	assert.Equal(t, 1, sess.UserMessageCount)
}

func TestWarpNativeEventStoreBodies(t *testing.T) {
	path, seed, conn := newWarpTestDB(t)
	defer conn.Close()
	seed.AddConversation(t.Context(), "native", `{"conversation_usage_metadata":{"tool_usage_metadata":{"read_files_stats":{"count":1}}}}`, "2026-10-01 00:00:00")
	seed.AddExchange(t.Context(), "exchange-native", "native", "2026-10-01 00:00:00", `[{"Query":{"text":"[Thinking] is literal","context":[]}}]`, "/workspace/project", "completed", "fixture-model")
	results, err := parseWarpAll(path, "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	msgs := results[0].Messages
	require.Len(t, msgs, 2)
	assert.Equal(t, "exchange-native", msgs[0].SourceUUID)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.Empty(t, msgs[1].Content)
	assert.Equal(t, 6, msgs[1].ContentLength)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "[Read]", msgs[1].ToolCalls[0].Rendering)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_call"}}}, msgs[1].ContentLayout)
}

func TestOpenCodeReviewNativeEventStoreBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.jsonl")
	writeSourceFile(t, path, `{"type":"session_start","sessionId":"native"}
{"type":"llm_request","uuid":"user-native","filePath":"a.go","taskType":"main_task","messages":[{"role":"user","content":"[Thinking] is literal"}]}
{"type":"llm_response","uuid":"assistant-native","parentUuid":"user-native","filePath":"a.go","taskType":"main_task","content":"answer","reasoning_content":"plan","tool_calls":[{"id":"call-x","name":"read","arguments":"{}"}]}
{"type":"tool_call","uuid":"result-native","filePath":"a.go","taskType":"main_task","tool_name":"read","result":"output-needle","ok":true}
{"type":"tool_call","uuid":"orphan-native","filePath":"b.go","taskType":"main_task","tool_name":"read","result":"orphan-output","ok":false}
{"type":"llm_response","uuid":"usage-native","content":"","usage":{"prompt_tokens":10,"completion_tokens":2}}
`)
	result := openCodeReviewParseForTest(t, path)
	msgs := result.Messages
	require.Len(t, msgs, 4)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "answer", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 6, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 4}, {Kind: "text", End: 6}, {Kind: "tool_call"}}}, msgs[1].ContentLayout)
	require.Len(t, msgs[1].ToolCalls, 1)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "output-needle", msgs[1].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "orphan-native", msgs[2].SourceUUID)
	assert.True(t, msgs[2].IsSystem)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "orphan-output", msgs[2].ToolResultText)
	assert.Equal(t, 13, msgs[2].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{}}, msgs[3].ContentLayout)
	assert.Equal(t, 10, msgs[3].ContextTokens)
}
