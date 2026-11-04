//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPushClaudeHookOutput(t *testing.T) {
	ctx := t.Context()
	url := testPGURL(t)
	const schema = "agentsview_push_claude_hook_test"
	cleanNamedPGSchema(t, url, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	pg, err := Open(ctx, url, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, EnsureSchema(ctx, pg, schema))
	local := testDB(t)
	const id = "hook-output"
	const body = "Content:\n  中文 e\u0301 <script>literal</script>\n  preserved  \n"
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: id, Agent: "claude", Project: "hooks", Machine: "test-machine", MessageCount: 1,
	}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{
		SessionID: id, Ordinal: 0, Role: "user", Content: body, ContentLength: len(body),
		DialogueText: new(""), IsSystem: true, SourceType: "attachment", SourceSubtype: "hook_success",
		SourceUUID: "h1", SourceParentUUID: "u1", Timestamp: "2024-01-01T00:00:00Z",
	}}))
	sync := &Sync{pg: pg, local: local, machine: "test-machine", schema: schema, schemaDone: true}
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err)
	var content, role, sourceType, subtype, uuid, parent string
	var dialogue sql.NullString
	var system bool
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT content, role, is_system, source_type, source_subtype, source_uuid, source_parent_uuid, dialogue_text FROM messages WHERE session_id = $1`, id,
	).Scan(&content, &role, &system, &sourceType, &subtype, &uuid, &parent, &dialogue))
	assert.Equal(t, body, content)
	assert.Equal(t, "user", role)
	assert.True(t, system)
	assert.Equal(t, "attachment", sourceType)
	assert.Equal(t, "hook_success", subtype)
	assert.Equal(t, "h1", uuid)
	assert.Equal(t, "u1", parent)
	require.True(t, dialogue.Valid)
	assert.Empty(t, dialogue.String)
}
