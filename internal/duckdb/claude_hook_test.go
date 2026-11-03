//go:build !(windows && arm64)

package duckdb

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestClaudeHookOutputMirrorRoundTrip(t *testing.T) {
	const id = "hook-output"
	const body = "Content:\n  中文 e\u0301 <script>literal</script>\n  preserved  \n"
	store := newDuckWindowStore(t, func(local *db.DB) {
		require.NoError(t, local.UpsertSession(t.Context(), db.Session{
			ID: id, Agent: "claude", Project: "hooks", Machine: "local", MessageCount: 1,
		}))
		require.NoError(t, local.InsertMessages(t.Context(), []db.Message{{
			SessionID: id, Ordinal: 0, Role: "user", Content: body, ContentLength: len(body),
			DialogueText: new(""), IsSystem: true, SourceType: "attachment", SourceSubtype: "hook_success",
			SourceUUID: "h1", SourceParentUUID: "u1", Timestamp: "2024-01-01T00:00:00Z",
		}}))
	})
	messages, err := store.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	hook := messages[0]
	assert.True(t, hook.IsSystem)
	assert.Equal(t, "user", hook.Role)
	assert.Equal(t, "attachment", hook.SourceType)
	assert.Equal(t, "hook_success", hook.SourceSubtype)
	assert.Equal(t, "h1", hook.SourceUUID)
	assert.Equal(t, "u1", hook.SourceParentUUID)
	assert.Equal(t, body, hook.Content)
	assert.Equal(t, len(body), hook.ContentLength)
	// Transcript readers omit the internal dialogue projection. The mirror
	// must still retain a known empty projection rather than a coverage gap.
	var dialogue sql.NullString
	require.NoError(t, store.duck.QueryRowContext(t.Context(),
		"SELECT dialogue_text FROM messages WHERE session_id = ? AND ordinal = 0", id,
	).Scan(&dialogue))
	require.True(t, dialogue.Valid)
	assert.Empty(t, dialogue.String)
}
