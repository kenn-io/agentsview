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

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/sync"
)

// Timestamp constants for test data.
const (
	tsZero    = "2024-01-01T00:00:00Z"
	tsZeroS5  = "2024-01-01T00:00:05Z"
	tsEarly   = "2024-01-01T10:00:00Z"
	tsEarlyS1 = "2024-01-01T10:00:01Z"
	tsEarlyS5 = "2024-01-01T10:00:05Z"
)

// --- Assertion Helpers ---

func assertSessionState(t *testing.T, database *db.DB, sessionID string, check func(*db.Session)) {
	t.Helper()
	sess, err := database.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetSession(%q): %v", sessionID, err)
	}
	if sess == nil {
		t.Fatalf("Session %q not found", sessionID)
		return
	}
	if check != nil {
		check(sess)
	}
}

func assertSessionMessageCount(t *testing.T, database *db.DB, sessionID string, want int) {
	t.Helper()
	assertSessionState(t, database, sessionID, func(sess *db.Session) {
		if sess.MessageCount != want {
			t.Errorf("session %q message_count = %d, want %d", sessionID, sess.MessageCount, want)
		}
	})
}

func assertSessionProject(t *testing.T, database *db.DB, sessionID string, want string) {
	t.Helper()
	assertSessionState(t, database, sessionID, func(sess *db.Session) {
		if sess.Project != want {
			t.Errorf("session %q project = %q, want %q", sessionID, sess.Project, want)
		}
	})
}

func runSyncAndAssert(t *testing.T, engine *sync.Engine, want sync.SyncStats) sync.SyncStats {
	t.Helper()
	stats := engine.SyncAll(context.Background(), nil)
	if diff := cmp.Diff(want, stats,
		cmpopts.IgnoreUnexported(sync.SyncStats{}),
	); diff != "" {
		t.Fatalf("SyncAll() mismatch (-want +got):\n%s", diff)
	}
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
	if err != nil {
		t.Fatalf(
			"clear mtime for %s: %v", sessionID, err,
		)
	}

	if err := e.engine.SyncSingleSession(sessionID); err != nil {
		t.Fatalf("SyncSingleSession: %v", err)
	}

	_, mtime, ok := e.db.GetSessionFileInfo(sessionID)
	if !ok {
		t.Fatal("session file info not found")
	}
	if mtime == 0 {
		t.Error("SyncSingleSession did not store mtime")
	}

	runSyncAndAssert(t, e.engine, sync.SyncStats{TotalSessions: 0 + 1, Synced: 0, Skipped: 1})
}

func fetchMessages(t *testing.T, database *db.DB, sessionID string) []db.Message {
	t.Helper()
	msgs, err := database.GetAllMessages(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetAllMessages(%q): %v", sessionID, err)
	}
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
	if len(msgs) != len(wantRoles) {
		t.Fatalf("got %d messages, want %d",
			len(msgs), len(wantRoles))
	}
	for i, want := range wantRoles {
		if msgs[i].Role != want {
			t.Errorf("msgs[%d].Role = %q, want %q",
				i, msgs[i].Role, want)
		}
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
	if len(msgs) != len(wantContent) {
		t.Fatalf("got %d messages, want %d",
			len(msgs), len(wantContent))
	}
	for i, want := range wantContent {
		if msgs[i].Content != want {
			t.Errorf("msgs[%d].Content = %q, want %q",
				i, msgs[i].Content, want)
		}
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
	if err != nil {
		t.Fatalf("count tool_calls for %q: %v",
			sessionID, err)
	}
	if got != want {
		t.Errorf("tool_calls count for %q = %d, want %d",
			sessionID, got, want)
	}
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
	if err != nil {
		t.Fatalf("GetSessionFull: %v", err)
	}
	if sess == nil {
		t.Fatalf("session %q not found", sessionID)
		return
	}
	sess.Project = project
	if err := e.db.UpsertSession(*sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
}

// openCodeTestDB manages an OpenCode SQLite database for tests.
type openCodeTestDB struct {
	path string
	db   *sql.DB
}

// createOpenCodeDB creates a minimal OpenCode SQLite database
// with the required schema (project, session, message, part
// tables). Returns a handle for inserting test data.
func createOpenCodeDB(t *testing.T, dir string) *openCodeTestDB {
	t.Helper()
	path := filepath.Join(dir, "opencode.db")
	d, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("opening opencode test db: %v", err)
	}
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
	if _, err := d.Exec(schema); err != nil {
		t.Fatalf("creating opencode schema: %v", err)
	}
	return &openCodeTestDB{path: path, db: d}
}

func (oc *openCodeTestDB) mustExec(t *testing.T, msg, query string, args ...any) {
	t.Helper()
	if _, err := oc.db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
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
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
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
	if err != nil {
		t.Fatalf("marshal message update: %v", err)
	}
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
	if err != nil {
		t.Fatalf("marshal text part: %v", err)
	}
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
	if err != nil {
		t.Fatalf("marshal tool part: %v", err)
	}
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
	root string
}

func createOpenCodeStorageFixture(
	t *testing.T, root string,
) *openCodeStorageFixture {
	t.Helper()
	return &openCodeStorageFixture{root: root}
}

func (oc *openCodeStorageFixture) writeJSON(
	t *testing.T, path string, data any,
) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func (oc *openCodeStorageFixture) addSession(
	t *testing.T,
	projectID, sessionID, directory, title string,
	timeCreated, timeUpdated int64,
) string {
	t.Helper()
	return oc.writeJSON(t, filepath.Join(
		oc.root, "storage", "session", projectID,
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
