package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Archive-source reparses replace messages through the batch writer. Rows
// that only gain source IDs must stay in place there too, so pins and notes
// survive, including on system rows that the legacy pin match skips.
func TestWriteSessionBatchKeepsRowsThatGainSourceUUIDs(t *testing.T) {
	d := testDB(t)
	const sessionID = "batch-gain"
	v1 := []Message{
		diffTestMsg(sessionID, 0, "user", "be brief", func(m *Message) { m.IsSystem = true }),
		diffTestMsg(sessionID, 1, "user", "question"),
		diffTestMsg(sessionID, 2, "assistant", "answer"),
	}
	write := SessionBatchWrite{
		Session:         Session{ID: sessionID, Project: "project", Agent: "deepseek-harness", Machine: "local"},
		Messages:        v1,
		ReplaceMessages: true,
	}
	result, err := d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	require.Equal(t, 1, result.WrittenSessions)
	ids := messageIDsByOrdinal(t, d, sessionID)
	note := "keep the prompt"
	_, err = d.PinMessage(t.Context(), sessionID, ids[0], &note)
	require.NoError(t, err)
	revisionBefore, err := d.TranscriptRevision(t.Context(), sessionID)
	require.NoError(t, err)

	write.Messages = append([]Message(nil), v1...)
	for i, id := range []string{"s-1", "u-1", "a-1"} {
		write.Messages[i].SourceUUID = id
	}
	result, err = d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	require.Equal(t, 1, result.WrittenSessions)

	assert.Equal(t, ids, messageIDsByOrdinal(t, d, sessionID))
	stored, err := d.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, []string{"s-1", "u-1", "a-1"},
		[]string{stored[0].SourceUUID, stored[1].SourceUUID, stored[2].SourceUUID})
	pins, err := d.ListPinnedMessages(t.Context(), sessionID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, ids[0], pins[0].MessageID)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, note, *pins[0].Note)
	revisionAfter, err := d.TranscriptRevision(t.Context(), sessionID)
	require.NoError(t, err)
	assert.NotEqual(t, revisionBefore, revisionAfter,
		"mirrors must still see the new source IDs")
}

// A gained ID that an earlier export row already held is a returning native
// identity, not a new one. Reconciliation must give the row its original
// export ID back.
func TestConversationExportGainedSourceIDRestoresTombstone(t *testing.T) {
	d := testDB(t)
	const sessionID = "gain-export"
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: sessionID, Project: "project", Machine: "local", Agent: "deepseek-harness"}))
	_, err := d.EnsureConversationExportInitialized(t.Context())
	require.NoError(t, err)
	withID := func(id string) func(*Message) {
		return func(m *Message) { m.SourceUUID = id }
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), sessionID, []Message{
		diffTestMsg(sessionID, 0, "user", "question"),
		diffTestMsg(sessionID, 1, "assistant", "answer", withID("a-1")),
	}))
	original := liveConversationMessageID(t, d, sessionID, 1)

	// The reply loses its native ID, so its export row is reissued and the
	// original becomes a tombstone that still holds a-1.
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), sessionID, []Message{
		diffTestMsg(sessionID, 0, "user", "question"),
		diffTestMsg(sessionID, 1, "assistant", "edited answer"),
	}))
	require.NotEqual(t, original, liveConversationMessageID(t, d, sessionID, 1))

	require.NoError(t, d.ReplaceSessionMessages(t.Context(), sessionID, []Message{
		diffTestMsg(sessionID, 0, "user", "question"),
		diffTestMsg(sessionID, 1, "assistant", "edited answer", withID("a-1")),
	}))
	assert.Equal(t, original, liveConversationMessageID(t, d, sessionID, 1))
}

func liveConversationMessageID(t *testing.T, d *DB, sessionID string, ordinal int) string {
	t.Helper()
	var id string
	require.NoError(t, d.Reader().QueryRow(t.Context(),
		"SELECT message_id FROM conversation_messages WHERE session_id = ? AND ordinal = ? AND removed = 0",
		sessionID, ordinal).Scan(&id))
	return id
}

func TestTranscriptMessagesGainSourceIDs(t *testing.T) {
	withID := func(id string) func(*Message) {
		return func(m *Message) { m.SourceUUID = id }
	}
	stored := []Message{
		diffTestMsg("s", 0, "user", "question"),
		diffTestMsg("s", 1, "assistant", "answer"),
	}
	tests := []struct {
		name     string
		stored   []Message
		incoming []Message
		want     bool
	}{
		{
			name:   "rows gain unique ids",
			stored: stored,
			incoming: []Message{
				diffTestMsg("s", 0, "user", "question", withID("u-1")),
				diffTestMsg("s", 1, "assistant", "answer", withID("a-1")),
			},
			want: true,
		},
		{
			name:     "nothing gained",
			stored:   stored,
			incoming: stored,
			want:     false,
		},
		{
			name:   "shared id cannot identify either row",
			stored: stored,
			incoming: []Message{
				diffTestMsg("s", 0, "user", "question", withID("m-1")),
				diffTestMsg("s", 1, "assistant", "answer", withID("m-1")),
			},
			want: false,
		},
		{
			name:   "content changed",
			stored: stored,
			incoming: []Message{
				diffTestMsg("s", 0, "user", "question", withID("u-1")),
				diffTestMsg("s", 1, "assistant", "longer answer", withID("a-1")),
			},
			want: false,
		},
		{
			name: "stored id replaced",
			stored: []Message{
				diffTestMsg("s", 0, "user", "question", withID("old")),
			},
			incoming: []Message{
				diffTestMsg("s", 0, "user", "question", withID("new")),
			},
			want: false,
		},
		{
			name:   "row appended",
			stored: stored,
			incoming: []Message{
				diffTestMsg("s", 0, "user", "question", withID("u-1")),
				diffTestMsg("s", 1, "assistant", "answer", withID("a-1")),
				diffTestMsg("s", 2, "user", "next", withID("u-2")),
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, transcriptMessagesGainSourceIDs(tt.stored, tt.incoming))
		})
	}
}
