package sync_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func sessionIDs(sessions []db.Session) []string {
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

func mainConvoMsgs() []shelleyMsg {
	return []shelleyMsg{
		{1, "user",
			`{"Role":0,"Content":[{"Type":2,"Text":"hello shelley"}]}`,
			"", "2026-06-15T10:00:00Z"},
		{2, "agent",
			`{"Role":1,"Content":[{"Type":2,"Text":"hi"},` +
				`{"ID":"toolu_x","Type":5,"ToolName":"bash","ToolInput":{"cmd":"ls"}}]}`,
			`{"input_tokens":500,"cache_read_input_tokens":0,` +
				`"cache_creation_input_tokens":0,"output_tokens":50,` +
				`"model":"claude-sonnet-4-6"}`,
			"2026-06-15T10:00:05Z"},
		{3, "tool",
			`{"Role":0,"Content":[{"Type":6,"ToolUseID":"toolu_x",` +
				`"ToolResult":[{"Type":2,"Text":"file1"}]}]}`,
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

func TestSyncSingleSessionShelleyForceRewritesUnchangedSession(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:06Z", mainConvoMsgs())

	engine, database := newShelleyEngine(t, dir)
	require.NoError(t, engine.SyncSingleSession("shelley:cMAIN1"))
	sess, err := database.GetSession(context.Background(), "shelley:cMAIN1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 2, sess.MessageCount)

	sess.MessageCount = 0
	require.NoError(t, database.UpsertSession(*sess))

	require.NoError(t, engine.SyncSingleSession("shelley:cMAIN1"))

	sess, err = database.GetSession(context.Background(), "shelley:cMAIN1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, dbPath+"#cMAIN1",
		database.GetSessionFilePath("shelley:cMAIN1"), "stored file path")
}

func TestSyncPathsShelleyDeletedPhysicalDBRemovesSessions(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:06Z", mainConvoMsgs())

	engine, database := newShelleyEngine(t, dir)
	stats := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced)
	require.NoError(t, os.Remove(dbPath))

	engine.SyncPaths([]string{dbPath})

	sess, err := database.GetSession(context.Background(), "shelley:cMAIN1")
	require.NoError(t, err)
	assert.Nil(t, sess)
}

// TestSourceMtimeShelleyResolvesVirtualPath guards the live per-session
// watcher: SourceMtime must resolve a shelley.db#<id> virtual path to the
// conversation's updated_at, not fall through to os.Stat (which fails on a
// virtual path and returns 0, which the watcher reads as "source gone").
func TestSourceMtimeShelleyResolvesVirtualPath(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:06Z", mainConvoMsgs())

	engine, _ := newShelleyEngine(t, dir)
	assert.Positive(t, engine.SourceMtime("shelley:cMAIN1"),
		"SourceMtime must resolve the virtual path, not return 0")
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
			{1, "user", `{"Role":0,"Content":[{"Type":2,"Text":"second"}]}`,
				"", "2026-06-15T10:01:00Z"},
			{2, "agent", `{"Role":1,"Content":[{"Type":2,"Text":"ok"}]}`,
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

func TestSyncShelleyRemotePathRewriterSkip(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:06Z", mainConvoMsgs())

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentShelley: {dir},
		},
		Machine:  "host",
		IDPrefix: "host~",
		PathRewriter: func(path string) string {
			return "host:" + path
		},
	})

	stats := engine.SyncAll(context.Background(), nil)
	require.False(t, stats.Aborted, "first sync aborted: %+v", stats)
	assert.Equal(t, 1, stats.Synced, "first sync should write the session")

	const sessionID = "host~shelley:cMAIN1"
	storedPath := "host:" + dbPath + "#cMAIN1"
	assert.Equal(t, storedPath,
		database.GetSessionFilePath(sessionID), "stored remote path")
	before, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err, "GetSessionFull before second sync")
	require.NotNil(t, before, "session before second sync")
	require.NotNil(t, before.LocalModifiedAt,
		"local_modified_at before second sync")
	_, storedMtime, ok := database.GetFileInfoByPath(storedPath)
	require.True(t, ok, "remote virtual path should be indexed")
	assert.NotZero(t, storedMtime, "stored mtime")

	time.Sleep(20 * time.Millisecond)
	stats = engine.SyncAll(context.Background(), nil)
	require.False(t, stats.Aborted, "second sync aborted: %+v", stats)
	assert.Equal(t, 0, stats.Synced,
		"unchanged remote Shelley conversation should be skipped")

	after, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err, "GetSessionFull after second sync")
	require.NotNil(t, after, "session after second sync")
	require.NotNil(t, after.LocalModifiedAt,
		"local_modified_at after second sync")
	assert.Equal(t, *before.LocalModifiedAt, *after.LocalModifiedAt,
		"skip lookup must use the rewritten virtual path")
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
			{1, "user", `{"Role":0,"Content":[{"Type":2,"Text":"old work"}]}`,
				"", "2026-06-15T09:00:00Z"},
			{2, "agent", `{"Role":1,"Content":[{"Type":2,"Text":"old reply"}]}`,
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
			{1, "user", `{"Role":0,"Content":[{"Type":2,"Text":"q"}]}`,
				"", "2026-06-15T10:00:00Z"},
			{2, "agent", `{"Role":1,"Content":[{"Type":2,"Text":"first answer"}]}`,
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
		`{"Role":1,"Content":[{"Type":2,"Text":"second answer"}]}`,
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

// TestSyncShelleyStoresRealTimestamp guards against a synthetic future
// file_mtime. ListSessionsModifiedBetween filters file_mtime <= now for
// PG/DuckDB push, so a Shelley row whose stored mtime drifted past its
// updated_at second could be dropped from a same-second push. The stored
// file_mtime must equal the conversation's real updated_at instant.
func TestSyncShelleyStoresRealTimestamp(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	const updatedAt = "2026-06-15T10:00:05Z"
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", updatedAt, mainConvoMsgs())

	engine, database := newShelleyEngine(t, dir)
	require.False(t, engine.SyncAll(context.Background(), nil).Aborted)

	want, err := time.Parse(time.RFC3339, updatedAt)
	require.NoError(t, err)
	_, mtime, ok := database.GetSessionFileInfo("shelley:cMAIN1")
	require.True(t, ok, "session file info present")
	assert.Equal(t, want.UnixNano(), mtime,
		"stored file_mtime is the real updated_at, not a synthetic offset")
}

// TestSyncShelleySameSecondInPlaceRewrite covers the gap that
// updated_at + MAX(sequence_id) alone cannot close: a message rewritten
// in place where updated_at stays in the same wall-clock second and no
// new row is appended (sequence_id unchanged). The content fingerprint
// detects it, so a re-sync replaces the stored transcript instead of
// skipping it as unchanged.
func TestSyncShelleySameSecondInPlaceRewrite(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:10Z", []shelleyMsg{
			{1, "user", `{"Role":0,"Content":[{"Type":2,"Text":"q"}]}`,
				"", "2026-06-15T10:00:00Z"},
			{2, "agent", `{"Role":1,"Content":[{"Type":2,"Text":"partial"}]}`,
				"", "2026-06-15T10:00:10Z"},
		})

	engine, database := newShelleyEngine(t, dir)
	require.False(t, engine.SyncAll(context.Background(), nil).Aborted)
	assertMessageContent(t, database, "shelley:cMAIN1", "q", "partial")
	before, err := database.GetSessionFull(
		context.Background(), "shelley:cMAIN1",
	)
	require.NoError(t, err, "GetSessionFull before rewrite")
	require.NotNil(t, before, "session before rewrite")
	require.NotNil(t, before.FileMtime, "file_mtime before rewrite")
	require.NotNil(t, before.FileHash, "file_hash before rewrite")
	require.NotNil(t, before.LocalModifiedAt,
		"local_modified_at before rewrite")

	// Rewrite the agent message in place. Crucially, updated_at is left
	// untouched (same second) and sequence_id is unchanged, so only the
	// content fingerprint differs.
	time.Sleep(20 * time.Millisecond)
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE messages SET llm_data = ? WHERE conversation_id = 'cMAIN1' AND sequence_id = 2`,
		`{"Role":1,"Content":[{"Type":2,"Text":"the full streamed answer"}]}`,
	)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.False(t, engine.SyncAll(context.Background(), nil).Aborted)
	// Detected via the content fingerprint: replaced, not skipped.
	assertSessionMessageCount(t, database, "shelley:cMAIN1", 2)
	assertMessageContent(
		t, database, "shelley:cMAIN1", "q", "the full streamed answer",
	)
	after, err := database.GetSessionFull(
		context.Background(), "shelley:cMAIN1",
	)
	require.NoError(t, err, "GetSessionFull after rewrite")
	require.NotNil(t, after, "session after rewrite")
	require.NotNil(t, after.FileMtime, "file_mtime after rewrite")
	require.NotNil(t, after.FileHash, "file_hash after rewrite")
	require.NotNil(t, after.LocalModifiedAt,
		"local_modified_at after rewrite")
	assert.Equal(t, *before.FileMtime, *after.FileMtime,
		"same-second rewrite keeps the real Shelley updated_at timestamp")
	assert.NotEqual(t, *before.FileHash, *after.FileHash,
		"same-count same-second rewrite changes the content fingerprint")
	assert.Greater(t, *after.LocalModifiedAt, *before.LocalModifiedAt,
		"successful rewrite must bump local_modified_at for push windows")

	candidates, err := database.ListSessionsModifiedBetween(
		context.Background(),
		*before.LocalModifiedAt,
		time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano),
		nil,
		nil,
	)
	require.NoError(t, err, "ListSessionsModifiedBetween after rewrite")
	assert.Contains(t, sessionIDs(candidates), "shelley:cMAIN1",
		"local_modified_at must select the rewritten session for pushes")
}

// TestSyncShelleySameSecondMetadataRewrite covers conversation fields the
// parser reads outside the message payloads. A same-second slug/cwd edit
// must change the stored fingerprint so the bulk skip re-parses the
// conversation and refreshes session metadata.
func TestSyncShelleySameSecondMetadataRewrite(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:10Z", []shelleyMsg{
			{1, "user", `{"Role":0,"Content":[{"Type":2,"Text":"q"}]}`,
				"", "2026-06-15T10:00:00Z"},
			{2, "agent", `{"Role":1,"Content":[{"Type":2,"Text":"answer"}]}`,
				"", "2026-06-15T10:00:10Z"},
		})

	engine, database := newShelleyEngine(t, dir)
	require.False(t, engine.SyncAll(context.Background(), nil).Aborted)
	before, err := database.GetSessionFull(
		context.Background(), "shelley:cMAIN1",
	)
	require.NoError(t, err, "GetSessionFull before metadata rewrite")
	require.NotNil(t, before, "session before metadata rewrite")
	require.NotNil(t, before.SessionName, "session_name before rewrite")
	require.NotNil(t, before.FileMtime, "file_mtime before rewrite")
	require.NotNil(t, before.FileHash, "file_hash before rewrite")
	assert.Equal(t, "main", *before.SessionName)
	assert.Equal(t, "app", before.Project)

	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE conversations
		    SET slug = 'renamed',
		        cwd = '/home/u/dev/renamed-app'
		  WHERE conversation_id = 'cMAIN1'`,
	)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.False(t, engine.SyncAll(context.Background(), nil).Aborted)
	after, err := database.GetSessionFull(
		context.Background(), "shelley:cMAIN1",
	)
	require.NoError(t, err, "GetSessionFull after metadata rewrite")
	require.NotNil(t, after, "session after metadata rewrite")
	require.NotNil(t, after.SessionName, "session_name after rewrite")
	require.NotNil(t, after.FileMtime, "file_mtime after rewrite")
	require.NotNil(t, after.FileHash, "file_hash after rewrite")
	assert.Equal(t, *before.FileMtime, *after.FileMtime,
		"same-second metadata rewrite keeps the real updated_at timestamp")
	assert.NotEqual(t, *before.FileHash, *after.FileHash,
		"metadata rewrite must change the Shelley fingerprint")
	assert.Equal(t, "renamed", *after.SessionName)
	assert.Equal(t, "renamed_app", after.Project)
}

// TestSyncShelleyLengthPreservingRewrite is the case a byte-length signal
// cannot catch: a same-second in-place edit that changes content while
// keeping the exact byte length (and sequence_id and updated_at). Only a
// real content digest detects it, so the re-sync must replace the stored
// transcript rather than skip it.
func TestSyncShelleyLengthPreservingRewrite(t *testing.T) {
	dir := t.TempDir()
	dbPath := createShelleyDB(t, dir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:10Z", []shelleyMsg{
			{1, "user", `{"Role":0,"Content":[{"Type":2,"Text":"q"}]}`,
				"", "2026-06-15T10:00:00Z"},
			{2, "agent", `{"Role":1,"Content":[{"Type":2,"Text":"answer aaaa"}]}`,
				"", "2026-06-15T10:00:10Z"},
		})

	engine, database := newShelleyEngine(t, dir)
	require.False(t, engine.SyncAll(context.Background(), nil).Aborted)
	assertMessageContent(t, database, "shelley:cMAIN1", "q", "answer aaaa")

	// Same byte length, different content. updated_at and sequence_id are
	// untouched, so a byte-length signal would skip this as unchanged.
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE messages SET llm_data = ? WHERE conversation_id = 'cMAIN1' AND sequence_id = 2`,
		`{"Role":1,"Content":[{"Type":2,"Text":"answer bbbb"}]}`,
	)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.False(t, engine.SyncAll(context.Background(), nil).Aborted)
	assertSessionMessageCount(t, database, "shelley:cMAIN1", 2)
	assertMessageContent(t, database, "shelley:cMAIN1", "q", "answer bbbb")
}
