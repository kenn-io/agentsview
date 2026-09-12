package parser

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sqliteRowObserverTestSchema = `
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY
	);
	CREATE TABLE messages (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL
	);
`

func newSQLiteRowObserverTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	_, err = database.Exec(sqliteRowObserverTestSchema)
	require.NoError(t, err)
	return dbPath, database
}

func sqliteRowObserverTestSpec() sqliteRowObserverSpec {
	return sqliteRowObserverSpec{
		open: func(dbPath string, _ bool) (*sql.DB, error) {
			return sql.Open("sqlite3", "file:"+sqliteURIPath(dbPath)+"?mode=ro")
		},
		schemaIdentity: func(context.Context, *sql.DB) (string, error) {
			return "v1", nil
		},
		tables: func(context.Context, *sql.DB) ([]sqliteObservedTable, error) {
			return []sqliteObservedTable{
				{
					name: "sessions", cursorExpression: "rowid",
					sessionIDExpression: "id", identityExpression: "CAST(id AS TEXT)",
				},
				{
					name: "messages", cursorExpression: "rowid",
					sessionIDExpression: "session_id",
					identityExpression:  "session_id || char(31) || id",
				},
			}, nil
		},
	}
}

func TestSQLiteRowObserverColdStartAndIncrementalRows(t *testing.T) {
	dbPath, database := newSQLiteRowObserverTestDB(t)
	_, err := database.Exec(`INSERT INTO sessions (id) VALUES ('a'), ('b')`)
	require.NoError(t, err)
	observer := newSQLiteRowObserver(sqliteRowObserverTestSpec())

	ids, cold, snapshot, err := observer.changedSessionIDs(
		t.Context(), dbPath, false,
	)
	require.NoError(t, err)
	assert.True(t, cold)
	assert.Empty(t, ids)
	_, stillCold, _, err := observer.changedSessionIDs(
		t.Context(), dbPath, false,
	)
	require.NoError(t, err)
	assert.True(t, stillCold,
		"a failed enumeration that withholds commit must remain cold")
	observer.commit(dbPath, snapshot)

	_, err = database.Exec(`
		INSERT INTO messages (id, session_id) VALUES ('m-1', 'b');
		INSERT INTO sessions (id) VALUES ('c');
		INSERT INTO messages (id, session_id) VALUES ('m-2', 'c');
	`)
	require.NoError(t, err)
	ids, cold, _, err = observer.changedSessionIDs(t.Context(), dbPath, false)
	require.NoError(t, err)
	assert.False(t, cold)
	assert.Equal(t, []string{"b", "c"}, ids)
}

func TestSQLiteRowObserverKeepsRowsCommittedAfterDiscoveryCapture(t *testing.T) {
	dbPath, database := newSQLiteRowObserverTestDB(t)
	_, err := database.Exec(`INSERT INTO sessions (id) VALUES ('a')`)
	require.NoError(t, err)
	observer := newSQLiteRowObserver(sqliteRowObserverTestSpec())

	discoverySnapshot, err := observer.capture(t.Context(), dbPath, false)
	require.NoError(t, err)
	_, err = database.Exec(
		`INSERT INTO messages (id, session_id) VALUES ('m-1', 'a')`,
	)
	require.NoError(t, err)
	observer.commit(dbPath, discoverySnapshot)

	ids, cold, _, err := observer.changedSessionIDs(t.Context(), dbPath, false)
	require.NoError(t, err)
	assert.False(t, cold)
	assert.Equal(t, []string{"a"}, ids,
		"a row committed after discovery capture must remain visible")
}

func TestSQLiteRowObserverInvalidCursorDoesNotHideOtherTableAppends(t *testing.T) {
	dbPath, database := newSQLiteRowObserverTestDB(t)
	_, err := database.Exec(`
		INSERT INTO sessions (id) VALUES ('a'), ('b');
		INSERT INTO messages (id, session_id) VALUES ('m-1', 'a'), ('m-2', 'b');
	`)
	require.NoError(t, err)
	observer := newSQLiteRowObserver(sqliteRowObserverTestSpec())
	_, cold, snapshot, err := observer.changedSessionIDs(
		t.Context(), dbPath, false,
	)
	require.NoError(t, err)
	require.True(t, cold)
	observer.commit(dbPath, snapshot)

	_, err = database.Exec(`
		DELETE FROM messages WHERE id = 'm-2';
		INSERT INTO sessions (id) VALUES ('c');
	`)
	require.NoError(t, err)
	ids, cold, _, err := observer.changedSessionIDs(t.Context(), dbPath, false)
	require.NoError(t, err)
	assert.False(t, cold)
	assert.Equal(t, []string{"c"}, ids,
		"a retreated table must not suppress appends from another table")
}

func TestSQLiteRowObserverDoesNotRetreatPublishedCursors(t *testing.T) {
	dbPath, database := newSQLiteRowObserverTestDB(t)
	_, err := database.Exec(`INSERT INTO sessions (id) VALUES ('a'), ('b')`)
	require.NoError(t, err)
	observer := newSQLiteRowObserver(sqliteRowObserverTestSpec())

	_, cold, staleSnapshot, err := observer.changedSessionIDs(
		t.Context(), dbPath, false,
	)
	require.NoError(t, err)
	require.True(t, cold)
	observer.commit(dbPath, staleSnapshot)

	_, err = database.Exec(
		`INSERT INTO messages (id, session_id) VALUES ('m-1', 'a')`,
	)
	require.NoError(t, err)
	ids, cold, currentSnapshot, err := observer.changedSessionIDs(
		t.Context(), dbPath, false,
	)
	require.NoError(t, err)
	require.False(t, cold)
	require.Equal(t, []string{"a"}, ids)
	observer.commit(dbPath, currentSnapshot)

	// Simulate a slower discovery pass publishing its older pre-enumeration
	// snapshot after the watcher already published a newer cursor.
	observer.commit(dbPath, staleSnapshot)
	_, err = database.Exec(
		`INSERT INTO messages (id, session_id) VALUES ('m-2', 'b')`,
	)
	require.NoError(t, err)
	ids, cold, _, err = observer.changedSessionIDs(t.Context(), dbPath, false)
	require.NoError(t, err)
	assert.False(t, cold)
	assert.Equal(t, []string{"b"}, ids)
}

func TestSQLiteRowObserverTreatsDatabaseReplacementAsCold(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SQLite file identity is unavailable on Windows")
	}
	dbPath, database := newSQLiteRowObserverTestDB(t)
	_, err := database.Exec(`INSERT INTO sessions (id) VALUES ('old')`)
	require.NoError(t, err)
	observer := newSQLiteRowObserver(sqliteRowObserverTestSpec())
	_, cold, snapshot, err := observer.changedSessionIDs(
		t.Context(), dbPath, false,
	)
	require.NoError(t, err)
	require.True(t, cold)
	observer.commit(dbPath, snapshot)

	require.NoError(t, database.Close())
	require.NoError(t, os.Rename(dbPath, dbPath+".old"))
	replacement, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = replacement.Close() })
	_, err = replacement.Exec(sqliteRowObserverTestSchema)
	require.NoError(t, err)
	_, err = replacement.Exec(`INSERT INTO sessions (id) VALUES ('new')`)
	require.NoError(t, err)

	ids, cold, _, err := observer.changedSessionIDs(t.Context(), dbPath, false)
	require.NoError(t, err)
	assert.True(t, cold)
	assert.Empty(t, ids)
}
