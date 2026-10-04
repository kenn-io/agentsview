package importer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
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

const testConversationsWithAttachmentJSON = `[{
  "uuid": "import-test-002",
  "name": "Attachment Chat",
  "summary": "",
  "created_at": "2026-02-02T09:00:00.000000Z",
  "updated_at": "2026-02-02T09:15:00.000000Z",
  "account": {"uuid":"acct-1"},
  "chat_messages": [
    {
      "uuid":"m1",
      "text":"Can you show me the config?",
      "content":[{"type":"text","text":"Can you show me the config?"}],
      "sender":"human",
      "created_at":"2026-02-02T09:00:00.000000Z",
      "updated_at":"2026-02-02T09:00:00.000000Z",
      "attachments":[],
      "files":[]
    },
    {
      "uuid":"m2",
      "text":"Sure, here it is.",
      "content":[{"type":"text","text":"Sure, here it is."}],
      "sender":"assistant",
      "created_at":"2026-02-02T09:00:05.000000Z",
      "updated_at":"2026-02-02T09:00:05.000000Z",
      "attachments":[
        {
          "file_name":"agent.yaml",
          "extracted_content":"model: claude-3.7\nmode: debug"
        }
      ],
      "files":[]
    }
  ]
}]`

const testConversationsWithoutAttachmentJSON = `[{
  "uuid": "import-test-002",
  "name": "Attachment Chat",
  "summary": "",
  "created_at": "2026-02-02T09:00:00.000000Z",
  "updated_at": "2026-02-02T09:15:00.000000Z",
  "account": {"uuid":"acct-1"},
  "chat_messages": [
    {
      "uuid":"m1",
      "text":"Can you show me the config?",
      "content":[{"type":"text","text":"Can you show me the config?"}],
      "sender":"human",
      "created_at":"2026-02-02T09:00:00.000000Z",
      "updated_at":"2026-02-02T09:00:00.000000Z",
      "attachments":[],
      "files":[]
    },
    {
      "uuid":"m2",
      "text":"Sure, here it is.",
      "content":[{"type":"text","text":"Sure, here it is."}],
      "sender":"assistant",
      "created_at":"2026-02-02T09:00:05.000000Z",
      "updated_at":"2026-02-02T09:00:05.000000Z",
      "attachments":[],
      "files":[]
    }
  ]
}]`

func testDB(t *testing.T) *db.DB {
	t.Helper()
	return dbtest.OpenTestDB(t)
}

func TestImportClaudeAI(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	stats, err := ImportClaudeAI(
		ctx, d, strings.NewReader(testConversationsJSON), nil, "workstation",
	)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	assert.Equal(t, 0, stats.Updated)

	s, err := d.GetSession(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "claude.ai", s.Project)
	assert.Equal(t, "claude-ai", s.Agent)
	assert.Equal(t, "workstation", s.Machine)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "First Chat", *s.DisplayName)

	msgs, err := d.GetAllMessages(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	assert.Len(t, msgs, 2)
}

func TestImportClaudeAIIncludesAttachmentContent(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	stats, err := ImportClaudeAI(
		ctx, d, strings.NewReader(testConversationsWithAttachmentJSON), nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	assert.Equal(t, 0, stats.Updated)

	msgs, err := d.GetAllMessages(
		ctx, "claude-ai:import-test-002",
	)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Can you show me the config?", msgs[0].Content)
	assert.Equal(t,
		"Sure, here it is.\n\n[Attachment: agent.yaml]\nmodel: claude-3.7\nmode: debug",
		msgs[1].Content,
	)
}

func TestImportClaudeAIReimportRefreshesAttachmentContent(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	stats, err := ImportClaudeAI(
		ctx, d, strings.NewReader(testConversationsWithoutAttachmentJSON), nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)

	msgs, err := d.GetAllMessages(
		ctx, "claude-ai:import-test-002",
	)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Sure, here it is.", msgs[1].Content)

	stats, err = ImportClaudeAI(
		ctx, d, strings.NewReader(testConversationsWithAttachmentJSON), nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.Imported)
	assert.Equal(t, 1, stats.Updated)
	assert.Equal(t, 0, stats.Skipped)

	msgs, err = d.GetAllMessages(
		ctx, "claude-ai:import-test-002",
	)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t,
		"Sure, here it is.\n\n[Attachment: agent.yaml]\nmodel: claude-3.7\nmode: debug",
		msgs[1].Content,
	)
}

func TestImportClaudeAI_ReimportSkipsUnchanged(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

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
	ctx := t.Context()

	_, err := ImportClaudeAI(
		ctx, d, strings.NewReader(testConversationsJSON), nil,
	)
	require.NoError(t, err)

	newName := "My Custom Name"
	err = d.RenameSession(ctx,
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
	ctx := t.Context()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "conversations-000.json"),
		[]byte(testChatGPTConv), 0o644,
	))
	assetsDir := filepath.Join(t.TempDir(), "assets")

	stats, err := ImportChatGPT(
		ctx, d, dir, assetsDir, nil, "workstation",
	)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	assert.Equal(t, 0, stats.Skipped)

	s, err := d.GetSession(ctx, "chatgpt:cg-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "chatgpt.com", s.Project)
	assert.Equal(t, "workstation", s.Machine)
}

func TestImportChatGPTSanitizesParserRows(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	longModel := strings.Repeat("m", db.MaxModelLen+16)
	conv := `[{
  "id":"cg-sanitize","conversation_id":"cg-sanitize",
  "title":"Sanitize","create_time":7258118400.0,
  "update_time":7258118460.0,"current_node":"n2",
  "mapping":{
    "r":{"id":"r","parent":null,"children":["n1"],"message":null},
    "n1":{"id":"n1","parent":"r","children":["n2"],"message":{
      "id":"m1","create_time":7258118400.0,
      "author":{"role":"user","name":null,"metadata":{}},
      "content":{"content_type":"text","parts":["Hello\u0000\u001b[31m"]},
      "status":"finished_successfully","metadata":{}}},
    "n2":{"id":"n2","parent":"n1","children":[],"message":{
      "id":"m2","create_time":7258118401.0,
      "author":{"role":"assistant","name":null,"metadata":{}},
      "content":{"content_type":"text","parts":["Hi"]},
      "status":"finished_successfully",
      "metadata":{"model_slug":"` + longModel + `"}}}
  }
}]`

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "conversations-000.json"),
		[]byte(conv), 0o644,
	))
	assetsDir := filepath.Join(t.TempDir(), "assets")

	stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)

	s, err := d.GetSession(ctx, "chatgpt:cg-sanitize")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Nil(t, s.StartedAt)
	assert.Nil(t, s.EndedAt)

	msgs, err := d.GetAllMessages(ctx, "chatgpt:cg-sanitize")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Hello[31m", msgs[0].Content)
	assert.Empty(t, msgs[0].Timestamp)
	assert.Empty(t, msgs[1].Timestamp)
	assert.Len(t, msgs[1].Model, db.MaxModelLen)
}

func TestUpsertConversationPreservesSessionIdentity(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	status, err := upsertConversation(
		ctx,
		d,
		parser.ParseResult{
			Session: parser.ParsedSession{
				ID:               "import-identity-001",
				Project:          "claude.ai",
				Machine:          "workstation",
				Agent:            parser.AgentClaude,
				AgentLabel:       " Claude Code ",
				Entrypoint:       " claude-sdk ",
				FirstMessage:     "hello",
				StartedAt:        time.Unix(1706745600, 0).UTC(),
				EndedAt:          time.Unix(1706745660, 0).UTC(),
				MessageCount:     1,
				UserMessageCount: 1,
			},
			Messages: []parser.ParsedMessage{
				{
					Ordinal:       0,
					Role:          parser.RoleUser,
					Content:       "hello",
					Timestamp:     time.Unix(1706745600, 0).UTC(),
					ContentLength: len("hello"),
				},
			},
		},
		newLazyFTS(ctx, d, nil),
	)
	require.NoError(t, err)
	assert.Equal(t, importNew, status)

	s, err := d.GetSession(ctx, "import-identity-001")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "claude", s.Agent)
	assert.Equal(t, " Claude Code ", s.AgentLabel)
	assert.Equal(t, " claude-sdk ", s.Entrypoint)
}

func TestImportAdvancesLocalModifiedAt(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

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
	ctx := t.Context()

	// First import — establishes session_name and local_modified_at.
	_, err := ImportClaudeAI(ctx, d, strings.NewReader(testConversationsJSON), nil)
	require.NoError(t, err)

	// Backdate the fixture so the skip-path bump is detectably later without
	// depending on wall-clock scheduling.
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE sessions SET local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-1 second') "+
				"WHERE id = 'claude-ai:import-test-001'")
		return err
	}))

	full1, err := d.GetSessionFull(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.NotNil(t, full1.LocalModifiedAt)
	t1 := *full1.LocalModifiedAt

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
	assert.Greater(t, t2, t1,
		"local_modified_at must advance on skip-path reimport (t1=%s t2=%s)", t1, t2)

	// Confirm session_name was also updated.
	s, err := d.GetSession(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "Renamed Chat", *s.DisplayName)
}

func TestImportSetsDisplayName(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

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
	ctx := t.Context()

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

func TestImportChatGPTSanitizesSessionNameOnReimport(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "conversations-000.json"),
		[]byte(testChatGPTConv), 0o644,
	))
	assetsDir := filepath.Join(t.TempDir(), "assets")

	_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)

	dirty := strings.ReplaceAll(
		testChatGPTConv,
		`"title":"Test"`,
		`"title":"Renamed\u0000\u001b[31m"`,
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "conversations-000.json"),
		[]byte(dirty), 0o644,
	))

	_, err = ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)

	s, err := d.GetSession(ctx, "chatgpt:cg-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "Renamed[31m", *s.DisplayName)
}

func TestImportChatGPT_ReimportPreservesExistingFields(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

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

func TestImportChatGPT_ReimportSkipsUnchanged(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

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

func TestImportChatGPTAppendsVerifiedHistory(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations-000.json")
	assetsDir := filepath.Join(t.TempDir(), "assets")
	require.NoError(t, os.WriteFile(path, []byte(testChatGPTConv), 0o644))
	_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)

	name := "My saved title"
	require.NoError(t, d.RenameSession(ctx, "chatgpt:cg-1", &name))
	before, err := d.GetAllMessages(ctx, "chatgpt:cg-1")
	require.NoError(t, err)
	require.Len(t, before, 1)
	_, err = d.PinMessage(ctx, "chatgpt:cg-1", before[0].ID, nil)
	require.NoError(t, err)

	appended := testChatGPTConvWithAppend()
	require.NoError(t, os.WriteFile(path, []byte(appended), 0o644))

	stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Updated)
	assert.Zero(t, stats.Errors)
	after, err := d.GetAllMessages(ctx, "chatgpt:cg-1")
	require.NoError(t, err)
	require.Len(t, after, 2)
	assert.Equal(t, before[0].ID, after[0].ID)
	assert.Equal(t, before[0].Content, after[0].Content)

	session, err := d.GetSession(ctx, "chatgpt:cg-1")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.DisplayName)
	assert.Equal(t, name, *session.DisplayName)

	pins, err := d.ListPinnedMessages(ctx, "chatgpt:cg-1", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, before[0].ID, pins[0].MessageID)

	results, err := d.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "newly appended answer", Sources: []string{"messages"},
		IncludeOneShot: true, IncludeAutomated: true, Limit: 10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, results.Matches)
	assert.Equal(t, "chatgpt:cg-1", results.Matches[0].SessionID)

	stats, err = ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	assert.Zero(t, stats.Updated)
}

func TestImportChatGPTRejectsShorterOrDivergentHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial string
		data    string
		reason  RefusalReason
	}{
		{name: "shorter", initial: testChatGPTConvWithAppend(), data: testChatGPTConv, reason: RefusalShorterExport},
		{name: "divergent", initial: testChatGPTConvWithAppend(), data: strings.Replace(testChatGPTConvWithAppend(), `"Hello"`, `"Changed archived message"`, 1), reason: RefusalDiverged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			dir := t.TempDir()
			path := filepath.Join(dir, "conversations-000.json")
			assetsDir := filepath.Join(t.TempDir(), "assets")
			require.NoError(t, os.WriteFile(path, []byte(tc.initial), 0o644))
			_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			before, err := d.GetAllMessages(ctx, "chatgpt:cg-1")
			require.NoError(t, err)

			require.NoError(t, os.WriteFile(path, []byte(tc.data), 0o644))
			stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Errors)
			assert.Zero(t, stats.Updated)
			assert.Equal(t, []ImportRefusal{{SessionID: "chatgpt:cg-1", Reason: tc.reason}}, stats.Refusals)
			after, err := d.GetAllMessages(ctx, "chatgpt:cg-1")
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func testChatGPTConvWithAppend() string {
	var conversations []map[string]any
	if err := json.Unmarshal([]byte(testChatGPTConv), &conversations); err != nil {
		panic(err)
	}
	conversation := conversations[0]
	conversation["current_node"] = "n2"
	mapping := conversation["mapping"].(map[string]any)
	mapping["n1"].(map[string]any)["children"] = []any{"n2"}
	mapping["n2"] = map[string]any{
		"id": "n2", "parent": "n1", "children": []any{},
		"message": map[string]any{
			"id": "m2", "create_time": 1706745660.0,
			"author":  map[string]any{"role": "assistant", "name": nil, "metadata": map[string]any{}},
			"content": map[string]any{"content_type": "text", "parts": []any{"A newly appended answer with searchable phrase"}},
			"status":  "finished_successfully", "metadata": map[string]any{},
		},
	}
	encoded, err := json.Marshal(conversations)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func TestImportChatGPTReimportComparesStoredForm(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations-000.json")
	assetsDir := filepath.Join(t.TempDir(), "assets")
	// The control rune is stripped at the storage boundary, so the raw
	// parsed message differs from the archived row.
	data := strings.Replace(testChatGPTConv, `"Hello"`, `"Hel\u0001lo"`, 1)
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))
	_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)

	stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	assert.Zero(t, stats.Errors)
	assert.Equal(t, 1, stats.Skipped)
}

func TestImportChatGPTAppendInvalidatesSignals(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations-000.json")
	assetsDir := filepath.Join(t.TempDir(), "assets")
	require.NoError(t, os.WriteFile(path, []byte(testChatGPTConv), 0o644))
	_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	score := 42
	require.NoError(t, d.UpdateSessionSignals(ctx, "chatgpt:cg-1", db.SessionSignalUpdate{
		HealthScore:    &score,
		QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion},
	}))

	require.NoError(t, os.WriteFile(path, []byte(testChatGPTConvWithAppend()), 0o644))
	stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Updated)

	session, err := d.GetSession(ctx, "chatgpt:cg-1")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Zero(t, session.QualitySignalVersion)
	assert.Nil(t, session.HealthScore)
}

func TestImportChatGPTUsageArchiveSkipsAppend(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations-000.json")
	assetsDir := filepath.Join(t.TempDir(), "assets")
	require.NoError(t, os.WriteFile(path, []byte(testChatGPTConv), 0o644))
	_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	d.SetArchiveContent(config.ArchiveContentUsage)
	before, err := d.GetAllMessages(ctx, "chatgpt:cg-1")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(path, []byte(testChatGPTConvWithAppend()), 0o644))
	stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	assert.Zero(t, stats.Errors)
	assert.Zero(t, stats.Updated)
	assert.Equal(t, 1, stats.Skipped)
	after, err := d.GetAllMessages(ctx, "chatgpt:cg-1")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// claudeAIConversationWithMessages returns testConversationsJSON trimmed or
// extended to n messages; extra turns get unique text and timestamps.
func claudeAIConversationWithMessages(t *testing.T, n int) string {
	t.Helper()
	var conversations []map[string]any
	require.NoError(t, json.Unmarshal([]byte(testConversationsJSON), &conversations))
	msgs := conversations[0]["chat_messages"].([]any)
	for i := len(msgs); i < n; i++ {
		text := fmt.Sprintf("Appended turn %d", i)
		sender := "human"
		if i%2 == 1 {
			sender = "assistant"
		}
		ts := fmt.Sprintf("2026-02-01T09:0%d:00.000000Z", i)
		msgs = append(msgs, map[string]any{
			"uuid": fmt.Sprintf("m%d", i+1), "text": text,
			"content": []any{map[string]any{"type": "text", "text": text}},
			"sender":  sender, "created_at": ts, "updated_at": ts,
			"attachments": []any{}, "files": []any{},
		})
	}
	conversations[0]["chat_messages"] = msgs[:n]
	encoded, err := json.Marshal(conversations)
	require.NoError(t, err)
	return string(encoded)
}

func TestImportClaudeAIRejectsShorterExport(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	_, err := ImportClaudeAI(ctx, d, strings.NewReader(testConversationsJSON), nil)
	require.NoError(t, err)
	before, err := d.GetAllMessages(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.Len(t, before, 2)

	stats, err := ImportClaudeAI(ctx, d,
		strings.NewReader(claudeAIConversationWithMessages(t, 1)), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Errors)
	assert.Zero(t, stats.Updated)
	assert.Equal(t, []ImportRefusal{{SessionID: "claude-ai:import-test-001", Reason: RefusalShorterExport}}, stats.Refusals)

	after, err := d.GetAllMessages(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	s, err := d.GetSession(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, 2, s.MessageCount)
}

func TestImportClaudeAIAppendKeepsArchivedRows(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	_, err := ImportClaudeAI(ctx, d, strings.NewReader(testConversationsJSON), nil)
	require.NoError(t, err)
	before, err := d.GetAllMessages(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	_, err = d.PinMessage(ctx, "claude-ai:import-test-001", before[1].ID, nil)
	require.NoError(t, err)

	stats, err := ImportClaudeAI(ctx, d,
		strings.NewReader(claudeAIConversationWithMessages(t, 4)), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Updated)
	assert.Zero(t, stats.Errors)

	after, err := d.GetAllMessages(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.Len(t, after, 4)
	assert.Equal(t, before[0].ID, after[0].ID)
	assert.Equal(t, before[1].ID, after[1].ID)
	pins, err := d.ListPinnedMessages(ctx, "claude-ai:import-test-001", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, before[1].ID, pins[0].MessageID)
}

func TestImportClaudeAIReimportComparesStoredForm(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	// The control rune is stripped at the storage boundary, so the raw
	// parsed message differs from the archived row.
	data := strings.Replace(testConversationsJSON,
		`"text": "Hello",`, `"text": "Hel\u0001lo",`, 1)
	data = strings.Replace(data,
		`{"type":"text","text":"Hello"}`, `{"type":"text","text":"Hel\u0001lo"}`, 1)
	_, err := ImportClaudeAI(ctx, d, strings.NewReader(data), nil)
	require.NoError(t, err)

	stats, err := ImportClaudeAI(ctx, d, strings.NewReader(data), nil)
	require.NoError(t, err)
	assert.Zero(t, stats.Errors)
	assert.Zero(t, stats.Updated)
	assert.Equal(t, 1, stats.Skipped)
}

// chatGPTToolNode renders one ChatGPT mapping node. Non-text content types
// carry their text in the "text" field, as code and execution output do.
func chatGPTToolNode(id, parent, child, role, contentType, text string, ts float64) string {
	children := "[]"
	if child != "" {
		children = `["` + child + `"]`
	}
	content := `{"content_type":"text","parts":["` + text + `"]}`
	if contentType != "text" {
		content = `{"content_type":"` + contentType + `","text":"` + text + `"}`
	}
	return `"` + id + `":{"id":"` + id + `","parent":"` + parent +
		`","children":` + children + `,"message":{"id":"m-` + id +
		`","create_time":` + fmt.Sprint(ts) + `,"author":{"role":"` + role +
		`"},"content":` + content +
		`,"status":"finished_successfully","metadata":{}}}`
}

type chatGPTNodeSpec struct{ role, contentType, text string }

// chatGPTChainConv renders one linear conversation "cg-tool" from nodes.
func chatGPTChainConv(chain ...chatGPTNodeSpec) string {
	nodes := make([]string, len(chain))
	parent := "r"
	for i, n := range chain {
		id := fmt.Sprintf("n%d", i+1)
		child := ""
		if i+1 < len(chain) {
			child = fmt.Sprintf("n%d", i+2)
		}
		nodes[i] = chatGPTToolNode(id, parent, child, n.role, n.contentType, n.text, float64(1706745600+10*i))
		parent = id
	}
	return `[{"id":"cg-tool","conversation_id":"cg-tool","title":"Tool",` +
		`"create_time":1706745600.0,"update_time":1706745710.0,` +
		`"current_node":"` + parent + `","mapping":{` +
		`"r":{"id":"r","parent":null,"children":["n1"],"message":null},` +
		strings.Join(nodes, ",") + `}}]`
}

// chatGPTToolRunConv is a user turn and an assistant turn whose code run
// has output only when output is non-empty. later adds a user turn and an
// assistant reply after the run.
func chatGPTToolRunConv(output string, later bool) string {
	chain := []chatGPTNodeSpec{
		{"user", "text", "Run print(42)"},
		{"assistant", "text", "Running it."},
		{"tool", "code", "print(42)"},
	}
	if output != "" {
		chain = append(chain, chatGPTNodeSpec{"tool", "execution_output", output})
	}
	if later {
		chain = append(chain,
			chatGPTNodeSpec{"user", "text", "Now print 43"},
			chatGPTNodeSpec{"assistant", "text", "Tuesday answer printed 43"},
		)
	}
	return chatGPTChainConv(chain...)
}

// chatGPTTwoToolRunsConv is two code runs on separate assistant turns; each
// has output only when its output is non-empty.
func chatGPTTwoToolRunsConv(firstOutput, secondOutput string) string {
	chain := []chatGPTNodeSpec{
		{"user", "text", "Run print(41)"},
		{"assistant", "text", "Running it."},
		{"tool", "code", "print(41)"},
	}
	if firstOutput != "" {
		chain = append(chain, chatGPTNodeSpec{"tool", "execution_output", firstOutput})
	}
	chain = append(chain,
		chatGPTNodeSpec{"user", "text", "Run print(42)"},
		chatGPTNodeSpec{"assistant", "text", "Running again."},
		chatGPTNodeSpec{"tool", "code", "print(42)"},
	)
	if secondOutput != "" {
		chain = append(chain, chatGPTNodeSpec{"tool", "execution_output", secondOutput})
	}
	return chatGPTChainConv(chain...)
}

// chatGPTImageOutput is execution output holding an inline image, escaped
// for a JSON string. Blank lines delimit the JSON image section inside the
// code fence the ChatGPT parser adds.
func chatGPTImageOutput(t *testing.T) string {
	t.Helper()
	encoded, err := json.Marshal("\n\n" + `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]` + "\n\n")
	require.NoError(t, err)
	return strings.Trim(string(encoded), `"`)
}

func chatGPTTranscriptRevision(t *testing.T, d *db.DB) int {
	t.Helper()
	session, err := d.GetSession(t.Context(), "chatgpt:cg-tool")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.TranscriptRevision)
	rev, err := strconv.Atoi(*session.TranscriptRevision)
	require.NoError(t, err)
	return rev
}

func TestImportChatGPTFillsArchivedToolResult(t *testing.T) {
	for _, tt := range []struct {
		name    string
		archive config.ArchiveContent
		images  config.ToolResultImages
		image   bool
		later   bool
	}{
		{name: "filled and continued", archive: config.ArchiveContentFull, later: true},
		{name: "filled only", archive: config.ArchiveContentFull},
		{name: "transcripts filled and continued", archive: config.ArchiveContentTranscripts, later: true},
		{name: "offload image filled", archive: config.ArchiveContentFull, images: config.ToolResultImagesOffload, image: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(tt.archive)
			toolAssets := t.TempDir()
			d.SetAssetsDir(toolAssets)
			d.SetToolResultImages(tt.images)
			ctx := t.Context()
			dir := t.TempDir()
			path := filepath.Join(dir, "conversations-000.json")
			assetsDir := t.TempDir()
			output := "42"
			if tt.image {
				output = chatGPTImageOutput(t)
			}

			// Monday: exported while the code run had no output yet.
			require.NoError(t, os.WriteFile(path, []byte(chatGPTToolRunConv("", false)), 0o644))
			stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			before, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
			require.NoError(t, err)
			require.Len(t, before, 2)
			require.Len(t, before[1].ToolCalls, 1)
			require.Empty(t, before[1].ToolCalls[0].ResultContent)
			require.Zero(t, before[1].ToolCalls[0].ResultContentLength)
			_, err = d.PinMessage(ctx, "chatgpt:cg-tool", before[1].ID, nil)
			require.NoError(t, err)
			revBefore := chatGPTTranscriptRevision(t, d)

			// Tuesday: the output is filled in, maybe with new turns after it.
			require.NoError(t, os.WriteFile(path, []byte(chatGPTToolRunConv(output, tt.later)), 0o644))
			stats, err = ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			assert.Zero(t, stats.Errors)
			assert.Equal(t, 1, stats.Updated)

			after, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
			require.NoError(t, err)
			want := 2
			if tt.later {
				want = 4
			}
			require.Len(t, after, want)
			assert.Equal(t, before[0].ID, after[0].ID)
			assert.Equal(t, before[1].ID, after[1].ID)
			require.Len(t, after[1].ToolCalls, 1)
			result := after[1].ToolCalls[0]
			switch {
			case tt.image:
				assert.Positive(t, result.ResultContentLength)
				assert.NotContains(t, result.ResultContent, "data:image")
				entries, err := os.ReadDir(toolAssets)
				require.NoError(t, err)
				assert.NotEmpty(t, entries, "the filled image must be offloaded")
			case tt.archive == config.ArchiveContentTranscripts:
				assert.Empty(t, result.ResultContent)
				assert.Equal(t, 10, result.ResultContentLength)
			default:
				assert.Equal(t, "```\n42\n```", result.ResultContent)
				assert.Equal(t, 10, result.ResultContentLength)
			}
			if tt.later {
				assert.Equal(t, "Tuesday answer printed 43", after[3].Content)
			}
			assert.Greater(t, chatGPTTranscriptRevision(t, d), revBefore)

			// The messages API reads through this window with its revision.
			observed := ""
			window, err := d.GetMessagesWindow(ctx, "chatgpt:cg-tool", db.MessageWindow{
				From: new(0), Limit: 10, Asc: true, ObservedRevision: &observed,
			})
			require.NoError(t, err)
			require.Len(t, window, want)
			require.Len(t, window[1].ToolCalls, 1)
			assert.Equal(t, result.ResultContent, window[1].ToolCalls[0].ResultContent)
			assert.Equal(t, result.ResultContentLength, window[1].ToolCalls[0].ResultContentLength)
			assert.Equal(t, strconv.Itoa(chatGPTTranscriptRevision(t, d)), observed)

			pins, err := d.ListPinnedMessages(ctx, "chatgpt:cg-tool", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, before[1].ID, pins[0].MessageID)

			// The same export again is unchanged.
			stats, err = ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			assert.Zero(t, stats.Errors)
			assert.Equal(t, 1, stats.Skipped)
		})
	}
}

func TestImportChatGPTPolicyChangeBetweenImports(t *testing.T) {
	for _, tt := range []struct {
		name                  string
		firstArchive, archive config.ArchiveContent
		firstImages, images   config.ToolResultImages
		image                 bool
	}{
		{name: "full to transcripts", firstArchive: config.ArchiveContentFull, archive: config.ArchiveContentTranscripts},
		{name: "transcripts to full", firstArchive: config.ArchiveContentTranscripts, archive: config.ArchiveContentFull},
		{name: "offload to keep", firstImages: config.ToolResultImagesOffload, images: config.ToolResultImagesKeep, image: true},
		{name: "offload to drop", firstImages: config.ToolResultImagesOffload, images: config.ToolResultImagesDrop, image: true},
		{name: "keep to offload", firstImages: config.ToolResultImagesKeep, images: config.ToolResultImagesOffload, image: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			dbPath := filepath.Join(t.TempDir(), "test.db")
			dbtest.EnsureTestDBAt(t, dbPath)
			toolAssets := t.TempDir()
			dir := t.TempDir()
			path := filepath.Join(dir, "conversations-000.json")
			assetsDir := t.TempDir()
			first := "41"
			if tt.image {
				first = chatGPTImageOutput(t)
			}

			// A separate handle per import lets the archive policy loosen,
			// which a single handle refuses.
			d, err := db.OpenWithArchiveContent(ctx, dbPath, tt.firstArchive)
			require.NoError(t, err)
			d.SetAssetsDir(toolAssets)
			d.SetToolResultImages(tt.firstImages)
			require.NoError(t, os.WriteFile(path, []byte(chatGPTTwoToolRunsConv(first, "")), 0o644))
			stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			before, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
			require.NoError(t, err)
			require.Len(t, before, 4)
			require.Positive(t, before[1].ToolCalls[0].ResultContentLength)
			require.Zero(t, before[3].ToolCalls[0].ResultContentLength)
			require.NoError(t, d.Close())

			d, err = db.OpenWithArchiveContent(ctx, dbPath, tt.archive)
			require.NoError(t, err)
			t.Cleanup(func() { d.Close() })
			d.SetAssetsDir(toolAssets)
			d.SetToolResultImages(tt.images)

			stats, err = ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			assert.Zero(t, stats.Errors)
			assert.Equal(t, 1, stats.Skipped, "an unchanged export must be skipped after a policy change")

			require.NoError(t, os.WriteFile(path, []byte(chatGPTTwoToolRunsConv(first, "42")), 0o644))
			stats, err = ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			assert.Zero(t, stats.Errors)
			assert.Equal(t, 1, stats.Updated)

			after, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
			require.NoError(t, err)
			require.Len(t, after, 4)
			for i := range before {
				assert.Equal(t, before[i].ID, after[i].ID)
			}
			assert.Equal(t, before[1].ToolCalls, after[1].ToolCalls, "unfilled rows must keep their stored results")
			assert.Equal(t, 10, after[3].ToolCalls[0].ResultContentLength)
		})
	}
}

func TestImportChatGPTFillKeepsCompletedSiblingCall(t *testing.T) {
	conv := func(second string) string {
		chain := []chatGPTNodeSpec{
			{"user", "text", "Run two cells"},
			{"assistant", "text", "Running both."},
			{"tool", "code", "print(41)"},
			{"tool", "execution_output", "41"},
			{"tool", "code", "print(42)"},
		}
		if second != "" {
			chain = append(chain, chatGPTNodeSpec{"tool", "execution_output", second})
		}
		return chatGPTChainConv(chain...)
	}
	d := testDB(t)
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations-000.json")
	assetsDir := t.TempDir()
	require.NoError(t, os.WriteFile(path, []byte(conv("")), 0o644))
	_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	before, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
	require.NoError(t, err)
	require.Len(t, before, 2)
	require.Len(t, before[1].ToolCalls, 2)
	require.Equal(t, "```\n41\n```", before[1].ToolCalls[0].ResultContent)
	d.SetArchiveContent(config.ArchiveContentTranscripts)

	require.NoError(t, os.WriteFile(path, []byte(conv("42")), 0o644))
	stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	assert.Zero(t, stats.Errors)
	assert.Equal(t, 1, stats.Updated)

	after, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
	require.NoError(t, err)
	require.Len(t, after, 2)
	require.Len(t, after[1].ToolCalls, 2)
	assert.Equal(t, before[1].ToolCalls[0], after[1].ToolCalls[0], "the completed call must keep its stored result")
	assert.Empty(t, after[1].ToolCalls[1].ResultContent)
	assert.Equal(t, 10, after[1].ToolCalls[1].ResultContentLength)
}

func TestImportChatGPTKeepsCompletedToolResult(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations-000.json")
	assetsDir := t.TempDir()
	require.NoError(t, os.WriteFile(path, []byte(chatGPTToolRunConv("42", false)), 0o644))
	_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	before, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
	require.NoError(t, err)
	require.Len(t, before, 2)

	// A result that changed from one value to another keeps the archived one.
	require.NoError(t, os.WriteFile(path, []byte(chatGPTToolRunConv("4242", true)), 0o644))
	stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	assert.Zero(t, stats.Errors)
	assert.Equal(t, 1, stats.Updated)

	after, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
	require.NoError(t, err)
	require.Len(t, after, 4)
	assert.Equal(t, before, after[:2])
	assert.Equal(t, "```\n42\n```", after[1].ToolCalls[0].ResultContent)
	assert.Equal(t, "Tuesday answer printed 43", after[3].Content)
}

func TestImportChatGPTComparesToolCallStructure(t *testing.T) {
	user := chatGPTNodeSpec{"user", "text", "Run print(42)"}
	asst := chatGPTNodeSpec{"assistant", "text", "Running it."}
	code := chatGPTNodeSpec{"tool", "code", "print(42)"}
	search := chatGPTNodeSpec{"tool", "tether_quote", "A quote"}
	later := []chatGPTNodeSpec{{"user", "text", "Now print 43"}, {"assistant", "text", "Printed 43"}}
	for _, tt := range []struct {
		name     string
		initial  string
		data     string
		category string
		accepted bool
	}{
		{name: "renamed call", initial: chatGPTChainConv(user, asst, code), data: chatGPTChainConv(user, asst, search)},
		{name: "changed category", initial: chatGPTChainConv(user, asst, code), data: chatGPTChainConv(user, asst, code), category: "Changed"},
		{name: "fewer calls", initial: chatGPTChainConv(user, asst, code, code), data: chatGPTChainConv(user, asst, code)},
		{name: "extra call on last archived message", initial: chatGPTChainConv(user, asst, code), data: chatGPTChainConv(append([]chatGPTNodeSpec{user, asst, code, search}, later...)...), accepted: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			dir := t.TempDir()
			path := filepath.Join(dir, "conversations-000.json")
			assetsDir := t.TempDir()
			require.NoError(t, os.WriteFile(path, []byte(tt.initial), 0o644))
			_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			if tt.category != "" {
				// The parser derives the category from the name, so the
				// archived row stands in for a category the export changed.
				require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx,
						"UPDATE tool_calls SET category = ? WHERE session_id = 'chatgpt:cg-tool'",
						tt.category)
					return err
				}))
			}
			before, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
			require.NoError(t, err)
			require.Len(t, before, 2)
			require.NotEmpty(t, before[1].ToolCalls)

			require.NoError(t, os.WriteFile(path, []byte(tt.data), 0o644))
			stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			after, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
			require.NoError(t, err)
			if !tt.accepted {
				assert.Equal(t, 1, stats.Errors)
				assert.Zero(t, stats.Updated)
				assert.Equal(t, before, after)
				return
			}
			assert.Zero(t, stats.Errors)
			assert.Equal(t, 1, stats.Updated)
			require.Len(t, after, 4)
			assert.Equal(t, before, after[:2], "archived calls must stay as stored")
			assert.Equal(t, "Printed 43", after[3].Content)
		})
	}
}

func TestImportChatGPTSkipsTrashedSession(t *testing.T) {
	for _, tt := range []struct {
		name string
		data string
	}{
		{name: "unchanged", data: chatGPTToolRunConv("", false)},
		{name: "appended", data: chatGPTToolRunConv("", true)},
		{name: "filled", data: chatGPTToolRunConv("42", true)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			dir := t.TempDir()
			path := filepath.Join(dir, "conversations-000.json")
			assetsDir := t.TempDir()
			require.NoError(t, os.WriteFile(path, []byte(chatGPTToolRunConv("", false)), 0o644))
			_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			require.NoError(t, d.SoftDeleteSession(ctx, "chatgpt:cg-tool"))

			require.NoError(t, os.WriteFile(path, []byte(tt.data), 0o644))
			stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			assert.Zero(t, stats.Errors)
			assert.Zero(t, stats.Updated)
			assert.Equal(t, 1, stats.Skipped)
			assert.Empty(t, stats.Refusals)
			full, err := d.GetSessionFull(ctx, "chatgpt:cg-tool")
			require.NoError(t, err)
			require.NotNil(t, full)
			assert.NotNil(t, full.DeletedAt, "the session must stay trashed")
		})
	}
}

// failFillStore fails the batch write just before it commits.
type failFillStore struct{ *db.DB }

func (s failFillStore) WriteSessionBatchAtomic(
	ctx context.Context, writes []db.SessionBatchWrite,
	_ ...func() error,
) (db.SessionBatchResult, error) {
	return s.DB.WriteSessionBatchAtomic(ctx, writes, func() error {
		return errors.New("commit failed")
	})
}

func TestImportChatGPTFailedFillKeepsSessionRow(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations-000.json")
	assetsDir := t.TempDir()

	require.NoError(t, os.WriteFile(path, []byte(chatGPTToolRunConv("", false)), 0o644))
	_, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	beforeMsgs, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
	require.NoError(t, err)
	require.Len(t, beforeMsgs, 2)
	_, err = d.PinMessage(ctx, "chatgpt:cg-tool", beforeMsgs[1].ID, nil)
	require.NoError(t, err)
	before, err := d.GetSession(ctx, "chatgpt:cg-tool")
	require.NoError(t, err)
	require.NotNil(t, before)

	renamed := strings.Replace(chatGPTToolRunConv("42", true), `"title":"Tool"`, `"title":"Tool renamed"`, 1)
	require.NoError(t, os.WriteFile(path, []byte(renamed), 0o644))
	stats, _ := ImportChatGPT(ctx, failFillStore{d}, dir, assetsDir, nil)
	assert.Equal(t, 1, stats.Errors)
	assert.Equal(t, []ImportRefusal{{SessionID: "chatgpt:cg-tool", Reason: RefusalTransient}}, stats.Refusals)

	after, err := d.GetSession(ctx, "chatgpt:cg-tool")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	name, found, err := d.GetSessionName(ctx, "chatgpt:cg-tool")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "Tool", name, "a failed fill must keep the old title")
	afterMsgs, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
	require.NoError(t, err)
	assert.Equal(t, beforeMsgs, afterMsgs)
	pins, err := d.ListPinnedMessages(ctx, "chatgpt:cg-tool", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, beforeMsgs[1].ID, pins[0].MessageID)

	// A later import of the same export fills the result in.
	stats, err = ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	assert.Zero(t, stats.Errors)
	assert.Equal(t, 1, stats.Updated)
	assert.Empty(t, stats.Refusals)
}

func TestImportChatGPTReimportRestoresExportImage(t *testing.T) {
	var conversations []map[string]any
	require.NoError(t, json.Unmarshal([]byte(testChatGPTConvWithAppend()), &conversations))
	mapping := conversations[0]["mapping"].(map[string]any)
	second := mapping["n2"].(map[string]any)["message"].(map[string]any)
	second["content"] = map[string]any{
		"content_type": "multimodal_text",
		"parts": []any{"See this:", map[string]any{
			"content_type":  "image_asset_pointer",
			"asset_pointer": "file-service://file-img1",
		}},
	}
	encoded, err := json.Marshal(conversations)
	require.NoError(t, err)

	d := testDB(t)
	ctx := t.Context()
	dir := t.TempDir()
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "file-img1-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee.png"), png, 0o644,
	))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "conversations-000.json"), encoded, 0o644))
	assetsDir := filepath.Join(t.TempDir(), "assets")
	_, err = ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	entries, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, os.Remove(filepath.Join(assetsDir, entries[0].Name())))

	stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
	require.NoError(t, err)
	assert.Zero(t, stats.Errors)
	assert.Equal(t, 1, stats.Skipped)
	restored, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Len(t, restored, 1, "an unchanged re-import copies the export image again")
}

func TestImportStatsRecord(t *testing.T) {
	var stats ImportStats
	stats.record("a", importNew, nil)
	stats.record("b", importUpdated, nil)
	stats.record("c", importSkipped, nil)
	assert.Equal(t, ImportStats{Imported: 1, Updated: 1, Skipped: 1}, stats)

	diskFull := errors.New("disk full")
	for _, tc := range []struct {
		name string
		err  error
		want RefusalReason
	}{
		{name: "tagged diverged", err: refuse(RefusalDiverged, errors.New("export history diverges from the archived messages")), want: RefusalDiverged},
		{name: "tagged shorter", err: refuse(RefusalShorterExport, errors.New("export has 1 messages, archive has 2")), want: RefusalShorterExport},
		{name: "trashed", err: fmt.Errorf("upserting session: %w", db.ErrSessionTrashed), want: RefusalTrashed},
		{name: "storage shorten", err: fmt.Errorf("writing session: %w", &db.SessionWouldShortenError{SessionID: "s", ExistingMessages: 2, IncomingMessages: 1}), want: RefusalShorterExport},
		{name: "untagged", err: diskFull, want: RefusalTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stats ImportStats
			stats.record("s1", importNew, tc.err)
			assert.Equal(t, ImportStats{Errors: 1, Refusals: []ImportRefusal{{SessionID: "s1", Reason: tc.want}}}, stats)
		})
	}

	tagged := refuse(RefusalDiverged, diskFull)
	assert.Equal(t, diskFull.Error(), tagged.Error())
	assert.ErrorIs(t, tagged, diskFull)
}

func TestImportClaudeAIReportsTrashedSession(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	_, err := ImportClaudeAI(ctx, d, strings.NewReader(testConversationsJSON), nil)
	require.NoError(t, err)
	require.NoError(t, d.SoftDeleteSession(ctx, "claude-ai:import-test-001"))

	stats, err := ImportClaudeAI(ctx, d, strings.NewReader(testConversationsJSON), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Errors)
	assert.Zero(t, stats.Skipped)
	assert.Equal(t, []ImportRefusal{{SessionID: "claude-ai:import-test-001", Reason: RefusalTrashed}}, stats.Refusals)
	full, err := d.GetSessionFull(ctx, "claude-ai:import-test-001")
	require.NoError(t, err)
	require.NotNil(t, full)
	assert.NotNil(t, full.DeletedAt, "the session must stay trashed")
}

// chatGPTLongTextConv is a user turn holding text and an assistant reply.
func chatGPTLongTextConv(text string) string {
	return chatGPTChainConv(
		chatGPTNodeSpec{"user", "text", text},
		chatGPTNodeSpec{"assistant", "text", "Noted."},
	)
}

func TestImportChatGPTExtendsTruncatedText(t *testing.T) {
	full := strings.Repeat("lorem ipsum ", 50) + "zephyrquill closing words"
	cut := full[:500]
	require.Greater(t, strings.Index(full, "zephyrquill"), 500)
	firstStamp := `"m-n1","create_time":` + fmt.Sprint(float64(1706745600))
	shiftedStamp := strings.Replace(chatGPTLongTextConv(full), firstStamp, `"m-n1","create_time":1706745605`, 1)
	require.NotEqual(t, chatGPTLongTextConv(full), shiftedStamp)

	setup := func(t *testing.T, archive config.ArchiveContent, initial string) (*db.DB, string, string, string) {
		t.Helper()
		d := testDB(t)
		d.SetArchiveContent(archive)
		dir := t.TempDir()
		path := filepath.Join(dir, "conversations-000.json")
		assetsDir := t.TempDir()
		require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))
		stats, err := ImportChatGPT(t.Context(), d, dir, assetsDir, nil)
		require.NoError(t, err)
		require.Equal(t, 1, stats.Imported)
		return d, dir, path, assetsDir
	}

	t.Run("repair only", func(t *testing.T) {
		d, dir, path, assetsDir := setup(t, config.ArchiveContentFull, chatGPTLongTextConv(cut))
		ctx := t.Context()
		before, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
		require.NoError(t, err)
		require.Len(t, before, 2)
		require.Equal(t, cut, before[0].Content)
		_, err = d.PinMessage(ctx, "chatgpt:cg-tool", before[0].ID, nil)
		require.NoError(t, err)
		score := 42
		require.NoError(t, d.UpdateSessionSignals(ctx, "chatgpt:cg-tool", db.SessionSignalUpdate{
			HealthScore:    &score,
			QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion},
		}))
		revBefore := chatGPTTranscriptRevision(t, d)

		require.NoError(t, os.WriteFile(path, []byte(chatGPTLongTextConv(full)), 0o644))
		stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
		require.NoError(t, err)
		assert.Zero(t, stats.Errors)
		assert.Equal(t, 1, stats.Updated)

		after, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
		require.NoError(t, err)
		require.Len(t, after, 2)
		assert.Equal(t, before[0].ID, after[0].ID)
		assert.Equal(t, before[1].ID, after[1].ID)
		assert.Equal(t, full, after[0].Content)
		assert.Equal(t, len(full), after[0].ContentLength)
		assert.Equal(t, before[1], after[1])

		pins, err := d.ListPinnedMessages(ctx, "chatgpt:cg-tool", "")
		require.NoError(t, err)
		require.Len(t, pins, 1)
		assert.Equal(t, before[0].ID, pins[0].MessageID)

		session, err := d.GetSession(ctx, "chatgpt:cg-tool")
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Zero(t, session.QualitySignalVersion)
		assert.Nil(t, session.HealthScore)

		results, err := d.SearchContent(ctx, db.ContentSearchFilter{
			Pattern: "zephyrquill", Sources: []string{"messages"},
			IncludeOneShot: true, IncludeAutomated: true, Limit: 10,
		})
		require.NoError(t, err)
		require.NotEmpty(t, results.Matches)
		assert.Equal(t, "chatgpt:cg-tool", results.Matches[0].SessionID)

		revAfter := chatGPTTranscriptRevision(t, d)
		assert.Greater(t, revAfter, revBefore)
		observed := ""
		window, err := d.GetMessagesWindow(ctx, "chatgpt:cg-tool", db.MessageWindow{
			From: new(0), Limit: 10, Asc: true, ObservedRevision: &observed,
		})
		require.NoError(t, err)
		require.Len(t, window, 2)
		assert.Equal(t, full, window[0].Content)
		assert.Equal(t, strconv.Itoa(revAfter), observed)

		stats, err = ImportChatGPT(ctx, d, dir, assetsDir, nil)
		require.NoError(t, err)
		assert.Zero(t, stats.Errors)
		assert.Equal(t, 1, stats.Skipped)
	})

	t.Run("repair fill and append", func(t *testing.T) {
		conv := func(text, first, second string, later bool) string {
			chain := []chatGPTNodeSpec{
				{"user", "text", "Run two cells"},
				{"assistant", "text", text},
				{"tool", "code", "print(41)"},
				{"tool", "execution_output", first},
				{"tool", "code", "print(42)"},
			}
			if second != "" {
				chain = append(chain, chatGPTNodeSpec{"tool", "execution_output", second})
			}
			if later {
				chain = append(chain,
					chatGPTNodeSpec{"user", "text", "Now print 43"},
					chatGPTNodeSpec{"assistant", "text", "Tuesday answer printed 43"},
				)
			}
			return chatGPTChainConv(chain...)
		}
		d, dir, path, assetsDir := setup(t, config.ArchiveContentFull, conv(cut, "41", "", false))
		ctx := t.Context()
		before, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
		require.NoError(t, err)
		require.Len(t, before, 2)
		require.Equal(t, cut, before[1].Content)
		require.Len(t, before[1].ToolCalls, 2)
		require.Equal(t, "```\n41\n```", before[1].ToolCalls[0].ResultContent)
		require.Empty(t, before[1].ToolCalls[1].ResultContent)

		require.NoError(t, os.WriteFile(path, []byte(conv(full, "4141", "42", true)), 0o644))
		stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
		require.NoError(t, err)
		assert.Zero(t, stats.Errors)
		assert.Equal(t, 1, stats.Updated)

		after, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
		require.NoError(t, err)
		require.Len(t, after, 4)
		assert.Equal(t, before[0].ID, after[0].ID)
		assert.Equal(t, before[1].ID, after[1].ID)
		assert.Equal(t, before[0], after[0])
		assert.Equal(t, full, after[1].Content)
		assert.Equal(t, len(full), after[1].ContentLength)
		require.Len(t, after[1].ToolCalls, 2)
		assert.Equal(t, before[1].ToolCalls[0], after[1].ToolCalls[0], "the completed call must keep its stored result")
		assert.Equal(t, "```\n42\n```", after[1].ToolCalls[1].ResultContent)
		assert.Equal(t, "Tuesday answer printed 43", after[3].Content)
	})

	for _, tt := range []struct {
		name    string
		archive config.ArchiveContent
		initial string
		data    string
	}{
		{name: "export text shorter", initial: chatGPTLongTextConv(full), data: chatGPTLongTextConv(cut)},
		{name: "text changed inside prefix", initial: chatGPTLongTextConv(cut), data: chatGPTLongTextConv(strings.Replace(full, "lorem", "LOREM", 1))},
		{name: "empty archived text", initial: chatGPTLongTextConv(""), data: chatGPTLongTextConv(full)},
		{name: "extended text with changed timestamp", initial: chatGPTLongTextConv(cut), data: shiftedStamp},
		{name: "archived trailing space", initial: chatGPTLongTextConv(cut + " "), data: chatGPTLongTextConv(cut + "Xmore text")},
		{name: "extended text turned system", initial: chatGPTChainConv(chatGPTNodeSpec{"user", "text", "Hi"}, chatGPTNodeSpec{"assistant", "text", cut}), data: chatGPTChainConv(chatGPTNodeSpec{"user", "text", "Hi"}, chatGPTNodeSpec{"system", "text", full})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, dir, path, assetsDir := setup(t, config.ArchiveContentFull, tt.initial)
			ctx := t.Context()
			before, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
			require.NoError(t, err)
			require.Len(t, before, 2)

			require.NoError(t, os.WriteFile(path, []byte(tt.data), 0o644))
			stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Errors)
			assert.Zero(t, stats.Updated)
			after, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}

	t.Run("transcripts archive", func(t *testing.T) {
		d, dir, path, assetsDir := setup(t, config.ArchiveContentTranscripts, chatGPTLongTextConv(cut))
		ctx := t.Context()
		before, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
		require.NoError(t, err)
		require.Len(t, before, 2)

		require.NoError(t, os.WriteFile(path, []byte(chatGPTLongTextConv(full)), 0o644))
		stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
		require.NoError(t, err)
		assert.Zero(t, stats.Errors)
		assert.Equal(t, 1, stats.Updated)
		after, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
		require.NoError(t, err)
		require.Len(t, after, 2)
		assert.Equal(t, before[0].ID, after[0].ID)
		assert.Equal(t, full, after[0].Content)
	})

	t.Run("usage archive", func(t *testing.T) {
		d, dir, path, assetsDir := setup(t, config.ArchiveContentFull, chatGPTLongTextConv(cut))
		ctx := t.Context()
		d.SetArchiveContent(config.ArchiveContentUsage)
		before, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
		require.NoError(t, err)

		require.NoError(t, os.WriteFile(path, []byte(chatGPTLongTextConv(full)), 0o644))
		stats, err := ImportChatGPT(ctx, d, dir, assetsDir, nil)
		require.NoError(t, err)
		assert.Zero(t, stats.Errors)
		assert.Zero(t, stats.Updated)
		assert.Equal(t, 1, stats.Skipped)
		after, err := d.GetAllMessages(ctx, "chatgpt:cg-tool")
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})
}
