package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

func TestReadSessionReplicationSnapshotIncludesCanonicalDependents(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "replication-snapshot", "alpha")
	require.NoError(t, database.ReplaceSessionMessages("replication-snapshot", []Message{{
		SessionID: "replication-snapshot", Ordinal: 0, Role: "assistant",
		Content: "snapshot message", ToolCalls: []ToolCall{{
			ToolName: "Read", Category: "Read", ToolUseID: "tool-1",
			ResultEvents: []ToolResultEvent{{
				Source: "tool", Status: "completed", Content: "snapshot result",
			}},
		}},
	}}))
	require.NoError(t, database.bunWriter.RunInTx(t.Context(), nil,
		func(ctx context.Context, tx bun.Tx) error {
			return ReplaceUsageEventRows(ctx, tx, "replication-snapshot", []bunmodel.UsageEvent{{
				SessionID: "replication-snapshot", Source: "message",
				Model: "model", DedupKey: "usage-1",
			}})
		}))
	require.NoError(t, database.ReplaceSessionSecretFindings(
		"replication-snapshot", []SecretFinding{{
			SessionID: "replication-snapshot", RuleName: "token",
			Confidence: "definite", LocationKind: "message",
		}}, 1, "rules-v1",
	))
	messages, err := database.GetAllMessages(t.Context(), "replication-snapshot")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	_, err = database.PinMessage("replication-snapshot", messages[0].ID, nil)
	require.NoError(t, err)
	_, err = database.getWriter().Exec(
		`UPDATE sessions SET session_name = 'raw-name', display_name = NULL WHERE id = ?`,
		"replication-snapshot",
	)
	require.NoError(t, err)

	snapshot, err := database.ReadSessionReplicationSnapshot(
		t.Context(), "replication-snapshot",
	)
	require.NoError(t, err)
	assert.Equal(t, "replication-snapshot", snapshot.Session.ID)
	assert.Nil(t, snapshot.Session.DisplayName)
	assert.Equal(t, "raw-name", *snapshot.Session.SessionName)
	require.Len(t, snapshot.Messages, 1)
	require.Len(t, snapshot.Messages[0].ToolCalls, 1)
	require.Len(t, snapshot.Messages[0].ToolCalls[0].ResultEvents, 1)
	require.Len(t, snapshot.UsageEvents, 1)
	require.Len(t, snapshot.SecretFindings, 1)
	require.Len(t, snapshot.PinnedMessages, 1)
	fingerprint, err := CanonicalSessionReplicationFingerprint(snapshot, "owner")
	require.NoError(t, err)
	repeat, err := CanonicalSessionReplicationFingerprint(snapshot, "owner")
	require.NoError(t, err)
	assert.Equal(t, fingerprint, repeat)
	snapshot.Messages[0].Content = "changed snapshot message"
	changed, err := CanonicalSessionReplicationFingerprint(snapshot, "owner")
	require.NoError(t, err)
	assert.NotEqual(t, fingerprint, changed)
}
