//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
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
		VALUES ($1, $2, $3, $4, $5::timestamptz, $6, $7)
	`, sessionID, ordinal, role, content, ts, hasToolUse, len(content))
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
	assert.Nil(t, got.Turns[0].Calls[0].DurationMs)
	assert.Zero(t, got.ToolDurationMs)
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
	assert.Nil(t, got.Turns[0].Calls[0].DurationMs)
	assert.Zero(t, got.ToolDurationMs)
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

func TestPGGetSessionTiming_ClosedSubagentTimestampsMeasureExecution(t *testing.T) {
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
	assert.Equal(t, new("timing-child"), got.Turns[0].Calls[0].SubagentSessionID)
	assert.Equal(t, int64(134_000), got.ToolDurationMs)
	require.Len(t, got.ByCategory, 1)
	assert.Equal(t, db.CategoryTotal{Category: "Task", DurationMs: 134_000, CallCount: 1}, got.ByCategory[0])
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

func TestPGGetSessionTiming_ActivityTiming(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)
	pg, err := Open(pgURL, testSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	type execution struct {
		category, start, end string
		wantDuration         *int64
	}
	for _, tc := range []struct {
		name                                     string
		executions                               []execution
		noPrompt, staleEnd, carriers, openChild  bool
		closedChild, legacyPrefix                bool
		wantDuration, wantTool, wantUnattributed int64
		wantCategories                           []db.CategoryTotal
	}{
		{name: "measured thinking followed by tool", executions: []execution{{"Bash", "02", "04", new(int64(2000))}}, wantDuration: 6000, wantTool: 2000, wantUnattributed: 4000, wantCategories: []db.CategoryTotal{{Category: "Bash", DurationMs: 2000, CallCount: 1}}},
		{name: "missing execution", executions: []execution{{category: "Bash"}}, wantDuration: 6000, wantUnattributed: 6000, wantCategories: []db.CategoryTotal{{Category: "Bash", CallCount: 1}}},
		{name: "same and cross category overlap", executions: []execution{{"Bash", "02", "04", new(int64(2000))}, {"Bash", "03", "05", new(int64(2000))}, {"Read", "04", "06", new(int64(2000))}}, wantDuration: 6000, wantTool: 4000, wantUnattributed: 2000, wantCategories: []db.CategoryTotal{{Category: "Bash", DurationMs: 3000, CallCount: 2}, {Category: "Read", DurationMs: 2000, CallCount: 1}}},
		{name: "stale session end", staleEnd: true, executions: []execution{{"Bash", "02", "04", new(int64(2000))}}, wantDuration: 1000, wantUnattributed: 1000, wantCategories: []db.CategoryTotal{{Category: "Bash", CallCount: 1}}},
		{name: "no visible prompt", noPrompt: true, executions: []execution{{"Bash", "02", "04", new(int64(2000))}}, wantTool: 2000, wantCategories: []db.CategoryTotal{{Category: "Bash", DurationMs: 2000, CallCount: 1}}},
		{name: "system and tool result carriers", carriers: true, executions: []execution{{"Bash", "02", "04", new(int64(2000))}}, wantDuration: 6000, wantTool: 2000, wantUnattributed: 4000, wantCategories: []db.CategoryTotal{{Category: "Bash", DurationMs: 2000, CallCount: 1}}},
		{name: "open child", openChild: true, executions: []execution{{category: "Task"}}, wantDuration: 6000, wantUnattributed: 6000, wantCategories: []db.CategoryTotal{{Category: "Task", CallCount: 1}}},
		{name: "legacy prefix and child precedence", closedChild: true, legacyPrefix: true, executions: []execution{{"Task", "01", "01.500", new(int64(2000))}}, wantDuration: 6000, wantTool: 2000, wantUnattributed: 4000, wantCategories: []db.CategoryTotal{{Category: "Task", DurationMs: 2000, CallCount: 1}}},
		{name: "zero execution", executions: []execution{{"Bash", "02", "02", new(int64(0))}}, wantDuration: 6000, wantUnattributed: 6000, wantCategories: []db.CategoryTotal{{Category: "Bash", CallCount: 1}}},
		{name: "backward execution", executions: []execution{{"Bash", "04", "02", nil}}, wantDuration: 6000, wantUnattributed: 6000, wantCategories: []db.CategoryTotal{{Category: "Bash", CallCount: 1}}},
		{name: "open execution", executions: []execution{{"Bash", "02", "", nil}}, wantDuration: 6000, wantUnattributed: 6000, wantCategories: []db.CategoryTotal{{Category: "Bash", CallCount: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const sessionID = "timing-activity"
			timingResetSession(t, pg, sessionID)
			timingResetSession(t, pg, "timing-activity-child")
			end := "2026-04-26T10:00:06Z"
			if tc.staleEnd {
				end = "2026-04-26T10:00:01Z"
			}
			timingInsertSessionPG(t, pg, sessionID, "2026-04-26T10:00:00Z", end)
			role := "user"
			if tc.noPrompt {
				role = "assistant"
			}
			timingInsertMessagePG(t, pg, sessionID, 0, role, "go", "2026-04-26T10:00:00Z", false)
			timingInsertMessagePG(t, pg, sessionID, 1, "assistant", "thinking then tools", "2026-04-26T10:00:01Z", true)
			_, err = pg.Exec(`UPDATE messages SET has_thinking = TRUE, thinking_text = 'considering' WHERE session_id = $1 AND ordinal = 1`, sessionID)
			require.NoError(t, err)
			if !tc.noPrompt && !tc.staleEnd {
				timingInsertMessagePG(t, pg, sessionID, 5, "user", "next", "2026-04-26T10:00:06Z", false)
			}
			if tc.legacyPrefix {
				timingInsertMessagePG(t, pg, sessionID, 2, "user", "This session is being continued from another session.", "2026-04-26T10:00:05Z", false)
			}
			if tc.carriers {
				timingInsertMessagePG(t, pg, sessionID, 2, "user", "system", "2026-04-26T10:00:02Z", false)
				timingInsertMessagePG(t, pg, sessionID, 3, "user", "result", "2026-04-26T10:00:03Z", false)
				timingInsertMessagePG(t, pg, sessionID, 4, "user", "", "2026-04-26T10:00:04Z", false)
				_, err = pg.Exec(`UPDATE messages SET is_system = ordinal = 2, source_subtype = CASE WHEN ordinal = 3 THEN 'tool_result' ELSE '' END WHERE session_id = $1 AND ordinal IN (2,3)`, sessionID)
				require.NoError(t, err)
			}
			child := ""
			if tc.openChild || tc.closedChild {
				child = "timing-activity-child"
				childEnd := ""
				if tc.closedChild {
					childEnd = "2026-04-26T10:00:04Z"
				}
				timingInsertSessionPG(t, pg, child, "2026-04-26T10:00:02Z", childEnd)
			}
			for i, call := range tc.executions {
				id := fmt.Sprintf("call-%d", i)
				timingInsertToolCallPG(t, pg, sessionID, 1, i, id, call.category, call.category, child)
				for j, event := range []struct{ status, timestamp string }{{"started", call.start}, {"completed", call.end}} {
					if event.timestamp == "" {
						continue
					}
					_, err = pg.Exec(`INSERT INTO tool_result_events (session_id, tool_call_message_ordinal, call_index, tool_use_id, source, status, content, timestamp, event_index) VALUES ($1, 1, $2, $3, 'tool_execution', $4, '', $5::timestamptz, $6)`, sessionID, i, id, event.status, "2026-04-26T10:00:"+event.timestamp+"Z", j)
					require.NoError(t, err)
				}
			}
			got, err := store.GetSessionTiming(context.Background(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.wantTool, got.ToolDurationMs)
			assert.Equal(t, db.ActivityTotals{ToolMs: tc.wantTool, UnattributedMs: tc.wantUnattributed}, got.ActivityTotals)
			assert.ElementsMatch(t, tc.wantCategories, got.ByCategory)
			assert.Equal(t, len(tc.executions), got.ToolCallCount)
			require.Len(t, got.Turns, 1)
			assert.Equal(t, int64(1), got.Turns[0].MessageID)
			require.Len(t, got.Turns[0].Calls, len(tc.executions))
			for i, call := range tc.executions {
				assert.Equal(t, call.wantDuration, got.Turns[0].Calls[i].DurationMs)
			}
			if tc.noPrompt {
				assert.Empty(t, got.Activity)
				return
			}
			count := 2
			if tc.staleEnd {
				count = 1
			}
			require.Len(t, got.Activity, count)
			assert.Equal(t, int64(0), got.Activity[0].MessageID)
			assert.EqualValues(t, 0, got.Activity[0].Ordinal)
			assert.Equal(t, tc.wantDuration, got.Activity[0].DurationMs)
			assert.Equal(t, tc.wantTool, got.Activity[0].ToolMs)
			assert.Equal(t, tc.wantUnattributed, got.Activity[0].UnattributedMs)
			assert.False(t, got.Activity[0].Running)
		})
	}
}
