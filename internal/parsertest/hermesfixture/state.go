package hermesfixture

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// OpenState creates a synthetic WAL-mode home and leaves the writer open.
// The schema mirrors the pinned Hermes state sessions/message columns.
func OpenState(t *testing.T, home string) *sql.DB {
	t.Helper()
	writer, err := sql.Open("sqlite3", filepath.Join(home, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	_, err = writer.ExecContext(t.Context(), `PRAGMA journal_mode=WAL;
CREATE TABLE sessions (
 id TEXT PRIMARY KEY, source TEXT NOT NULL DEFAULT 'cli', model TEXT DEFAULT 'fixture-model',
 parent_session_id TEXT, started_at REAL NOT NULL DEFAULT 1778767200, ended_at REAL DEFAULT 1778767800,
 message_count INTEGER DEFAULT 0, input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
 cache_read_tokens INTEGER DEFAULT 0, cache_write_tokens INTEGER DEFAULT 0, reasoning_tokens INTEGER DEFAULT 0,
 estimated_cost_usd REAL, actual_cost_usd REAL, cost_status TEXT, cost_source TEXT, title TEXT, api_call_count INTEGER DEFAULT 0
);
CREATE TABLE messages (
 id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT,
 tool_call_id TEXT, tool_calls TEXT, timestamp REAL NOT NULL DEFAULT 1778767210,
 finish_reason TEXT, reasoning TEXT, reasoning_content TEXT, reasoning_details TEXT,
 codex_reasoning_items TEXT, codex_message_items TEXT
);`)
	require.NoError(t, err)
	CreateLedger(t, writer)
	return writer
}
