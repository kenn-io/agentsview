package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An archive written before message usage revisions existed records each
// message's usage fingerprint when it opens, so its first later usage change
// is published.
func TestConversationUsageRevisionsSurviveUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
	msgs := []Message{
		{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Run it", SourceUUID: "user-one"},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Done", SourceUUID: "reply-one", Model: "sample-model"},
	}
	require.NoError(t, d.InsertMessages(t.Context(), msgs))
	_, err = d.EnsureConversationExportInitialized(t.Context())
	require.NoError(t, err)
	for _, statement := range []string{
		"DROP TRIGGER conversation_usage_message_insert", "DROP TRIGGER conversation_usage_message_update",
		"DROP TRIGGER conversation_usage_event_insert", "DROP TRIGGER conversation_usage_event_delete",
		"DROP TRIGGER conversation_usage_event_update_old", "DROP TRIGGER conversation_usage_event_update_new",
		"DROP TRIGGER conversation_usage_key_insert", "DROP TRIGGER conversation_usage_key_move",
		"DROP INDEX idx_conversation_messages_usage_revision",
		"ALTER TABLE conversation_messages DROP COLUMN usage_key",
		"ALTER TABLE conversation_messages DROP COLUMN usage_revision",
	} {
		_, err := d.getWriter().Exec(t.Context(), statement)
		require.NoError(t, err, statement)
	}
	require.NoError(t, d.Close())

	upgraded, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	head, err := upgraded.ReadChangeStreamHead(t.Context())
	require.NoError(t, err)

	msgs[1].TokenUsage = []byte(`{"input_tokens":20,"output_tokens":5}`)
	require.NoError(t, upgraded.ReplaceSessionMessages(t.Context(), "chat", msgs))
	batch, err := upgraded.ReadChangeStream(t.Context(), head.Head)
	require.NoError(t, err)
	require.False(t, batch.Reset)
	require.Len(t, batch.Messages, 1)
	assert.Equal(t, "assistant", batch.Messages[0].Role)
	require.NotNil(t, batch.Messages[0].Usage)
	assert.Equal(t, 20, batch.Messages[0].Usage.InputTokens)
	assert.Equal(t, 5, batch.Messages[0].Usage.OutputTokens)
}
