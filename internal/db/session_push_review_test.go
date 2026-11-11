package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionPushQueuedCleanupPreservesSnapshot(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		name := "immediate"
		if rebuild {
			name = "rebuilt-queue"
		}
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			receipt, write := sessionPushFixture("child")
			write.Session.ParentSessionID = Ptr("historical-absent-parent")
			write.Session.RelationshipType = "subagent"
			_, err := d.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "native-parent", Agent: "claude", Machine: "local"}))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "native-parent", Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Task", SubagentSessionID: "child"}}}}))
			captured, err := d.SubagentChildSessionIDs(t.Context(), []string{"native-parent"})
			require.NoError(t, err)
			assert.Empty(t, captured, "receipt-owned children never become cleanup targets")
			// A persisted queue may predate the receipt fence; it must also be safe.
			require.NoError(t, d.QueueSubagentParentCleanupRepairs(t.Context(), []string{"child"}))
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "native-parent", nil))
			if rebuild {
				path := d.Path()
				require.NoError(t, d.Close())
				d = testDB(t)
				require.NoError(t, d.CopyArchiveIdentityFrom(t.Context(), path))
				require.NoError(t, d.CopyAttachedArchiveStateFrom(t.Context(), path))
				require.NoError(t, d.CopySyncStateFrom(t.Context(), path))
				_, err = d.CopyOrphanedDataFrom(t.Context(), path)
				require.NoError(t, err)
			}
			before, err := d.GetSessionFull(t.Context(), "child")
			require.NoError(t, err)
			require.NotNil(t, before)
			require.Equal(t, Ptr("historical-absent-parent"), before.ParentSessionID)
			_, err = d.RepairQueuedSubagentParentsContext(t.Context(), nil)
			require.NoError(t, err)
			after, err := d.GetSessionFull(t.Context(), "child")
			require.NoError(t, err)
			assert.Equal(t, before, after, "repair must preserve the entire accepted row")
			requireSessionPushReceipt(t, d, "child")
		})
	}
}
