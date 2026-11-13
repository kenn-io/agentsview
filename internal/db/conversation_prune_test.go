package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conversationTombstoneCounts returns the live and removed conversation rows.
func conversationTombstoneCounts(t *testing.T, d *DB) (live, removed int) {
	t.Helper()
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COALESCE(SUM(removed = 0), 0), COALESCE(SUM(removed), 0) FROM conversation_messages`).Scan(&live, &removed)
	}))
	return live, removed
}

// seedConversationTombstones activates the conversation index for a session
// without native IDs and rewrites it once, leaving a tombstone per message.
func seedConversationTombstones(t *testing.T, d *DB) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
	msgs := []Message{
		{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Question"},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Answer"},
	}
	require.NoError(t, d.InsertMessages(ctx, msgs))
	_, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	// Changing every row is a full replacement, so both rows are retired.
	msgs[0].Content, msgs[1].Content = "Other question", "Other answer"
	require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
}

func TestPruneConversationTombstonesRestartsOlderReaders(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
	msgs := []Message{
		{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Question"},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Answer"},
	}
	require.NoError(t, d.InsertMessages(ctx, msgs))
	before, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	beforeHead, err := d.ReadChangeStreamHead(ctx)
	require.NoError(t, err)

	msgs[0].Content, msgs[1].Content = "Other question", "Other answer"
	require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
	after, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: before.Checkpoint})
	require.NoError(t, err)
	afterHead, err := d.ReadChangeStreamHead(ctx)
	require.NoError(t, err)
	live, removed := conversationTombstoneCounts(t, d)
	require.Equal(t, 2, live)
	require.Equal(t, 2, removed)

	pruned, err := d.PruneConversationTombstones(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 2, pruned)
	live, removed = conversationTombstoneCounts(t, d)
	assert.Equal(t, 2, live)
	assert.Zero(t, removed)

	// A reader that never saw the deletions restarts.
	_, err = d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: before.Checkpoint})
	require.ErrorIs(t, err, ErrConversationReconciliationRequired)
	stale, err := d.ReadChangeStream(ctx, beforeHead.Head, nil)
	require.NoError(t, err)
	assert.True(t, stale.Reset)

	// A reader that already saw them keeps its position.
	quiet, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: after.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, quiet.Changes)
	current, err := d.ReadChangeStream(ctx, afterHead.Head, nil)
	require.NoError(t, err)
	assert.False(t, current.Reset)
	assert.Empty(t, current.Messages)

	// A full walk sees only the live rows.
	full, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	var messages int
	for _, change := range full.Changes {
		if change.Type == "message" {
			messages++
			assert.False(t, change.Deleted)
		}
	}
	assert.Equal(t, 2, messages)

	// Nothing left to prune leaves the floor where it was.
	pruned, err = d.PruneConversationTombstones(ctx)
	require.NoError(t, err)
	assert.Zero(t, pruned)
	_, err = d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: after.Checkpoint})
	require.NoError(t, err)
}

func TestCompactPrunesConversationTombstones(t *testing.T) {
	d := testDB(t)
	seedConversationTombstones(t, d)
	_, removed := conversationTombstoneCounts(t, d)
	require.Positive(t, removed)

	_, err := d.Compact(t.Context(), CompactOptions{StagingDir: t.TempDir()})
	require.NoError(t, err)

	live, removed := conversationTombstoneCounts(t, d)
	assert.Equal(t, 2, live)
	assert.Zero(t, removed)
}
