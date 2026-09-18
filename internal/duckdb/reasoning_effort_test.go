//go:build !(windows && arm64)

package duckdb

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestReasoningEffortDuckDBReadAndWrite(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	store, fixture := newSyncedStore(t)
	_, err := store.duck.ExecContext(ctx,
		`UPDATE messages SET reasoning_effort = ? WHERE session_id = ? AND ordinal = 1`,
		"high", fixture.alphaID,
	)
	require.NoError(err)

	messages, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(err)
	require.Len(messages, 2)
	require.Equal("high", messages[1].ReasoningEffort)
}

func TestReasoningEffortDuckDBInsertMessagePath(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
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
	require.NoError(insertMessages(ctx, store.duck, []db.Message{message}))

	messages, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(err)
	require.Len(messages, 3)
	require.Equal("high", messages[2].ReasoningEffort)
}

func TestReasoningEffortDuckDBRebuildsOldSchemaMirror(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	messages, err := local.GetAllMessages(ctx, "sess-1")
	require.NoError(err)
	require.Len(messages, 2)
	messages[1].ReasoningEffort = "high"
	require.NoError(local.ReplaceSessionMessages("sess-1", messages))

	_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(err)

	setMirrorMetadataValue(t, path, schemaVersionMetadataKey, strconv.Itoa(SchemaVersion-1))

	result, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(err)
	assert.True(result.Diagnostics.Full)
	assert.Contains(result.Diagnostics.RebuildReason, "schema")

	conn, err := Open(path)
	require.NoError(err)
	defer conn.Close()
	var effort string
	require.NoError(conn.QueryRowContext(ctx, `
		SELECT reasoning_effort FROM messages
		WHERE session_id = 'sess-1' AND ordinal = 1`,
	).Scan(&effort))
	assert.Equal("high", effort)
}
