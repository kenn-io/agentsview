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
	// Resume after a startup that added one column but did not backfill it.
	_, err = conn.ExecContext(ctx, "ALTER TABLE messages ADD COLUMN usage_input Int64 MATERIALIZED JSONExtractInt(token_usage, 'input_tokens')")
	require.NoError(t, err)
	_, err = NewStore(ctx, Target{URL: dsn, Database: database})
	require.ErrorContains(t, err, "missing")
	store, err := (Backend{}).OpenServeStore(ctx, storage.ReplicaTarget{URL: dsn, Schema: database})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	var raw string
	var present uint8
	var input, output, create, create1h, read, reasoning, web int64
	err = conn.QueryRowContext(ctx, `SELECT token_usage, usage_present, usage_input,
		usage_output, usage_cache_create, usage_cache_create_1h, usage_cache_read,
		usage_reasoning, usage_web FROM messages WHERE session_id = 'upgrade'`).Scan(
		&raw, &present, &input, &output, &create, &create1h, &read, &reasoning, &web)
	require.NoError(t, err)
	require.Equal(t, tokens, raw)
	require.Equal(t, uint8(1), present)
	require.Equal(t, []int64{12, 34, 56, 7, 8, 9, 10}, []int64{input, output, create, create1h, read, reasoning, web})
	var storedOutput int64
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT usage_output FROM usage_messages WHERE session_id = 'upgrade'").Scan(&storedOutput))
	require.Equal(t, int64(34), storedOutput, "startup must copy messages that existed before the view")
	var stored int
	err = conn.QueryRowContext(ctx, `SELECT uniqExact(column) FROM system.parts_columns
		WHERE database = currentDatabase() AND table = 'messages' AND active
		AND column LIKE 'usage_%'`).Scan(&stored)
	require.NoError(t, err)
	require.Equal(t, 8, stored, "old rows must be stored, not parsed on each read")
	var mutationsBefore, mutationsAfter int
	err = conn.QueryRowContext(ctx, `SELECT count() FROM system.mutations
		WHERE database = currentDatabase() AND table = 'messages'`).Scan(&mutationsBefore)
	require.NoError(t, err)
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	err = conn.QueryRowContext(ctx, `SELECT count() FROM system.mutations
		WHERE database = currentDatabase() AND table = 'messages'`).Scan(&mutationsAfter)
	require.NoError(t, err)
	require.Equal(t, mutationsBefore, mutationsAfter, "completed backfill must not run again")
	_, err = conn.ExecContext(ctx,
		"INSERT INTO messages (session_id, ordinal, token_usage, push_version) VALUES (?, ?, ?, ?)", "upgrade", int64(0), `{"input_tokens":99}`, uint64(2))
	require.NoError(t, err)
	err = conn.QueryRowContext(ctx, "SELECT usage_input, usage_output FROM messages WHERE session_id = 'upgrade'").Scan(&input, &output)
	require.NoError(t, err)
	require.Equal(t, int64(99), input)
	require.Zero(t, output)
	require.Equal(t, 1, chtest.Count(t, conn, "messages", ""))
}
