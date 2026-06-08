package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

const testConversationsJSON = `[
  {
    "uuid": "import-test-001",
    "name": "First Chat",
    "summary": "",
    "created_at": "2026-02-01T09:00:00.000000Z",
    "updated_at": "2026-02-01T09:15:00.000000Z",
    "account": {"uuid": "acct-1"},
    "chat_messages": [
      {
        "uuid": "m1",
        "text": "Hello",
        "content": [{"type":"text","text":"Hello"}],
        "sender": "human",
        "created_at": "2026-02-01T09:00:00.000000Z",
        "updated_at": "2026-02-01T09:00:00.000000Z",
        "attachments": [],
        "files": []
      },
      {
        "uuid": "m2",
        "text": "Hi there!",
        "content": [{"type":"text","text":"Hi there!"}],
        "sender": "assistant",
        "created_at": "2026-02-01T09:00:05.000000Z",
        "updated_at": "2026-02-01T09:00:05.000000Z",
        "attachments": [],
        "files": []
      }
    ]
  }
]`

func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/test.db")
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	return d
}

func TestImportClaudeAI(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	stats, err := ImportClaudeAI(
		ctx, d, strings.NewReader(testConversationsJSON), nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	assert.Equal(t, 0, stats.Updated)

	s, err := d.GetSession(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "claude.ai", s.Project)
	assert.Equal(t, "claude-ai", s.Agent)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "First Chat", *s.DisplayName)

	msgs, err := d.GetAllMessages(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	assert.Len(t, msgs, 2)
}

func TestImportClaudeAI_ReimportSkipsUnchanged(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	_, err := ImportClaudeAI(
		ctx, d, strings.NewReader(testConversationsJSON), nil,
	)
	require.NoError(t, err)

	// Re-importing the same file skips conversations whose
	// message count has not changed.
	stats, err := ImportClaudeAI(
		ctx, d, strings.NewReader(testConversationsJSON), nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.Imported)
	assert.Equal(t, 0, stats.Updated)
	assert.Equal(t, 1, stats.Skipped)

	// Messages are still intact.
	msgs, err := d.GetAllMessages(
		ctx, "claude-ai:import-test-001",
	)
	require.NoError(t, err)
	assert.Len(t, msgs, 2)
}

func TestImportClaudeAI_PreservesDisplayNameOnReimport(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	_, err := ImportClaudeAI(
		ctx, d, strings.NewReader(testConversationsJSON), nil,
	)
	require.NoError(t, err)

	newName := "My Custom Name"
	err = d.RenameSession(
		"claude-ai:import-test-001", &newName,
	)
	require.NoError(t, err)

	_, err = ImportClaudeAI(
		ctx, d, strings.NewReader(testConversationsJSON), nil,
	)
	require.NoError(t, err)

	s, err := d.GetSession(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.NotNil(t, s)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "My Custom Name", *s.DisplayName)
}

const testChatGPTConv = `[{
  "id":"cg-1","conversation_id":"cg-1","title":"Test",
  "create_time":1706745600.0,"update_time":1706745660.0,
  "current_node":"n1","mapping":{
    "r":{"id":"r","parent":null,"children":["n1"],
         "message":null},
    "n1":{"id":"n1","parent":"r","children":[],"message":{
      "id":"m1","create_time":1706745600.0,
      "author":{"role":"user","name":null,"metadata":{}},
      "content":{"content_type":"text","parts":["Hello"]},
      "status":"finished_successfully","metadata":{}}}
  }
}]`

func TestImportChatGPT(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "conversations-000.json"),
		[]byte(testChatGPTConv), 0o644,
	))
	assetsDir := filepath.Join(t.TempDir(), "assets")

	stats, err := ImportChatGPT(
		ctx, d, dir, assetsDir, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	assert.Equal(t, 0, stats.Skipped)

	s, err := d.GetSession(ctx, "chatgpt:cg-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "chatgpt.com", s.Project)
}

func TestImportSetsAgentNameSource(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	stats, err := ImportClaudeAI(
		ctx, d, strings.NewReader(testConversationsJSON), nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)

	s, err := d.GetSession(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.NotNil(t, s)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "First Chat", *s.DisplayName)
	require.NotNil(t, s.NameSource)
	assert.Equal(t, "agent", *s.NameSource)
}

func TestImportChatGPT_SkipsExisting(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "conversations-000.json"),
		[]byte(testChatGPTConv), 0o644,
	))
	assetsDir := filepath.Join(t.TempDir(), "assets")

	_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)

	stats, err := ImportChatGPT(
		ctx, d, dir, assetsDir, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.Imported)
	assert.Equal(t, 1, stats.Skipped)
}
