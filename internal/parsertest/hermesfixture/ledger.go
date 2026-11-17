// Package hermesfixture provides Hermes state fixtures without importing the parser.
package hermesfixture

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// CreateLedger creates the Hermes model usage ledger. Schema from the pinned producer:
// https://github.com/NousResearch/hermes-agent/blob/3ba2b0787ed3a73ec3b08085ed0137f235209cf4/hermes_state_usage.py
func CreateLedger(t *testing.T, conn *sql.DB) {
	t.Helper()
	_, err := conn.ExecContext(t.Context(), `CREATE TABLE session_model_usage (
		session_id TEXT, model TEXT, billing_provider TEXT DEFAULT '', billing_base_url TEXT DEFAULT '', billing_mode TEXT DEFAULT '', task TEXT DEFAULT '',
		api_call_count INTEGER DEFAULT 1, input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
		cache_read_tokens INTEGER DEFAULT 0, cache_write_tokens INTEGER DEFAULT 0, reasoning_tokens INTEGER DEFAULT 0,
		estimated_cost_usd REAL DEFAULT 0, actual_cost_usd REAL DEFAULT 0, cost_status TEXT, cost_source TEXT,
		first_seen REAL DEFAULT 1778767200, last_seen REAL DEFAULT 1778767800)`)
	require.NoError(t, err)
}

// CreateWatcherSchema adds native revision tracking and message indexes.
func CreateWatcherSchema(t *testing.T, conn *sql.DB) {
	t.Helper()
	_, err := conn.ExecContext(t.Context(), `ALTER TABLE sessions ADD COLUMN transcript_revision INTEGER DEFAULT 0;
 CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id,timestamp);
 CREATE INDEX idx_messages_session_id ON messages(session_id,id);
 CREATE TABLE schema_version(version INTEGER); INSERT INTO schema_version VALUES (33);
 CREATE TRIGGER messages_transcript_revision_update AFTER UPDATE OF role,content,api_content,tool_calls,tool_call_id,tool_name,reasoning,reasoning_content,reasoning_details ON messages BEGIN UPDATE sessions SET transcript_revision=transcript_revision+1 WHERE id=new.session_id; END;
 CREATE TRIGGER messages_display_revision_update AFTER UPDATE OF codex_reasoning_items,codex_message_items,display_kind,display_metadata ON messages BEGIN UPDATE sessions SET transcript_revision=transcript_revision+1 WHERE id=new.session_id; END;`)
	require.NoError(t, err)
}
