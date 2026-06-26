package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "GetSessionTiming")
	assert.Equal(t, 1, got.TurnCount, "TurnCount")
	assert.Equal(t, 1, got.ToolCallCount, "ToolCallCount")
	assert.False(t, got.Running, "Running")
	require.Len(t, got.Turns, 1, "len(Turns)")
	require.NotNil(t, got.Turns[0].DurationMs, "turn duration")
	assert.Equal(t, int64(29_000), *got.Turns[0].DurationMs, "turn duration")
	require.NotNil(t, got.Turns[0].Calls[0].DurationMs, "call duration")
	assert.Equal(t, int64(29_000), *got.Turns[0].Calls[0].DurationMs, "call duration")
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
	require.NoError(t, err, "GetSessionTiming")
	require.NotNil(t, got.Turns[0].DurationMs,
		"turn duration nil, want 20000 (fallback to ended_at)")
	assert.Equal(t, int64(20_000), *got.Turns[0].DurationMs,
		"turn duration (fallback to ended_at)")
	require.NotNil(t, got.Turns[0].Calls[0].DurationMs, "call duration")
	assert.Equal(t, int64(20_000), *got.Turns[0].Calls[0].DurationMs,
		"call duration (solo non-subagent inherits turn duration)")
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
	require.NoError(t, err, "GetSessionTiming")
	assert.True(t, got.Running, "Running")
	assert.Nil(t, got.Turns[0].DurationMs, "turn duration (running)")
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
	require.NoError(t, err, "GetSessionTiming")
	assert.Nil(t, got.Turns[0].DurationMs, "turn duration (clamp)")
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
	require.NoError(t, err, "GetSessionTiming")
	assert.Equal(t, 0, got.TurnCount, "TurnCount")
}

// TestActiveGapCapConstantsAgree guards the two spellings of the active
// gap cap against drifting apart: the velocity metric uses the seconds
// form and the active-duration SQL uses the milliseconds form.
func TestActiveGapCapConstantsAgree(t *testing.T) {
	assert.Equal(
		t, ActiveGapCapMs, int(ActiveGapCapSec*1000),
		"ActiveGapCapMs must equal ActiveGapCapSec in milliseconds",
	)
}

func TestGetSessionTiming_MarshalsEmptyCollectionsAsArrays(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	timingInsertSession(t, d, "notool",
		"2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	timingInsertMessage(t, d, "notool", 0, "user",
		"hi", "2026-04-26T10:00:00Z", false)
	timingInsertMessage(t, d, "notool", 1, "assistant",
		"hi back", "2026-04-26T10:00:01Z", false)

	noTool, err := d.GetSessionTiming(ctx, "notool")
	require.NoError(t, err, "GetSessionTiming(notool)")
	require.NotNil(t, noTool.ByCategory, "ByCategory is nil, want empty slice")
	require.NotNil(t, noTool.Turns, "Turns is nil, want empty slice")

	timingInsertSession(t, d, "missing-calls",
		"2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	timingInsertMessage(t, d, "missing-calls", 0, "user",
		"go", "2026-04-26T10:00:00Z", false)
	timingInsertMessage(t, d, "missing-calls", 1, "assistant",
		"legacy tool marker", "2026-04-26T10:00:01Z", true)
	timingInsertMessage(t, d, "missing-calls", 2, "user",
		"done", "2026-04-26T10:00:30Z", false)

	missingCalls, err := d.GetSessionTiming(ctx, "missing-calls")
	require.NoError(t, err, "GetSessionTiming(missing-calls)")
	require.Len(t, missingCalls.Turns, 1, "len(Turns)")
	require.NotNil(t, missingCalls.Turns[0].Calls,
		"Turn Calls is nil, want empty slice")

	payload, err := json.Marshal(missingCalls)
	require.NoError(t, err, "Marshal timing")
	body := string(payload)
	for _, field := range []string{
		`"by_category":null`,
		`"turns":null`,
		`"calls":null`,
	} {
		assert.NotContains(t, body, field, "timing JSON contains %s", field)
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
	require.NoError(t, err, "GetSessionTiming")
	dms := got.Turns[0].Calls[0].DurationMs
	require.NotNil(t, dms, "subagent duration")
	assert.Equal(t, int64(134_000), *dms, "subagent duration")
	assert.Equal(t, 1, got.SubagentCount, "SubagentCount")
}

func TestGetSessionTiming_MissingSessionReturnsNil(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	got, err := d.GetSessionTiming(ctx, "no-such")
	require.NoError(t, err, "GetSessionTiming")
	assert.Nil(t, got, "GetSessionTiming")
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
			assert.Equal(t, tc.want, got)
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
	require.NoError(t, err, "timingInsertSession %s", id)
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
	require.NoError(t, err, "timingInsertMessage %s/%d", sessionID, ordinal)
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
	require.NoError(t, err, "timingMsgID %s/%d", sessionID, ordinal)
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
	require.NoError(t, err, "timingInsertToolCall %s/%d", sessionID, messageID)
}
