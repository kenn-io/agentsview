//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestPGParserParentProvenanceRoundTripsAndRepushes(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_parser_parent_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	ctx := context.Background()
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	defer pg.Close()
	require.NoError(t, EnsureSchema(ctx, pg, schema))

	local, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	defer local.Close()

	parserParent := "parent-a"
	for _, sess := range []db.Session{
		{ID: "parent-a", Project: "project", Machine: "machine", Agent: "claude"},
		{ID: "parent-b", Project: "project", Machine: "machine", Agent: "claude"},
		{
			ID: "child", Project: "project", Machine: "machine", Agent: "claude",
			ParentSessionID: &parserParent, RelationshipType: "subagent",
		},
	} {
		require.NoError(t, local.UpsertSession(sess))
	}
	require.NoError(t, local.InsertMessages([]db.Message{{
		SessionID: "parent-a", Ordinal: 0, Role: "assistant",
		Content: "spawn child", HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName: "Task", Category: "Task", SubagentSessionID: "child",
		}},
	}}))

	syncer := &Sync{
		pg: pg, local: local, machine: "machine", schema: schema, schemaDone: true,
	}
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	assertPGSessionParents(t, pg, "child", "parent-a", "parent-a")
	store := newStore(pg)
	got, err := store.GetSession(ctx, "child")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.ParserParentSessionID)
	assert.Equal(t, "parent-a", *got.ParserParentSessionID)

	// Re-parsing changes only the immutable parser provenance: the spawn edge
	// immediately restores the effective parent to parent-a.
	parserParent = "parent-b"
	require.NoError(t, local.UpsertSession(db.Session{
		ID: "child", Project: "project", Machine: "machine", Agent: "claude",
		ParentSessionID: &parserParent, RelationshipType: "subagent",
	}))
	require.NoError(t, local.LinkSubagentSessions())
	require.NoError(t, local.SetSyncState("last_push_at", ""))
	require.NoError(t, local.SetSyncState(lastPushBoundaryStateKey, ""))

	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assertPGSessionParents(t, pg, "child", "parent-a", "parent-b")
}

func TestPGParserParentBackfillIgnoresLegacyProvenanceMarker(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_parser_parent_backfill_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	ctx := t.Context()
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	defer pg.Close()
	require.NoError(t, EnsureSchema(ctx, pg, schema))

	local, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	defer local.Close()
	parentID := "parent"
	for _, sess := range []db.Session{
		{ID: parentID, Project: "project", Machine: "machine", Agent: "claude"},
		{
			ID: "child", Project: "project", Machine: "machine", Agent: "claude",
			ParentSessionID: &parentID, RelationshipType: "subagent",
		},
	} {
		require.NoError(t, local.UpsertSession(sess))
	}

	syncer := &Sync{
		pg: pg, local: local, machine: "machine", schema: schema, schemaDone: true,
	}
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	_, err = pg.ExecContext(ctx, `
		UPDATE sessions SET parser_parent_session_id = NULL WHERE id = 'child'`)
	require.NoError(t, err)
	require.NoError(t, local.DeleteSyncState(sessionProvenanceBackfillStateKey))
	require.NoError(t, local.SetSyncState("pg_session_provenance_backfill_v1", "1"))

	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	var parserParent sql.NullString
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT parser_parent_session_id FROM sessions WHERE id = 'child'`,
	).Scan(&parserParent))
	require.True(t, parserParent.Valid,
		"a legacy v1 marker must not suppress the parser-parent backfill")
	assert.Equal(t, parentID, parserParent.String)
}

func assertPGSessionParents(
	t *testing.T, pg *sql.DB, sessionID, wantParent, wantParserParent string,
) {
	t.Helper()
	var parent, parserParent string
	err := pg.QueryRowContext(t.Context(), `
		SELECT parent_session_id, parser_parent_session_id
		FROM sessions WHERE id = $1`, sessionID,
	).Scan(&parent, &parserParent)
	require.NoError(t, err)
	assert.Equal(t, wantParent, parent)
	assert.Equal(t, wantParserParent, parserParent)
}
