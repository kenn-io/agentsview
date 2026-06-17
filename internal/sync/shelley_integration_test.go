package sync_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

const shelleyTestSchema = `
CREATE TABLE conversations (
	conversation_id TEXT PRIMARY KEY,
	slug TEXT,
	user_initiated BOOLEAN NOT NULL DEFAULT TRUE,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	cwd TEXT,
	archived BOOLEAN NOT NULL DEFAULT FALSE,
	parent_conversation_id TEXT,
	model TEXT,
	current_generation INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE messages (
	message_id TEXT PRIMARY KEY,
	conversation_id TEXT NOT NULL,
	sequence_id INTEGER NOT NULL,
	type TEXT NOT NULL,
	llm_data TEXT,
	user_data TEXT,
	usage_data TEXT,
	display_data TEXT,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	generation INTEGER NOT NULL DEFAULT 1,
	excluded_from_context BOOLEAN NOT NULL DEFAULT FALSE
);
`

// shelleyMsg is a minimal message fixture for the integration DB.
type shelleyMsg struct {
	seq      int
	msgType  string
	llmData  string
	usageDat string
	created  string
}

func createShelleyDB(t *testing.T, dir string) string {
	t.Helper()
	dbPath := filepath.Join(dir, "shelley.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open shelley test db")
	defer db.Close()
	_, err = db.Exec(shelleyTestSchema)
	require.NoError(t, err, "create shelley schema")
	return dbPath
}

func seedShelleyConvo(
	t *testing.T, dbPath, id, slug, cwd, model, parent string,
	userInitiated bool, created, updated string, msgs []shelleyMsg,
) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(
		`INSERT INTO conversations
			(conversation_id, slug, user_initiated, created_at,
			 updated_at, cwd, parent_conversation_id, model)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, slug, userInitiated, created, updated, cwd,
		nullIfEmpty(parent), nullIfEmpty(model),
	)
	require.NoError(t, err, "insert conversation %s", id)
	for _, m := range msgs {
		_, err = db.Exec(
			`INSERT INTO messages
				(message_id, conversation_id, sequence_id, type,
				 llm_data, usage_data, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id+"-m"+string(rune('0'+m.seq)), id, m.seq, m.msgType,
			nullIfEmpty(m.llmData), nullIfEmpty(m.usageDat), m.created,
		)
		require.NoError(t, err, "insert message %s/%d", id, m.seq)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func newShelleyEngine(t *testing.T, dir string) (*sync.Engine, *db.DB) {
	t.Helper()
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentShelley: {dir},
		},
		Machine: "local",
	})
	return engine, database
}

func mainConvoMsgs() []shelleyMsg {
	return []shelleyMsg{
		{1, "user",
			`{"Role":0,"Content":[{"Type":0,"Text":"hello shelley"}]}`,
			"", "2026-06-15T10:00:00Z"},
		{2, "agent",
			`{"Role":1,"Content":[{"Type":0,"Text":"hi"},` +
				`{"ID":"toolu_x","Type":3,"ToolName":"bash","ToolInput":{"cmd":"ls"}}]}`,
			`{"input_tokens":500,"cache_read_input_tokens":0,` +
				`"cache_creation_input_tokens":0,"output_tokens":50,` +
				`"model":"claude-sonnet-4-6"}`,
			"2026-06-15T10:00:05Z"},
		{3, "tool",
			`{"Role":0,"Content":[{"Type":4,"ToolUseID":"toolu_x",` +
				`"ToolResult":[{"Type":0,"Text":"file1"}]}]}`,
			"", "2026-06-15T10:00:06Z"},
	}
}

func TestSyncSingleSessionShelleyUsesVirtualSourcePath(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:06Z", mainConvoMsgs())

	engine, database := newShelleyEngine(t, dir)

	assert.Equal(t, dbPath+"#cMAIN1",
		engine.FindSourceFile("shelley:cMAIN1"), "virtual source path")
	require.NoError(t, engine.SyncSingleSession("shelley:cMAIN1"))

	sess, err := database.GetSession(context.Background(), "shelley:cMAIN1")
	require.NoError(t, err)
	require.NotNil(t, sess, "session present")
	// The user prompt and the agent reply remain; the tool-result-only
	// carrier message is paired into the tool call and dropped.
	assert.Equal(t, 2, sess.MessageCount, "message count")
	assert.Equal(t, "app", sess.Project, "project")
	assert.Equal(t, dbPath+"#cMAIN1",
		database.GetSessionFilePath("shelley:cMAIN1"), "stored file path")
}

func TestSyncAllShelleyIngestsConversations(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:06Z", mainConvoMsgs())
	seedShelleyConvo(t, dbPath, "cAUX1", "aux", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:01:00Z", "2026-06-15T10:01:10Z", []shelleyMsg{
			{1, "user", `{"Role":0,"Content":[{"Type":0,"Text":"second"}]}`,
				"", "2026-06-15T10:01:00Z"},
			{2, "agent", `{"Role":1,"Content":[{"Type":0,"Text":"ok"}]}`,
				"", "2026-06-15T10:01:10Z"},
		})

	engine, database := newShelleyEngine(t, dir)
	stats := engine.SyncAll(context.Background(), nil)
	require.False(t, stats.Aborted, "sync aborted: %+v", stats)
	assert.Equal(t, 2, stats.Synced, "synced count")

	assertSessionMessageCount(t, database, "shelley:cMAIN1", 2)
	assertSessionMessageCount(t, database, "shelley:cAUX1", 2)
	assertToolCallCount(t, database, "shelley:cMAIN1", 1)
	assertMessageContent(t, database, "shelley:cAUX1", "second", "ok")

	// The tool result from the dropped carrier message is paired into
	// the tool call's result_content.
	var resultContent string
	require.NoError(t, database.Reader().QueryRow(
		`SELECT COALESCE(result_content, '') FROM tool_calls WHERE session_id = ?`,
		"shelley:cMAIN1",
	).Scan(&resultContent), "query tool result content")
	assert.Contains(t, resultContent, "file1", "tool result preserved on tool call")
}

// TestResyncAllShelleyRebuildsFromDB verifies that a full resync
// re-parses the present Shelley DB without aborting and preserves
// content. Shelley is a Zed-style single-DB agent, so it re-syncs fresh
// (Synced > 0) rather than relying on archive-preservation accounting.
func TestResyncAllShelleyRebuildsFromDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:06Z", mainConvoMsgs())

	engine, database := newShelleyEngine(t, dir)
	require.False(t, engine.SyncAll(context.Background(), nil).Aborted)

	stats := engine.ResyncAll(context.Background(), nil)
	assert.False(t, stats.Aborted, "resync aborted: %+v", stats)
	assert.NotZero(t, stats.Synced, "resync should re-parse fresh")
	assertSessionMessageCount(t, database, "shelley:cMAIN1", 2)
}

// TestResyncAllShelleyPreservesRemovedConversation verifies that a
// conversation deleted from the source DB survives a resync via
// orphan-copy, satisfying the archive-preservation requirement
// (existing session data must survive when source rows disappear).
func TestResyncAllShelleyPreservesRemovedConversation(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:06Z", mainConvoMsgs())
	seedShelleyConvo(t, dbPath, "cGONE1", "gone", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T09:00:00Z", "2026-06-15T09:00:10Z", []shelleyMsg{
			{1, "user", `{"Role":0,"Content":[{"Type":0,"Text":"old work"}]}`,
				"", "2026-06-15T09:00:00Z"},
			{2, "agent", `{"Role":1,"Content":[{"Type":0,"Text":"old reply"}]}`,
				"", "2026-06-15T09:00:10Z"},
		})

	engine, database := newShelleyEngine(t, dir)
	require.False(t, engine.SyncAll(context.Background(), nil).Aborted)
	assertSessionMessageCount(t, database, "shelley:cGONE1", 2)

	// Remove cGONE1 from the source DB entirely.
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(`DELETE FROM messages WHERE conversation_id = 'cGONE1'`)
	require.NoError(t, err)
	_, err = conn.Exec(`DELETE FROM conversations WHERE conversation_id = 'cGONE1'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	stats := engine.ResyncAll(context.Background(), nil)
	assert.False(t, stats.Aborted, "resync aborted: %+v", stats)

	// cMAIN1 re-parsed, cGONE1 preserved from the old DB.
	assertSessionMessageCount(t, database, "shelley:cMAIN1", 2)
	gone, err := database.GetSession(context.Background(), "shelley:cGONE1")
	require.NoError(t, err)
	require.NotNil(t, gone, "removed conversation should survive resync")
	assert.Equal(t, 2, gone.MessageCount, "preserved message count")
}

// TestSyncShelleyForceReplaceOnInPlaceUpdate verifies that when a
// message's content changes in place (Shelley rewrites rows and bumps
// updated_at), a re-sync fully replaces the session's messages rather
// than appending duplicates.
func TestSyncShelleyForceReplaceOnInPlaceUpdate(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:10Z", []shelleyMsg{
			{1, "user", `{"Role":0,"Content":[{"Type":0,"Text":"q"}]}`,
				"", "2026-06-15T10:00:00Z"},
			{2, "agent", `{"Role":1,"Content":[{"Type":0,"Text":"first answer"}]}`,
				"", "2026-06-15T10:00:10Z"},
		})

	engine, database := newShelleyEngine(t, dir)
	require.False(t, engine.SyncAll(context.Background(), nil).Aborted)
	assertMessageContent(t, database, "shelley:cMAIN1", "q", "first answer")

	// Rewrite the agent message in place and bump updated_at so the
	// per-session skip detects the change.
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE messages SET llm_data = ? WHERE conversation_id = 'cMAIN1' AND sequence_id = 2`,
		`{"Role":1,"Content":[{"Type":0,"Text":"second answer"}]}`,
	)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE conversations SET updated_at = '2026-06-15T10:05:00Z' WHERE conversation_id = 'cMAIN1'`,
	)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.False(t, engine.SyncAll(context.Background(), nil).Aborted)
	// Replaced, not appended: still two messages, new content.
	assertSessionMessageCount(t, database, "shelley:cMAIN1", 2)
	assertMessageContent(t, database, "shelley:cMAIN1", "q", "second answer")
}
