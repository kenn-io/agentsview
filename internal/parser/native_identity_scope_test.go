package parser

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPositAssistantLocalIdentityIncludesConversation(t *testing.T) {
	root := t.TempDir()
	var messages []ParsedMessage
	for _, id := range []string{"first", "second"} {
		path := filepath.Join(root, "default", id, "conversation.json")
		writeSourceFile(t, path, `{"schemaVersion":"3","root":{"id":"`+id+`","timestamp":1790812800000},"messages":[{"id":"assistant-node","isActive":true,"lmMessageIds":[1]}]}`)
		writeSourceFile(t, filepath.Join(filepath.Dir(path), "lm-messages.jsonl"), `{"id":1,"message":{"role":"assistant","content":"answer","providerOptions":{"providerMetadata":{"positai":{"usage":{"inputTokens":10,"outputTokens":2}}}}}}`+"\n")
		_, msgs, _, err := parsePositAssistantConversation(positAssistantSource{Root: root, Path: path}, "local")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		messages = append(messages, msgs[0])
	}
	assert.Equal(t, "first:1", messages[0].SourceUUID)
	assert.Equal(t, "second:1", messages[1].SourceUUID)
	assert.NotEqual(t, messages[0].SourceUUID, messages[1].SourceUUID)
	assert.Equal(t, "answer", messages[0].Content)
	assert.Equal(t, "answer", messages[1].Content)
	assert.Equal(t, 10, messages[0].ContextTokens)
	assert.Equal(t, 2, messages[0].OutputTokens)
	assert.Equal(t, 10, messages[1].ContextTokens)
	assert.Equal(t, 2, messages[1].OutputTokens)
}

func TestGooseLocalIdentityIncludesConversation(t *testing.T) {
	var messages []ParsedMessage
	for _, id := range []string{"first", "second"} {
		fixture := newGooseTestFixture(t)
		fixture.insertSession(t, id, id, "user", "")
		fixture.insertMessage(t, id, "assistant", `[{"type":"text","text":"answer"}]`, 1700000000)
		provider, ok := NewProvider(AgentGoose, ProviderConfig{Roots: []string{fixture.pathRoot}})
		require.True(t, ok)
		sources, err := provider.Discover(t.Context())
		require.NoError(t, err)
		require.Len(t, sources, 1)
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		require.Len(t, outcome.Results[0].Result.Messages, 1)
		messages = append(messages, outcome.Results[0].Result.Messages[0])
	}
	assert.Equal(t, "first:1", messages[0].SourceUUID)
	assert.Equal(t, "second:1", messages[1].SourceUUID)
	assert.NotEqual(t, messages[0].SourceUUID, messages[1].SourceUUID)
}

func TestHermesLocalIdentityIncludesConversation(t *testing.T) {
	var messages []ParsedMessage
	for _, id := range []string{"first", "second"} {
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, "sessions"), 0o755))
		createHermesTestStateDB(t, root, []hermesTestSessionRow{{id: id, startedAt: 1788878363}}, []hermesTestMessageRow{{sessionID: id, role: "assistant", content: "answer", timestamp: 1788878364}})
		results, err := parseHermesTestArchive(t, root, "project", "local")
		require.NoError(t, err)
		require.Len(t, results, 1)
		require.Len(t, results[0].Messages, 1)
		messages = append(messages, results[0].Messages[0])
	}
	assert.Equal(t, "first:1", messages[0].SourceUUID)
	assert.Equal(t, "second:1", messages[1].SourceUUID)
	assert.NotEqual(t, messages[0].SourceUUID, messages[1].SourceUUID)
}

func TestPiebaldLocalIdentityIncludesConversation(t *testing.T) {
	var messages []ParsedMessage
	for _, id := range []string{"7", "8"} {
		path := newPiebaldTestDB(t)
		execPiebaldTestSQL(t, path, `INSERT INTO chats (id,title,created_at,updated_at,message_count) VALUES (`+id+`,'native','2026-10-01','2026-10-01',2)`)
		execPiebaldTestSQL(t, path, `INSERT INTO messages (id,parent_chat_id,role,created_at,updated_at,status) VALUES (70,`+id+`,'assistant','2026-10-01','2026-10-01','completed')`)
		execPiebaldTestSQL(t, path, `INSERT INTO messages (id,parent_chat_id,parent_message_id,role,created_at,updated_at,status,input_tokens,output_tokens) VALUES (71,`+id+`,70,'assistant','2026-10-01','2026-10-01','completed',10,2)`)
		seedPiebaldTextParts(t, path, piebaldTextPartSeed{partID: 700, msgID: 70, idx: 0, text: "answer"})
		_, msgs := parsePiebaldOneSession(t, path, id, "local")
		require.Len(t, msgs, 2)
		messages = append(messages, msgs[0], msgs[1])
	}
	assert.Equal(t, "7:70", messages[0].SourceUUID)
	assert.Equal(t, "7:70", messages[1].SourceParentUUID)
	assert.Equal(t, "8:70", messages[2].SourceUUID)
	assert.Equal(t, "8:70", messages[3].SourceParentUUID)
	assert.NotEqual(t, messages[0].SourceUUID, messages[2].SourceUUID)
}

func TestShelleyLocalIdentityIncludesConversation(t *testing.T) {
	var messages []ParsedMessage
	for _, id := range []string{"first", "second"} {
		_, path, conn := newShelleyTestDB(t)
		seedShelleyConversation(t, conn, id, id, "/workspace/project", "fixture-model", "", true, "2026-10-01T00:00:00Z", "2026-10-01T00:00:01Z")
		seedShelleyMessage(t, conn, id, 1, 1, "agent", `{"Content":[{"Type":2,"Text":"answer"}]}`, "", "", "2026-10-01T00:00:01Z")
		mustExec(t, conn, "UPDATE messages SET message_id='local-1'")
		info, err := os.Stat(path)
		require.NoError(t, err)
		result, err := parseShelleyConversationDirectForTest(t, path, id, "local", info)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Len(t, result.Messages, 1)
		messages = append(messages, result.Messages[0])
	}
	assert.Equal(t, "first:local-1", messages[0].SourceUUID)
	assert.Equal(t, "second:local-1", messages[1].SourceUUID)
	assert.NotEqual(t, messages[0].SourceUUID, messages[1].SourceUUID)
}

func TestHermesLocalIdentitySurvivesStateRawExport(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sessions"), 0o755))
	createHermesTestStateDB(t, root, []hermesTestSessionRow{{id: "first", startedAt: 1788878363}}, []hermesTestMessageRow{{sessionID: "first", role: "assistant", content: "answer", timestamp: 1788878364}})
	var raw bytes.Buffer
	require.NoError(t, writeHermesStateSessionJSONL(t.Context(), &raw, filepath.Join(root, "state.db"), "first"))
	path := filepath.Join(t.TempDir(), "exported.jsonl")
	writeSourceFile(t, path, raw.String())
	_, msgs, err := parseHermesTestSession(t, path, "project", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "first:1", msgs[0].SourceUUID)
	assert.Equal(t, "answer", msgs[0].Content)
}
