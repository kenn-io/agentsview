//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/storage"
)

// Existing mirrors must gain stored usage before serving, without changing
// source rows; later pushes must compute the counters without a client change.
func TestClickHouseStoredUsageUpgrade(t *testing.T) {
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	for _, table := range mirrorTables {
		_, err := conn.ExecContext(ctx, table.createSQL())
		require.NoError(t, err)
	}
	const tokens = `{"input_tokens":12,"output_tokens":34,"cache_creation_input_tokens":56,"cache_creation":{"ephemeral_1h_input_tokens":7},"cache_read_input_tokens":8,"reasoning_tokens":9,"server_tool_use":{"web_search_requests":10}}`
	_, err := conn.ExecContext(ctx,
		"INSERT INTO messages (session_id, ordinal, token_usage, push_version) VALUES (?, ?, ?, ?)", "upgrade", int64(0), tokens, uint64(1))
	require.NoError(t, err)
	_, err = NewStore(ctx, Target{URL: dsn, Database: database})
	require.ErrorContains(t, err, "missing")
	store, err := (Backend{}).OpenServeStore(ctx, storage.ReplicaTarget{URL: dsn, Schema: database})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	var raw string
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT token_usage FROM messages WHERE session_id = 'upgrade'").Scan(&raw))
	require.Equal(t, tokens, raw)
	var messageColumns int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count() FROM system.columns
		WHERE database = currentDatabase() AND table = 'messages' AND name LIKE 'usage_%'`).Scan(&messageColumns))
	require.Zero(t, messageColumns, "the source table must keep only the raw JSON")
	var present uint8
	var input, output, create, create1h, read, reasoning, web int64
	err = conn.QueryRowContext(ctx, `SELECT usage_present, usage_input,
		usage_output, usage_cache_create, usage_cache_create_1h, usage_cache_read,
		usage_reasoning, usage_web FROM usage_messages WHERE session_id = 'upgrade'`).Scan(
		&present, &input, &output, &create, &create1h, &read, &reasoning, &web)
	require.NoError(t, err, "startup must copy messages that existed before the view")
	require.Equal(t, uint8(1), present)
	require.Equal(t, []int64{12, 34, 56, 7, 8, 9, 10}, []int64{input, output, create, create1h, read, reasoning, web})
	// A completed fill must not run again: remove its row and check that the
	// next startup leaves the table alone.
	_, err = conn.ExecContext(ctx,
		"DELETE FROM usage_messages WHERE session_id = 'upgrade' SETTINGS mutations_sync = 1")
	require.NoError(t, err)
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	require.Zero(t, chtest.Count(t, conn, "usage_messages", "session_id = 'upgrade'"))
	_, err = conn.ExecContext(ctx,
		"INSERT INTO messages (session_id, ordinal, token_usage, push_version) VALUES (?, ?, ?, ?)", "upgrade", int64(0), `{"input_tokens":99}`, uint64(2))
	require.NoError(t, err)
	err = conn.QueryRowContext(ctx, "SELECT usage_input, usage_output FROM usage_messages WHERE session_id = 'upgrade'").Scan(&input, &output)
	require.NoError(t, err, "the view must store usage for new pushes")
	require.Equal(t, int64(99), input)
	require.Zero(t, output)
	require.Equal(t, 1, chtest.Count(t, conn, "messages", ""))
}
