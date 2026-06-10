package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestImportAdvancesLocalModifiedAt(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	_, err := ImportClaudeAI(
		ctx, d, strings.NewReader(testConversationsJSON), nil,
	)
	require.NoError(t, err)

	// local_modified_at must be non-NULL after import so incremental PG push
	// picks up session_name changes without relying on file_mtime.
	// In practice this is set by replaceSecretFindingsTx which is called
	// inside ReplaceSessionMessages on every message-replacing import.
	full, err := d.GetSessionFull(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.NotNil(t, full)
	require.NotNil(t, full.LocalModifiedAt,
		"local_modified_at must be set after import so PG push picks up session_name changes")
}

func TestImportSkipPathBumpsLocalModifiedAt(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// First import — establishes session_name and local_modified_at.
	_, err := ImportClaudeAI(ctx, d, strings.NewReader(testConversationsJSON), nil)
	require.NoError(t, err)

	full1, err := d.GetSessionFull(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.NotNil(t, full1.LocalModifiedAt)
	t1 := *full1.LocalModifiedAt

	// Ensure wall-clock advances so a bumped timestamp is detectably later.
	time.Sleep(2 * time.Millisecond)

	// Re-import with same messages but a different name. Message count and
	// ended_at are unchanged, so upsertConversation takes the skip path and
	// returns importSkipped without calling ReplaceSessionMessages.
	renamed := strings.ReplaceAll(testConversationsJSON, `"First Chat"`, `"Renamed Chat"`)
	_, err = ImportClaudeAI(ctx, d, strings.NewReader(renamed), nil)
	require.NoError(t, err)

	full2, err := d.GetSessionFull(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.NotNil(t, full2.LocalModifiedAt)
	t2 := *full2.LocalModifiedAt

	// local_modified_at must be bumped on the skip path so incremental PG
	// push picks up the session_name change.
	assert.True(t, t2 > t1,
		"local_modified_at must advance on skip-path reimport (t1=%s t2=%s)", t1, t2)

	// Confirm session_name was also updated.
	s, err := d.GetSession(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "Renamed Chat", *s.DisplayName)
}

func TestImportSetsDisplayName(t *testing.T) {
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
}

func TestImportChatGPT_UpdatesSessionNameOnReimport(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "conversations-000.json"),
		[]byte(testChatGPTConv), 0o644,
	))
	assetsDir := filepath.Join(t.TempDir(), "assets")

	// First import.
	_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)

	// testChatGPTConv has title "Test" and id "cg-1".
	s, err := d.GetSession(ctx, "chatgpt:cg-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "Test", *s.DisplayName)

	// Re-import with updated title.
	updated := strings.ReplaceAll(testChatGPTConv, `"title":"Test"`, `"title":"Renamed GPT Session"`)
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "conversations-000.json"),
		[]byte(updated), 0o644,
	))
	_, err = ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)

	// session_name must be updated even though messages are skipped.
	s, err = d.GetSession(ctx, "chatgpt:cg-1")
	require.NoError(t, err)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "Renamed GPT Session", *s.DisplayName,
		"session_name should be refreshed on ChatGPT re-import")
}

func TestImportChatGPT_ReimportPreservesExistingFields(t *testing.T) {
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

	// Capture original fields.
	orig, err := d.GetSession(ctx, "chatgpt:cg-1")
	require.NoError(t, err)
	require.NotNil(t, orig)
	origFirstMsg := orig.FirstMessage
	origStarted := orig.StartedAt
	origEnded := orig.EndedAt
	origMsgCount := orig.MessageCount

	// Re-import with only the title changed.
	renamed := strings.ReplaceAll(testChatGPTConv, `"title":"Test"`, `"title":"New Title"`)
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "conversations-000.json"),
		[]byte(renamed), 0o644,
	))
	_, err = ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)

	after, err := d.GetSession(ctx, "chatgpt:cg-1")
	require.NoError(t, err)
	require.NotNil(t, after)

	// session_name updated.
	require.NotNil(t, after.DisplayName)
	assert.Equal(t, "New Title", *after.DisplayName)

	// All other fields preserved.
	assert.Equal(t, origFirstMsg, after.FirstMessage, "first_message must not change")
	assert.Equal(t, origStarted, after.StartedAt, "started_at must not change")
	assert.Equal(t, origEnded, after.EndedAt, "ended_at must not change")
	assert.Equal(t, origMsgCount, after.MessageCount, "message_count must not change")
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
