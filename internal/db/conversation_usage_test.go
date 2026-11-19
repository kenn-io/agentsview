package db

import (
	"path/filepath"
	"strconv"
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
	batch, err := upgraded.ReadChangeStream(t.Context(), head.Head, nil)
	require.NoError(t, err)
	require.False(t, batch.Reset)
	require.Len(t, batch.Messages, 1)
	assert.Equal(t, "assistant", batch.Messages[0].Role)
	require.NotNil(t, batch.Messages[0].Usage)
	assert.Equal(t, 20, batch.Messages[0].Usage.InputTokens)
	assert.Equal(t, 5, batch.Messages[0].Usage.OutputTokens)
}

// A resync copies archive metadata into a database whose own triggers have
// already stamped message usage revisions. The copy must keep that database's
// counter, or a stream reading from the head re-sends unchanged messages.
func TestCopySessionMetadataKeepsConversationUsageRevision(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.db")
	old := testDBAtPath(t, oldPath, "old")
	_, err := old.getWriter().Exec(t.Context(),
		`INSERT INTO archive_metadata(key,value) VALUES ('conversation_usage_revision','1')
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	fresh := testDBAtPath(t, filepath.Join(dir, "fresh.db"), "fresh")
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	_, err = fresh.EnsureConversationExportInitialized(t.Context())
	require.NoError(t, err)
	require.NoError(t, fresh.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
	msgs := []Message{
		{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Run it", SourceUUID: "user-one"},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Done", SourceUUID: "reply-one", Model: "sample-model"},
	}
	require.NoError(t, fresh.InsertMessages(t.Context(), msgs))
	for _, usage := range []string{`{"input_tokens":20,"output_tokens":5}`, `{"input_tokens":30,"output_tokens":8}`} {
		msgs[1].TokenUsage = []byte(usage)
		require.NoError(t, fresh.ReplaceSessionMessages(t.Context(), "chat", msgs))
	}

	require.NoError(t, fresh.CopySessionMetadataFrom(t.Context(), oldPath))
	head, err := fresh.ReadChangeStreamHead(t.Context())
	require.NoError(t, err)
	batch, err := fresh.ReadChangeStream(t.Context(), head.Head, nil)
	require.NoError(t, err)
	assert.False(t, batch.Reset)
	assert.Empty(t, batch.Messages, "nothing changed after the head")
}

// A usage backlog longer than one page must not let the conversation pages
// send a message that a later usage page sends again.
func TestChangeStreamSendsEditedMessageOnceBehindUsageBacklog(t *testing.T) {
	d := testDB(t)
	_, err := d.EnsureConversationExportInitialized(t.Context())
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
	msgs := make([]Message, MaxSessionLimit+2)
	for i := range msgs {
		msgs[i] = Message{
			SessionID: "chat", Ordinal: i, Role: "assistant", Content: "reply " + strconv.Itoa(i),
			SourceUUID: "reply-" + strconv.Itoa(i), Model: "sample-model",
		}
	}
	require.NoError(t, d.InsertMessages(t.Context(), msgs))
	head, err := d.ReadChangeStreamHead(t.Context())
	require.NoError(t, err)

	// Ordinals 1 through 501 change usage only, then ordinal 0 changes both
	// text and usage.
	for i := 1; i < len(msgs); i++ {
		msgs[i].TokenUsage = []byte(`{"input_tokens":10,"output_tokens":1}`)
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
	msgs[0].Content = "reply 0, edited"
	msgs[0].TokenUsage = []byte(`{"input_tokens":40,"output_tokens":4}`)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))

	var editedID string
	require.NoError(t, d.getReader().QueryRowContext(t.Context(),
		`SELECT message_id FROM conversation_messages WHERE session_id='chat' AND ordinal=0 AND removed=0`).Scan(&editedID))
	sent := map[string]int{}
	var edited []ChangeStreamMessage
	cursor := head.Head
	for {
		batch, err := d.ReadChangeStream(t.Context(), cursor, nil)
		require.NoError(t, err)
		require.False(t, batch.Reset)
		for _, message := range batch.Messages {
			sent[message.MessageID]++
			if message.MessageID == editedID {
				edited = append(edited, message)
			}
		}
		cursor = batch.To
		if !batch.More {
			break
		}
	}
	assert.Len(t, sent, len(msgs), "every changed message is sent")
	for id, count := range sent {
		assert.Equal(t, 1, count, "message %s", id)
	}
	require.Len(t, edited, 1)
	require.NotNil(t, edited[0].Usage)
	assert.Equal(t, 40, edited[0].Usage.InputTokens)
}

// Sessions written before the usage change journal existed have no journal
// row. A replay from the beginning must still send the ones without projected
// messages, more than a page of them.
func TestChangeStreamReplaysSessionsThatPredateUsageJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	_, err = d.EnsureConversationExportInitialized(t.Context())
	require.NoError(t, err)
	want := make([]string, MaxSessionLimit+1)
	for i := range want {
		want[i] = "empty-" + strconv.Itoa(i)
		require.NoError(t, d.UpsertSession(t.Context(), Session{ID: want[i], Project: "sample", Machine: "local", Agent: "codex"}))
	}
	_, err = d.getWriter().Exec(t.Context(), "DELETE FROM usage_session_changes")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	upgraded, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	var got []string
	cursor := ""
	for {
		batch, err := upgraded.ReadChangeStream(t.Context(), cursor, nil)
		require.NoError(t, err)
		for _, session := range batch.Sessions {
			assert.False(t, session.Deleted, session.SessionID)
			got = append(got, session.SessionID)
		}
		cursor = batch.To
		if !batch.More {
			break
		}
	}
	assert.ElementsMatch(t, want, got)
}
