//go:build !(windows && arm64)

package duckdb

import (
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
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
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
	require.NoError(err)

	_, err = store.duck.ExecContext(ctx, `
		INSERT INTO tool_result_events (
			id, session_id, tool_call_message_ordinal, call_index,
			source, status, content, content_length, event_index
		) VALUES (90002, ?, 1, -1, 'tool', 'complete', 'bad', 3, 0)`,
		fixture.alphaID)
	require.NoError(err)

	// Must not panic; the negative-index rows are simply skipped.
	msgs, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(err)
	require.Len(msgs, 2)

	// The valid tool call and its result event are preserved intact.
	require.Len(msgs[1].ToolCalls, 1)
	assert.Equal("search", msgs[1].ToolCalls[0].ToolName)
	require.Len(msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal("duck result", msgs[1].ToolCalls[0].ResultEvents[0].Content)
}

// TestDuckMessageHydratesToolCallFilePathAndCallIndex mirrors the SQLite
// round-trip coverage (db.TestResolveToolCallsDerivesPositionalCallIndex):
// the DuckDB message hydrator must populate db.ToolCall.FilePath and
// CallIndex so GetMessages/GetAllMessages consumers see them at parity with
// SQLite.
func TestDuckMessageHydratesToolCallFilePathAndCallIndex(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertSession(db.Session{
		ID: "tc", Project: "p", Machine: "local", Agent: "claude",
		MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}), "upsert session")
	// One assistant message with three tool calls; the write path numbers
	// them positionally (0,1,2) and each carries a distinct file_path.
	require.NoError(local.InsertMessages([]db.Message{{
		SessionID: "tc", Ordinal: 0, Role: "assistant", Content: "tools",
		HasToolUse: true,
		ToolCalls: []db.ToolCall{
			{ToolName: "Read", Category: "Read", FilePath: "a.go"},
			{ToolName: "Edit", Category: "Edit", FilePath: "b.go"},
			{ToolName: "Write", Category: "Write", FilePath: "c.go"},
		},
	}}), "insert messages")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(err, "push to duckdb mirror")
	store := NewStoreFromDB(syncer.DB())

	msgs, err := store.GetAllMessages(ctx, "tc")
	require.NoError(err, "get all messages")
	require.Len(msgs, 1)
	calls := msgs[0].ToolCalls
	require.Len(calls, 3)
	for i, tc := range calls {
		assert.Equal(i, tc.CallIndex, "call %d index", i)
	}
	assert.Equal("a.go", calls[0].FilePath)
	assert.Equal("b.go", calls[1].FilePath)
	assert.Equal("c.go", calls[2].FilePath)
}
