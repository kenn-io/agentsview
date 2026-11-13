package db

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

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

func TestPruneConversationTombstonesFreshWalkPagesBelowFloor(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "long", Project: "sample", Machine: "local", Agent: "codex"}))
	long := make([]Message, MaxSessionLimit+20)
	for i := range long {
		long[i] = Message{SessionID: "long", Ordinal: i, Role: "user", Content: fmt.Sprintf("Message %d", i)}
	}
	require.NoError(t, d.InsertMessages(ctx, long))
	// The tombstones come after every long-session row, so the whole session
	// sits below the pruned floor.
	seedConversationTombstones(t, d)
	pruned, err := d.PruneConversationTombstones(ctx)
	require.NoError(t, err)
	require.Positive(t, pruned)

	// A fresh export walks every page.
	var exported int
	cursor := ""
	for {
		page, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Cursor: cursor, Limit: 100})
		require.NoError(t, err)
		for _, change := range page.Changes {
			if change.Type == "message" {
				exported++
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	assert.Equal(t, len(long)+2, exported)

	// A fresh change stream resets once, then pages forward.
	var streamed int
	batch, err := d.ReadChangeStream(ctx, "", nil)
	require.NoError(t, err)
	require.True(t, batch.Reset)
	require.True(t, batch.More)
	streamed += len(batch.Messages)
	for batch.More {
		batch, err = d.ReadChangeStream(ctx, batch.To, nil)
		require.NoError(t, err)
		require.False(t, batch.Reset)
		streamed += len(batch.Messages)
	}
	assert.Equal(t, len(long)+2, streamed)
}

func TestPruneConversationTombstonesBodyReadReportsSuperseded(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
	msgs := []Message{{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Question"}}
	require.NoError(t, d.InsertMessages(ctx, msgs))
	listed, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, listed.Changes, 1)
	msgs[0].Content = "Other question"
	require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
	_, err = d.PruneConversationTombstones(ctx)
	require.NoError(t, err)

	_, err = d.GetConversationMessage(ctx, ConversationMessageOptions{
		DatabaseID: listed.DatabaseID, SessionID: "chat",
		MessageID: listed.Changes[0].MessageID, Revision: listed.Changes[0].Revision,
	})
	require.ErrorIs(t, err, ErrConversationRevisionChanged)
}

func TestPruneConversationTombstonesKeepsNativeIdentity(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{
		{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Question", SourceUUID: "question"},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Answer", SourceUUID: "answer"},
	}
	require.NoError(t, d.InsertMessages(ctx, msgs))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	ids := map[int]string{}
	for _, change := range initial.Changes {
		ids[change.Ordinal] = change.MessageID
	}
	require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs[:1]))
	_, err = d.PruneConversationTombstones(ctx)
	require.NoError(t, err)
	_, removed := conversationTombstoneCounts(t, d)
	assert.Equal(t, 1, removed, "a tombstone with a native ID is kept")

	require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
	restored, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	var returned []string
	for _, change := range restored.Changes {
		if change.Type == "message" && change.Ordinal == 1 && !change.Deleted {
			returned = append(returned, change.MessageID)
		}
	}
	assert.Equal(t, []string{ids[1]}, returned)
}

func TestPruneExpiredConversationTombstonesKeepsRetentionWindow(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	const retention = 7 * 24 * time.Hour
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// A cold archive has no index to prune and starts no clock.
	pruned, err := d.PruneExpiredConversationTombstones(ctx, start, retention)
	require.NoError(t, err)
	assert.Zero(t, pruned)

	seedConversationTombstones(t, d)
	_, old := conversationTombstoneCounts(t, d)
	require.Positive(t, old)

	// The first run starts the clock at the current head.
	pruned, err = d.PruneExpiredConversationTombstones(ctx, start, retention)
	require.NoError(t, err)
	assert.Zero(t, pruned)

	// Deletions after the clock started belong to the next window.
	msgs := []Message{
		{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Third question"},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Third answer"},
	}
	require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
	_, removed := conversationTombstoneCounts(t, d)
	require.Equal(t, old+2, removed)

	pruned, err = d.PruneExpiredConversationTombstones(ctx, start.Add(retention-time.Hour), retention)
	require.NoError(t, err)
	assert.Zero(t, pruned, "nothing is pruned inside the retention window")

	pruned, err = d.PruneExpiredConversationTombstones(ctx, start.Add(retention), retention)
	require.NoError(t, err)
	assert.EqualValues(t, old, pruned)
	live, removed := conversationTombstoneCounts(t, d)
	assert.Equal(t, 2, live)
	assert.Equal(t, 2, removed, "deletions newer than the clock start stay")

	// The clock restarted, so the newer deletions wait a full window.
	pruned, err = d.PruneExpiredConversationTombstones(ctx, start.Add(2*retention-time.Hour), retention)
	require.NoError(t, err)
	assert.Zero(t, pruned)
	pruned, err = d.PruneExpiredConversationTombstones(ctx, start.Add(2*retention), retention)
	require.NoError(t, err)
	assert.EqualValues(t, 2, pruned)
}
