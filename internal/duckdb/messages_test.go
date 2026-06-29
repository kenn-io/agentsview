//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestGetAllMessagesSkipsNegativeCallIndex guards against a panic when the
// DuckDB mirror holds a tool_calls or tool_result_events row with a negative
// call_index (a corrupt or malformed mirror row). Such a row would skip the
// grow loop / pass the upper-bound check and index ToolCalls[-1], crashing
// message loading with "index out of range [-1]". The Postgres store already
// guards callIndex < 0; the DuckDB store must behave the same way.
func TestGetAllMessagesSkipsNegativeCallIndex(t *testing.T) {
	ctx := context.Background()
	store, fixture := newSyncedStore(t)

	// alpha message ordinal 1 has exactly one real tool call ("search").
	// Inject malformed rows with call_index = -1 for that same message.
	_, err := store.duck.ExecContext(ctx, `
		INSERT INTO tool_calls (
			id, message_id, session_id, tool_name, category,
			call_index, tool_use_id
		)
		SELECT 90001, m.id, m.session_id, 'bad', 'other', -1, 'bad-tool'
		FROM messages m
		WHERE m.session_id = ? AND m.ordinal = 1`, fixture.alphaID)
	require.NoError(t, err)

	_, err = store.duck.ExecContext(ctx, `
		INSERT INTO tool_result_events (
			id, session_id, tool_call_message_ordinal, call_index,
			source, status, content, content_length, event_index
		) VALUES (90002, ?, 1, -1, 'tool', 'complete', 'bad', 3, 0)`,
		fixture.alphaID)
	require.NoError(t, err)

	// Must not panic; the negative-index rows are simply skipped.
	msgs, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)

	// The valid tool call and its result event are preserved intact.
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "search", msgs[1].ToolCalls[0].ToolName)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "duck result", msgs[1].ToolCalls[0].ResultEvents[0].Content)
}

// TestDuckMessageHydratesToolCallFilePathAndCallIndex mirrors the SQLite
// round-trip coverage (db.TestResolveToolCallsDerivesPositionalCallIndex):
// the DuckDB message hydrator must populate db.ToolCall.FilePath and
// CallIndex so GetMessages/GetAllMessages consumers see them at parity with
// SQLite.
func TestDuckMessageHydratesToolCallFilePathAndCallIndex(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	require.NoError(t, local.UpsertSession(db.Session{
		ID: "tc", Project: "p", Machine: "local", Agent: "claude",
		MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}), "upsert session")
	// One assistant message with three tool calls; the write path numbers
	// them positionally (0,1,2) and each carries a distinct file_path.
	require.NoError(t, local.InsertMessages([]db.Message{{
		SessionID: "tc", Ordinal: 0, Role: "assistant", Content: "tools",
		HasToolUse: true,
		ToolCalls: []db.ToolCall{
			{ToolName: "Read", Category: "Read", FilePath: "a.go"},
			{ToolName: "Edit", Category: "Edit", FilePath: "b.go"},
			{ToolName: "Write", Category: "Write", FilePath: "c.go"},
		},
	}}), "insert messages")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err, "push to duckdb mirror")
	store := NewStoreFromDB(syncer.DB())

	msgs, err := store.GetAllMessages(ctx, "tc")
	require.NoError(t, err, "get all messages")
	require.Len(t, msgs, 1)
	calls := msgs[0].ToolCalls
	require.Len(t, calls, 3)
	for i, tc := range calls {
		assert.Equal(t, i, tc.CallIndex, "call %d index", i)
	}
	assert.Equal(t, "a.go", calls[0].FilePath)
	assert.Equal(t, "b.go", calls[1].FilePath)
	assert.Equal(t, "c.go", calls[2].FilePath)
}
