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
		"ALTER TABLE usage_messages MODIFY COLUMN model String",
		"ALTER TABLE usage_session_snapshots MODIFY COLUMN agent String DEFAULT 'claude'",
		// Not a LowCardinality wrap of the spec's type: must stay as is.
		"ALTER TABLE messages MODIFY COLUMN prompt_source Nullable(String)",
	} {
		_, err := conn.ExecContext(ctx, alter+" SETTINGS mutations_sync = 1")
		require.NoError(t, err, alter)
	}
	_, err := conn.ExecContext(ctx, `INSERT INTO messages
		(session_id, ordinal, role, push_version) VALUES ('lc', 0, 'assistant', 1)`)
	require.NoError(t, err)

	require.NoError(t, EnsureSchemaOn(ctx, conn))

	cols, err := readColumns(ctx, conn)
	require.NoError(t, err)
	want := map[string]map[string]string{
		"messages": {
			"role":          "LowCardinality(String)",
			"prompt_source": "Nullable(String)",
		},
		"usage_messages":          {"model": "LowCardinality(String)"},
		"usage_session_snapshots": {"agent": "LowCardinality(String)"},
	}
	got := map[string]map[string]string{}
	for table, columns := range want {
		got[table] = map[string]string{}
		for column := range columns {
			got[table][column] = cols[table][column]
		}
	}
	assert.Equal(t, want, got)

	var role string
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT role FROM messages WHERE session_id = 'lc'").Scan(&role))
	assert.Equal(t, "assistant", role)
}
