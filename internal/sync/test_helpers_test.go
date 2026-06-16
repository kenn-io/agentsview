package sync_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/sync"
)

// Timestamp constants for test data.
const (
	tsZero    = "2024-01-01T00:00:00Z"
	tsZeroS1  = "2024-01-01T00:00:01Z"
	tsZeroS5  = "2024-01-01T00:00:05Z"
	tsEarly   = "2024-01-01T10:00:00Z"
	tsEarlyS1 = "2024-01-01T10:00:01Z"
	tsEarlyS5 = "2024-01-01T10:00:05Z"
)

// --- Assertion Helpers ---

func assertSessionState(t *testing.T, database *db.DB, sessionID string, check func(*db.Session)) {
	t.Helper()
	sess, err := database.GetSession(context.Background(), sessionID)
	require.NoError(t, err, "GetSession(%q)", sessionID)
	require.NotNil(t, sess, "Session %q not found", sessionID)
	if check != nil {
		check(sess)
	}
}

func assertSessionMessageCount(t *testing.T, database *db.DB, sessionID string, want int) {
	t.Helper()
	assertSessionState(t, database, sessionID, func(sess *db.Session) {
		assert.Equal(t, want, sess.MessageCount, "session %q message_count", sessionID)
	})
}

func assertSessionProject(t *testing.T, database *db.DB, sessionID string, want string) {
	t.Helper()
	assertSessionState(t, database, sessionID, func(sess *db.Session) {
		assert.Equal(t, want, sess.Project, "session %q project", sessionID)
	})
}

func runSyncAndAssert(t *testing.T, engine *sync.Engine, want sync.SyncStats) sync.SyncStats {
	t.Helper()
	stats := engine.SyncAll(context.Background(), nil)
	diff := cmp.Diff(want, stats,
		cmpopts.IgnoreUnexported(sync.SyncStats{}),
	)
	require.Empty(t, diff, "SyncAll() mismatch (-want +got):\n%s", diff)
	return stats
}

// assertResyncRoundTrip clears file_mtime to force a resync,
// runs SyncSingleSession, and verifies the session is stored
// and a subsequent SyncAll skips.
func (e *testEnv) assertResyncRoundTrip(
	t *testing.T, sessionID string,
) {
	t.Helper()

	// Clear mtime to force resync on next check.
	err := e.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE sessions SET file_mtime = NULL"+
				" WHERE id = ?",
			sessionID,
		)
		return err
	})
	require.NoError(t, err, "clear mtime for %s", sessionID)

	require.NoError(t, e.engine.SyncSingleSession(sessionID))

	_, mtime, ok := e.db.GetSessionFileInfo(sessionID)
	require.True(t, ok, "session file info not found")
	assert.NotZero(t, mtime, "SyncSingleSession did not store mtime")

	runSyncAndAssert(t, e.engine, sync.SyncStats{TotalSessions: 0 + 1, Synced: 0, Skipped: 1})
}

func fetchMessages(t *testing.T, database *db.DB, sessionID string) []db.Message {
	t.Helper()
	msgs, err := database.GetAllMessages(context.Background(), sessionID)
	require.NoError(t, err, "GetAllMessages(%q)", sessionID)
	return msgs
}

// assertMessageRoles verifies that a session's messages have
// the expected roles in order.
func assertMessageRoles(
	t *testing.T, database *db.DB,
	sessionID string, wantRoles ...string,
) {
	t.Helper()
	msgs := fetchMessages(t, database, sessionID)
	require.Len(t, msgs, len(wantRoles))
	for i, want := range wantRoles {
		assert.Equal(t, want, msgs[i].Role, "msgs[%d].Role", i)
	}
}

// assertMessageContent verifies that a session's messages
// have the expected content strings in ordinal order.
func assertMessageContent(
	t *testing.T, database *db.DB,
	sessionID string, wantContent ...string,
) {
	t.Helper()
	msgs := fetchMessages(t, database, sessionID)
	require.Len(t, msgs, len(wantContent))
	for i, want := range wantContent {
		assert.Equal(t, want, msgs[i].Content, "msgs[%d].Content", i)
	}
}

// assertToolCallCount verifies that the total number of
// tool_calls rows for a session matches the expected count.
func assertToolCallCount(
	t *testing.T, database *db.DB,
	sessionID string, want int,
) {
	t.Helper()
	var got int
	err := database.Reader().QueryRow(
		"SELECT COUNT(*) FROM tool_calls"+
			" WHERE session_id = ?",
		sessionID,
	).Scan(&got)
	require.NoError(t, err, "count tool_calls for %q", sessionID)
	assert.Equal(t, want, got, "tool_calls count for %q", sessionID)
}

// updateSessionProject fetches the session, updates its
// Project field, and upserts it back. Reduces boilerplate
// for tests that need to override a single field.
func (e *testEnv) updateSessionProject(
	t *testing.T, sessionID, project string,
) {
	t.Helper()
	sess, err := e.db.GetSessionFull(
		context.Background(), sessionID,
	)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess, "session %q not found", sessionID)
	sess.Project = project
	require.NoError(t, e.db.UpsertSession(*sess), "UpsertSession")
}

// openCodeTestDB manages an OpenCode SQLite database for tests.
type openCodeTestDB struct {
	path string
	db   *sql.DB
}

type kiroSQLiteTestDB struct {
	path string
	db   *sql.DB
}

// createOpenCodeDB creates a minimal OpenCode SQLite database
// with the required schema (project, session, message, part
// tables). Returns a handle for inserting test data.
func createOpenCodeDB(t *testing.T, dir string) *openCodeTestDB {
	t.Helper()
	return createOpenCodeLikeDB(
		t, filepath.Join(dir, "opencode.db"), "opencode",
	)
}

func createKiloDB(t *testing.T, dir string) *openCodeTestDB {
	t.Helper()
	return createOpenCodeLikeDB(
		t, filepath.Join(dir, "kilo.db"), "kilo",
	)
}

func createOpenCodeLikeDB(
	t *testing.T, path, label string,
) *openCodeTestDB {
	t.Helper()
	d, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "opening %s test db", label)
	t.Cleanup(func() { d.Close() })

	schema := `
		CREATE TABLE project (
			id TEXT PRIMARY KEY,
			worktree TEXT NOT NULL
		);
		CREATE TABLE session (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			parent_id TEXT,
			title TEXT,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL
		);
		CREATE TABLE message (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			data TEXT NOT NULL,
			time_created INTEGER NOT NULL
		);
		CREATE TABLE part (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			message_id TEXT NOT NULL,
			data TEXT NOT NULL,
			time_created INTEGER NOT NULL
		);
	`
	_, err = d.Exec(schema)
	require.NoError(t, err, "creating %s schema", label)
	return &openCodeTestDB{path: path, db: d}
}

func createKiroSQLiteDB(t *testing.T, dir string) *kiroSQLiteTestDB {
	t.Helper()
	path := filepath.Join(dir, "data.sqlite3")
	d, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "opening kiro sqlite test db")
	t.Cleanup(func() { d.Close() })
	schema := `
		CREATE TABLE conversations_v2 (
			key TEXT NOT NULL,
			conversation_id TEXT NOT NULL,
			value TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (key, conversation_id)
		);
	`
	_, err = d.Exec(schema)
	require.NoError(t, err, "creating kiro sqlite schema")
	return &kiroSQLiteTestDB{path: path, db: d}
}

func readKiroSQLiteFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(
		"..", "parser", "testdata", "kiro_sqlite", name,
	)
	data, err := os.ReadFile(path)
	require.NoError(t, err, "read kiro sqlite fixture %s", name)
	return string(data)
}

func (ks *kiroSQLiteTestDB) addSession(
	t *testing.T, key, id, payload string,
	createdAt, updatedAt int64,
) {
	t.Helper()
	_, err := ks.db.Exec(
		`INSERT INTO conversations_v2
			(key, conversation_id, value, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		key, id, payload, createdAt, updatedAt,
	)
	require.NoError(t, err, "insert kiro sqlite session")
}

func (ks *kiroSQLiteTestDB) updateSession(
	t *testing.T, id, payload string, updatedAt int64,
) {
	t.Helper()
	_, err := ks.db.Exec(
		`UPDATE conversations_v2
		    SET value = ?, updated_at = ?
		  WHERE conversation_id = ?`,
		payload, updatedAt, id,
	)
	require.NoError(t, err, "update kiro sqlite session")
}

func writeLegacyKiroSession(
	t *testing.T, dir, id, prompt string,
) {
	t.Helper()
	jsonlPath := filepath.Join(dir, id+".jsonl")
	metaPath := filepath.Join(dir, id+".json")
	require.NoError(t, os.WriteFile(
		jsonlPath,
		[]byte(`{"kind":"Prompt","data":{"content":[{"kind":"text","data":"`+
			prompt+`"}]}}`+"\n"+
			`{"kind":"AssistantMessage","data":{"content":[{"kind":"text","data":"legacy assistant"}]}}`+
			"\n"),
		0o644,
	), "write legacy kiro jsonl")
	require.NoError(t, os.WriteFile(
		metaPath,
		[]byte(`{"session_id":"`+id+`","cwd":"/home/user/code/legacy-kiro","created_at":"2026-05-17T09:00:00Z","updated_at":"2026-05-17T09:01:00Z"}`),
		0o644,
	), "write legacy kiro metadata")
}

func (oc *openCodeTestDB) mustExec(t *testing.T, msg, query string, args ...any) {
	t.Helper()
	_, err := oc.db.Exec(query, args...)
	require.NoError(t, err, msg)
}

func (oc *openCodeTestDB) addProject(
	t *testing.T, id, worktree string,
) {
	t.Helper()
	oc.mustExec(t, "insert project",
		"INSERT INTO project (id, worktree) VALUES (?, ?)",
		id, worktree,
	)
}

func (oc *openCodeTestDB) addSession(
	t *testing.T,
	id, projectID string,
	timeCreated, timeUpdated int64,
) {
	t.Helper()
	oc.mustExec(t, "insert session",
		`INSERT INTO session
			(id, project_id, time_created, time_updated)
		 VALUES (?, ?, ?, ?)`,
		id, projectID, timeCreated, timeUpdated,
	)
}

func (oc *openCodeTestDB) updateSessionTime(
	t *testing.T, id string, timeUpdated int64,
) {
	t.Helper()
	oc.mustExec(t, "update session time",
		"UPDATE session SET time_updated = ? WHERE id = ?",
		timeUpdated, id,
	)
}

func (oc *openCodeTestDB) addMessage(
	t *testing.T,
	id, sessionID, role string,
	timeCreated int64,
) {
	t.Helper()
	data, err := json.Marshal(map[string]string{
		"role": role,
	})
	require.NoError(t, err, "marshal message")
	oc.mustExec(t, "insert message",
		`INSERT INTO message
			(id, session_id, data, time_created)
		 VALUES (?, ?, ?, ?)`,
		id, sessionID, string(data), timeCreated,
	)
}

func (oc *openCodeTestDB) updateMessageData(
	t *testing.T, id string, data map[string]any,
) {
	t.Helper()
	raw, err := json.Marshal(data)
	require.NoError(t, err, "marshal message update")
	oc.mustExec(t, "update message data",
		"UPDATE message SET data = ? WHERE id = ?",
		string(raw), id,
	)
}

func (oc *openCodeTestDB) addTextPart(
	t *testing.T,
	id, sessionID, messageID, content string,
	timeCreated int64,
) {
	t.Helper()
	data, err := json.Marshal(map[string]string{
		"type":    "text",
		"content": content,
	})
	require.NoError(t, err, "marshal text part")
	oc.mustExec(t, "insert part",
		`INSERT INTO part
			(id, session_id, message_id, data, time_created)
		 VALUES (?, ?, ?, ?, ?)`,
		id, sessionID, messageID, string(data), timeCreated,
	)
}

func (oc *openCodeTestDB) addToolPart(
	t *testing.T,
	id, sessionID, messageID string,
	toolName, callID string,
	timeCreated int64,
) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"type":   "tool",
		"tool":   toolName,
		"callID": callID,
	})
	require.NoError(t, err, "marshal tool part")
	oc.mustExec(t, "insert tool part",
		`INSERT INTO part
			(id, session_id, message_id, data, time_created)
		 VALUES (?, ?, ?, ?, ?)`,
		id, sessionID, messageID, string(data), timeCreated,
	)
}

func (oc *openCodeTestDB) deleteMessages(
	t *testing.T, sessionID string,
) {
	t.Helper()
	oc.mustExec(t, "delete messages",
		"DELETE FROM message WHERE session_id = ?",
		sessionID,
	)
}

func (oc *openCodeTestDB) deleteParts(
	t *testing.T, sessionID string,
) {
	t.Helper()
	oc.mustExec(t, "delete parts",
		"DELETE FROM part WHERE session_id = ?",
		sessionID,
	)
}

// replaceTextContent deletes all messages and parts for a
// session, then re-inserts them with new content but the same
// ordinal structure (user msg + assistant msg).
func (oc *openCodeTestDB) replaceTextContent(
	t *testing.T,
	sessionID string,
	userContent, assistantContent string,
	timeCreated int64,
) {
	t.Helper()
	oc.deleteMessages(t, sessionID)
	oc.deleteParts(t, sessionID)

	umID := fmt.Sprintf("%s-msg-user-v2", sessionID)
	amID := fmt.Sprintf("%s-msg-asst-v2", sessionID)
	oc.addMessage(t, umID, sessionID, "user", timeCreated)
	oc.addMessage(
		t, amID, sessionID, "assistant", timeCreated+1,
	)
	oc.addTextPart(
		t, umID+"-p", sessionID, umID,
		userContent, timeCreated,
	)
	oc.addTextPart(
		t, amID+"-p", sessionID, amID,
		assistantContent, timeCreated+1,
	)
}

type openCodeStorageFixture struct {
	root          string
	sessionSubdir string
}

func createOpenCodeStorageFixture(
	t *testing.T, root string,
) *openCodeStorageFixture {
	t.Helper()
	return &openCodeStorageFixture{root: root, sessionSubdir: "session"}
}

// createMiMoCodeStorageFixture builds a fixture for MiMoCode, which
// stores session JSON under storage/session_diff instead of
// storage/session while sharing the message/part layout.
func createMiMoCodeStorageFixture(
	t *testing.T, root string,
) *openCodeStorageFixture {
	t.Helper()
	return &openCodeStorageFixture{root: root, sessionSubdir: "session_diff"}
}

func (oc *openCodeStorageFixture) writeJSON(
	t *testing.T, path string, data any,
) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir %s", filepath.Dir(path))
	raw, err := json.Marshal(data)
	require.NoError(t, err, "marshal %s", path)
	require.NoError(t, os.WriteFile(path, raw, 0o644), "write %s", path)
	return path
}

func (oc *openCodeStorageFixture) addSession(
	t *testing.T,
	projectID, sessionID, directory, title string,
	timeCreated, timeUpdated int64,
) string {
	t.Helper()
	return oc.writeJSON(t, filepath.Join(
		oc.root, "storage", oc.sessionSubdir, projectID,
		sessionID+".json",
	), map[string]any{
		"id":        sessionID,
		"projectID": projectID,
		"directory": directory,
		"title":     title,
		"time": map[string]any{
			"created": timeCreated,
			"updated": timeUpdated,
		},
	})
}

func (oc *openCodeStorageFixture) addMessage(
	t *testing.T,
	sessionID, messageID, role string,
	timeCreated int64,
	extra map[string]any,
) string {
	t.Helper()
	data := map[string]any{
		"id":        messageID,
		"sessionID": sessionID,
		"role":      role,
		"time": map[string]any{
			"created": timeCreated,
		},
	}
	maps.Copy(data, extra)
	return oc.writeJSON(t, filepath.Join(
		oc.root, "storage", "message", sessionID,
		messageID+".json",
	), data)
}

func (oc *openCodeStorageFixture) addTextPart(
	t *testing.T,
	sessionID, messageID, partID, text string,
	timeCreated int64,
) string {
	t.Helper()
	return oc.writeJSON(t, filepath.Join(
		oc.root, "storage", "part", messageID,
		partID+".json",
	), map[string]any{
		"id":        partID,
		"sessionID": sessionID,
		"messageID": messageID,
		"type":      "text",
		"text":      text,
		"time": map[string]any{
			"created": timeCreated,
		},
	})
}
