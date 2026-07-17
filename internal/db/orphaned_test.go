package db

import (
	"context"
	"database/sql"
	"fmt"
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

func TestCopyOrphanedDataPreservesUsageEvents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	insertSession(t, srcDB, "copilot:orphan", "proj", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = new("2026-07-17T10:00:00Z")
	})
	ordinal := 3
	cost := 0.42
	credits := 2.75
	require.NoError(t, srcDB.ReplaceSessionUsageEvents(
		"copilot:orphan",
		[]UsageEvent{
			{
				MessageOrdinal:           &ordinal,
				Source:                   "message",
				Model:                    "claude-sonnet-4-6",
				InputTokens:              11,
				OutputTokens:             12,
				CacheCreationInputTokens: 13,
				CacheReadInputTokens:     14,
				ReasoningTokens:          15,
				CostUSD:                  &cost,
				CostStatus:               "reported",
				CostSource:               "copilot",
				OccurredAt:               "2026-07-17T10:01:00Z",
				DedupKey:                 "message-3",
			},
			{
				Source:     "shutdown",
				Model:      "claude-sonnet-4-6",
				AICredits:  &credits,
				OccurredAt: "2026-07-17T10:02:00Z",
				DedupKey:   "shutdown-final",
			},
		},
	), "seed source usage events")
	require.NoError(t, srcDB.Close(), "close source")

	dstDB := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
	defer dstDB.Close()
	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 1, count)

	got, err := dstDB.GetUsageEvents(ctx, "copilot:orphan")
	require.NoError(t, err, "GetUsageEvents")
	require.Len(t, got, 2)
	byKey := make(map[string]UsageEvent, len(got))
	for _, event := range got {
		byKey[event.DedupKey] = event
	}
	messageEvent := byKey["message-3"]
	require.NotNil(t, messageEvent.MessageOrdinal)
	assert.Equal(t, ordinal, *messageEvent.MessageOrdinal)
	assert.Equal(t, "message", messageEvent.Source)
	assert.Equal(t, "claude-sonnet-4-6", messageEvent.Model)
	assert.Equal(t, 11, messageEvent.InputTokens)
	assert.Equal(t, 12, messageEvent.OutputTokens)
	assert.Equal(t, 13, messageEvent.CacheCreationInputTokens)
	assert.Equal(t, 14, messageEvent.CacheReadInputTokens)
	assert.Equal(t, 15, messageEvent.ReasoningTokens)
	require.NotNil(t, messageEvent.CostUSD)
	assert.Equal(t, cost, *messageEvent.CostUSD)
	assert.Equal(t, "reported", messageEvent.CostStatus)
	assert.Equal(t, "copilot", messageEvent.CostSource)
	assert.Equal(t, "2026-07-17T10:01:00Z", messageEvent.OccurredAt)
	creditEvent := byKey["shutdown-final"]
	require.NotNil(t, creditEvent.AICredits)
	assert.Equal(t, credits, *creditEvent.AICredits)

	usage, err := dstDB.GetSessionUsage(ctx, "copilot:orphan", false)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, usage)
	assert.Equal(t, credits, usage.AICredits)
	assert.Equal(t, AICreditsSourceReported, usage.AICreditsSource)
}

func TestCopyOrphanedDataSupportsUsageEventsWithoutAICreditsColumn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	insertSession(t, srcDB, "legacy-orphan", "proj")
	require.NoError(t, srcDB.ReplaceSessionUsageEvents(
		"legacy-orphan",
		[]UsageEvent{{
			Source: "shutdown", Model: "legacy-model",
			InputTokens: 7, OccurredAt: "2026-07-17T10:00:00Z",
			DedupKey: "legacy-event",
		}},
	), "seed legacy usage event")
	_, err := srcDB.getWriter().Exec(
		"ALTER TABLE usage_events DROP COLUMN ai_credits")
	require.NoError(t, err, "remove newer source column")
	require.NoError(t, srcDB.Close(), "close source")

	dstDB := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
	defer dstDB.Close()
	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 1, count)

	got, err := dstDB.GetUsageEvents(ctx, "legacy-orphan")
	require.NoError(t, err, "GetUsageEvents")
	require.Len(t, got, 1)
	assert.Equal(t, 7, got[0].InputTokens)
	assert.Nil(t, got[0].AICredits)
	assert.Equal(t, "legacy-event", got[0].DedupKey)
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
	// Dirty content only exists in archives written before
	// sanitizedSourceDataVersion; sources at or above it skip the
	// sanitize pass entirely.
	_, err = srcDB.getWriter().ExecContext(ctx, fmt.Sprintf(
		"PRAGMA user_version = %d", sanitizedSourceDataVersion-1,
	))
	require.NoError(t, err, "downgrade source data version")
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

// TestCopySkipsSanitizeForSanitizedSource guards the resync fast
// path: each sanitize pass is skipped once the source data version
// proves ingest already sanitized that field — content/results at
// sanitizedSourceDataVersion, input_json at the later
// sanitizedInputSourceDataVersion — and skipped rows survive the
// copy verbatim.
func TestCopySkipsSanitizeForSanitizedSource(t *testing.T) {
	const rawContent = "nul\x00byte\x01kept"
	const rawToolInput = "{\"cmd\":\"a\x00b\x01\"}"
	const rawToolResult = "result\x00kept\x01"
	copies := []struct {
		name  string
		trash bool
		copy  func(dst *DB, srcPath string) (int, error)
	}{
		{
			name: "orphaned",
			copy: func(dst *DB, srcPath string) (int, error) {
				return dst.CopyOrphanedDataFrom(srcPath)
			},
		},
		{
			name:  "trashed",
			trash: true,
			copy: func(dst *DB, srcPath string) (int, error) {
				return dst.CopyTrashedDataFrom(srcPath)
			},
		},
	}
	versions := []struct {
		name          string
		sourceVersion int
		wantInput     string
	}{
		{
			// Ingest at v58 sanitized content and results but not
			// input_json, so only the input pass runs for it.
			name:          "content-sanitized source pays input pass",
			sourceVersion: sanitizedSourceDataVersion,
			wantInput:     SanitizeUTF8(rawToolInput),
		},
		{
			// A source at the input watermark is fully clean; every
			// pass is skipped and rows copy verbatim.
			name:          "fully sanitized source copies verbatim",
			sourceVersion: sanitizedInputSourceDataVersion,
			wantInput:     rawToolInput,
		},
	}
	for _, cp := range copies {
		for _, ver := range versions {
			t.Run(cp.name+"/"+ver.name, func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				srcPath := filepath.Join(dir, "old.db")
				srcDB := testDBAtPath(t, srcPath, "src")
				insertSession(t, srcDB, "sess", "proj")
				insertMessages(t, srcDB, userMsg("sess", 0, "clean"))
				_, err := srcDB.getWriter().ExecContext(ctx,
					`UPDATE messages SET content = ? WHERE session_id = ?`,
					rawContent, "sess",
				)
				require.NoError(t, err, "plant raw content")
				var messageID int64
				require.NoError(t, srcDB.getWriter().QueryRowContext(ctx,
					`SELECT id FROM messages WHERE session_id = ?`, "sess",
				).Scan(&messageID), "read message id")
				_, err = srcDB.getWriter().ExecContext(ctx,
					`INSERT INTO tool_calls (
						message_id, session_id, tool_name, category,
						tool_use_id, input_json, result_content_length,
						result_content, call_index
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					messageID, "sess", "Bash", "execution", "tool-1",
					rawToolInput, len(rawToolResult), rawToolResult, 0,
				)
				require.NoError(t, err, "plant raw tool call")
				_, err = srcDB.getWriter().ExecContext(ctx, fmt.Sprintf(
					"PRAGMA user_version = %d", ver.sourceVersion,
				))
				require.NoError(t, err, "set source data version")
				if cp.trash {
					require.NoError(t, srcDB.SoftDeleteSession("sess"),
						"soft delete source session")
				}
				require.NoError(t, srcDB.Close(), "close source")

				dstDB := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
				defer dstDB.Close()
				count, err := cp.copy(dstDB, srcPath)
				require.NoError(t, err, "copy from source")
				require.Equal(t, 1, count, "copied sessions")

				var got string
				require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
					`SELECT content FROM messages WHERE session_id = ?`,
					"sess",
				).Scan(&got), "query copied message")
				assert.Equal(t, rawContent, got,
					"content-sanitized source must copy content verbatim")

				var gotInput, gotResult string
				require.NoError(t, dstDB.getReader().QueryRowContext(ctx,
					`SELECT input_json, result_content
					 FROM tool_calls WHERE session_id = ?`,
					"sess",
				).Scan(&gotInput, &gotResult), "query copied tool call")
				assert.Equal(t, ver.wantInput, gotInput)
				assert.Equal(t, rawToolResult, gotResult,
					"tool result must copy verbatim for sanitized sources")
			})
		}
	}
}
