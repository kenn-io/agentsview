package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteURIPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "/data/opencode.db", want: "/data/opencode.db"},
		{name: "hash", in: "/data/pro#ject/x.db", want: "/data/pro%23ject/x.db"},
		{name: "question mark", in: "/data/a?b/x.db", want: "/data/a%3Fb/x.db"},
		{name: "percent", in: "/data/100%/x.db", want: "/data/100%25/x.db"},
		{
			name: "percent sequence stays literal",
			in:   "/data/a%3Fb/x.db",
			want: "/data/a%253Fb/x.db",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sqliteURIPath(tc.in))
		})
	}
}

func TestOpenSQLiteWithSpecialCharPath(t *testing.T) {
	// '#' would end the URI path (dropping mode=ro into the fragment) and
	// '%41' would percent-decode to 'A' if the path were not escaped.
	dir := filepath.Join(t.TempDir(), "pro#ject %41")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	dbPath := filepath.Join(dir, "opencode.db")

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec("CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1)")
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	db, err := openOpenCodeDB(dbPath)
	require.NoError(t, err)
	defer db.Close()

	var n int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM t").Scan(&n))
	assert.Equal(t, 1, n)

	_, err = db.Exec("INSERT INTO t VALUES (2)")
	require.Error(t, err, "mode=ro must survive special characters in the path")
}

func TestOpenSQLiteReadOnlyLiveWAL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	_, err = writer.Exec(`PRAGMA journal_mode=WAL;
		PRAGMA wal_autocheckpoint=0;
		CREATE TABLE messages (content TEXT);
		INSERT INTO messages VALUES ('committed in WAL')`)
	require.NoError(t, err)

	reader, err := openSQLiteReadOnly(dbPath, sqliteReadOptions{busyTimeoutMS: 3000})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	var content string
	require.NoError(t, reader.QueryRow("SELECT content FROM messages").Scan(&content))
	assert.Equal(t, "committed in WAL", content)

	_, err = writer.Exec("UPDATE messages SET content = 'later commit'")
	require.NoError(t, err)
	require.NoError(t, reader.QueryRow("SELECT content FROM messages").Scan(&content))
	assert.Equal(t, "later commit", content)
	_, err = reader.Exec("DELETE FROM messages")
	require.Error(t, err)
}

func TestOpenSQLiteReadOnlyStableSnapshot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "snapshot.db")
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = writer.Exec(`PRAGMA journal_mode=WAL;
		CREATE TABLE messages (content TEXT);
		INSERT INTO messages VALUES ('snapshot content')`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	reader, err := openSQLiteReadOnly(dbPath, sqliteReadOptions{
		stableSnapshot: true,
		busyTimeoutMS:  3000,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	var content string
	require.NoError(t, reader.QueryRow("SELECT content FROM messages").Scan(&content))
	assert.Equal(t, "snapshot content", content)
	// Immutable archive copies must not create live WAL coordination files.
	assert.NoFileExists(t, dbPath+"-wal")
	assert.NoFileExists(t, dbPath+"-shm")
	_, err = reader.Exec("DELETE FROM messages")
	require.Error(t, err)
}
