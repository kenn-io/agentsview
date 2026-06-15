package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageCoveringIndexMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	d, err := Open(path)
	requireNoError(t, err, "initial open")
	insertSession(t, d, "s1", "proj")
	d.Close()

	requireIndexPresence(t, path, "idx_messages_usage_covering", 1)
	requireIndexPresence(t, path, "idx_messages_usage_timestamp", 0)

	conn, err := sql.Open("sqlite3", path)
	requireNoError(t, err, "raw open")
	_, err = conn.Exec(`DROP INDEX IF EXISTS idx_messages_usage_covering`)
	requireNoError(t, err, "drop covering index")
	_, err = conn.Exec(`CREATE INDEX idx_messages_usage_timestamp
		ON messages(timestamp, session_id, ordinal)
		WHERE token_usage != '' AND model != '' AND model != '<synthetic>'`)
	requireNoError(t, err, "recreate legacy index")
	conn.Close()

	d, err = Open(path)
	requireNoError(t, err, "reopen")
	defer d.Close()

	requireIndexPresence(t, path, "idx_messages_usage_covering", 1)
	requireIndexPresence(t, path, "idx_messages_usage_timestamp", 0)

	var sessions int
	row := d.reader.Load().QueryRow(`SELECT count(*) FROM sessions`)
	requireNoError(t, row.Scan(&sessions), "count sessions")
	require.Equal(t, 1, sessions, "session row must survive migration")
}

func requireIndexPresence(t *testing.T, path, name string, want int) {
	t.Helper()
	conn, err := sql.Open("sqlite3", path)
	requireNoError(t, err, "raw open for index check")
	defer conn.Close()

	var got int
	err = conn.QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type = 'index' AND name = ?`, name,
	).Scan(&got)
	requireNoError(t, err, "query sqlite_master")
	require.Equal(t, want, got, "index %s presence", name)
}
