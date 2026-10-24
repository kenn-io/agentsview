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
