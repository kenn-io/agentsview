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
	require := require.New(t)

	// '#' would end the URI path (dropping mode=ro into the fragment) and
	// '%41' would percent-decode to 'A' if the path were not escaped.
	dir := filepath.Join(t.TempDir(), "pro#ject %41")
	require.NoError(os.MkdirAll(dir, 0o755))
	dbPath := filepath.Join(dir, "opencode.db")

	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = writer.ExecContext(t.Context(), "CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1)")
	require.NoError(err)
	require.NoError(writer.Close())

	db, err := openOpenCodeDB(dbPath)
	require.NoError(err)
	defer db.Close()

	var n int
	require.NoError(db.QueryRowContext(t.Context(), "SELECT count(*) FROM t").Scan(&n))
	assert.Equal(t, 1, n)

	_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES (2)")
	require.Error(err, "mode=ro must survive special characters in the path")
}

func TestOpenSQLiteReadOnlyStableSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dbPath := filepath.Join(t.TempDir(), "snapshot.db")
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = writer.ExecContext(t.Context(), `PRAGMA journal_mode=WAL;
		CREATE TABLE messages (content TEXT);
		INSERT INTO messages VALUES ('snapshot content')`)
	require.NoError(err)
	require.NoError(writer.Close())

	reader, err := openSQLiteReadOnly(dbPath, sqliteReadOptions{
		stableSnapshot: true,
		busyTimeoutMS:  3000,
	})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reader.Close()) })
	var content string
	require.NoError(reader.QueryRowContext(t.Context(), "SELECT content FROM messages").Scan(&content))
	assert.Equal("snapshot content", content)
	// Immutable archive copies must not create live WAL coordination files.
	assert.NoFileExists(dbPath + "-wal")
	assert.NoFileExists(dbPath + "-shm")
	_, err = reader.ExecContext(t.Context(), "DELETE FROM messages")
	require.Error(err)
}
