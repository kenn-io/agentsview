package db

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivityTiming_MissingPhaseEvidenceStaysUnattributed(t *testing.T) {
	for _, paired := range []bool{true, false} {
		name := "missing execution evidence"
		if paired {
			name = "thinking followed by measured tool"
		}
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			timingInsertSession(t, d, "activity", "2026-04-26T10:00:00Z", "2026-04-26T10:00:06Z")
			timingInsertMessage(t, d, "activity", 0, "user", "run", "2026-04-26T10:00:00Z", false)
			timingInsertMessage(t, d, "activity", 1, "assistant", "considering the request", "2026-04-26T10:00:00.500Z", false)
			_, err := d.getWriter().Exec(t.Context(), `UPDATE messages SET has_thinking = 1, thinking_text = 'considering the request' WHERE session_id = 'activity' AND ordinal = 1`)
			require.NoError(t, err)
			timingInsertMessage(t, d, "activity", 2, "assistant", "running", "2026-04-26T10:00:01Z", true)
			timingInsertToolCall(t, d, "activity", timingMsgID(t, d, "activity", 2), "tool", "Bash", "Bash", "")
			if paired {
				timingInsertToolResultEvent(t, d, "activity", 2, 0, "tool", "started", "2026-04-26T10:00:02Z", 0)
				timingInsertToolResultEvent(t, d, "activity", 2, 0, "tool", "completed", "2026-04-26T10:00:04Z", 1)
			}
			timingInsertMessage(t, d, "activity", 3, "user", "next", "2026-04-26T10:00:06Z", false)
			got, err := d.GetSessionTiming(t.Context(), "activity")
			require.NoError(t, err)
			require.Len(t, got.Turns, 1)
			require.Len(t, got.Turns[0].Calls, 1)
			payload, err := json.Marshal(got)
			require.NoError(t, err)
			wantTool, wantUnknown := int64(0), float64(6000)
			if paired {
				wantTool, wantUnknown = 2000, 4000
				require.NotNil(t, got.Turns[0].Calls[0].DurationMs)
				assert.Equal(t, int64(2000), *got.Turns[0].Calls[0].DurationMs)
			} else {
				assert.Nil(t, got.Turns[0].Calls[0].DurationMs)
			}
			assert.Equal(t, wantTool, got.ToolDurationMs)
			var body map[string]any
			require.NoError(t, json.Unmarshal(payload, &body))
			activity, ok := body["activity"].([]any)
			require.True(t, ok, "activity must be an array")
			require.NotEmpty(t, activity)
			assert.Equal(t, map[string]any{
				"message_id": float64(timingMsgID(t, d, "activity", 0)), "ordinal": float64(0),
				"started_at": "2026-04-26T10:00:00Z", "duration_ms": float64(6000),
				"tool_ms": float64(wantTool), "unattributed_ms": wantUnknown,
				"running": false,
			}, activity[0])
		})
	}
}

func TestActivityTiming_SessionBoundsPreserveCallEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		call                                 CallRow
		wantTool, wantCall, wantUnattributed int64
	}{
		{
			name: "execution begins before session",
			call: CallRow{
				MessageID: 2, Category: "Bash",
				ExecutionStart: "2026-04-26T09:59:50Z",
				ExecutionEnd:   "2026-04-26T10:00:10Z",
			},
			wantTool: 10000, wantCall: 20000, wantUnattributed: 50000,
		},
		{
			name: "child ends after session",
			call: CallRow{
				MessageID: 2, Category: "Task", SubagentSessionID: new("child"),
				SubagentStart: "2026-04-26T10:00:06Z",
				SubagentEnd:   "2026-04-26T10:10:00Z",
			},
			wantTool: 54000, wantCall: 594000, wantUnattributed: 6000,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AssembleTiming(
				&Session{ID: "bounded", StartedAt: new("2026-04-26T10:00:00Z"), EndedAt: new("2026-04-26T10:01:00Z")},
				[]TurnRow{
					{MessageID: 1, Role: "user", ContentLength: 3, Timestamp: "2026-04-26T10:00:00Z"},
					{MessageID: 2, Ordinal: 1, Role: "assistant", HasToolUse: true, Timestamp: "2026-04-26T10:00:06Z"},
				},
				[]CallRow{tc.call}, time.Date(2026, 4, 26, 10, 11, 0, 0, time.UTC),
			)
			assert.Equal(t, int64(60000), got.TotalDurationMs)
			assert.Equal(t, tc.wantTool, got.ToolDurationMs)
			assert.Equal(t, []CategoryTotal{{Category: tc.call.Category, CallCount: 1, DurationMs: tc.wantTool}}, got.ByCategory)
			require.Len(t, got.Activity, 1)
			assert.Equal(t, int64(60000), got.Activity[0].DurationMs)
			assert.Equal(t, tc.wantTool, got.Activity[0].ToolMs)
			assert.Equal(t, tc.wantUnattributed, got.Activity[0].UnattributedMs)
			require.NotNil(t, got.SlowestCall)
			assert.Equal(t, new(tc.wantCall), got.SlowestCall.DurationMs)
		})
	}
}

func TestActivityTiming_Intervals(t *testing.T) {
	stamp := func(offset time.Duration) string {
		return time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC).Add(offset).Format(time.RFC3339Nano)
	}
	prompt := func(ordinal int64, offset time.Duration) TurnRow {
		return TurnRow{MessageID: ordinal + 1, Ordinal: ordinal, Role: "user", ContentLength: 3, Timestamp: stamp(offset)}
	}
	call := func(category string, start, end time.Duration) CallRow {
		return CallRow{MessageID: 2, ToolUseID: category, ToolName: category, Category: category, ExecutionStart: stamp(start), ExecutionEnd: stamp(end)}
	}
	paired := call("Bash", 2*time.Second, 4*time.Second)
	toolRow := TurnRow{MessageID: 2, Ordinal: 1, Role: "assistant", Timestamp: stamp(time.Second), HasToolUse: true}
	for _, tc := range []struct {
		name       string
		prompts    []TurnRow
		calls      []CallRow
		ended      string
		running    bool
		tool       int64
		windows    []int64
		tools      []int64
		unknown    []int64
		categories []CategoryTotal
		durations  []*int64
		slowest    *int64
	}{
		{name: "empty", ended: stamp(6 * time.Second)},
		{durations: []*int64{nil}, name: "missing", prompts: []TurnRow{prompt(0, 0)}, calls: []CallRow{{MessageID: 2, Category: "Bash"}}, ended: stamp(6 * time.Second), windows: []int64{6000}, tools: []int64{0}, unknown: []int64{6000}, categories: []CategoryTotal{{Category: "Bash", CallCount: 1}}},
		{name: "paired", prompts: []TurnRow{prompt(0, 0)}, calls: []CallRow{paired}, ended: stamp(6 * time.Second), tool: 2000, windows: []int64{6000}, tools: []int64{2000}, unknown: []int64{4000}},
		{name: "invalid sibling preserves measurement", prompts: []TurnRow{prompt(0, 0)}, calls: []CallRow{paired, {MessageID: 2, Category: "Read", ExecutionStart: "invalid", ExecutionEnd: stamp(5 * time.Second)}}, ended: stamp(6 * time.Second), tool: 2000, windows: []int64{6000}, tools: []int64{2000}, unknown: []int64{4000}, categories: []CategoryTotal{{Category: "Bash", DurationMs: 2000, CallCount: 1}, {Category: "Read", CallCount: 1}}},
		{name: "execution outside session", prompts: []TurnRow{prompt(0, 0)}, calls: []CallRow{paired}, ended: stamp(time.Second), windows: []int64{1000}, tools: []int64{0}, unknown: []int64{1000}},
		{name: "no visible prompt", calls: []CallRow{paired}, ended: stamp(time.Second)},
		{name: "split at prompt", prompts: []TurnRow{prompt(0, 0), prompt(2, 3*time.Second)}, calls: []CallRow{paired}, ended: stamp(6 * time.Second), tool: 2000, windows: []int64{3000, 3000}, tools: []int64{1000, 1000}, unknown: []int64{2000, 2000}},
		{durations: []*int64{new(int64(2000))}, slowest: new(int64(2000)), name: "clip before prompt", prompts: []TurnRow{prompt(0, 3*time.Second)}, calls: []CallRow{paired}, ended: stamp(6 * time.Second), tool: 2000, windows: []int64{3000}, tools: []int64{1000}, unknown: []int64{2000}},
		{name: "same and cross category overlap", prompts: []TurnRow{prompt(0, 0)}, calls: []CallRow{paired, call("Bash", 3*time.Second, 5*time.Second), call("Read", 4*time.Second, 6*time.Second)}, ended: stamp(6 * time.Second), tool: 4000, windows: []int64{6000}, tools: []int64{4000}, unknown: []int64{2000}, categories: []CategoryTotal{{Category: "Bash", DurationMs: 3000, CallCount: 2}, {Category: "Read", DurationMs: 2000, CallCount: 1}}},
		{name: "disjoint intervals", prompts: []TurnRow{prompt(0, 0)}, calls: []CallRow{call("Bash", time.Second, 2*time.Second), call("Bash", 4*time.Second, 5*time.Second)}, ended: stamp(6 * time.Second), tool: 2000, windows: []int64{6000}, tools: []int64{2000}, unknown: []int64{4000}},
		{name: "running tail", prompts: []TurnRow{prompt(0, 0), prompt(2, 3*time.Second)}, calls: []CallRow{paired}, running: true, tool: 2000, windows: []int64{3000, 3000}, tools: []int64{1000, 1000}, unknown: []int64{2000, 2000}},
		{durations: []*int64{new(int64(0))}, slowest: new(int64(0)), name: "zero execution", prompts: []TurnRow{prompt(0, 0)}, calls: []CallRow{call("Bash", 2*time.Second, 2*time.Second)}, ended: stamp(6 * time.Second), windows: []int64{6000}, tools: []int64{0}, unknown: []int64{6000}},
		{durations: []*int64{nil}, name: "backward execution", prompts: []TurnRow{prompt(0, 0)}, calls: []CallRow{call("Bash", 4*time.Second, 2*time.Second)}, ended: stamp(6 * time.Second), windows: []int64{6000}, tools: []int64{0}, unknown: []int64{6000}},
		{durations: []*int64{nil}, name: "malformed execution", prompts: []TurnRow{prompt(0, 0)}, calls: []CallRow{{MessageID: 2, Category: "Bash", ExecutionStart: "invalid", ExecutionEnd: stamp(4 * time.Second)}}, ended: stamp(6 * time.Second), windows: []int64{6000}, tools: []int64{0}, unknown: []int64{6000}},
		{durations: []*int64{nil}, name: "open execution", prompts: []TurnRow{prompt(0, 0)}, calls: []CallRow{{MessageID: 2, Category: "Task", ExecutionStart: stamp(2 * time.Second)}}, running: true, windows: []int64{6000}, tools: []int64{0}, unknown: []int64{6000}},
		{durations: []*int64{nil}, name: "missing start", prompts: []TurnRow{prompt(0, 0)}, calls: []CallRow{{MessageID: 2, Category: "Bash", ExecutionEnd: stamp(4 * time.Second)}}, ended: stamp(6 * time.Second), windows: []int64{6000}, tools: []int64{0}, unknown: []int64{6000}},
		{name: "backward prompt", prompts: []TurnRow{prompt(0, 3*time.Second), prompt(2, time.Second)}, calls: []CallRow{paired}, ended: stamp(6 * time.Second), tool: 2000, windows: []int64{3000}, tools: []int64{1000}, unknown: []int64{2000}},
		{name: "fractional split conserves milliseconds", prompts: []TurnRow{prompt(0, 0), prompt(2, 1500*time.Microsecond)}, calls: []CallRow{call("Bash", 500*time.Microsecond, 2500*time.Microsecond)}, ended: stamp(3 * time.Millisecond), tool: 2, windows: []int64{1, 2}, tools: []int64{1, 1}, unknown: []int64{0, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := stamp(0)
			sess := &Session{ID: "activity", StartedAt: &start, EndedAt: &tc.ended}
			if tc.running {
				sess.EndedAt = nil
			}
			turns := append(append([]TurnRow{}, tc.prompts...), toolRow)
			got := AssembleTiming(sess, turns, tc.calls, time.Date(2026, 4, 26, 10, 0, 6, 0, time.UTC))
			assert.Equal(t, tc.running, got.Running)
			assert.Equal(t, tc.tool, got.ToolDurationMs)
			var activityTool int64
			for _, value := range tc.tools {
				activityTool += value
			}
			if len(tc.prompts) == 0 {
				activityTool = tc.tool
			}
			assert.Equal(t, activityTool, got.ActivityTotals.ToolMs)
			require.NotNil(t, got.Activity)
			require.Len(t, got.Activity, len(tc.windows))
			var unknown int64
			for i, row := range got.Activity {
				assert.Equal(t, tc.windows[i], row.DurationMs)
				assert.Equal(t, tc.tools[i], row.ToolMs)
				assert.Equal(t, tc.unknown[i], row.UnattributedMs)
				assert.Equal(t, row.DurationMs, row.ToolMs+row.UnattributedMs)
				assert.GreaterOrEqual(t, row.UnattributedMs, int64(0))
				assert.Equal(t, tc.running && i == len(got.Activity)-1, row.Running)
				unknown += row.UnattributedMs
			}
			assert.Equal(t, unknown, got.ActivityTotals.UnattributedMs)
			assert.Equal(t, len(tc.calls), got.ToolCallCount)
			if tc.categories != nil {
				assert.Equal(t, tc.categories, got.ByCategory)
			}
			if tc.durations != nil {
				for i, duration := range tc.durations {
					assert.Equal(t, duration, got.Turns[0].Calls[i].DurationMs)
				}
				if tc.slowest == nil {
					assert.Nil(t, got.SlowestCall)
				} else {
					require.NotNil(t, got.SlowestCall)
					assert.Equal(t, tc.slowest, got.SlowestCall.DurationMs)
				}
			}
		})
	}
}

func TestActivityTiming_StoredExecutionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, start, end, source, status string
		want                             *int64
	}{
		{name: "valid zero", start: "2026-04-26T10:00:02Z", end: "2026-04-26T10:00:02Z", source: "tool_execution", status: "completed", want: new(int64(0))},
		{name: "epoch zero", start: "1970-01-01T00:00:00Z", end: "1970-01-01T00:00:00Z", source: "tool_execution", status: "completed", want: new(int64(0))},
		{name: "errored completion", start: "2026-04-26T10:00:02Z", end: "2026-04-26T10:00:04Z", source: "tool_execution", status: "errored", want: new(int64(2000))},
		{name: "malformed start", start: "invalid", end: "2026-04-26T10:00:04Z", source: "tool_execution", status: "completed"},
		{name: "malformed end", start: "2026-04-26T10:00:02Z", end: "invalid", source: "tool_execution", status: "completed"},
		{name: "backward", start: "2026-04-26T10:00:04Z", end: "2026-04-26T10:00:02Z", source: "tool_execution", status: "completed"},
		{name: "open child", start: "2026-04-26T10:00:02Z", source: "tool_execution", status: "started"},
		{name: "other source", start: "2026-04-26T10:00:02Z", end: "2026-04-26T10:00:04Z", source: "tool_result", status: "completed"},
		{name: "other status", start: "2026-04-26T10:00:02Z", end: "2026-04-26T10:00:04Z", source: "tool_execution", status: "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			start := "2026-04-26T10:00:00Z"
			if tc.name == "epoch zero" {
				start = "1970-01-01T00:00:00Z"
			}
			timingInsertSession(t, d, "evidence", start, "2026-04-26T10:00:06Z")
			timingInsertSession(t, d, "child", "2026-04-26T10:00:01Z", "")
			timingInsertMessage(t, d, "evidence", 0, "user", "run", start, false)
			timingInsertMessage(t, d, "evidence", 1, "assistant", "tool", "2026-04-26T10:00:01Z", true)
			timingInsertToolCall(t, d, "evidence", timingMsgID(t, d, "evidence", 1), "task", "Agent", "Task", "child")
			timingInsertToolResultEvent(t, d, "evidence", 1, 0, "task", "started", tc.start, 0)
			timingInsertToolResultEvent(t, d, "evidence", 1, 0, "task", tc.status, tc.end, 1)
			_, err := d.getWriter().Exec(t.Context(), `UPDATE tool_result_events SET source = ? WHERE session_id = 'evidence'`, tc.source)
			require.NoError(t, err)
			got, err := d.GetSessionTiming(t.Context(), "evidence")
			require.NoError(t, err)
			require.Len(t, got.Turns, 1)
			require.Len(t, got.Turns[0].Calls, 1)
			assert.Equal(t, tc.want, got.Turns[0].Calls[0].DurationMs)
			assert.Equal(t, "child", *got.Turns[0].Calls[0].SubagentSessionID)
			assert.Equal(t, 1, got.SubagentCount)
			assert.Equal(t, 1, got.ToolCallCount)
			wantTool := int64(0)
			if tc.want != nil {
				wantTool = *tc.want
			}
			assert.Equal(t, wantTool, got.ToolDurationMs)
			require.Len(t, got.ByCategory, 1)
			assert.Equal(t, CategoryTotal{Category: "Task", DurationMs: wantTool, CallCount: 1}, got.ByCategory[0])
		})
	}
}

func TestActivityTiming_ClosedSubagentEvidence(t *testing.T) {
	start := "2026-04-26T10:00:00Z"
	end := "2026-04-26T10:00:06Z"
	child := "child"
	got := AssembleTiming(
		&Session{ID: "activity", StartedAt: &start, EndedAt: &end},
		[]TurnRow{
			{MessageID: 1, Ordinal: 0, Role: "user", ContentLength: 3, Timestamp: start},
			{MessageID: 2, Ordinal: 1, Role: "assistant", HasToolUse: true, ContentLength: 3, Timestamp: "2026-04-26T10:00:01Z"},
		},
		[]CallRow{{
			MessageID: 2, Category: "Task", SubagentSessionID: &child,
			ExecutionStart: "2026-04-26T10:00:01Z", ExecutionEnd: "2026-04-26T10:00:01.500Z",
			SubagentStart: "2026-04-26T10:00:02Z", SubagentEnd: "2026-04-26T10:00:04Z",
		}},
		time.Date(2026, 4, 26, 10, 0, 6, 0, time.UTC),
	)
	require.Len(t, got.Turns, 1)
	require.NotNil(t, got.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(2000), *got.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(2000), got.ToolDurationMs)
	assert.Equal(t, ActivityTotals{ToolMs: 2000, UnattributedMs: 4000}, got.ActivityTotals)
	assert.Equal(t, CategoryTotal{Category: "Task", DurationMs: 2000, CallCount: 1}, got.ByCategory[0])
	assert.Equal(t, int64(2000), *got.SlowestCall.DurationMs)
}

func TestActivityTiming_InvalidChildFallsBackToExecution(t *testing.T) {
	start := "2026-04-26T10:00:00Z"
	end := "2026-04-26T10:00:06Z"
	child := "child"
	for _, tc := range []struct {
		name, childStart, childEnd string
	}{
		{name: "missing child"},
		{name: "malformed child", childStart: "invalid", childEnd: "2026-04-26T10:00:04Z"},
		{name: "backward child", childStart: "2026-04-26T10:00:04Z", childEnd: "2026-04-26T10:00:02Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AssembleTiming(
				&Session{ID: "activity", StartedAt: &start, EndedAt: &end},
				[]TurnRow{
					{MessageID: 1, Ordinal: 0, Role: "user", ContentLength: 3, Timestamp: start},
					{MessageID: 2, Ordinal: 1, Role: "assistant", HasToolUse: true, ContentLength: 3, Timestamp: "2026-04-26T10:00:01Z"},
				},
				[]CallRow{{
					MessageID: 2, Category: "Task", SubagentSessionID: &child,
					ExecutionStart: "2026-04-26T10:00:01Z", ExecutionEnd: "2026-04-26T10:00:02Z",
					SubagentStart: tc.childStart, SubagentEnd: tc.childEnd,
				}},
				time.Date(2026, 4, 26, 10, 0, 6, 0, time.UTC),
			)
			require.Len(t, got.Turns, 1)
			require.NotNil(t, got.Turns[0].Calls[0].DurationMs)
			assert.Equal(t, int64(1000), *got.Turns[0].Calls[0].DurationMs)
			assert.Equal(t, int64(1000), got.ToolDurationMs)
			assert.Equal(t, ActivityTotals{ToolMs: 1000, UnattributedMs: 5000}, got.ActivityTotals)
			assert.Equal(t, CategoryTotal{Category: "Task", DurationMs: 1000, CallCount: 1}, got.ByCategory[0])
			require.NotNil(t, got.SlowestCall)
			assert.Equal(t, int64(1000), *got.SlowestCall.DurationMs)
		})
	}
}

func TestActivityTiming_PreservesEvidenceBeforeFirstPrompt(t *testing.T) {
	start := "2026-04-26T10:00:00Z"
	end := "2026-04-26T10:00:06Z"
	got := AssembleTiming(
		&Session{ID: "activity", StartedAt: &start, EndedAt: &end},
		[]TurnRow{
			{MessageID: 1, Ordinal: 0, Role: "user", ContentLength: 3, Timestamp: "2026-04-26T10:00:03Z"},
			{MessageID: 2, Ordinal: 1, Role: "assistant", HasToolUse: true, ContentLength: 3, Timestamp: "2026-04-26T10:00:03Z"},
		},
		[]CallRow{{
			MessageID: 2, Category: "Bash",
			ExecutionStart: "2026-04-26T10:00:01Z", ExecutionEnd: "2026-04-26T10:00:02Z",
		}},
		time.Date(2026, 4, 26, 10, 0, 6, 0, time.UTC),
	)
	require.Len(t, got.Turns, 1)
	require.NotNil(t, got.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(1000), *got.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(1000), got.ToolDurationMs)
	assert.Equal(t, ActivityTotals{UnattributedMs: 3000}, got.ActivityTotals)
	assert.Equal(t, CategoryTotal{Category: "Bash", DurationMs: 1000, CallCount: 1}, got.ByCategory[0])
	assert.Equal(t, int64(1000), *got.SlowestCall.DurationMs)
}

func TestActivityTiming_VisiblePrompts(t *testing.T) {
	for _, tc := range []struct {
		name, role, content, subtype, timestamp string
		system                                  bool
		length                                  int
	}{
		{name: "system", role: "user", content: "carrier", system: true, length: 3, timestamp: "2026-04-26T10:00:03Z"},
		{name: "system prefix", role: "user", content: "This session is being continued from another session.", length: 3, timestamp: "2026-04-26T10:00:03Z"},
		{name: "tool result", role: "user", content: "carrier", subtype: "tool_result", length: 30, timestamp: "2026-04-26T10:00:03Z"},
		{name: "empty carrier", role: "user", content: "carrier", timestamp: "2026-04-26T10:00:03Z"},
		{name: "assistant", role: "assistant", content: "carrier", length: 30, timestamp: "2026-04-26T10:00:03Z"},
		{name: "missing timestamp", role: "user", content: "carrier", length: 3},
		{name: "malformed timestamp", role: "user", content: "carrier", length: 3, timestamp: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			timingInsertSession(t, d, "carriers", "2026-04-26T10:00:00Z", "2026-04-26T10:00:06Z")
			timingInsertMessage(t, d, "carriers", 0, "user", "run", "2026-04-26T10:00:00Z", false)
			content := tc.content
			if content == "" {
				content = "carrier"
			}
			timingInsertMessage(t, d, "carriers", 1, tc.role, content, tc.timestamp, false)
			_, err := d.getWriter().Exec(t.Context(), `UPDATE messages SET is_system = ?, source_subtype = ?, content_length = ? WHERE session_id = 'carriers' AND ordinal = 1`, tc.system, tc.subtype, tc.length)
			require.NoError(t, err)
			got, err := d.GetSessionTiming(t.Context(), "carriers")
			require.NoError(t, err)
			require.Len(t, got.Activity, 1)
			assert.Equal(t, 0, got.Activity[0].Ordinal)
			assert.Equal(t, int64(6000), got.Activity[0].UnattributedMs)
			assert.Zero(t, got.ToolDurationMs)
		})
	}
}
