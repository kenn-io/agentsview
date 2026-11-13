//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
)

// A mirror created before these columns were LowCardinality must convert
// them at startup and keep their values. Other type differences stay as
// they are, since converting them could lose data.
func TestEnsureSchemaConvertsStringColumnsToLowCardinality(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	require.NoError(t, EnsureSchemaOn(ctx, conn))

	// Put back the column types an older mirror has.
	for _, alter := range []string{
		"ALTER TABLE messages MODIFY COLUMN role String",
		"ALTER TABLE messages MODIFY COLUMN model String",
		"ALTER TABLE tool_calls MODIFY COLUMN tool_name String",
		"ALTER TABLE sessions MODIFY COLUMN agent String DEFAULT 'claude'",
		"ALTER TABLE usage_messages MODIFY COLUMN model String",
		"ALTER TABLE usage_session_snapshots MODIFY COLUMN agent String DEFAULT 'claude'",
		// Not a LowCardinality wrap of the spec's type: must stay as is.
		"ALTER TABLE messages MODIFY COLUMN prompt_source Nullable(String)",
	} {
		_, err := conn.ExecContext(ctx, alter+" SETTINGS mutations_sync = 1")
		require.NoError(t, err, alter)
	}
	for _, insert := range []string{
		`INSERT INTO messages (session_id, ordinal, role, model, push_version)
		 VALUES ('lc', 0, 'assistant', 'model-a', 1)`,
		`INSERT INTO tool_calls (session_id, message_ordinal, call_index, tool_name, push_version)
		 VALUES ('lc', 0, 0, 'Bash', 1)`,
		`INSERT INTO sessions (id, agent, push_version) VALUES ('lc', 'codex', 1)`,
	} {
		_, err := conn.ExecContext(ctx, insert)
		require.NoError(t, err)
	}

	require.NoError(t, EnsureSchemaOn(ctx, conn))

	types := map[string]string{}
	rows, err := conn.QueryContext(ctx, `SELECT table || '.' || name, type
		FROM system.columns WHERE database = currentDatabase()`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var name, typ string
		require.NoError(t, rows.Scan(&name, &typ))
		types[name] = typ
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, map[string]string{
		"messages.role":                 "LowCardinality(String)",
		"messages.model":                "LowCardinality(String)",
		"tool_calls.tool_name":          "LowCardinality(String)",
		"sessions.agent":                "LowCardinality(String)",
		"usage_messages.model":          "LowCardinality(String)",
		"usage_session_snapshots.agent": "LowCardinality(String)",
		"messages.prompt_source":        "Nullable(String)",
	}, map[string]string{
		"messages.role":                 types["messages.role"],
		"messages.model":                types["messages.model"],
		"tool_calls.tool_name":          types["tool_calls.tool_name"],
		"sessions.agent":                types["sessions.agent"],
		"usage_messages.model":          types["usage_messages.model"],
		"usage_session_snapshots.agent": types["usage_session_snapshots.agent"],
		"messages.prompt_source":        types["messages.prompt_source"],
	})

	var role, model, agent, toolName string
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT role, model FROM messages WHERE session_id = 'lc'").Scan(&role, &model))
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT agent FROM sessions WHERE id = 'lc'").Scan(&agent))
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT tool_name FROM tool_calls WHERE session_id = 'lc'").Scan(&toolName))
	assert.Equal(t, []string{"assistant", "model-a", "codex", "Bash"},
		[]string{role, model, agent, toolName})
}
