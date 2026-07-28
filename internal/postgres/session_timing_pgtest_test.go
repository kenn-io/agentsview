//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// timingResetSession deletes any prior fixtures for the given session id
// (and cascades to messages and tool_calls) so each test starts clean.
func timingResetSession(t *testing.T, pg *sql.DB, sessionID string) {
	t.Helper()
	_, err := pg.Exec(`DELETE FROM sessions WHERE id = $1`, sessionID)
	require.NoError(t, err, "reset session")
}

func timingInsertSessionPG(
	t *testing.T, pg *sql.DB, id, started, ended string,
) {
	t.Helper()
	var endedAt any
	if ended != "" {
		endedAt = ended
	}
	var startedAt any
	if started != "" {
		startedAt = started
	}
	_, err := pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, started_at, ended_at,
			 message_count, user_message_count)
		VALUES ($1, '', '', 'claude',
		        $2::timestamptz, $3::timestamptz, 0, 0)
	`, id, startedAt, endedAt)
	require.NoError(t, err, "insert session %s", id)
}

func timingInsertMessagePG(
	t *testing.T, pg *sql.DB, sessionID string, ordinal int,
	role, content, ts string, hasToolUse bool,
) {
	t.Helper()
	_, err := pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 has_tool_use, content_length)
		VALUES ($1, $2, $3, $4, $5::timestamptz, $6, 0)
	`, sessionID, ordinal, role, content, ts, hasToolUse)
	require.NoError(t, err, "insert message %s/%d", sessionID, ordinal)
}

func timingInsertToolCallPG(
	t *testing.T, pg *sql.DB, sessionID string,
	msgOrdinal, callIndex int,
	toolUseID, toolName, category, subagentSessionID string,
) {
	t.Helper()
	var sub any
	if subagentSessionID != "" {
		sub = subagentSessionID
	}
	_, err := pg.Exec(`
		INSERT INTO tool_calls
			(session_id, tool_name, category, call_index,
			 tool_use_id, input_json,
			 subagent_session_id, message_ordinal)
		VALUES ($1, $2, $3, $4, $5, '{}', $6, $7)
	`, sessionID, toolName, category, callIndex, toolUseID,
		sub, msgOrdinal)
	require.NoError(t, err, "insert tool_call %s/%d", sessionID, msgOrdinal)
}

func timingInsertToolResultEventPG(
	t *testing.T, pg *sql.DB, sessionID string,
	msgOrdinal, callIndex int,
	toolUseID, status, timestamp string,
) {
	t.Helper()
	_, err := pg.Exec(`
		INSERT INTO tool_result_events
			(session_id, tool_call_message_ordinal, call_index,
			 tool_use_id, source, status, content, content_length,
			 timestamp, event_index)
		VALUES ($1, $2, $3, $4, 'copilot-cli', $5, '', 0,
		        $6::timestamptz,
		        CASE WHEN $5 = 'started' THEN 0 ELSE 1 END)
	`, sessionID, msgOrdinal, callIndex, toolUseID, status, timestamp)
	require.NoError(t, err,
		"insert tool_result_event %s/%d", sessionID, msgOrdinal)
}

func TestPGGetSessionTiming_CopilotExactToolEventsExcludeIdleGap(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	timingResetSession(t, pg, "timing-copilot-exact")
	timingInsertSessionPG(t, pg, "timing-copilot-exact",
		"2026-07-24T11:56:20Z", "2026-07-25T00:34:49.483Z")
	timingInsertMessagePG(t, pg, "timing-copilot-exact", 0, "user",
		"finish", "2026-07-24T11:56:20Z", false)
	timingInsertMessagePG(t, pg, "timing-copilot-exact", 1, "assistant",
		"task_complete", "2026-07-24T11:56:24.189Z", true)
	timingInsertToolCallPG(t, pg, "timing-copilot-exact", 1, 0,
		"call_example", "task_complete", "Other", "")
	timingInsertToolResultEventPG(t, pg, "timing-copilot-exact", 1, 0,
		"call_example", "started", "2026-07-24T11:56:24.198Z")
	timingInsertToolResultEventPG(t, pg, "timing-copilot-exact", 1, 0,
		"call_example", "completed", "2026-07-24T11:56:27.923Z")
	timingInsertMessagePG(t, pg, "timing-copilot-exact", 2, "user",
		"next request", "2026-07-25T00:34:49.483Z", false)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-copilot-exact",
	)
	require.NoError(t, err, "GetSessionTiming")
	require.Len(t, got.Turns, 1)
	require.Len(t, got.Turns[0].Calls, 1)
	require.NotNil(t, got.Turns[0].DurationMs)
	assert.Equal(t, int64(3_725), *got.Turns[0].DurationMs)
	require.NotNil(t, got.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(3_725), *got.Turns[0].Calls[0].DurationMs)
}

func TestPGGetSessionTiming_Solo(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	timingResetSession(t, pg, "timing-solo")
	timingInsertSessionPG(t, pg, "timing-solo",
		"2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	timingInsertMessagePG(t, pg, "timing-solo", 0, "user",
		"go", "2026-04-26T10:00:00Z", false)
	timingInsertMessagePG(t, pg, "timing-solo", 1, "assistant",
		"running test", "2026-04-26T10:00:01Z", true)
	timingInsertToolCallPG(t, pg, "timing-solo", 1, 0,
		"tu_1", "Bash", "Bash", "")
	timingInsertMessagePG(t, pg, "timing-solo", 2, "user",
		"ok", "2026-04-26T10:00:30Z", false)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-solo",
	)
	require.NoError(t, err, "GetSessionTiming")
	require.NotNil(t, got, "GetSessionTiming returned nil, want timing")
	assert.Equal(t, 1, got.TurnCount)
	assert.Equal(t, 1, got.ToolCallCount)
	assert.False(t, got.Running)
	require.Len(t, got.Turns, 1)
	require.NotNil(t, got.Turns[0].DurationMs)
	assert.Equal(t, int64(29_000), *got.Turns[0].DurationMs)
	require.NotNil(t, got.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(29_000), *got.Turns[0].Calls[0].DurationMs)
}

func TestPGGetSessionTiming_LastMessageFallsBackToSessionEnd(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	timingResetSession(t, pg, "timing-fallback")
	timingInsertSessionPG(t, pg, "timing-fallback",
		"2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	timingInsertMessagePG(t, pg, "timing-fallback", 0, "user",
		"run", "2026-04-26T10:00:00Z", false)
	timingInsertMessagePG(t, pg, "timing-fallback", 1, "assistant",
		"doing", "2026-04-26T10:00:10Z", true)
	timingInsertToolCallPG(t, pg, "timing-fallback", 1, 0,
		"tu_1", "Bash", "Bash", "")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-fallback",
	)
	require.NoError(t, err, "GetSessionTiming")
	require.NotNil(t, got.Turns[0].DurationMs,
		"turn duration nil, want 20000 (fallback to ended_at)")
	assert.Equal(t, int64(20_000), *got.Turns[0].DurationMs)
	require.NotNil(t, got.Turns[0].Calls[0].DurationMs,
		"solo non-subagent inherits turn duration")
	assert.Equal(t, int64(20_000), *got.Turns[0].Calls[0].DurationMs)
}

func TestPGGetSessionTiming_RunningSessionLastTurnNull(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	timingResetSession(t, pg, "timing-running")
	timingInsertSessionPG(t, pg, "timing-running",
		"2026-04-26T10:00:00Z", "")
	timingInsertMessagePG(t, pg, "timing-running", 0, "user",
		"run", "2026-04-26T10:00:00Z", false)
	timingInsertMessagePG(t, pg, "timing-running", 1, "assistant",
		"doing", "2026-04-26T10:00:10Z", true)
	timingInsertToolCallPG(t, pg, "timing-running", 1, 0,
		"tu_1", "Bash", "Bash", "")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-running",
	)
	require.NoError(t, err, "GetSessionTiming")
	assert.True(t, got.Running)
	assert.Nil(t, got.Turns[0].DurationMs, "turn duration should be nil (running)")
}

func TestPGGetSessionTiming_NonMonotonicTimestampClampsNull(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	timingResetSession(t, pg, "timing-nonmono")
	timingInsertSessionPG(t, pg, "timing-nonmono",
		"2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	timingInsertMessagePG(t, pg, "timing-nonmono", 0, "user",
		"run", "2026-04-26T10:00:20Z", false)
	timingInsertMessagePG(t, pg, "timing-nonmono", 1, "assistant",
		"broken", "2026-04-26T10:00:25Z", true)
	timingInsertToolCallPG(t, pg, "timing-nonmono", 1, 0,
		"tu_1", "Bash", "Bash", "")
	timingInsertMessagePG(t, pg, "timing-nonmono", 2, "user",
		"ok", "2026-04-26T10:00:00Z", false)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-nonmono",
	)
	require.NoError(t, err, "GetSessionTiming")
	assert.Nil(t, got.Turns[0].DurationMs, "turn duration should be nil (clamp)")
}

func TestPGGetSessionTiming_NoToolUseHasNoTurnDuration(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	timingResetSession(t, pg, "timing-notool")
	timingInsertSessionPG(t, pg, "timing-notool",
		"2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	timingInsertMessagePG(t, pg, "timing-notool", 0, "user",
		"hi", "2026-04-26T10:00:00Z", false)
	timingInsertMessagePG(t, pg, "timing-notool", 1, "assistant",
		"hi back", "2026-04-26T10:00:01Z", false)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-notool",
	)
	require.NoError(t, err, "GetSessionTiming")
	assert.Equal(t, 0, got.TurnCount)
}

func TestPGGetSessionTiming_SubagentExactDuration(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	timingResetSession(t, pg, "timing-parent")
	timingResetSession(t, pg, "timing-child")
	timingInsertSessionPG(t, pg, "timing-parent",
		"2026-04-26T10:00:00Z", "2026-04-26T10:05:00Z")
	timingInsertSessionPG(t, pg, "timing-child",
		"2026-04-26T10:00:01Z", "2026-04-26T10:02:15Z")
	timingInsertMessagePG(t, pg, "timing-parent", 0, "user",
		"go", "2026-04-26T10:00:00Z", false)
	timingInsertMessagePG(t, pg, "timing-parent", 1, "assistant",
		"spawning", "2026-04-26T10:00:01Z", true)
	timingInsertToolCallPG(t, pg, "timing-parent", 1, 0,
		"tu_a", "Agent", "Task", "timing-child")
	timingInsertMessagePG(t, pg, "timing-parent", 2, "user",
		"done", "2026-04-26T10:02:16Z", false)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-parent",
	)
	require.NoError(t, err, "GetSessionTiming")
	require.NotNil(t, got.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(134_000), *got.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, 1, got.SubagentCount)
}

func TestPGGetSessionTiming_MissingSessionReturnsNil(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "no-such-session",
	)
	require.NoError(t, err, "GetSessionTiming")
	assert.Nil(t, got)
}
