package db

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageSessionCoveringIndexColumns(t *testing.T) {
	database := testDB(t)

	rows, err := database.getReader().Query(
		`SELECT name FROM pragma_index_info('idx_messages_usage_session_covering')
		 ORDER BY seqno`,
	)
	require.NoError(t, err)
	defer rows.Close()

	var got []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		got = append(got, name)
	}
	require.NoError(t, rows.Err())

	want := []string{
		"session_id", "ordinal", "timestamp", "role", "model",
		"claude_message_id", "claude_request_id", "token_usage", "source_uuid",
	}
	assert.Equal(t, want, got)
}

func TestUsageIndexesConcurrentRepair(t *testing.T) {
	database := testDB(t)
	_, err := database.getWriter().Exec(`
		DROP INDEX idx_messages_usage_session_covering;
		CREATE INDEX idx_messages_usage_session_covering
		ON messages(session_id, ordinal, timestamp, model)
		WHERE token_usage != '' AND model != '' AND model != '<synthetic>'`)
	require.NoError(t, err)
	database.writer.Load().SetMaxOpenConns(2)
	start := make(chan struct{})
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			<-start
			errors <- ensureUsageIndexesLocked(database.getWriter())
		})
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	rows, err := database.getReader().Query(
		`SELECT name FROM pragma_index_info('idx_messages_usage_session_covering')
		 ORDER BY seqno`)
	require.NoError(t, err)
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		columns = append(columns, column)
	}
	assert.Equal(t, usageSessionCoveringIndexColumns, columns)
}

func TestUsageIndexesMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	d, err := Open(path)
	requireNoError(t, err, "initial open")
	insertSession(t, d, "s1", "proj")
	d.Close()

	requireIndexPresence(t, path, "idx_messages_usage_covering", 0)
	requireIndexPresence(t, path, "idx_messages_claude_snapshot", 1)
	requireIndexPresence(t, path, "idx_messages_activity_timestamp", 1)
	requireIndexPresence(t, path, "idx_messages_usage_session_covering", 1)
	requireIndexPresence(t, path, "idx_messages_usage_timestamp", 1)

	conn, err := sql.Open("sqlite3", path)
	requireNoError(t, err, "raw open")
	_, err = conn.Exec(`DROP INDEX IF EXISTS idx_messages_usage_covering`)
	requireNoError(t, err, "drop covering index")
	_, err = conn.Exec(`CREATE INDEX idx_messages_usage_covering
		ON messages(timestamp, session_id, ordinal, model,
		            claude_message_id, claude_request_id, token_usage)
		WHERE token_usage != '' AND model != '' AND model != '<synthetic>'`)
	requireNoError(t, err, "recreate stale covering index")
	_, err = conn.Exec(`DROP INDEX IF EXISTS idx_messages_claude_snapshot`)
	requireNoError(t, err, "drop Claude snapshot index")
	_, err = conn.Exec(`DROP INDEX IF EXISTS idx_messages_usage_timestamp`)
	requireNoError(t, err, "drop current timestamp index")
	_, err = conn.Exec(`CREATE INDEX idx_messages_usage_timestamp
		ON messages(timestamp, session_id, ordinal)
		WHERE token_usage != '' AND model != '' AND model != '<synthetic>'`)
	requireNoError(t, err, "recreate legacy index")
	_, err = conn.Exec(`DROP INDEX IF EXISTS idx_messages_activity_timestamp`)
	requireNoError(t, err, "drop current activity index")
	_, err = conn.Exec(`CREATE INDEX idx_messages_activity_timestamp
		ON messages(timestamp, session_id, ordinal)
		WHERE role = 'assistant' AND model != '<synthetic>'`)
	requireNoError(t, err, "recreate stale activity index")
	conn.Close()

	d, err = Open(path)
	requireNoError(t, err, "reopen")
	defer d.Close()

	requireIndexPresence(t, path, "idx_messages_usage_covering", 0)
	requireIndexPresence(t, path, "idx_messages_claude_snapshot", 1)
	requireIndexPresence(t, path, "idx_messages_activity_timestamp", 1)
	requireIndexPresence(t, path, "idx_messages_usage_session_covering", 1)
	requireIndexPresence(t, path, "idx_messages_usage_timestamp", 1)

	rows, err := d.getReader().Query(
		`SELECT name FROM pragma_index_info('idx_messages_usage_session_covering')
		 ORDER BY seqno`,
	)
	requireNoError(t, err, "read migrated covering columns")
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		requireNoError(t, rows.Scan(&name), "scan migrated covering column")
		columns = append(columns, name)
	}
	requireNoError(t, rows.Err(), "iterate migrated covering columns")
	require.Equal(t, []string{
		"session_id", "ordinal", "timestamp", "role", "model",
		"claude_message_id", "claude_request_id", "token_usage", "source_uuid",
	}, columns)
	assertUsageIndexColumns(t, d, "idx_messages_activity_timestamp",
		[]string{"timestamp", "session_id", "ordinal", "model"})

	var sessions int
	row := d.reader.Load().QueryRow(`SELECT count(*) FROM sessions`)
	requireNoError(t, row.Scan(&sessions), "count sessions")
	require.Equal(t, 1, sessions, "session row must survive migration")
}

func assertUsageIndexColumns(t *testing.T, d *DB, index string, want []string) {
	t.Helper()
	rows, err := d.getReader().Query(
		`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	requireNoError(t, err, "read index columns")
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		requireNoError(t, rows.Scan(&name), "scan index column")
		got = append(got, name)
	}
	requireNoError(t, rows.Err(), "iterate index columns")
	require.Equal(t, want, got, "index %s columns", index)
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
