package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecWithoutCancelDropsTempTableWithCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	pool, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open sqlite")
	defer pool.Close()

	baseCtx := context.Background()
	conn, err := pool.Conn(baseCtx)
	require.NoError(t, err, "pin sqlite connection")
	defer conn.Close()

	_, err = conn.ExecContext(baseCtx, `
		CREATE TEMP TABLE _test_cleanup (
			id TEXT PRIMARY KEY
		)`)
	require.NoError(t, err, "create temp table")

	ctx, cancel := context.WithCancel(baseCtx)
	cancel()

	_, err = execWithoutCancel(ctx, conn,
		"DROP TABLE IF EXISTS _test_cleanup")
	require.NoError(t, err, "drop with canceled context")

	_, err = conn.ExecContext(baseCtx, `
		CREATE TEMP TABLE _test_cleanup (
			id TEXT PRIMARY KEY
		)`)
	require.NoError(t, err, "recreate temp table after cleanup")
}

func TestCopyOrphanedDataSanitizesCopiedContent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	insertSession(t, srcDB, "poison-orphan", "proj")
	insertMessages(t, srcDB, userMsg("poison-orphan", 0, "clean"))
	var messageID int64
	require.NoError(t, srcDB.getWriter().QueryRowContext(ctx,
		`SELECT id FROM messages WHERE session_id = ? AND ordinal = 0`,
		"poison-orphan",
	).Scan(&messageID), "query source message id")

	messageContent := "message\x00body\x01\nkept"
	toolInput := "{\"cmd\":\"tool\x00input\x04\"}"
	emptyToolInput := "\x00\x04"
	toolResult := "tool\x00result\x02"
	emptyToolResult := "\x00\x04"
	eventContent := "event\x00content\x03"
	const (
		messageLengthExcess = 7
		toolLengthExcess    = 11
		eventLengthExcess   = 5
		emptyResultLength   = 7
	)
	_, err := srcDB.getWriter().ExecContext(ctx,
		`UPDATE messages
		 SET content = ?, content_length = ?
		 WHERE id = ?`,
		messageContent, len(messageContent)+messageLengthExcess, messageID,
	)
	require.NoError(t, err, "plant poisoned message")
	_, err = srcDB.getWriter().ExecContext(ctx,
		`INSERT INTO tool_calls (
			message_id, session_id, tool_name, category,
			tool_use_id, input_json, result_content_length,
			result_content, call_index
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		messageID, "poison-orphan", "Read", "file", "tool-1",
		toolInput, len(toolResult)+toolLengthExcess, toolResult, 0,
	)
	require.NoError(t, err, "plant poisoned tool call")
	_, err = srcDB.getWriter().ExecContext(ctx,
		`INSERT INTO tool_calls (
			message_id, session_id, tool_name, category,
			tool_use_id, input_json, call_index
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		messageID, "poison-orphan", "Read", "file", "tool-empty",
		emptyToolInput, 1,
	)
	require.NoError(t, err, "plant empty-sanitized tool input")
	_, err = srcDB.getWriter().ExecContext(ctx,
		`INSERT INTO tool_calls (
			message_id, session_id, tool_name, category,
			tool_use_id, result_content_length, result_content,
			call_index
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		messageID, "poison-orphan", "Read", "file", "tool-empty-result",
		emptyResultLength, emptyToolResult, 2,
	)
	require.NoError(t, err, "plant empty-sanitized tool result")
	_, err = srcDB.getWriter().ExecContext(ctx,
		`INSERT INTO tool_result_events (
			session_id, tool_call_message_ordinal, call_index,
			tool_use_id, source, status, content, content_length,
			event_index
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"poison-orphan", 0, 0, "tool-1", "tool_result", "ok",
		eventContent, len(eventContent)+eventLengthExcess, 0,
	)
	require.NoError(t, err, "plant poisoned tool result event")
	require.NoError(t, srcDB.Close(), "close source")

	dstPath := filepath.Join(dir, "new.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 1, count, "expected one orphan")

	var gotMessage string
	var gotMessageLength int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content, content_length
		 FROM messages
		 WHERE session_id = ? AND ordinal = 0`,
		"poison-orphan",
	).Scan(&gotMessage, &gotMessageLength), "query copied message")
	wantMessage := SanitizeUTF8(messageContent)
	assert.Equal(t, wantMessage, gotMessage)
	assert.Equal(t, len(wantMessage)+messageLengthExcess, gotMessageLength)

	var gotToolInput string
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT input_json
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 0`,
		"poison-orphan",
	).Scan(&gotToolInput), "query copied tool input")
	assert.Equal(t, SanitizeUTF8(toolInput), gotToolInput)

	var gotEmptyToolInput sql.NullString
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT input_json
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 1`,
		"poison-orphan",
	).Scan(&gotEmptyToolInput), "query empty copied tool input")
	assert.False(t, gotEmptyToolInput.Valid)

	var gotToolResult string
	var gotToolResultLength int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT result_content, result_content_length
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 0`,
		"poison-orphan",
	).Scan(&gotToolResult, &gotToolResultLength), "query copied tool call")
	wantToolResult := SanitizeUTF8(toolResult)
	assert.Equal(t, wantToolResult, gotToolResult)
	assert.Equal(t, len(wantToolResult)+toolLengthExcess, gotToolResultLength)

	var gotEmptyToolResult sql.NullString
	var gotEmptyToolResultLength sql.NullInt64
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT result_content, result_content_length
		 FROM tool_calls
		 WHERE session_id = ? AND call_index = 2`,
		"poison-orphan",
	).Scan(
		&gotEmptyToolResult,
		&gotEmptyToolResultLength,
	), "query empty copied tool call result")
	assert.False(t, gotEmptyToolResult.Valid)
	require.True(t, gotEmptyToolResultLength.Valid)
	assert.Equal(t,
		int64(emptyResultLength-len(emptyToolResult)),
		gotEmptyToolResultLength.Int64,
	)

	var gotEventContent string
	var gotEventLength int
	require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
		`SELECT content, content_length
		 FROM tool_result_events
		 WHERE session_id = ? AND event_index = 0`,
		"poison-orphan",
	).Scan(&gotEventContent, &gotEventLength), "query copied tool result event")
	wantEventContent := SanitizeUTF8(eventContent)
	assert.Equal(t, wantEventContent, gotEventContent)
	assert.Equal(t, len(wantEventContent)+eventLengthExcess, gotEventLength)
}
