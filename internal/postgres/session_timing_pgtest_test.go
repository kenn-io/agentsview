//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"
)

// timingResetSession deletes any prior fixtures for the given session id
// (and cascades to messages and tool_calls) so each test starts clean.
func timingResetSession(t *testing.T, pg *sql.DB, sessionID string) {
	t.Helper()
	if _, err := pg.Exec(
		`DELETE FROM sessions WHERE id = $1`, sessionID,
	); err != nil {
		t.Fatalf("reset session: %v", err)
	}
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
	if err != nil {
		t.Fatalf("insert session %s: %v", id, err)
	}
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
	if err != nil {
		t.Fatalf("insert message %s/%d: %v",
			sessionID, ordinal, err)
	}
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
	if err != nil {
		t.Fatalf("insert tool_call %s/%d: %v",
			sessionID, msgOrdinal, err)
	}
}

func TestPGGetSessionTiming_Solo(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
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
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-solo",
	)
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got == nil {
		t.Fatal("GetSessionTiming returned nil, want timing")
	}
	if got.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", got.TurnCount)
	}
	if got.ToolCallCount != 1 {
		t.Errorf("ToolCallCount = %d, want 1", got.ToolCallCount)
	}
	if got.Running {
		t.Errorf("Running = true, want false")
	}
	if len(got.Turns) != 1 {
		t.Fatalf("len(Turns) = %d, want 1", len(got.Turns))
	}
	if got.Turns[0].DurationMs == nil ||
		*got.Turns[0].DurationMs != 29_000 {
		t.Errorf("turn duration = %v, want 29000",
			got.Turns[0].DurationMs)
	}
	if got.Turns[0].Calls[0].DurationMs == nil ||
		*got.Turns[0].Calls[0].DurationMs != 29_000 {
		t.Errorf("call duration = %v, want 29000",
			got.Turns[0].Calls[0].DurationMs)
	}
}

func TestPGGetSessionTiming_LastMessageFallsBackToSessionEnd(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
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
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-fallback",
	)
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got.Turns[0].DurationMs == nil {
		t.Fatalf("turn duration = nil, want 20000 " +
			"(fallback to ended_at)")
	}
	if *got.Turns[0].DurationMs != 20_000 {
		t.Errorf("turn duration = %d, want 20000 "+
			"(fallback to ended_at)",
			*got.Turns[0].DurationMs)
	}
	if got.Turns[0].Calls[0].DurationMs == nil ||
		*got.Turns[0].Calls[0].DurationMs != 20_000 {
		t.Errorf("call duration = %v, want 20000 "+
			"(solo non-subagent inherits turn duration)",
			got.Turns[0].Calls[0].DurationMs)
	}
}

func TestPGGetSessionTiming_RunningSessionLastTurnNull(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
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
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-running",
	)
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if !got.Running {
		t.Errorf("Running = false, want true")
	}
	if got.Turns[0].DurationMs != nil {
		t.Errorf("turn duration = %v, want nil (running)",
			*got.Turns[0].DurationMs)
	}
}

func TestPGGetSessionTiming_NonMonotonicTimestampClampsNull(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
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
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-nonmono",
	)
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got.Turns[0].DurationMs != nil {
		t.Errorf("turn duration = %v, want nil (clamp)",
			*got.Turns[0].DurationMs)
	}
}

func TestPGGetSessionTiming_NoToolUseHasNoTurnDuration(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	timingResetSession(t, pg, "timing-notool")
	timingInsertSessionPG(t, pg, "timing-notool",
		"2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	timingInsertMessagePG(t, pg, "timing-notool", 0, "user",
		"hi", "2026-04-26T10:00:00Z", false)
	timingInsertMessagePG(t, pg, "timing-notool", 1, "assistant",
		"hi back", "2026-04-26T10:00:01Z", false)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-notool",
	)
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got.TurnCount != 0 {
		t.Errorf("TurnCount = %d, want 0", got.TurnCount)
	}
}

func TestPGGetSessionTiming_SubagentExactDuration(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
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
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "timing-parent",
	)
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	dms := got.Turns[0].Calls[0].DurationMs
	if dms == nil || *dms != 134_000 {
		t.Errorf("subagent duration = %v, want 134000", dms)
	}
	if got.SubagentCount != 1 {
		t.Errorf("SubagentCount = %d, want 1", got.SubagentCount)
	}
}

func TestPGGetSessionTiming_MissingSessionReturnsNil(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	got, err := store.GetSessionTiming(
		context.Background(), "no-such-session",
	)
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got != nil {
		t.Errorf("GetSessionTiming = %v, want nil", got)
	}
}
