package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativeConversationPublicationRejectsLegacyAndKeepsVerifiedIdentity(t *testing.T) {
	for _, sourceID := range []string{"native-one", ""} {
		t.Run("source="+sourceID, func(t *testing.T) {
			d := testDB(t)
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Agent: "other-agent"}))
			legacy := Message{SessionID: "chat", Role: "assistant", Content: "Unclassified transcript", SourceUUID: sourceID}
			legacy.SetContentLayout(nil)
			require.NoError(t, d.InsertMessages(t.Context(), []Message{legacy}))
			initial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, initial.Changes, 1)
			old := initial.Changes[0]
			assert.Equal(t, "visible_text_unavailable", old.Gap)
			assert.Empty(t, old.Digest)
			assert.Zero(t, old.TextBytes)
			body, err := d.GetConversationMessage(t.Context(), ConversationMessageOptions{DatabaseID: initial.DatabaseID, SessionID: "chat", MessageID: old.MessageID, Revision: old.Revision})
			require.NoError(t, err)
			assert.Nil(t, body.Text)

			native := Message{SessionID: "chat", Role: "assistant", Content: "[Thinking]\nVerified dialogue", ThinkingText: "private reasoning", SourceUUID: sourceID}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", []Message{native}))
			delta, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: initial.Checkpoint})
			require.NoError(t, err)
			if sourceID != "" {
				require.Len(t, delta.Changes, 1)
				assert.Equal(t, old.MessageID, delta.Changes[0].MessageID)
				assert.Empty(t, delta.Changes[0].Gap)
			} else {
				require.Len(t, delta.Changes, 2)
				assert.Equal(t, old.MessageID, delta.Changes[0].MessageID)
				assert.True(t, delta.Changes[0].Deleted)
				assert.NotEqual(t, old.MessageID, delta.Changes[1].MessageID)
				assert.Equal(t, "identity_ambiguous", delta.Changes[1].Gap)
			}
			current := delta.Changes[len(delta.Changes)-1]
			body, err = d.GetConversationMessage(t.Context(), ConversationMessageOptions{DatabaseID: delta.DatabaseID, SessionID: "chat", MessageID: current.MessageID, Revision: current.Revision})
			require.NoError(t, err)
			require.NotNil(t, body.Text)
			assert.Equal(t, "[Thinking]\nVerified dialogue", *body.Text)
			_, err = d.GetConversationMessage(t.Context(), ConversationMessageOptions{DatabaseID: delta.DatabaseID, SessionID: "chat", MessageID: old.MessageID, Revision: old.Revision})
			require.ErrorIs(t, err, ErrConversationRevisionChanged)
		})
	}
}

func TestNativeConversationUpgradeAndCopiesReplacePollutedPublication(t *testing.T) {
	for _, mode := range []string{"upgrade", "orphan", "trash"} {
		t.Run(mode, func(t *testing.T) {
			source := testDB(t)
			require.NoError(t, source.UpsertSession(t.Context(), Session{ID: "archived", Agent: "codex"}))
			require.NoError(t, source.InsertMessages(t.Context(), []Message{{SessionID: "archived", Role: "assistant", Content: "Unclassified transcript", SourceUUID: "native-one"}}))
			initial, err := source.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, initial.Changes, 1)
			old := initial.Changes[0]
			// An older index published a flattened body without provenance.
			require.NoError(t, source.Update(t.Context(), func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(t.Context(), `UPDATE messages SET content_layout=NULL WHERE session_id='archived'`); err != nil {
					return err
				}
				_, err := tx.ExecContext(t.Context(), `DELETE FROM archive_metadata WHERE key='conversation_dialogue_recipe'`)
				return err
			}))
			var destination *DB
			if mode == "upgrade" {
				path := source.Path()
				require.NoError(t, source.Close())
				reader, err := OpenReadOnly(t.Context(), path)
				require.NoError(t, err)
				unready, err := reader.ExportConversationChanges(t.Context(), ConversationExportOptions{})
				require.ErrorIs(t, err, ErrConversationInitializationRequired)
				assert.Empty(t, unready.Checkpoint)
				_, err = reader.GetConversationMessage(t.Context(), ConversationMessageOptions{DatabaseID: initial.DatabaseID, SessionID: "archived", MessageID: old.MessageID, Revision: old.Revision})
				require.ErrorIs(t, err, ErrConversationInitializationRequired)
				require.NoError(t, reader.Close())
				destination, err = OpenIsolated(t.Context(), path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, destination.Close()) })
			} else {
				destination = testDB(t)
				if mode == "trash" {
					require.NoError(t, source.SoftDeleteSession(t.Context(), "archived"))
					_, err = destination.CopyTrashedDataFrom(source.Path())
					require.NoError(t, err)
					_, err = destination.RestoreSession(t.Context(), "archived")
				} else {
					_, err = destination.CopyOrphanedDataFrom(source.Path())
				}
				require.NoError(t, err)
			}
			page, err := destination.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			var messageChanges []ConversationChange
			for _, change := range page.Changes {
				if change.Type == "message" {
					messageChanges = append(messageChanges, change)
				}
			}
			require.Len(t, messageChanges, 1)
			change := messageChanges[0]
			assert.Equal(t, old.MessageID, change.MessageID)
			assert.Equal(t, "visible_text_unavailable", change.Gap)
			assert.Zero(t, change.TextBytes)
			body, err := destination.GetConversationMessage(t.Context(), ConversationMessageOptions{DatabaseID: page.DatabaseID, SessionID: "archived", MessageID: change.MessageID, Revision: change.Revision})
			require.NoError(t, err)
			assert.Nil(t, body.Text)
			stored, err := destination.GetAllMessages(t.Context(), "archived")
			require.NoError(t, err)
			require.Len(t, stored, 1)
			assert.Equal(t, "Unclassified transcript", stored[0].Content)
			assert.Nil(t, stored[0].ContentLayout)
		})
	}
}

func TestNativeConversationChangedLegacyWithoutIDIsAmbiguous(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "legacy", Agent: "codex"}))
	message := Message{SessionID: "legacy", Role: "assistant", Content: "Old unclassified body"}
	message.SetContentLayout(nil)
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message}))
	initial, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, initial.Changes, 1)
	assert.Equal(t, "visible_text_unavailable", initial.Changes[0].Gap)

	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "legacy", []Message{message}))
	unchanged, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: initial.Checkpoint})
	require.NoError(t, err)
	assert.Empty(t, unchanged.Changes)
	message.Content = "Changed unclassified body"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "legacy", []Message{message}))
	delta, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: unchanged.Checkpoint})
	require.NoError(t, err)
	require.Len(t, delta.Changes, 2)
	assert.True(t, delta.Changes[0].Deleted)
	assert.Equal(t, initial.Changes[0].MessageID, delta.Changes[0].MessageID)
	assert.NotEqual(t, initial.Changes[0].MessageID, delta.Changes[1].MessageID)
	assert.Equal(t, "identity_ambiguous", delta.Changes[1].Gap)
	assert.Zero(t, delta.Changes[1].TextBytes)
}
