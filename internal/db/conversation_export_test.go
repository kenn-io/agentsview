package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
)

func TestConversationExportUsesStoredMessagesForEveryAgent(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "gemini", "opencode", "other-agent"} {
		t.Run(agent, func(t *testing.T) {
			d := testDB(t)
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: agent}))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{
				{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Check the saved conversation.", SourceUUID: "user-one"},
				{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "The saved reply.", ThinkingText: "Separate reasoning", SourceUUID: "reply-one"},
				{SessionID: "chat", Ordinal: 2, Role: "user", Content: "Internal instructions", IsSystem: true},
				{SessionID: "chat", Ordinal: 3, Role: "tool", Content: "Tool output"},
			}))
			changes, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, changes.Changes, 2)
			var text []string
			for _, change := range changes.Changes {
				body, err := d.GetConversationMessage(t.Context(), ConversationMessageOptions{
					DatabaseID: changes.DatabaseID, SessionID: "chat", MessageID: change.MessageID, Revision: change.Revision,
				})
				require.NoError(t, err)
				require.NotNil(t, body.Text)
				text = append(text, *body.Text)
			}
			assert.Equal(t, []string{"Check the saved conversation.", "The saved reply."}, text)
		})
	}
}

func TestConversationExportFreshArchiveSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.db")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	d, err := OpenFreshIsolatedContext(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{
		SessionID: "chat", Role: "user", Content: "Check this code",
		SourceUUID: "user-one",
	}}))
	initial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 1)
	change := initial.Changes[0]
	assert.Empty(t, change.Gap)
	body, err := d.GetConversationMessage(t.Context(), ConversationMessageOptions{
		DatabaseID: initial.DatabaseID, SessionID: "chat", MessageID: change.MessageID, Revision: change.Revision,
	})
	require.NoError(t, err)
	require.NotNil(t, body.Text)
	assert.Equal(t, "Check this code", *body.Text)

	require.NoError(t, d.Close())
	reopened, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	current, err := reopened.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	assert.Equal(t, initial.Changes, current.Changes, "reopening must preserve identities without adding placeholder gaps")
	delta, err := reopened.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, delta.Changes)
}

func TestConversationExportUsageReopenRejectsStoredBodies(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(policy)
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "gemini"}))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{
				SessionID: "chat", Role: "assistant", Content: "Saved reply", SourceUUID: "reply-one",
			}}))
			initial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, initial.Changes, 1)
			original := initial.Changes[0]
			opts := ConversationMessageOptions{
				DatabaseID: initial.DatabaseID, SessionID: "chat", MessageID: original.MessageID, Revision: original.Revision,
			}
			body, err := d.GetConversationMessage(t.Context(), opts)
			require.NoError(t, err)
			require.NotNil(t, body.Text)
			assert.Equal(t, "Saved reply", *body.Text)
			require.NoError(t, d.Close())

			usage, err := OpenWithArchiveContent(t.Context(), d.Path(), config.ArchiveContentUsage)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, usage.Close()) })
			body, err = usage.GetConversationMessage(t.Context(), opts)
			require.ErrorIs(t, err, ErrArchiveContentExcluded)
			assert.Nil(t, body.Text)
			require.NoError(t, usage.Close())

			// Reopening does not rewrite normalized messages. If their text is
			// still stored, returning to the original policy makes it available.
			reopened, err := OpenWithArchiveContent(t.Context(), d.Path(), policy)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			body, err = reopened.GetConversationMessage(t.Context(), opts)
			require.NoError(t, err)
			require.NotNil(t, body.Text)
			assert.Equal(t, "Saved reply", *body.Text)
		})
	}
}

// A token-only rewrite must not resend prose, while a streamed text change
// must retain the source message's identity and publish the new body.
func TestConversationExportNativeMessageChanges(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{
		{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Question", SourceUUID: "user-one"},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Partial", SourceUUID: "reply-one"},
	}
	require.NoError(t, d.InsertMessages(t.Context(), msgs))
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
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
	quiet, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, quiet.Changes)

	msgs[1].Content = "Complete answer"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
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
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{{SessionID: "chat", Role: "assistant", Content: "a😀bcédef", SourceUUID: "reply"}}
	require.NoError(t, d.InsertMessages(t.Context(), msgs))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 1)
	change := initial.Changes[0]
	opts := ConversationMessageOptions{DatabaseID: initial.DatabaseID, SessionID: "chat", MessageID: change.MessageID, Revision: change.Revision, MaxBytes: 4}
	var joined string
	var joinedSb104 strings.Builder
	for {
		chunk, err := d.GetConversationMessage(ctx, opts)
		require.NoError(t, err)
		require.NotNil(t, chunk.Text)
		assert.LessOrEqual(t, len(*chunk.Text), 4)
		assert.Equal(t, change.Digest, chunk.Digest)
		assert.Equal(t, change.Project, chunk.Project)
		joinedSb104.WriteString(*chunk.Text)
		if chunk.NextOffset == chunk.TextBytes {
			break
		}
		require.Greater(t, chunk.NextOffset, opts.Offset)
		opts.Offset = chunk.NextOffset
	}
	joined += joinedSb104.String()
	assert.Equal(t, "a😀bcédef", joined)
	opts.Offset = 2
	_, err = d.GetConversationMessage(ctx, opts)
	require.Error(t, err)
	opts.Offset = 0
	opts.DatabaseID = "different-generation"
	_, err = d.GetConversationMessage(ctx, opts)
	require.ErrorIs(t, err, ErrConversationReconciliationRequired)
	opts.DatabaseID = initial.DatabaseID
	msgs[0].Content = "new text"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
	chunk, err := d.GetConversationMessage(ctx, opts)
	require.ErrorIs(t, err, ErrConversationRevisionChanged)
	assert.Nil(t, chunk.Text)
}

func TestConversationExportPaginationDefersConcurrentChanges(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := make([]Message, 12)
	for i := range msgs {
		text := fmt.Sprintf("Message %d", i)
		msgs[i] = Message{SessionID: "chat", Ordinal: i, Role: "assistant", Content: text, SourceUUID: fmt.Sprintf("reply-%d", i)}
	}
	require.NoError(t, d.InsertMessages(t.Context(), msgs))
	page, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Limit: 3})
	require.NoError(t, err)
	require.Len(t, page.Changes, 3)
	assert.Equal(t, []int{0, 1, 2}, []int{page.Changes[0].Ordinal, page.Changes[1].Ordinal, page.Changes[2].Ordinal})
	msgs[5].Content = "Revised"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
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
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "chat", Role: "user", Content: "Question", SourceUUID: "one"}}))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 1)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "new-project", Machine: "local", Agent: "claude"}))
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
	ctx := t.Context()
	source := testDB(t)
	for _, id := range []string{"chat", "orphan", "legacy"} {
		require.NoError(t, source.UpsertSession(t.Context(), Session{ID: id, Project: "sample", Machine: "local", Agent: "codex"}))
	}
	msgs := []Message{{SessionID: "chat", Role: "user", Content: "Question"}}
	require.NoError(t, source.InsertMessages(t.Context(), msgs))
	require.NoError(t, source.InsertMessages(t.Context(), []Message{{SessionID: "orphan", Role: "assistant", Content: "Retained", SourceUUID: "one"}}))
	require.NoError(t, source.InsertMessages(t.Context(), []Message{{SessionID: "legacy", Role: "assistant", Content: "Unproven"}}))
	initial, err := source.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 3)
	destination, err := Open(t.Context(), filepath.Join(t.TempDir(), "rebuilt.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, destination.Close()) })
	require.NoError(t, destination.CopyArchiveIdentityFrom(source.Path()))
	require.NoError(t, destination.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
	require.NoError(t, destination.InsertMessages(t.Context(), msgs))
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
	assert.Equal(t, "identity_unavailable", bySession["legacy"].Gap)
	_, err = destination.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.ErrorIs(t, err, ErrConversationReconciliationRequired)
}

func TestConversationExportCopiedUsagePolicyDropsBody(t *testing.T) {
	ctx := t.Context()
	source := testDB(t)
	require.NoError(t, source.UpsertSession(t.Context(), Session{ID: "orphan", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, source.InsertMessages(t.Context(), []Message{{SessionID: "orphan", Role: "assistant", Content: "Do not retain", SourceUUID: "one"}}))
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
	reader, err := OpenReadOnly(ctx, destination.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	body, err := reader.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: result.DatabaseID, SessionID: "orphan", MessageID: change.MessageID, Revision: change.Revision})
	require.NoError(t, err)
	assert.Nil(t, body.Text)
	assert.Equal(t, "archive_content_excluded", body.Gap)
}

func TestConversationExportUsesFinalCopiedContent(t *testing.T) {
	for _, trashed := range []bool{false, true} {
		t.Run(fmt.Sprintf("trashed=%t", trashed), func(t *testing.T) {
			source := testDB(t)
			require.NoError(t, source.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "opencode"}))
			require.NoError(t, source.InsertMessages(t.Context(), []Message{{
				SessionID: "chat", Role: "assistant", HasToolUse: true, Content: "Checking.\n[Bash]\n$ echo payload",
				ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"echo payload"}`}},
			}}))
			initial, err := source.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, initial.Changes, 1)
			if trashed {
				require.NoError(t, source.SoftDeleteSession(t.Context(), "chat"))
			}
			destination := testDB(t)
			destination.SetArchiveContent(config.ArchiveContentTranscripts)
			_, err = destination.CopyTrashedDataFrom(source.Path())
			require.NoError(t, err)
			_, err = destination.CopyOrphanedDataFrom(source.Path())
			require.NoError(t, err)
			if trashed {
				_, err = destination.RestoreSession(t.Context(), "chat")
				require.NoError(t, err)
			}
			changes, err := destination.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			messageCount := 0
			for _, change := range changes.Changes {
				if change.Type != "message" {
					continue
				}
				messageCount++
				assert.Equal(t, initial.Changes[0].MessageID, change.MessageID)
				body, err := destination.GetConversationMessage(t.Context(), ConversationMessageOptions{DatabaseID: changes.DatabaseID, SessionID: "chat", MessageID: change.MessageID, Revision: change.Revision})
				require.NoError(t, err)
				require.NotNil(t, body.Text)
				assert.Equal(t, "Checking.\n[Bash]", *body.Text)
			}
			assert.Equal(t, 1, messageCount)
		})
	}
}

func TestConversationExportResyncPreservesCopiedPolicyGap(t *testing.T) {
	for _, trashed := range []bool{false, true} {
		t.Run(fmt.Sprintf("trashed=%t", trashed), func(t *testing.T) {
			source := testDB(t)
			source.SetArchiveContent(config.ArchiveContentUsage)
			require.NoError(t, source.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
			require.NoError(t, source.InsertMessages(t.Context(), []Message{{SessionID: "chat", Role: "assistant", Content: "Unclassified transcript"}}))
			if trashed {
				require.NoError(t, source.SoftDeleteSession(t.Context(), "chat"))
			}
			destination := testDB(t)
			_, err := destination.CopyTrashedDataFrom(source.Path())
			require.NoError(t, err)
			_, err = destination.CopyOrphanedDataFrom(source.Path())
			require.NoError(t, err)
			if trashed {
				_, err = destination.RestoreSession(t.Context(), "chat")
				require.NoError(t, err)
			}
			copied, err := destination.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, copied.Changes, 2)
			for _, change := range copied.Changes {
				assert.Equal(t, "archive_content_excluded", change.Gap, change.Type)
			}
		})
	}
}

func TestConversationExportFullRewriteClearsPolicyGap(t *testing.T) {
	for _, sourceID := range []string{"", "source-one"} {
		t.Run("source="+sourceID, func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(config.ArchiveContentUsage)
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
			msgs := []Message{{SessionID: "chat", Role: "assistant", Content: "", SourceUUID: sourceID}}
			require.NoError(t, d.InsertMessages(t.Context(), msgs))
			initial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, initial.Changes, 2)
			message := initial.Changes[1]
			assert.Equal(t, "message", message.Type)
			assert.Equal(t, "archive_content_excluded", message.Gap)
			assert.Equal(t, "archive_content_excluded", initial.Changes[0].Gap)

			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
			quiet, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: initial.Checkpoint})
			require.NoError(t, err)
			assert.Empty(t, quiet.Changes, "an unchanged usage-only rewrite must not republish gaps")

			path := d.Path()
			require.NoError(t, d.Close())
			full, err := OpenIsolated(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, full.Close()) })
			require.NoError(t, full.ReplaceSessionMessages(t.Context(), "chat", msgs))
			updated, err := full.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: quiet.Checkpoint})
			require.NoError(t, err)
			require.Len(t, updated.Changes, 2)
			assert.Equal(t, message.MessageID, updated.Changes[0].MessageID)
			assert.Equal(t, "visible_text_unavailable", updated.Changes[0].Gap)
			assert.Equal(t, "session", updated.Changes[1].Type)
			assert.Empty(t, updated.Changes[1].Gap)
			body, err := full.GetConversationMessage(t.Context(), ConversationMessageOptions{
				DatabaseID: updated.DatabaseID, SessionID: "chat", MessageID: message.MessageID, Revision: updated.Changes[0].Revision,
			})
			require.NoError(t, err)
			assert.Nil(t, body.Text)
			assert.Equal(t, "visible_text_unavailable", body.Gap)

			require.NoError(t, full.ReplaceSessionMessages(t.Context(), "chat", msgs))
			quiet, err = full.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: updated.Checkpoint})
			require.NoError(t, err)
			assert.Empty(t, quiet.Changes)
		})
	}
}

func TestConversationExportUsageRewritePreservesPolicyGaps(t *testing.T) {
	for _, writer := range []string{"messages", "content", "batch", "atomic", "rebuild"} {
		t.Run(writer, func(t *testing.T) {
			d := testDB(t)
			session := Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}
			msgs := []Message{
				{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Keep this prompt", SourceUUID: "prompt-one"},
				{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Reply without source identity"},
				{SessionID: "chat", Ordinal: 2, Role: "user", Content: "Remove this prompt later", SourceUUID: "prompt-two"},
			}
			require.NoError(t, d.UpsertSession(t.Context(), session))
			require.NoError(t, d.InsertMessages(t.Context(), msgs))
			initial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, initial.Changes, 3)
			ids := map[int]string{}
			for _, change := range initial.Changes {
				ids[change.Ordinal] = change.MessageID
			}
			if writer == "rebuild" {
				source := d
				d = testDB(t)
				d.SetArchiveContent(config.ArchiveContentUsage)
				require.NoError(t, d.CopyArchiveIdentityFrom(source.Path()))
				require.NoError(t, d.UpsertSession(t.Context(), session))
				require.NoError(t, d.InsertMessages(t.Context(), msgs))
				_, err = d.CopyOrphanedDataFrom(source.Path())
				require.NoError(t, err)
			} else {
				d.SetArchiveContent(config.ArchiveContentUsage)
				switch writer {
				case "messages":
					require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
				case "content":
					require.NoError(t, d.ReplaceSessionContent(t.Context(), "chat", msgs, SessionSignalUpdate{}, nil))
				case "batch", "atomic":
					writes := []SessionBatchWrite{{Session: session, Messages: msgs, ReplaceMessages: true, DataVersion: CurrentDataVersion()}}
					var result SessionBatchResult
					if writer == "batch" {
						result, err = d.WriteSessionBatch(writes)
					} else {
						result, err = d.WriteSessionBatchAtomic(t.Context(), writes)
					}
					require.NoError(t, err)
					require.Equal(t, 1, result.WrittenSessions)
				}
			}
			page, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, page.Changes, 4, "three retained messages and the session coverage gap")
			reader, err := OpenReadOnly(t.Context(), d.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reader.Close()) })
			for _, change := range page.Changes {
				assert.False(t, change.Deleted)
				assert.Equal(t, "archive_content_excluded", change.Gap)
				assert.Empty(t, change.Digest)
				assert.Zero(t, change.TextBytes)
				if change.Type == "message" {
					assert.Equal(t, ids[change.Ordinal], change.MessageID)
					body, err := reader.GetConversationMessage(t.Context(), ConversationMessageOptions{DatabaseID: page.DatabaseID, SessionID: "chat", MessageID: change.MessageID, Revision: change.Revision})
					require.NoError(t, err)
					assert.Nil(t, body.Text, "excluded text must be removed from storage")
				}
			}
			require.NoError(t, reader.Close())
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
			quiet, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: page.Checkpoint})
			require.NoError(t, err)
			assert.Empty(t, quiet.Changes, "repeated usage writes preserve IDs even without source identity")
			require.NoError(t, d.Close())
			full, err := Open(t.Context(), d.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, full.Close()) })
			require.NoError(t, full.ReplaceSessionMessages(t.Context(), "chat", msgs[:1]))
			restored, err := full.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, restored.Changes, 4)
			for _, change := range restored.Changes {
				if change.Type == "message" {
					assert.Equal(t, ids[change.Ordinal], change.MessageID)
					assert.Equal(t, change.Ordinal != 0, change.Deleted, "a complete replacement distinguishes real removals")
				}
			}
		})
	}
}

func TestConversationExportNoSourceIdentityIsExplicit(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
	msgs := []Message{{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Question"}}
	require.NoError(t, d.InsertMessages(t.Context(), msgs))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 1)
	assert.Equal(t, "identity_unavailable", initial.Changes[0].Gap)

	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
	quiet, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, quiet.Changes)

	msgs[0].Content = "Different question"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
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
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "chat", Role: "user", Content: "Question", SourceUUID: "one"}}))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 1)
	id := initial.Changes[0].MessageID
	require.NoError(t, d.SoftDeleteSession(t.Context(), "chat"))
	deleted, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	require.Len(t, deleted.Changes, 2)
	assert.True(t, deleted.Changes[0].Deleted)
	_, err = d.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: deleted.DatabaseID, SessionID: "chat", MessageID: id, Revision: deleted.Changes[0].Revision})
	require.ErrorIs(t, err, ErrConversationRevisionChanged)
	_, err = d.RestoreSession(t.Context(), "chat")
	require.NoError(t, err)
	restored, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: deleted.Checkpoint})
	require.NoError(t, err)
	require.Len(t, restored.Changes, 2)
	assert.False(t, restored.Changes[0].Deleted)
	assert.Equal(t, id, restored.Changes[0].MessageID)
	body, err := d.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: restored.DatabaseID, SessionID: "chat", MessageID: id, Revision: restored.Changes[0].Revision})
	require.NoError(t, err)
	assert.Equal(t, "Question", *body.Text)
	require.NoError(t, d.DeleteSession(t.Context(), "chat"))
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
			require.NoError(t, d.UpsertSession(t.Context(), session))
			write := func(msgs []Message) {
				t.Helper()
				switch writer {
				case "content":
					require.NoError(t, d.ReplaceSessionContent(t.Context(), "chat", msgs, SessionSignalUpdate{}, nil))
				case "batch", "atomic":
					writes := []SessionBatchWrite{{Session: session, Messages: msgs, ReplaceMessages: true, DataVersion: CurrentDataVersion()}}
					var result SessionBatchResult
					var err error
					if writer == "batch" {
						result, err = d.WriteSessionBatch(writes)
					} else {
						result, err = d.WriteSessionBatchAtomic(ctx, writes)
					}
					require.NoError(t, err)
					require.Equal(t, 1, result.WrittenSessions)
				case "staged":
					require.NoError(t, d.ReplaceSessionContentStaged(ctx, "chat", msgs, newScratchStagedResults(t), nil, nil))
				}
			}
			msgs := []Message{{SessionID: "chat", Role: "assistant", SourceUUID: "one"}}
			write(msgs)
			unknown, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, unknown.Changes, 1)
			assert.Equal(t, "visible_text_unavailable", unknown.Changes[0].Gap)
			// A rewrite supplies text that was absent from the stored message.
			msgs[0].Content = "Reply"
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

func TestConversationExportArchivedRewritePreservesText(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "chat", Role: "assistant", Content: "Reply", SourceUUID: "one"}}))
	initial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	loaded, err := d.GetAllMessages(t.Context(), "chat")
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, "Reply", loaded[0].Content)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", loaded))
	quiet, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, quiet.Changes)
}

func TestConversationExportUsageOnlySessionGap(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	require.NoError(t, d.RenameSession(t.Context(), "missing", new("Name")))
	require.NoError(t, d.RefreshSessionName(t.Context(), "missing", new("Name")))
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude", MessageCount: 1, UserMessageCount: 1}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "chat", Role: "user", Content: "Question", SourceUUID: "one"}}))
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
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "chat", Role: "user", Content: "Question", SourceUUID: "one"}}))
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
	full, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, full.Close()) })
	require.NoError(t, full.ReplaceSessionMessages(t.Context(), "chat", []Message{{SessionID: "chat", Role: "user", Content: "Question", SourceUUID: "one"}}))
	reparsed, err := full.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: remapped.Checkpoint})
	require.NoError(t, err)
	require.Len(t, reparsed.Changes, 2)
	for _, change := range reparsed.Changes {
		assert.Empty(t, change.Gap)
	}
}

func TestConversationExportNativeRemovalAndReturn(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Question", SourceUUID: "one"}, {SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Reply", SourceUUID: "two"}}
	require.NoError(t, d.InsertMessages(t.Context(), msgs))
	initial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 2)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs[:1]))
	deleted, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	require.Len(t, deleted.Changes, 1)
	assert.Equal(t, initial.Changes[1].MessageID, deleted.Changes[0].MessageID)
	assert.True(t, deleted.Changes[0].Deleted)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
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
		require.NoError(t, source.UpsertSession(t.Context(), Session{ID: id, Project: "sample", Machine: "local", Agent: "claude"}))
	}
	require.NoError(t, source.InsertMessages(t.Context(), []Message{{SessionID: "chat", Role: "assistant", Content: "Reply", SourceUUID: "one"}}))
	require.NoError(t, source.DeleteSession(t.Context(), "chat"))
	require.NoError(t, source.DeleteSession(t.Context(), "empty"))
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

func TestConversationExportInitializesFromStoredArchive(t *testing.T) {
	for _, mode := range []string{"upgrade", "interrupted-upgrade", "orphan-copy", "usage-upgrade"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			if mode == "usage-upgrade" {
				d.SetArchiveContent(config.ArchiveContentUsage)
				require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "empty", Project: "sample", Machine: "local", Agent: "gemini"}))
			}
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "archived", Project: "sample", Machine: "local", Agent: "gemini"}))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "archived", Role: "assistant", Content: "A conversation retained in the database.", SourceUUID: "reply-one"}}))
			// Model an archive created before conversation-export tables existed.
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				rows, err := tx.QueryContext(t.Context(), `SELECT name FROM sqlite_master WHERE type='trigger' AND name LIKE 'conversation_%'`)
				if err != nil {
					return err
				}
				names, err := scanStrings(rows)
				if err != nil {
					return err
				}
				for _, name := range names {
					if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER "`+name+`"`); err != nil {
						return err
					}
				}
				_, err = tx.ExecContext(t.Context(), `DROP TABLE conversation_messages; DROP TABLE conversation_session_changes;
				 DELETE FROM archive_metadata WHERE key IN ('conversation_export_initialized','conversation_publication_revision')`)
				return err
			}))
			path := d.Path()
			require.NoError(t, d.Close())
			if mode == "interrupted-upgrade" {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				_, err := OpenWithProgress(ctx, path, config.ArchiveContentFull, func(progress OpenProgress) {
					// The schema transaction has committed, but conversation
					// initialization has not started.
					if progress.Detail == "Initializing full-text search" {
						cancel()
					}
				})
				require.ErrorIs(t, err, context.Canceled)
				reader, err := OpenReadOnly(t.Context(), path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, reader.Close()) })
				page, err := reader.ExportConversationChanges(t.Context(), ConversationExportOptions{})
				require.ErrorIs(t, err, ErrConversationInitializationRequired, "an interrupted upgrade must not export an empty conversation index")
				assert.Empty(t, page.Checkpoint)
				_, err = reader.GetConversationMessage(t.Context(), ConversationMessageOptions{
					DatabaseID: "archive", SessionID: "archived", MessageID: "message", Revision: "1",
				})
				require.ErrorIs(t, err, ErrConversationInitializationRequired)
				require.NoError(t, reader.Close())
			}
			if mode != "orphan-copy" {
				var err error
				if mode == "usage-upgrade" {
					d, err = OpenWithArchiveContent(t.Context(), path, config.ArchiveContentUsage)
				} else {
					d, err = OpenIsolated(t.Context(), path)
				}
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, d.Close()) })
			} else {
				d = testDB(t)
				_, err := d.CopyOrphanedDataFrom(path)
				require.NoError(t, err)
			}
			page, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			if mode == "usage-upgrade" {
				require.Len(t, page.Changes, 3)
				for _, change := range page.Changes {
					assert.Equal(t, "archive_content_excluded", change.Gap, change.Type)
				}
				return
			}
			require.Len(t, page.Changes, 1)
			ref := page.Changes[0]
			assert.False(t, ref.Deleted)
			assert.Empty(t, ref.Gap)
			body, err := d.GetConversationMessage(t.Context(), ConversationMessageOptions{DatabaseID: page.DatabaseID, SessionID: ref.SessionID, MessageID: ref.MessageID, Revision: ref.Revision})
			require.NoError(t, err)
			require.NotNil(t, body.Text)
			assert.Equal(t, "A conversation retained in the database.", *body.Text)
		})
	}
}

func TestConversationExportFailedWritePublishesNothing(t *testing.T) {
	d := testDB(t)
	before, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	// Projection happens before the physical insert; the absent session makes
	// that insert fail its foreign key and must roll back the projection too.
	err = d.InsertMessages(t.Context(), []Message{{SessionID: "absent", Role: "user", Content: "Question", SourceUUID: "one"}})
	require.Error(t, err)
	after, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: before.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, after.Changes)
	assert.Equal(t, before.Checkpoint, after.Checkpoint)
}

func TestConversationExportProjectSnapshotDuringTrash(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "chat", Role: "assistant", Content: "Reply", SourceUUID: "one"}}))
	require.NoError(t, d.UpsertProjectIdentityObservationWithSnapshotProject(ctx, export.ProjectIdentityObservation{SessionID: "chat", Project: "remapped", Machine: "local"}, "remapped"))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 2)
	messageID := initial.Changes[0].MessageID
	require.NoError(t, d.SoftDeleteSession(t.Context(), "chat"))
	require.NoError(t, d.UpsertProjectIdentityObservationWithSnapshotProject(ctx, export.ProjectIdentityObservation{SessionID: "chat", Project: "backfilled", Machine: "local"}, "backfilled"))
	trashed, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, trashed.Changes, 2)
	for _, change := range trashed.Changes {
		assert.True(t, change.Deleted)
	}
	_, err = d.RestoreSession(t.Context(), "chat")
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
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{{SessionID: "chat", Ordinal: 0, Role: "assistant", Content: "First", SourceUUID: "duplicate"}, {SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Second", SourceUUID: "duplicate"}}
	require.NoError(t, d.InsertMessages(t.Context(), msgs))
	initial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 2)
	for _, change := range initial.Changes {
		assert.Equal(t, "identity_ambiguous", change.Gap)
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs[1:]))
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
			require.NoError(b, d.UpsertSession(b.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
			text := strings.Repeat("sample prose ", 6000)
			msgs := make([]Message, count)
			for i := range msgs {
				msgs[i] = Message{SessionID: "chat", Ordinal: i, Role: "assistant", Content: "sample", SourceUUID: fmt.Sprintf("one-%d", i)}
			}
			msgs[0].Content = text
			require.NoError(b, d.InsertMessages(b.Context(), msgs))
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
					require.NoError(b, err)
					assert.Empty(b, got.Changes)
					assert.Equal(b, page.Checkpoint, got.Checkpoint)
				}
			})
			b.Run("body_64KiB", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got, err := d.GetConversationMessage(b.Context(), ConversationMessageOptions{DatabaseID: page.DatabaseID, SessionID: "chat", MessageID: target.MessageID, Revision: target.Revision})
					require.NoError(b, err)
					require.NotNil(b, got.Text)
					assert.Len(b, *got.Text, 64<<10)
				}
			})
			msgs[count-1].Content = "changed"
			require.NoError(b, d.ReplaceSessionMessages(b.Context(), "chat", msgs))
			b.Run("one_changed_message", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got, err := d.ExportConversationChanges(b.Context(), ConversationExportOptions{Checkpoint: page.Checkpoint})
					require.NoError(b, err)
					require.Len(b, got.Changes, 1)
					assert.Equal(b, count-1, got.Changes[0].Ordinal)
				}
			})
		})
	}
}
