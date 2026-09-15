package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
)

// A token-only rewrite must not resend prose, while a streamed text change
// must retain the source message's identity and publish the new body.
func TestConversationExportNativeMessageChanges(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{
		{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Question", VisibleText: new("Question"), ConversationSourceID: "user-one"},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Partial", VisibleText: new("Partial"), ConversationSourceID: "reply-one"},
	}
	require.NoError(t, d.InsertMessages(msgs))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 2)
	answerID := initial.Changes[1].MessageID
	answer, err := d.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: initial.DatabaseID, SessionID: "chat", MessageID: answerID, Revision: initial.Changes[1].Revision})
	require.NoError(t, err)
	require.NotNil(t, answer.Text)
	assert.Equal(t, "Partial", *answer.Text)

	msgs[1].OutputTokens = 10
	msgs[1].HasOutputTokens = true
	require.NoError(t, d.ReplaceSessionMessages("chat", msgs))
	quiet, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, quiet.Changes)

	msgs[1].Content, msgs[1].VisibleText = "Complete answer", new("Complete answer")
	require.NoError(t, d.ReplaceSessionMessages("chat", msgs))
	delta, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: quiet.Checkpoint})
	require.NoError(t, err)
	require.Len(t, delta.Changes, 1)
	assert.Equal(t, answerID, delta.Changes[0].MessageID)
	answer, err = d.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: delta.DatabaseID, SessionID: "chat", MessageID: answerID, Revision: delta.Changes[0].Revision})
	require.NoError(t, err)
	assert.Equal(t, "Complete answer", *answer.Text)
}

func TestConversationExportChunksPinBodyAndDatabase(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{{SessionID: "chat", Role: "assistant", Content: "a😀bcédef", VisibleText: new("a😀bcédef"), ConversationSourceID: "reply"}}
	require.NoError(t, d.InsertMessages(msgs))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 1)
	change := initial.Changes[0]
	opts := ConversationMessageOptions{DatabaseID: initial.DatabaseID, SessionID: "chat", MessageID: change.MessageID, Revision: change.Revision, MaxBytes: 4}
	var joined string
	for {
		chunk, err := d.GetConversationMessage(ctx, opts)
		require.NoError(t, err)
		require.NotNil(t, chunk.Text)
		assert.LessOrEqual(t, len(*chunk.Text), 4)
		assert.Equal(t, change.Digest, chunk.Digest)
		assert.Equal(t, change.Project, chunk.Project)
		joined += *chunk.Text
		if chunk.NextOffset == chunk.TextBytes {
			break
		}
		require.Greater(t, chunk.NextOffset, opts.Offset)
		opts.Offset = chunk.NextOffset
	}
	assert.Equal(t, "a😀bcédef", joined)
	opts.Offset = 2
	_, err = d.GetConversationMessage(ctx, opts)
	require.Error(t, err)
	opts.Offset = 0
	opts.DatabaseID = "different-generation"
	_, err = d.GetConversationMessage(ctx, opts)
	assert.ErrorIs(t, err, ErrConversationReconciliationRequired)
	opts.DatabaseID = initial.DatabaseID
	msgs[0].Content, msgs[0].VisibleText = "new text", new("new text")
	require.NoError(t, d.ReplaceSessionMessages("chat", msgs))
	chunk, err := d.GetConversationMessage(ctx, opts)
	assert.ErrorIs(t, err, ErrConversationRevisionChanged)
	assert.Nil(t, chunk.Text)
}

func TestConversationExportPaginationDefersConcurrentChanges(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := make([]Message, 12)
	for i := range msgs {
		text := fmt.Sprintf("Message %d", i)
		msgs[i] = Message{SessionID: "chat", Ordinal: i, Role: "assistant", Content: text, VisibleText: new(text), ConversationSourceID: fmt.Sprintf("reply-%d", i)}
	}
	require.NoError(t, d.InsertMessages(msgs))
	page, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Limit: 3})
	require.NoError(t, err)
	require.Len(t, page.Changes, 3)
	assert.Equal(t, []int{0, 1, 2}, []int{page.Changes[0].Ordinal, page.Changes[1].Ordinal, page.Changes[2].Ordinal})
	msgs[5].Content, msgs[5].VisibleText = "Revised", new("Revised")
	require.NoError(t, d.ReplaceSessionMessages("chat", msgs))
	seen := []int{0, 1, 2}
	var previous int64 = 3
	for page.NextCursor != "" {
		page, err = d.ExportConversationChanges(ctx, ConversationExportOptions{Cursor: page.NextCursor, Limit: 3})
		require.NoError(t, err)
		for _, change := range page.Changes {
			rev, err := strconv.ParseInt(change.Revision, 10, 64)
			require.NoError(t, err)
			assert.Greater(t, rev, previous)
			previous = rev
			seen = append(seen, change.Ordinal)
		}
	}
	assert.Equal(t, []int{0, 1, 2, 3, 4, 6, 7, 8, 9, 10, 11}, seen)
	next, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: page.Checkpoint})
	require.NoError(t, err)
	require.Len(t, next.Changes, 1)
	assert.Equal(t, 5, next.Changes[0].Ordinal)
}

func TestConversationExportProjectChangeDoesNotReviseBodies(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages([]Message{{SessionID: "chat", Role: "user", Content: "Question", VisibleText: new("Question"), ConversationSourceID: "one"}}))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 1)
	require.NoError(t, d.UpsertSession(Session{ID: "chat", Project: "new-project", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.UpsertProjectIdentityObservationWithSnapshotProject(ctx, export.ProjectIdentityObservation{SessionID: "chat", Project: "new-project", Machine: "local"}, "new-project"))
	changed, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	require.Len(t, changed.Changes, 1)
	assert.Equal(t, "session", changed.Changes[0].Type)
	assert.Empty(t, changed.Changes[0].MessageID)
	ref := initial.Changes[0]
	body, err := d.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: initial.DatabaseID, SessionID: "chat", MessageID: ref.MessageID, Revision: ref.Revision})
	require.NoError(t, err)
	assert.Equal(t, "new-project", body.Project.DisplayLabel)
	assert.Equal(t, ref.Digest, body.Digest)
	assert.Equal(t, ref.Revision, body.Revision)
}

func TestConversationExportResyncKeepsIdentityAndOrphans(t *testing.T) {
	ctx := context.Background()
	source := testDB(t)
	for _, id := range []string{"chat", "orphan", "legacy"} {
		require.NoError(t, source.UpsertSession(Session{ID: id, Project: "sample", Machine: "local", Agent: "codex"}))
	}
	msgs := []Message{{SessionID: "chat", Role: "user", Content: "Question", VisibleText: new("Question")}}
	require.NoError(t, source.InsertMessages(msgs))
	require.NoError(t, source.InsertMessages([]Message{{SessionID: "orphan", Role: "assistant", Content: "Retained", VisibleText: new("Retained"), ConversationSourceID: "one"}}))
	require.NoError(t, source.InsertMessages([]Message{{SessionID: "legacy", Role: "assistant", Content: "Unproven"}}))
	initial, err := source.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 3)
	destination, err := Open(filepath.Join(t.TempDir(), "rebuilt.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, destination.Close()) })
	require.NoError(t, destination.CopyArchiveIdentityFrom(source.Path()))
	require.NoError(t, destination.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
	require.NoError(t, destination.InsertMessages(msgs))
	_, err = destination.CopyOrphanedDataFrom(source.Path())
	require.NoError(t, err)
	rebuilt, err := destination.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	assert.Equal(t, initial.ArchiveID, rebuilt.ArchiveID)
	assert.NotEqual(t, initial.DatabaseID, rebuilt.DatabaseID)
	bySession := map[string]ConversationChange{}
	for _, change := range rebuilt.Changes {
		if change.Type == "message" {
			bySession[change.SessionID] = change
		}
	}
	for _, change := range initial.Changes {
		assert.Equal(t, change.MessageID, bySession[change.SessionID].MessageID)
	}
	assert.Equal(t, "visible_text_unavailable", bySession["legacy"].Gap)
	_, err = destination.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
	assert.ErrorIs(t, err, ErrConversationReconciliationRequired)
}

func TestConversationExportCopiedUsagePolicyDropsBody(t *testing.T) {
	ctx := context.Background()
	source := testDB(t)
	require.NoError(t, source.UpsertSession(Session{ID: "orphan", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, source.InsertMessages([]Message{{SessionID: "orphan", Role: "assistant", Content: "Do not retain", VisibleText: new("Do not retain"), ConversationSourceID: "one"}}))
	destination := testDB(t)
	destination.SetArchiveContent(config.ArchiveContentUsage)
	_, err := destination.CopyOrphanedDataFrom(source.Path())
	require.NoError(t, err)
	result, err := destination.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, result.Changes, 2)
	assert.Equal(t, "session", result.Changes[1].Type)
	assert.Equal(t, "archive_content_excluded", result.Changes[1].Gap)
	change := result.Changes[0]
	body, err := destination.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: result.DatabaseID, SessionID: "orphan", MessageID: change.MessageID, Revision: change.Revision})
	require.NoError(t, err)
	assert.Nil(t, body.Text)
	assert.Equal(t, "archive_content_excluded", body.Gap)
}

func TestConversationExportNoSourceIdentityIsExplicit(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
	msgs := []Message{{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Question", VisibleText: new("Question")}}
	require.NoError(t, d.InsertMessages(msgs))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 1)
	assert.Equal(t, "identity_unavailable", initial.Changes[0].Gap)

	require.NoError(t, d.ReplaceSessionMessages("chat", msgs))
	quiet, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, quiet.Changes)

	msgs[0].Content, msgs[0].VisibleText = "Different question", new("Different question")
	require.NoError(t, d.ReplaceSessionMessages("chat", msgs))
	delta, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: quiet.Checkpoint})
	require.NoError(t, err)
	require.Len(t, delta.Changes, 2)
	assert.True(t, delta.Changes[0].Deleted)
	assert.Equal(t, initial.Changes[0].MessageID, delta.Changes[0].MessageID)
	assert.Equal(t, "identity_ambiguous", delta.Changes[1].Gap)
	assert.NotEqual(t, initial.Changes[0].MessageID, delta.Changes[1].MessageID)
}

func TestConversationExportDeletionAndRestore(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages([]Message{{SessionID: "chat", Role: "user", Content: "Question", VisibleText: new("Question"), ConversationSourceID: "one"}}))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 1)
	id := initial.Changes[0].MessageID
	require.NoError(t, d.SoftDeleteSession("chat"))
	deleted, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	require.Len(t, deleted.Changes, 2)
	assert.True(t, deleted.Changes[0].Deleted)
	_, err = d.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: deleted.DatabaseID, SessionID: "chat", MessageID: id, Revision: deleted.Changes[0].Revision})
	assert.ErrorIs(t, err, ErrConversationRevisionChanged)
	_, err = d.RestoreSession("chat")
	require.NoError(t, err)
	restored, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: deleted.Checkpoint})
	require.NoError(t, err)
	require.Len(t, restored.Changes, 2)
	assert.False(t, restored.Changes[0].Deleted)
	assert.Equal(t, id, restored.Changes[0].MessageID)
	body, err := d.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: restored.DatabaseID, SessionID: "chat", MessageID: id, Revision: restored.Changes[0].Revision})
	require.NoError(t, err)
	assert.Equal(t, "Question", *body.Text)
	require.NoError(t, d.DeleteSession("chat"))
	purged, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: restored.Checkpoint})
	require.NoError(t, err)
	var removedMessage bool
	for _, change := range purged.Changes {
		if change.Type == "message" && change.MessageID == id {
			removedMessage = change.Deleted
		}
	}
	assert.True(t, removedMessage)
}

func TestConversationExportWriterLifecycle(t *testing.T) {
	for _, writer := range []string{"content", "batch", "atomic", "staged"} {
		t.Run(writer, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			session := Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude", MessageCount: 1}
			require.NoError(t, d.UpsertSession(session))
			write := func(msgs []Message) {
				t.Helper()
				switch writer {
				case "content":
					require.NoError(t, d.ReplaceSessionContent("chat", msgs, SessionSignalUpdate{}, nil))
				case "batch", "atomic":
					writes := []SessionBatchWrite{{Session: session, Messages: msgs, ReplaceMessages: true, DataVersion: CurrentDataVersion()}}
					var result SessionBatchResult
					var err error
					if writer == "batch" {
						result, err = d.WriteSessionBatch(writes)
					} else {
						result, err = d.WriteSessionBatchAtomic(writes)
					}
					require.NoError(t, err)
					require.Equal(t, 1, result.WrittenSessions)
				case "staged":
					require.NoError(t, d.ReplaceSessionContentStaged(ctx, "chat", msgs, newScratchStagedResults(t), nil, nil))
				}
			}
			msgs := []Message{{SessionID: "chat", Role: "assistant", Content: "Reply", ConversationSourceID: "one"}}
			write(msgs)
			unknown, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, unknown.Changes, 1)
			assert.Equal(t, "visible_text_unavailable", unknown.Changes[0].Gap)
			// The physical transcript is identical; the staged unchanged path must
			// still commit newly available parser proof.
			msgs[0].VisibleText = new("Reply")
			write(msgs)
			proven, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: unknown.Checkpoint})
			require.NoError(t, err)
			require.Len(t, proven.Changes, 1)
			assert.Equal(t, unknown.Changes[0].MessageID, proven.Changes[0].MessageID)
			body, err := d.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: proven.DatabaseID, SessionID: "chat", MessageID: proven.Changes[0].MessageID, Revision: proven.Changes[0].Revision})
			require.NoError(t, err)
			require.NotNil(t, body.Text)
			assert.Equal(t, "Reply", *body.Text)
			write(msgs)
			quiet, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: proven.Checkpoint})
			require.NoError(t, err)
			assert.Empty(t, quiet.Changes)
			assert.Equal(t, proven.Checkpoint, quiet.Checkpoint)
		})
	}
}

func TestConversationExportArchivedRewritePreservesProof(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages([]Message{{SessionID: "chat", Role: "assistant", Content: "Reply", VisibleText: new("Reply"), ConversationSourceID: "one"}}))
	initial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	loaded, err := d.GetAllMessages(t.Context(), "chat")
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Nil(t, loaded[0].VisibleText)
	require.NoError(t, d.ReplaceSessionMessages("chat", loaded))
	quiet, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, quiet.Changes)
}

func TestConversationExportUsageOnlySessionGap(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	require.NoError(t, d.RenameSession("missing", new("Name")))
	require.NoError(t, d.RefreshSessionName("missing", new("Name")))
	require.NoError(t, d.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude", MessageCount: 1, UserMessageCount: 1}))
	require.NoError(t, d.InsertMessages([]Message{{SessionID: "chat", Role: "user", Content: "Question", VisibleText: new("Question"), ConversationSourceID: "one"}}))
	page, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, page.Changes, 1)
	assert.Equal(t, "session", page.Changes[0].Type)
	assert.Equal(t, "archive_content_excluded", page.Changes[0].Gap)
	assert.Empty(t, page.Changes[0].Digest)
	assert.Zero(t, page.Changes[0].TextBytes)
	rows, err := d.GetAllMessages(t.Context(), "chat")
	require.NoError(t, err)
	assert.Empty(t, rows)
	require.NoError(t, d.InsertMessages([]Message{{SessionID: "chat", Role: "user", Content: "Question", VisibleText: new("Question"), ConversationSourceID: "one"}}))
	quiet, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: page.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, quiet.Changes)
	assert.Equal(t, page.Checkpoint, quiet.Checkpoint)
	require.NoError(t, d.UpsertProjectIdentityObservationWithSnapshotProject(t.Context(), export.ProjectIdentityObservation{SessionID: "chat", Project: "remapped", Machine: "local"}, "remapped"))
	remapped, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: quiet.Checkpoint})
	require.NoError(t, err)
	require.Len(t, remapped.Changes, 1)
	assert.Equal(t, "session", remapped.Changes[0].Type)
	assert.Equal(t, "archive_content_excluded", remapped.Changes[0].Gap)
	assert.Equal(t, "remapped", remapped.Changes[0].Project.DisplayLabel)
	// A new full-content handle can reparse the source; the old handle cannot
	// loosen its own policy. Newly proven content clears the old coverage gap.
	path := d.Path()
	require.NoError(t, d.Close())
	full, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, full.Close()) })
	require.NoError(t, full.ReplaceSessionMessages("chat", []Message{{SessionID: "chat", Role: "user", Content: "Question", VisibleText: new("Question"), ConversationSourceID: "one"}}))
	reparsed, err := full.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: remapped.Checkpoint})
	require.NoError(t, err)
	require.Len(t, reparsed.Changes, 2)
	for _, change := range reparsed.Changes {
		assert.Empty(t, change.Gap)
	}
}

func TestConversationExportNativeRemovalAndReturn(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Question", VisibleText: new("Question"), ConversationSourceID: "one"}, {SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Reply", VisibleText: new("Reply"), ConversationSourceID: "two"}}
	require.NoError(t, d.InsertMessages(msgs))
	initial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 2)
	require.NoError(t, d.ReplaceSessionMessages("chat", msgs[:1]))
	deleted, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	require.Len(t, deleted.Changes, 1)
	assert.Equal(t, initial.Changes[1].MessageID, deleted.Changes[0].MessageID)
	assert.True(t, deleted.Changes[0].Deleted)
	require.NoError(t, d.ReplaceSessionMessages("chat", msgs))
	restored, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: deleted.Checkpoint})
	require.NoError(t, err)
	require.Len(t, restored.Changes, 1)
	assert.Equal(t, initial.Changes[1].MessageID, restored.Changes[0].MessageID)
	assert.False(t, restored.Changes[0].Deleted)
	assert.Empty(t, restored.Changes[0].Gap)
}

func TestConversationExportResyncRetainsHardDeletion(t *testing.T) {
	source := testDB(t)
	for _, id := range []string{"chat", "empty"} {
		require.NoError(t, source.UpsertSession(Session{ID: id, Project: "sample", Machine: "local", Agent: "claude"}))
	}
	require.NoError(t, source.InsertMessages([]Message{{SessionID: "chat", Role: "assistant", Content: "Reply", VisibleText: new("Reply"), ConversationSourceID: "one"}}))
	require.NoError(t, source.DeleteSession("chat"))
	require.NoError(t, source.DeleteSession("empty"))
	destination := testDB(t)
	_, err := destination.CopyOrphanedDataFrom(source.Path())
	require.NoError(t, err)
	changes, err := destination.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	deletedSessions := map[string]bool{}
	deletedMessages := 0
	for _, change := range changes.Changes {
		assert.True(t, change.Deleted)
		if change.Type == "session" {
			deletedSessions[change.SessionID] = true
		} else {
			deletedMessages++
		}
	}
	assert.Equal(t, map[string]bool{"chat": true, "empty": true}, deletedSessions)
	assert.Equal(t, 1, deletedMessages)
}

func TestConversationExportLegacyArchiveGaps(t *testing.T) {
	for _, mode := range []string{"upgrade", "orphan-copy"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			require.NoError(t, d.UpsertSession(Session{ID: "legacy", Project: "sample", Machine: "local", Agent: "codex"}))
			require.NoError(t, d.InsertMessages([]Message{{SessionID: "legacy", Role: "assistant", Content: "raw flattened tool content"}}))
			// Model the pre-projection archive, without running a historical binary.
			require.NoError(t, d.Update(func(tx *sql.Tx) error {
				rows, err := tx.Query(`SELECT name FROM sqlite_master WHERE type='trigger' AND name LIKE 'conversation_%'`)
				if err != nil {
					return err
				}
				names, err := scanStrings(rows)
				if err != nil {
					return err
				}
				for _, name := range names {
					if _, err := tx.Exec(`DROP TRIGGER "` + name + `"`); err != nil {
						return err
					}
				}
				_, err = tx.Exec(`DROP TABLE conversation_messages; DROP TABLE conversation_session_changes;
				 DELETE FROM archive_metadata WHERE key IN ('conversation_export_initialized','conversation_publication_revision'); PRAGMA user_version=109`)
				return err
			}))
			path := d.Path()
			require.NoError(t, d.Close())
			if mode == "upgrade" {
				_, err := OpenReadOnly(path)
				require.Error(t, err)
				var schemaErr *SchemaUpgradeRequiredError
				require.ErrorAs(t, err, &schemaErr)
				var openErr error
				d, openErr = Open(path)
				require.NoError(t, openErr)
				t.Cleanup(func() { require.NoError(t, d.Close()) })
				assert.True(t, d.NeedsResync())
			} else {
				d = testDB(t)
				_, err := d.CopyOrphanedDataFrom(path)
				require.NoError(t, err)
			}
			page, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, page.Changes, 1)
			ref := page.Changes[0]
			assert.Equal(t, "visible_text_unavailable", ref.Gap)
			assert.Nil(t, ref.Timestamp)
			body, err := d.GetConversationMessage(t.Context(), ConversationMessageOptions{DatabaseID: page.DatabaseID, SessionID: "legacy", MessageID: ref.MessageID, Revision: ref.Revision})
			require.NoError(t, err)
			assert.Nil(t, body.Text)
		})
	}
}

func TestConversationExportFailedWritePublishesNothing(t *testing.T) {
	d := testDB(t)
	before, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	// Projection happens before the physical insert; the absent session makes
	// that insert fail its foreign key and must roll back the projection too.
	err = d.InsertMessages([]Message{{SessionID: "absent", Role: "user", Content: "Question", VisibleText: new("Question"), ConversationSourceID: "one"}})
	require.Error(t, err)
	after, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: before.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, after.Changes)
	assert.Equal(t, before.Checkpoint, after.Checkpoint)
}

func TestConversationExportProjectSnapshotDuringTrash(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages([]Message{{SessionID: "chat", Role: "assistant", Content: "Reply", VisibleText: new("Reply"), ConversationSourceID: "one"}}))
	require.NoError(t, d.UpsertProjectIdentityObservationWithSnapshotProject(ctx, export.ProjectIdentityObservation{SessionID: "chat", Project: "remapped", Machine: "local"}, "remapped"))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 2)
	messageID := initial.Changes[0].MessageID
	require.NoError(t, d.SoftDeleteSession("chat"))
	require.NoError(t, d.UpsertProjectIdentityObservationWithSnapshotProject(ctx, export.ProjectIdentityObservation{SessionID: "chat", Project: "backfilled", Machine: "local"}, "backfilled"))
	trashed, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, trashed.Changes, 2)
	for _, change := range trashed.Changes {
		assert.True(t, change.Deleted)
	}
	_, err = d.RestoreSession("chat")
	require.NoError(t, err)
	restored, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: trashed.Checkpoint})
	require.NoError(t, err)
	require.Len(t, restored.Changes, 2)
	var message ConversationChange
	for _, change := range restored.Changes {
		assert.False(t, change.Deleted)
		assert.Equal(t, "backfilled", change.Project.DisplayLabel)
		if change.Type == "message" {
			message = change
		}
	}
	assert.Equal(t, messageID, message.MessageID)
	body, err := d.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: restored.DatabaseID, SessionID: "chat", MessageID: message.MessageID, Revision: message.Revision})
	require.NoError(t, err)
	require.NotNil(t, body.Text)
	assert.Equal(t, "Reply", *body.Text)
}

func TestConversationExportDuplicateSourceIdentityRemainsAmbiguous(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{{SessionID: "chat", Ordinal: 0, Role: "assistant", Content: "First", VisibleText: new("First"), ConversationSourceID: "duplicate"}, {SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Second", VisibleText: new("Second"), ConversationSourceID: "duplicate"}}
	require.NoError(t, d.InsertMessages(msgs))
	initial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 2)
	for _, change := range initial.Changes {
		assert.Equal(t, "identity_ambiguous", change.Gap)
	}
	require.NoError(t, d.ReplaceSessionMessages("chat", msgs[1:]))
	removed, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	require.Len(t, removed.Changes, 3)
	assert.True(t, removed.Changes[0].Deleted)
	assert.True(t, removed.Changes[1].Deleted)
	assert.Equal(t, "identity_ambiguous", removed.Changes[2].Gap)
	assert.NotEqual(t, initial.Changes[0].MessageID, removed.Changes[2].MessageID)
	assert.NotEqual(t, initial.Changes[1].MessageID, removed.Changes[2].MessageID)
}

func BenchmarkConversationExport(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprintf("messages_%d", count), func(b *testing.B) {
			d := testDB(b)
			require.NoError(b, d.UpsertSession(Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
			text := strings.Repeat("sample prose ", 6000)
			msgs := make([]Message, count)
			for i := range msgs {
				msgs[i] = Message{SessionID: "chat", Ordinal: i, Role: "assistant", Content: "sample", VisibleText: new("sample"), ConversationSourceID: fmt.Sprintf("one-%d", i)}
			}
			msgs[0].Content, msgs[0].VisibleText = text, new(text)
			require.NoError(b, d.InsertMessages(msgs))
			page, err := d.ExportConversationChanges(b.Context(), ConversationExportOptions{})
			require.NoError(b, err)
			target := page.Changes[0]
			for page.NextCursor != "" {
				page, err = d.ExportConversationChanges(b.Context(), ConversationExportOptions{Cursor: page.NextCursor})
				require.NoError(b, err)
			}
			b.Run("empty_poll", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got, err := d.ExportConversationChanges(b.Context(), ConversationExportOptions{Checkpoint: page.Checkpoint})
					if err != nil || len(got.Changes) != 0 || got.Checkpoint != page.Checkpoint {
						b.Fatalf("empty poll: %v, %d changes", err, len(got.Changes))
					}
				}
			})
			b.Run("body_64KiB", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got, err := d.GetConversationMessage(b.Context(), ConversationMessageOptions{DatabaseID: page.DatabaseID, SessionID: "chat", MessageID: target.MessageID, Revision: target.Revision})
					if err != nil || got.Text == nil || len(*got.Text) != 64<<10 {
						b.Fatalf("chunk read: %v", err)
					}
				}
			})
			msgs[count-1].Content, msgs[count-1].VisibleText = "changed", new("changed")
			require.NoError(b, d.ReplaceSessionMessages("chat", msgs))
			b.Run("one_changed_message", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got, err := d.ExportConversationChanges(b.Context(), ConversationExportOptions{Checkpoint: page.Checkpoint})
					if err != nil || len(got.Changes) != 1 || got.Changes[0].Ordinal != count-1 {
						b.Fatalf("changed message: %v, %d changes", err, len(got.Changes))
					}
				}
			})
		})
	}
}
