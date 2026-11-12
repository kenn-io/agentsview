//go:build chtest

package clickhouse

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/storage"
)

// Existing mirrors must gain stored usage before serving, without changing
// source rows; later pushes must compute the counters without a client change.
func TestClickHouseStoredUsageUpgrade(t *testing.T) {
	t.Parallel()
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

// An older release stored the counters as MATERIALIZED columns of messages,
// and its view read them. The schema step must move the view to computing
// them from token_usage, so the mirror no longer depends on those columns.
func TestClickHouseStoredUsageViewUpgrade(t *testing.T) {
	t.Parallel()
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	conn := store.DB()
	stored := make([]string, 0, len(usageColumns))
	for _, column := range usageColumns {
		_, err := conn.ExecContext(ctx, "ALTER TABLE messages ADD COLUMN "+column.name+" "+column.typ+
			" MATERIALIZED "+column.expression)
		require.NoError(t, err)
		stored = append(stored, column.name)
	}
	for _, query := range []string{
		"DROP VIEW usage_messages_mv",
		"CREATE MATERIALIZED VIEW usage_messages_mv TO usage_messages AS SELECT " + usageMessageFields + ", " +
			strings.Join(stored, ", ") + ", toUInt128(push_version) * 2 + 1 AS revision FROM messages",
	} {
		_, err := conn.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	for _, column := range usageColumns {
		_, err := conn.ExecContext(ctx, "ALTER TABLE messages DROP COLUMN "+column.name)
		require.NoError(t, err)
	}
	_, err := conn.ExecContext(ctx, "INSERT INTO messages (session_id, ordinal, token_usage, push_version) VALUES (?, ?, ?, ?)",
		"view-upgrade", int64(0), `{"input_tokens":12,"output_tokens":34}`, uint64(1))
	require.NoError(t, err, "inserts must not need the dropped columns")
	var input, output int64
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT usage_input, usage_output FROM usage_messages WHERE session_id = 'view-upgrade'").Scan(&input, &output))
	require.Equal(t, []int64{12, 34}, []int64{input, output})
}

// A mirror whose usage_messages carries the usage_time minmax from an older
// release loses it on the next schema setup, and later setups queue no
// further drop.
func TestClickHouseUsageTimeIndexRetired(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	_, err := conn.ExecContext(ctx,
		"ALTER TABLE usage_messages ADD INDEX usage_time timestamp TYPE minmax GRANULARITY 1")
	require.NoError(t, err)

	require.NoError(t, EnsureSchemaOn(ctx, conn))
	require.NoError(t, EnsureSchemaOn(ctx, conn))

	var indexes int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT toInt64(count()) FROM system.data_skipping_indices
		WHERE database = currentDatabase() AND table = 'usage_messages'`).Scan(&indexes))
	assert.Zero(t, indexes)
	var drops int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT toInt64(count()) FROM system.mutations
		WHERE database = currentDatabase() AND table = 'usage_messages'
		  AND command LIKE '%DROP INDEX%'`).Scan(&drops))
	assert.Equal(t, 1, drops)
}

// TestClickHouseUsageMessagesBackfillAfterRecordedCostColumn covers a mirror
// whose usage_messages predates the recorded cost column and whose first
// fill never finished. The upgrade appends the column after revision, so the
// fill must write by column name.
func TestClickHouseUsageMessagesBackfillAfterRecordedCostColumn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	for _, table := range mirrorTables {
		_, err := conn.ExecContext(ctx, table.createSQL())
		require.NoError(t, err)
	}
	columns := []string{
		"session_id String", "ordinal Int64", "timestamp Nullable(DateTime64(6, 'UTC'))",
		"model String", "provider_id String", "claude_message_id String",
		"claude_request_id String", "source_uuid String", "push_version UInt64",
	}
	for _, column := range usageColumns {
		columns = append(columns, column.name+" "+column.typ)
	}
	columns = append(columns, "revision UInt128")
	_, err := conn.ExecContext(ctx, "CREATE TABLE usage_messages ("+strings.Join(columns, ", ")+
		") ENGINE = ReplacingMergeTree(revision) ORDER BY (session_id, ordinal)")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx,
		"INSERT INTO messages (session_id, ordinal, token_usage, push_version) VALUES (?, ?, ?, ?), (?, ?, ?, ?)",
		"costed", int64(0), `{"input_tokens":5,"recorded_cost_microdollars":1234}`, uint64(3),
		"plain", int64(0), `{"input_tokens":6}`, uint64(4))
	require.NoError(t, err)

	require.NoError(t, EnsureSchemaOn(ctx, conn))

	read := func(session string) (uint64, sql.NullInt64) {
		t.Helper()
		var revision uint64
		var cost sql.NullInt64
		require.NoError(t, conn.QueryRowContext(ctx,
			"SELECT toUInt64(revision), usage_recorded_cost FROM usage_messages FINAL WHERE session_id = ?",
			session).Scan(&revision, &cost))
		return revision, cost
	}
	revision, cost := read("costed")
	require.Equal(t, uint64(6), revision, "the fill writes push_version * 2 into revision")
	require.Equal(t, sql.NullInt64{Int64: 1234, Valid: true}, cost)
	revision, cost = read("plain")
	require.Equal(t, uint64(8), revision)
	require.False(t, cost.Valid, "a message without a recorded cost stores NULL")
}
