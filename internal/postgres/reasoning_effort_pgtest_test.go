//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReasoningEffortPostgresSchemaAndRead(t *testing.T) {
	ctx := context.Background()
	schema := "agentsview_reasoning_effort_test"
	pg, err := Open(testPGURL(t), schema, true)
	require.NoError(t, err)
	defer pg.Close()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(ctx, pg, schema))
	_, err = pg.Exec(`INSERT INTO sessions (id, project, machine, agent, created_at)
		VALUES ('effort-session', 'proj', 'machine', 'claude', NOW())`)
	require.NoError(t, err)
	_, err = pg.Exec(`INSERT INTO messages
		(session_id, ordinal, role, content, model, reasoning_effort)
		VALUES ('effort-session', 0, 'assistant', 'answer', 'model-test', 'high')`)
	require.NoError(t, err)

	store, err := NewStore(testPGURL(t), schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	messages, err := store.GetAllMessages(ctx, "effort-session")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "high", messages[0].ReasoningEffort)
	require.NoError(t, CheckSchemaCompat(ctx, pg))
}
