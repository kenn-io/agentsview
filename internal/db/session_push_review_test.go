package db

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestSessionPushRetentionConflictPublishesNoAssets(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		name := "excluded"
		if inherited {
			name = "inherited-trash"
		}
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			id := "selected"
			if inherited {
				d = upgradedCodexTrash(t)
				id = legacyTrashMissingPage
			} else {
				require.NoError(t, d.UpsertSession(t.Context(), Session{ID: id, Agent: "claude", Machine: "local"}))
				require.NoError(t, d.DeleteSession(t.Context(), id))
			}
			dir := t.TempDir()
			d.SetAssetsDir(dir)
			d.SetToolResultImages(config.ToolResultImagesOffload)
			receipt, write := sessionPushFixture(id)
			if inherited {
				write.Session.Agent = "codex"
			}
			write.Messages = []Message{testImageMessage(id)}
			write.Session.UserMessageCount = 0
			created, err := d.ApplySessionPush(t.Context(), receipt, write)
			require.ErrorIs(t, err, ErrSessionPushConflict)
			assert.False(t, created)
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Empty(t, entries, "retention rejection must precede image publication")
			stored, err := d.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			assert.Nil(t, stored)
			var receipts int
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT count(*) FROM session_push_receipts WHERE session_id=?`, id).Scan(&receipts))
			assert.Zero(t, receipts)
		})
	}
}

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

func TestSessionPushPassiveSpawnerDoesNotPreventNativeCleanup(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "native-child", Agent: "claude", Machine: "local", ParentSessionID: Ptr("absent"), RelationshipType: "subagent"}))
	receipt, write := sessionPushFixture("passive-parent")
	write.Messages[0].ToolCalls = []ToolCall{{ToolName: "Task", SubagentSessionID: "native-child"}}
	_, err := d.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	children, err := d.SubagentChildSessionIDs(t.Context(), []string{"passive-parent"})
	require.NoError(t, err)
	assert.Empty(t, children)
	require.NoError(t, d.QueueSubagentParentCleanupRepairs(t.Context(), []string{"native-child"}))
	_, err = d.RepairQueuedSubagentParentsContext(t.Context(), nil)
	require.NoError(t, err)
	child, err := d.GetSessionFull(t.Context(), "native-child")
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Nil(t, child.ParentSessionID, "passive transcript references must not block ordinary cleanup")
}
