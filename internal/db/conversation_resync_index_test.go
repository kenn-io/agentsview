package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConversationResyncSeeksOrdinalAndPreservesHistory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages int
		upgrade  bool
	}{
		{"fresh", 32, false},
		{"populated upgrade", 128, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := testDB(t)
			insertSession(t, source, "orphan", "sample")
			msgs := make([]Message, 2*tc.messages)
			for i := range msgs {
				msgs[i] = Message{
					SessionID: "orphan", Ordinal: i, Role: "assistant",
					Content: "Earlier reply", SourceUUID: fmt.Sprintf("earlier-%d", i),
				}
			}
			require.NoError(t, source.InsertMessages(t.Context(), msgs))
			// Publish the first messages so the rewrite below retires them.
			_, err := source.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			msgs = msgs[:tc.messages]
			for i := range msgs {
				msgs[i].Content = "Retained reply"
				msgs[i].SourceUUID = fmt.Sprintf("current-%d", i)
			}
			// Reusing ordinals leaves older message identities as tombstones.
			require.NoError(t, source.ReplaceSessionMessages(t.Context(), "orphan", msgs))
			before, err := source.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, before.Changes, 3*tc.messages)
			if tc.upgrade {
				_, err := source.getWriter().Exec(t.Context(), "DROP INDEX IF EXISTS idx_conversation_messages_ordinal")
				require.NoError(t, err)
				path := source.Path()
				require.NoError(t, source.Close())
				source, err = OpenIsolated(t.Context(), path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, source.Close()) })
			}

			// A session-only seek scans all retained history for each message.
			// Check the actual refresh query's cost shape without a clock budget.
			query := fmt.Sprintf(refreshConversationMessagesSQL, "session_id IN (SELECT id FROM sessions)")
			assert.Contains(t, queryPlanOf(t, source, query), "(session_id=? AND ordinal=?) LEFT-JOIN")
			path := source.Path()
			require.NoError(t, source.Close())
			destination := testDB(t)
			require.NoError(t, destination.CopyArchiveIdentityFrom(path))
			copied, err := destination.CopyOrphanedDataFrom(path)
			require.NoError(t, err)
			assert.Equal(t, 1, copied)
			assert.Contains(t, queryPlanOf(t, destination, query), "(session_id=? AND ordinal=?) LEFT-JOIN")
			after, err := destination.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			// Revisions belong to the new database generation; all message
			// identities, digests, ordinals, and deletion markers must survive.
			for i := range before.Changes {
				before.Changes[i].Revision = ""
			}
			deleted := 0
			for i := range after.Changes {
				after.Changes[i].Revision = ""
				if after.Changes[i].Deleted {
					deleted++
				}
			}
			assert.Equal(t, 2*tc.messages, deleted)
			assert.ElementsMatch(t, before.Changes, after.Changes)
		})
	}
}
