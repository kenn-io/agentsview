package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "open kiro sqlite test db")
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(kiroSQLiteSchema)
	require.NoError(t, err, "create kiro sqlite schema")
	return path, db
}

func readKiroFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(
		"testdata", "kiro_sqlite", name,
	))
	require.NoError(t, err, "read fixture %s", name)
	return string(data)
}

func seedKiroSQLiteSession(
	t *testing.T, db *sql.DB, key, id, payload string,
	createdAt, updatedAt int64,
) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO conversations_v2
			(key, conversation_id, value, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		key, id, payload, createdAt, updatedAt,
	)
	require.NoError(t, err, "seed kiro sqlite session")
}

func TestParseKiroSQLiteSession(t *testing.T) {
	dbPath, db := newKiroSQLiteTestDB(t)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/kiro-app", "sqlite-session",
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)

	sess, msgs, err := parseKiroSQLiteSession(
		dbPath, "sqlite-session", "test-machine",
	)
	require.NoError(t, err, "parseKiroSQLiteSession")
	require.NotNil(t, sess, "expected session")
	assert.Equal(t, "kiro:sqlite-session", sess.ID, "ID")
	assert.Equal(t, AgentKiro, sess.Agent, "Agent")
	assert.Equal(t, "test-machine", sess.Machine, "Machine")
	assert.Equal(t, "kiro_app", sess.Project, "Project")
	assert.Equal(t, "/home/user/code/kiro-app", sess.Cwd, "Cwd")
	assert.Equal(t, "Build the Kiro parser", sess.FirstMessage, "FirstMessage")
	assert.Equal(t, 4, sess.MessageCount, "MessageCount")
	assert.Equal(t, 2, sess.UserMessageCount, "UserMessageCount")
	assert.Equal(t, dbPath+"#sqlite-session", sess.File.Path, "File.Path")
	assert.Equal(t, int64(1779012030000)*1_000_000, sess.File.Mtime, "File.Mtime")

	require.Len(t, msgs, 4, "messages len")
	assert.Equal(t, RoleUser, msgs[0].Role, "msg[0].Role")
	assert.Equal(t, "Build the Kiro parser", msgs[0].Content, "msg[0].Content")
	assert.Equal(t, RoleAssistant, msgs[1].Role, "msg[1].Role")
	assert.Equal(t, "I can do that.", msgs[1].Content, "msg[1].Content")
	assert.True(t, msgs[3].HasToolUse, "msg[3].HasToolUse")
	require.Len(t, msgs[3].ToolCalls, 1, "msg[3].ToolCalls len")
	assert.Equal(t, "execute_bash", msgs[3].ToolCalls[0].ToolName, "msg[3].ToolName")
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
	require.NoError(t, err, "ListKiroSQLiteSessionMeta")
	require.Len(t, metas, 1, "metas len")
	assert.Equal(t, int64(9_000_000), metas[0].FileMtime, "meta mtime")
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
	require.NoError(t, err, "KiroSQLiteSourceMtime")
	assert.Equal(t, int64(7_000_000), mtime, "mtime")
}

func TestKiroSQLiteVirtualPathParts(t *testing.T) {
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
			gotDB, gotID, ok := kiroSQLiteVirtualPathParts(
				KiroSQLiteVirtualPath(tt.dbPath, tt.sessionID),
			)
			require.True(t, ok, "expected virtual path to parse")
			assert.Equal(t, tt.dbPath, gotDB, "dbPath")
			assert.Equal(t, tt.sessionID, gotID, "sessionID")
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
	_, _, err := parseKiroSQLiteSession(
		dbPath, "broken-session", "test-machine",
	)
	require.Error(t, err, "expected malformed payload error")
}
