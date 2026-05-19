package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

const kiroSQLiteSchema = `
CREATE TABLE conversations_v2 (
	key TEXT NOT NULL,
	conversation_id TEXT NOT NULL,
	value TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (key, conversation_id)
);
`

func newKiroSQLiteTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), kiroSQLiteDBName)
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open kiro sqlite test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(kiroSQLiteSchema); err != nil {
		t.Fatalf("create kiro sqlite schema: %v", err)
	}
	return path, db
}

func readKiroFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(
		"testdata", "kiro_sqlite", name,
	))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

func seedKiroSQLiteSession(
	t *testing.T, db *sql.DB, key, id, payload string,
	createdAt, updatedAt int64,
) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO conversations_v2
			(key, conversation_id, value, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		key, id, payload, createdAt, updatedAt,
	); err != nil {
		t.Fatalf("seed kiro sqlite session: %v", err)
	}
}

func TestParseKiroSQLiteSession(t *testing.T) {
	dbPath, db := newKiroSQLiteTestDB(t)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/kiro-app", "sqlite-session",
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)

	sess, msgs, err := ParseKiroSQLiteSession(
		dbPath, "sqlite-session", "test-machine",
	)
	if err != nil {
		t.Fatalf("ParseKiroSQLiteSession: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session")
	}
	assertEq(t, "ID", sess.ID, "kiro:sqlite-session")
	assertEq(t, "Agent", sess.Agent, AgentKiro)
	assertEq(t, "Machine", sess.Machine, "test-machine")
	assertEq(t, "Project", sess.Project, "kiro_app")
	assertEq(t, "Cwd", sess.Cwd, "/home/user/code/kiro-app")
	assertEq(t, "FirstMessage", sess.FirstMessage, "Build the Kiro parser")
	assertEq(t, "MessageCount", sess.MessageCount, 4)
	assertEq(t, "UserMessageCount", sess.UserMessageCount, 2)
	assertEq(t, "File.Path", sess.File.Path, dbPath+"#sqlite-session")
	assertEq(t, "File.Mtime", sess.File.Mtime, int64(1779012030000)*1_000_000)

	assertEq(t, "messages len", len(msgs), 4)
	assertEq(t, "msg[0].Role", msgs[0].Role, RoleUser)
	assertEq(t, "msg[0].Content", msgs[0].Content, "Build the Kiro parser")
	assertEq(t, "msg[1].Role", msgs[1].Role, RoleAssistant)
	assertEq(t, "msg[1].Content", msgs[1].Content, "I can do that.")
	assertEq(t, "msg[3].HasToolUse", msgs[3].HasToolUse, true)
	assertEq(t, "msg[3].ToolCalls len", len(msgs[3].ToolCalls), 1)
	assertEq(t, "msg[3].ToolName", msgs[3].ToolCalls[0].ToolName, "execute_bash")
}

func TestListKiroSQLiteSessionMetaUsesNewestLogicalRow(t *testing.T) {
	dbPath, db := newKiroSQLiteTestDB(t)
	payload := readKiroFixture(t, "standard_payload.json")
	seedKiroSQLiteSession(
		t, db, "/tmp/old", "dupe-session", payload,
		1, 2,
	)
	seedKiroSQLiteSession(
		t, db, "/tmp/new", "dupe-session", payload,
		1, 9,
	)

	metas, err := ListKiroSQLiteSessionMeta(dbPath)
	if err != nil {
		t.Fatalf("ListKiroSQLiteSessionMeta: %v", err)
	}
	assertEq(t, "metas len", len(metas), 1)
	assertEq(t, "meta mtime", metas[0].FileMtime, int64(9_000_000))
}

func TestKiroSQLiteSourceMtime(t *testing.T) {
	dbPath, db := newKiroSQLiteTestDB(t)
	seedKiroSQLiteSession(
		t, db, "/tmp/project", "sqlite-session",
		readKiroFixture(t, "standard_payload.json"),
		1, 7,
	)
	mtime, err := KiroSQLiteSourceMtime(
		KiroSQLiteVirtualPath(dbPath, "sqlite-session"),
	)
	if err != nil {
		t.Fatalf("KiroSQLiteSourceMtime: %v", err)
	}
	assertEq(t, "mtime", mtime, int64(7_000_000))
}

func TestParseKiroSQLiteVirtualPath(t *testing.T) {
	tests := []struct {
		name      string
		dbPath    string
		sessionID string
	}{
		{
			name:      "ordinary path",
			dbPath:    "/tmp/kiro/data.sqlite3",
			sessionID: "sqlite-session",
		},
		{
			name:      "path containing hash",
			dbPath:    "/tmp/work#1/kiro/data.sqlite3",
			sessionID: "sqlite-session",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDB, gotID, ok := ParseKiroSQLiteVirtualPath(
				KiroSQLiteVirtualPath(tt.dbPath, tt.sessionID),
			)
			if !ok {
				t.Fatal("expected virtual path to parse")
			}
			assertEq(t, "dbPath", gotDB, tt.dbPath)
			assertEq(t, "sessionID", gotID, tt.sessionID)
		})
	}
}

func TestParseKiroSQLiteSessionRejectsMalformedPayload(t *testing.T) {
	dbPath, db := newKiroSQLiteTestDB(t)
	seedKiroSQLiteSession(
		t, db, "/tmp/project", "broken-session",
		readKiroFixture(t, "malformed_payload.txt"),
		1, 2,
	)
	if _, _, err := ParseKiroSQLiteSession(
		dbPath, "broken-session", "test-machine",
	); err == nil {
		t.Fatal("expected malformed payload error")
	}
}
