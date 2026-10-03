package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVibeNativeMemoryBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native", "messages.jsonl")
	writeSourceFile(t, path, `{"role":"user","content":"[Thinking] is literal","message_id":"user-native"}
{"role":"assistant","content":"answer","reasoning_content":"plan","message_id":"assistant-native","tool_calls":[{"id":"call-x","function":{"name":"read_file","arguments":{"path":"input-needle"}}}]}
{"role":"assistant","content":"","reasoning_content":""}
{"role":"assistant","content":"","reasoning_signature":"opaque-signature"}
{"role":"tool","content":"output-needle","tool_call_id":""}
`)
	result, err := parseVibeTestSession(t, path, FileInfo{Path: path})
	require.NoError(t, err)
	require.Len(t, result.Messages, 5)
	msgs := result.Messages
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "assistant-native", msgs[1].SourceUUID)
	assert.Equal(t, "answer", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 6, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 4}, {Kind: "text", End: 6}, {Kind: "tool_call"}}}, msgs[1].ContentLayout)
	for _, index := range []int{2, 3} {
		assert.True(t, msgs[index].HasThinking)
		assert.Empty(t, msgs[index].ThinkingText)
		assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[index].ContentLayout)
	}
	assert.Equal(t, "output-needle", msgs[4].ToolResultText)
	assert.Equal(t, 13, msgs[4].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[4].ContentLayout)
	assert.Equal(t, 1, result.Session.UserMessageCount)
}

func TestShelleyNativeMemoryBodies(t *testing.T) {
	_, path, conn := newShelleyTestDB(t)
	seedShelleyConversation(t, conn, "native", "native", "/workspace/project", "fixture-model", "", true, "2026-10-01T00:00:00Z", "2026-10-01T00:00:03Z")
	seedShelleyMessage(t, conn, "native", 1, 1, "user", `{"Content":[{"Type":2,"Text":"[Thinking] is literal"}]}`, "", "", "2026-10-01T00:00:00Z")
	seedShelleyMessage(t, conn, "native", 2, 1, "agent", `{"Content":[{"Type":2,"Text":"first"},{"Type":3,"Thinking":"plan"},{"Type":5,"ID":"call-x","ToolName":"read","ToolInput":{"path":"input-needle"}},{"Type":2,"Text":"second"}]}`, "", "", "2026-10-01T00:00:01Z")
	seedShelleyMessage(t, conn, "native", 3, 1, "agent", `{"Content":[{"Type":4}]}`, "", "", "2026-10-01T00:00:02Z")
	seedShelleyMessage(t, conn, "native", 4, 1, "tool", `{"Content":[{"Type":6,"ToolUseID":"","ToolResult":[{"Type":2,"Text":"output-needle"}]}]}`, "", "", "2026-10-01T00:00:03Z")
	seedShelleyMessage(t, conn, "native", 5, 1, "agent", `{"Content":[{"Type":3,"Thinking":""}]}`, "", "", "2026-10-01T00:00:04Z")
	seedShelleyMessage(t, conn, "native", 6, 1, "tool", `{"Content":[{"Type":9,"Title":"Title","URL":"https://example.com"}]}`, "", "", "2026-10-01T00:00:05Z")
	info, err := os.Stat(path)
	require.NoError(t, err)
	result, err := parseShelleyConversationDirectForTest(t, path, "native", "local", info)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Messages, 6)
	msgs := result.Messages
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "firstsecond", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 11, msgs[1].ContentLength)
	assert.Equal(t, "native:native-m2", msgs[1].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", Start: 5, End: 11}}}, msgs[1].ContentLayout)
	assert.True(t, msgs[2].HasThinking)
	assert.Empty(t, msgs[2].ThinkingText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[2].ContentLayout)
	assert.Equal(t, "output-needle", msgs[3].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[3].ContentLayout)
	assert.True(t, msgs[4].HasThinking)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[4].ContentLayout)
	assert.Empty(t, msgs[5].Content)
	assert.Equal(t, "Title https://example.com", msgs[5].ToolResultText)
	assert.Equal(t, 25, msgs[5].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 25}}}, msgs[5].ContentLayout)
}

func TestPiebaldNativeMemoryBodies(t *testing.T) {
	path := newPiebaldTestDB(t)
	execPiebaldTestSQL(t, path, `INSERT INTO chats (id,title,created_at,updated_at,message_count) VALUES (7,'native','2026-10-01','2026-10-01',2)`)
	execPiebaldTestSQL(t, path, `INSERT INTO messages (id,parent_chat_id,role,created_at,updated_at,status) VALUES (70,7,'assistant','2026-10-01','2026-10-01','completed')`)
	execPiebaldTestSQL(t, path, `INSERT INTO messages (id,parent_chat_id,parent_message_id,role,created_at,updated_at,status,input_tokens,output_tokens) VALUES (71,7,70,'assistant','2026-10-01','2026-10-01','completed',10,2)`)
	seedPiebaldTextParts(t, path, piebaldTextPartSeed{partID: 700, msgID: 70, idx: 0, text: "first"}, piebaldTextPartSeed{partID: 701, msgID: 70, idx: 1, text: "plan", thinking: true}, piebaldTextPartSeed{partID: 703, msgID: 70, idx: 3, text: "second"}, piebaldTextPartSeed{partID: 704, msgID: 70, idx: 4, text: "", thinking: true})
	seedPiebaldToolPart(t, path, 702, 70, 2)
	_, msgs := parsePiebaldOneSession(t, path, "7", "local")
	require.Len(t, msgs, 2)
	assert.Equal(t, "first\nsecond", msgs[0].Content)
	assert.Equal(t, "plan", msgs[0].ThinkingText)
	assert.Equal(t, 16, msgs[0].ContentLength)
	assert.Equal(t, "7:70", msgs[0].SourceUUID)
	assert.Equal(t, "file contents", msgs[0].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "tool_result", End: 13}, {Kind: "text", Start: 6, End: 12}, {Kind: "thinking", Start: 4, End: 4}}}, msgs[0].ContentLayout)
	assert.Equal(t, 10, msgs[1].ContextTokens)
	assert.Equal(t, 2, msgs[1].OutputTokens)
	assert.Equal(t, "7:70", msgs[1].SourceParentUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{}}, msgs[1].ContentLayout)
}

func TestQwenPawNativeMemoryBodies(t *testing.T) {
	path := writeQwenPawSession(t, "default", "native", []string{
		`{"id":"user-native","role":"user","content":[{"type":"text","text":"[Thinking] is literal"}]}`,
		`{"id":"assistant-native","role":"assistant","content":[{"type":"text","text":"first"},{"type":"thinking","thinking":""},{"type":"tool_use","id":"call-x","name":"read","input":{"path":"input-needle"}},{"type":"text","text":"second"}]}`,
		`{"id":"output-native","role":"assistant","content":[{"type":"tool_result","id":"","output":"output-needle"}]}`,
	})
	_, msgs, err := parseQwenPawTestSession(t, path, "default", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.True(t, msgs[1].HasThinking)
	assert.Equal(t, "assistant-native", msgs[1].SourceUUID)
	assert.Equal(t, "first\nsecond", msgs[1].Content)
	assert.Equal(t, 12, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 5}, {Kind: "thinking"}, {Kind: "tool_call"}, {Kind: "text", Start: 6, End: 12}}}, msgs[1].ContentLayout)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
}

func TestPositAssistantNativeMemoryBodies(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "default", "native", "conversation.json")
	writeSourceFile(t, path, `{"schemaVersion":"3","root":{"id":"native","timestamp":1790812800000},"messages":[{"id":"node-user","isActive":true,"lmMessageIds":[0]},{"id":"node-assistant","isActive":true,"lmMessageIds":[1]},{"id":"node-output","isActive":true,"lmMessageIds":[2]},{"id":"node-empty","isActive":true,"lmMessageIds":[3]}]}`)
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "lm-messages.jsonl"), `{"id":0,"message":{"role":"user","content":"[Thinking] is literal"}}
{"id":1,"message":{"role":"assistant","content":[{"type":"text","text":"first<MESSAGESUMMARY>hidden</MESSAGESUMMARY>"},{"type":"reasoning","text":"plan"},{"type":"tool-call","toolCallId":"call-x","toolName":"read","input":{"path":"input-needle"}},{"type":"text","text":"second"}]}}
{"id":2,"message":{"role":"tool","content":[{"type":"tool-result","toolCallId":"","output":{"type":"text","value":"output-needle"}}]}}
{"id":3,"message":{"role":"assistant","content":[{"type":"reasoning","text":""}]}}
`)
	_, msgs, _, err := parsePositAssistantConversation(positAssistantSource{Root: root, Path: path}, "local")
	require.NoError(t, err)
	require.Len(t, msgs, 4)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "first\nsecond", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 12, msgs[1].ContentLength)
	assert.Equal(t, "native:1", msgs[1].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", Start: 6, End: 12}}}, msgs[1].ContentLayout)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
	assert.True(t, msgs[3].HasThinking)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[3].ContentLayout)
}

func TestZedNativeReasoningWhitespace(t *testing.T) {
	path := createZedThreadsDB(t, []zedTestThread{{id: "native-whitespace", dataType: "json", data: []byte(`{"messages":[{"Agent":{"content":[{"Thinking":{"text":" \nplan"}},{"Text":"answer"},{"Thinking":{"text":"next \n"}}]}}]}`)}})
	results, err := parseZedAll(path, "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	msg := results[0].Messages[0]
	assert.Equal(t, "plan\nnext", msg.ThinkingText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 4}, {Kind: "text", End: 6}, {Kind: "thinking", Start: 5, End: 9}}}, msg.ContentLayout)
}

func TestShelleyNativeUsageOnlyBody(t *testing.T) {
	_, path, conn := newShelleyTestDB(t)
	seedShelleyConversation(t, conn, "usage-native", "usage", "/workspace/project", "fixture-model", "", true, "2026-10-01T00:00:00Z", "2026-10-01T00:00:01Z")
	seedShelleyMessage(t, conn, "usage-native", 1, 1, "error", "", "", `{"input_tokens":10,"output_tokens":2}`, "2026-10-01T00:00:01Z")
	info, err := os.Stat(path)
	require.NoError(t, err)
	result, err := parseShelleyConversationDirectForTest(t, path, "usage-native", "local", info)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Messages, 1)
	msg := result.Messages[0]
	assert.True(t, msg.IsSystem)
	assert.Equal(t, 10, msg.ContextTokens)
	assert.Equal(t, 2, msg.OutputTokens)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{}}, msg.ContentLayout)
}

func TestPiebaldNativeReasoningWhitespace(t *testing.T) {
	path := newPiebaldTestDB(t)
	execPiebaldTestSQL(t, path, `INSERT INTO chats (id,title,created_at,updated_at,message_count) VALUES (7,'native','2026-10-01','2026-10-01',1)`)
	execPiebaldTestSQL(t, path, `INSERT INTO messages (id,parent_chat_id,role,created_at,updated_at,status) VALUES (70,7,'assistant','2026-10-01','2026-10-01','completed')`)
	seedPiebaldTextParts(t, path, piebaldTextPartSeed{partID: 700, msgID: 70, idx: 0, text: "plan", thinking: true}, piebaldTextPartSeed{partID: 701, msgID: 70, idx: 1, text: " \n ", thinking: true}, piebaldTextPartSeed{partID: 702, msgID: 70, idx: 2, text: "answer"}, piebaldTextPartSeed{partID: 703, msgID: 70, idx: 3, text: "next", thinking: true})
	_, msgs := parsePiebaldOneSession(t, path, "7", "local")
	require.Len(t, msgs, 1)
	assert.Equal(t, "plan\nnext", msgs[0].ThinkingText)
	assert.Equal(t, 15, msgs[0].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 4}, {Kind: "thinking", Start: 4, End: 4}, {Kind: "text", End: 6}, {Kind: "thinking", Start: 5, End: 9}}}, msgs[0].ContentLayout)
}
