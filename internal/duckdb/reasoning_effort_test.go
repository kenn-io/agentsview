//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestReasoningEffortDuckDBReadAndWrite(t *testing.T) {
	ctx := context.Background()
	store, fixture := newSyncedStore(t)
	_, err := store.duck.ExecContext(ctx,
		`UPDATE messages SET reasoning_effort = ? WHERE session_id = ? AND ordinal = 1`,
		"high", fixture.alphaID,
	)
	require.NoError(t, err)

	messages, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Equal(t, "high", messages[1].ReasoningEffort)
}

func TestReasoningEffortDuckDBInsertMessagePath(t *testing.T) {
	ctx := context.Background()
	store, fixture := newSyncedStore(t)
	message := db.Message{
		ID:              99_999_999,
		SessionID:       fixture.alphaID,
		Ordinal:         2,
		Role:            "assistant",
		Content:         "inserted",
		ContentLength:   len("inserted"),
		Model:           "model-test",
		ReasoningEffort: "high",
	}
	rows, _, _, err := db.CanonicalMessageRows([]db.Message{message})
	require.NoError(t, err)
	rows[0].ID = new(message.ID)
	require.NoError(t, db.AppendMessageRows(ctx, store.bun, fixture.alphaID, rows))

	messages, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	require.Equal(t, "high", messages[2].ReasoningEffort)
}

func TestReasoningEffortDuckDBRebuildsOldSchemaMirror(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	messages, err := local.GetAllMessages(ctx, "sess-1")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	messages[1].ReasoningEffort = "high"
	require.NoError(t, local.ReplaceSessionMessages("sess-1", messages))

	_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	setMirrorMetadataValue(t, path, schemaVersionMetadataKey, strconv.Itoa(SchemaVersion-1))

	result, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, result.Diagnostics.Full)
	assert.Contains(t, result.Diagnostics.RebuildReason, "schema")

	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	var effort string
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT reasoning_effort FROM messages
		WHERE session_id = 'sess-1' AND ordinal = 1`,
	).Scan(&effort))
	assert.Equal(t, "high", effort)
}
