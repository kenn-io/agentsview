package db

import (
	"context"
	"strings"
	"testing"
)

func TestGetSessionTiming_Solo(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	timingInsertSession(t, d, "s1",
		"2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	timingInsertMessage(t, d, "s1", 0, "user",
		"go", "2026-04-26T10:00:00Z", false)
	timingInsertMessage(t, d, "s1", 1, "assistant",
		"running test", "2026-04-26T10:00:01Z", true)
	timingInsertToolCall(t, d, "s1", timingMsgID(t, d, "s1", 1),
		"tu_1", "Bash", "Bash", "")
	timingInsertMessage(t, d, "s1", 2, "user",
		"ok", "2026-04-26T10:00:30Z", false)

	got, err := d.GetSessionTiming(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
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

func TestGetSessionTiming_LastMessageFallsBackToSessionEnd(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	timingInsertSession(t, d, "s1",
		"2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	timingInsertMessage(t, d, "s1", 0, "user",
		"run", "2026-04-26T10:00:00Z", false)
	timingInsertMessage(t, d, "s1", 1, "assistant",
		"doing", "2026-04-26T10:00:10Z", true)
	timingInsertToolCall(t, d, "s1", timingMsgID(t, d, "s1", 1),
		"tu_1", "Bash", "Bash", "")

	got, err := d.GetSessionTiming(ctx, "s1")
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

func TestGetSessionTiming_RunningSessionLastTurnNull(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	timingInsertSession(t, d, "s1",
		"2026-04-26T10:00:00Z", "")
	timingInsertMessage(t, d, "s1", 0, "user",
		"run", "2026-04-26T10:00:00Z", false)
	timingInsertMessage(t, d, "s1", 1, "assistant",
		"doing", "2026-04-26T10:00:10Z", true)
	timingInsertToolCall(t, d, "s1", timingMsgID(t, d, "s1", 1),
		"tu_1", "Bash", "Bash", "")

	got, err := d.GetSessionTiming(ctx, "s1")
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

func TestGetSessionTiming_NonMonotonicTimestampClampsNull(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	timingInsertSession(t, d, "s1",
		"2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	timingInsertMessage(t, d, "s1", 0, "user",
		"run", "2026-04-26T10:00:20Z", false)
	timingInsertMessage(t, d, "s1", 1, "assistant",
		"broken", "2026-04-26T10:00:25Z", true)
	timingInsertToolCall(t, d, "s1", timingMsgID(t, d, "s1", 1),
		"tu_1", "Bash", "Bash", "")
	timingInsertMessage(t, d, "s1", 2, "user",
		"ok", "2026-04-26T10:00:00Z", false)

	got, err := d.GetSessionTiming(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got.Turns[0].DurationMs != nil {
		t.Errorf("turn duration = %v, want nil (clamp)",
			*got.Turns[0].DurationMs)
	}
}

func TestGetSessionTiming_NoToolUseHasNoTurnDuration(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	timingInsertSession(t, d, "s1",
		"2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	timingInsertMessage(t, d, "s1", 0, "user",
		"hi", "2026-04-26T10:00:00Z", false)
	timingInsertMessage(t, d, "s1", 1, "assistant",
		"hi back", "2026-04-26T10:00:01Z", false)

	got, err := d.GetSessionTiming(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got.TurnCount != 0 {
		t.Errorf("TurnCount = %d, want 0", got.TurnCount)
	}
}

func TestGetSessionTiming_SubagentExactDuration(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	timingInsertSession(t, d, "parent",
		"2026-04-26T10:00:00Z", "2026-04-26T10:05:00Z")
	timingInsertSession(t, d, "child",
		"2026-04-26T10:00:01Z", "2026-04-26T10:02:15Z")
	timingInsertMessage(t, d, "parent", 0, "user",
		"go", "2026-04-26T10:00:00Z", false)
	timingInsertMessage(t, d, "parent", 1, "assistant",
		"spawning", "2026-04-26T10:00:01Z", true)
	timingInsertToolCall(t, d, "parent",
		timingMsgID(t, d, "parent", 1),
		"tu_a", "Agent", "Task", "child")
	timingInsertMessage(t, d, "parent", 2, "user",
		"done", "2026-04-26T10:02:16Z", false)

	got, err := d.GetSessionTiming(ctx, "parent")
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

func TestGetSessionTiming_MissingSessionReturnsNil(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	got, err := d.GetSessionTiming(ctx, "no-such")
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if got != nil {
		t.Errorf("GetSessionTiming = %v, want nil", got)
	}
}

func TestMakeInputPreview(t *testing.T) {
	cases := []struct {
		name      string
		category  string
		toolName  string
		inputJSON string
		want      string
	}{
		{
			name:      "claude bash uses command key",
			category:  "Bash",
			toolName:  "Bash",
			inputJSON: `{"command":"ls -la"}`,
			want:      "ls -la",
		},
		{
			name:      "codex exec_command uses cmd key via category",
			category:  "Bash",
			toolName:  "exec_command",
			inputJSON: `{"cmd":"nl -ba file.md","workdir":"/x"}`,
			want:      "nl -ba file.md",
		},
		{
			name:      "bash trims to first line",
			category:  "Bash",
			toolName:  "exec_command",
			inputJSON: `{"cmd":"echo a\necho b"}`,
			want:      "echo a",
		},
		{
			name:      "codex apply_patch falls through to category Edit",
			category:  "Edit",
			toolName:  "apply_patch",
			inputJSON: `{"file_path":"/tmp/foo.go"}`,
			want:      "/tmp/foo.go",
		},
		{
			name:      "skill prefers tool name over Tool category",
			category:  "Tool",
			toolName:  "Skill",
			inputJSON: `{"skill":"using-superpowers"}`,
			want:      "using-superpowers",
		},
		{
			name:      "unknown tool with Other category falls back to common keys",
			category:  "Other",
			toolName:  "weird_tool",
			inputJSON: `{"cmd":"do thing"}`,
			want:      "do thing",
		},
		{
			name:      "empty input returns empty",
			category:  "Bash",
			toolName:  "Bash",
			inputJSON: "",
			want:      "",
		},
		{
			name:      "invalid json returns empty",
			category:  "Bash",
			toolName:  "Bash",
			inputJSON: `{not json`,
			want:      "",
		},
		{
			name:     "long value is truncated with ellipsis",
			category: "Read",
			toolName: "Read",
			inputJSON: `{"file_path":"` +
				strings.Repeat("a", 150) + `"}`,
			want: strings.Repeat("a", 100) + "…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := makeInputPreview(
				tc.category, tc.toolName, tc.inputJSON,
			)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- helpers -----------------------------------------------------------
//
// Names are prefixed with "timing" to avoid colliding with the existing
// insertSession/insertMessage helpers in db_test.go, which take very
// different parameter shapes.

func timingInsertSession(t *testing.T, d *DB, id, started, ended string) {
	t.Helper()
	var endedAt any = nil
	if ended != "" {
		endedAt = ended
	}
	_, err := d.getWriter().ExecContext(context.Background(), `
		INSERT INTO sessions
			(id, project, machine, agent, message_count,
			 started_at, ended_at)
		VALUES (?, '', 'local', 'claude', 1, ?, ?)
	`, id, started, endedAt)
	if err != nil {
		t.Fatalf("timingInsertSession %s: %v", id, err)
	}
}

func timingInsertMessage(
	t *testing.T, d *DB,
	sessionID string, ordinal int,
	role, content, ts string, hasToolUse bool,
) {
	t.Helper()
	flag := 0
	if hasToolUse {
		flag = 1
	}
	_, err := d.getWriter().ExecContext(context.Background(), `
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 has_tool_use)
		VALUES (?, ?, ?, ?, ?, ?)
	`, sessionID, ordinal, role, content, ts, flag)
	if err != nil {
		t.Fatalf("timingInsertMessage %s/%d: %v",
			sessionID, ordinal, err)
	}
}

func timingMsgID(
	t *testing.T, d *DB, sessionID string, ordinal int,
) int64 {
	t.Helper()
	var id int64
	err := d.getReader().QueryRowContext(context.Background(),
		`SELECT id FROM messages
		 WHERE session_id = ? AND ordinal = ?`,
		sessionID, ordinal,
	).Scan(&id)
	if err != nil {
		t.Fatalf("timingMsgID %s/%d: %v",
			sessionID, ordinal, err)
	}
	return id
}

func timingInsertToolCall(
	t *testing.T, d *DB,
	sessionID string, messageID int64,
	toolUseID, toolName, category, subagentSessionID string,
) {
	t.Helper()
	var sub any = nil
	if subagentSessionID != "" {
		sub = subagentSessionID
	}
	_, err := d.getWriter().ExecContext(context.Background(), `
		INSERT INTO tool_calls
			(session_id, message_id, tool_use_id, tool_name,
			 category, input_json, subagent_session_id)
		VALUES (?, ?, ?, ?, ?, '{}', ?)
	`, sessionID, messageID, toolUseID, toolName, category, sub)
	if err != nil {
		t.Fatalf("timingInsertToolCall %s/%d: %v",
			sessionID, messageID, err)
	}
}
