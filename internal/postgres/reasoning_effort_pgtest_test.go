//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReasoningEffortPostgresSchemaAndRead(t *testing.T) {
	ctx := t.Context()
	schema := "agentsview_reasoning_effort_test"
	pg, err := Open(t.Context(), testPGURL(t), schema, true)
	require.NoError(t, err)
	defer pg.Close()
	_, err = pg.ExecContext(t.Context(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(ctx, pg, schema))
	_, err = pg.ExecContext(t.Context(), `INSERT INTO sessions (id, project, machine, agent, created_at)
		VALUES ('effort-session', 'proj', 'machine', 'claude', NOW())`)
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(), `INSERT INTO messages
		(session_id, ordinal, role, content, model, reasoning_effort)
		VALUES ('effort-session', 0, 'assistant', 'answer', 'model-test', 'high')`)
	require.NoError(t, err)

	store := &Store{pg: pg}
	messages, err := store.GetAllMessages(ctx, "effort-session")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "high", messages[0].ReasoningEffort)
	require.NoError(t, CheckSchemaCompat(ctx, pg))
}
