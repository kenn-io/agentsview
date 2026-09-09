package db

import (
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolCallsByPositionWithinSQLiteVariableLimit(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "project-a")
	calls := make([]ToolCall, 501)
	positions := make([]ToolCallPosition, len(calls))
	for i := range calls {
		calls[i] = ToolCall{ToolName: "exec_command", Category: "Bash"}
		positions[i] = ToolCallPosition{CallIndex: i}
	}
	insertMessages(t, d, Message{SessionID: "s1", Ordinal: 0, Role: "assistant", ToolCalls: calls})
	conn, err := d.getWriter().Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).SetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, 999)
		return nil
	}))
	require.NoError(t, conn.Close())
	tx, err := d.getWriter().Begin()
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	q := signalTxQuery{tx: tx, sessionID: "s1"}
	// Repeating the first position after the chunk boundary must not repeat its fact.
	facts, err := q.ToolCallsByPosition(t.Context(), append(positions, positions[0]))
	require.NoError(t, err)
	got := make([]ToolCallPosition, len(facts))
	for i, fact := range facts {
		got[i] = ToolCallPosition{MessageOrdinal: fact.MessageOrdinal, CallIndex: fact.CallIndex}
	}
	assert.ElementsMatch(t, positions, got)
}

func TestToolCallsByPositionKeepsExactOccurrences(t *testing.T) {
	d := testDB(t)
	for _, sessionID := range []string{"s1", "s2"} {
		insertSession(t, d, sessionID, "project-a")
		for ordinal := range 2 {
			insertMessages(t, d, Message{
				SessionID: sessionID, Ordinal: ordinal, Role: "assistant", HasToolUse: true,
				ToolCalls: []ToolCall{
					{SessionID: sessionID, ToolName: "exec_command", Category: "Bash", ToolUseID: "reused", ResultContent: "first"},
					{SessionID: sessionID, ToolName: "exec_command", Category: "Bash", ToolUseID: "reused", ResultContent: "second"},
				},
			})
		}
	}
	tx, err := d.getWriter().Begin()
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	q := signalTxQuery{tx: tx, sessionID: "s1"}
	facts, err := q.ToolCallsByPosition(t.Context(), []ToolCallPosition{
		{MessageOrdinal: 1, CallIndex: 0},
		{MessageOrdinal: 0, CallIndex: 1},
		{MessageOrdinal: 0, CallIndex: 1},
		{MessageOrdinal: 9, CallIndex: 0},
	})
	require.NoError(t, err)
	require.Len(t, facts, 2, "repeated positions and other sessions must not add facts")
	got := make(map[ToolCallPosition]string)
	for _, fact := range facts {
		got[ToolCallPosition{MessageOrdinal: fact.MessageOrdinal, CallIndex: fact.CallIndex}] = fact.ResultContent
	}
	assert.Equal(t, map[ToolCallPosition]string{
		{MessageOrdinal: 1, CallIndex: 0}: "first",
		{MessageOrdinal: 0, CallIndex: 1}: "second",
	}, got)
}
