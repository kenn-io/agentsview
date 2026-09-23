package friction

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/signals"
)

var inputTestStart = time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)

func inputAt(min int) time.Time { return inputTestStart.Add(time.Duration(min) * time.Minute) }

func TestBuildSessionInput(t *testing.T) {
	bashFail := RawToolCall{MessageOrdinal: 1, CallIndex: 0, ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"make"}`, EventStatus: "errored", LastEventContent: "boom"}
	readOK := RawToolCall{MessageOrdinal: 1, CallIndex: 1, ToolName: "Read", Category: "Read", InputJSON: `{"file_path":"a.go"}`, EventStatus: "completed", ResultContent: "package a"}

	t.Run("filtered rows and sorted calls", func(t *testing.T) {
		msgs := []RawMessage{
			{Ordinal: 5, Role: "user", Content: "correct it", Timestamp: inputAt(5)},
			{Ordinal: 0, Role: "user", Content: "build it", Timestamp: inputAt(0)},
			{Ordinal: 1, Role: "assistant", Content: "Building.", Timestamp: inputAt(1)},
			{Ordinal: 3, Role: "user", Content: "continuation", IsSystem: true, Timestamp: inputAt(3)},
			{Ordinal: 4, Role: "assistant", IsCompactBoundary: true, IsSystem: true, Timestamp: inputAt(4)},
			{Ordinal: 6, Role: "user", Content: "result", SourceSubtype: "tool_result", Timestamp: inputAt(6)},
			{Ordinal: 7, Role: "assistant", Content: "Fixed.", Timestamp: inputAt(7)},
			{Ordinal: 8, Role: "user", Content: "[Request interrupted by user]", IsSystem: true, SourceSubtype: "interrupted", Timestamp: inputAt(8)},
		}
		calls := []RawToolCall{readOK, bashFail}
		in := BuildSessionInput("s1", Dims{Agent: "claude"}, false, msgs, calls, BuildOptions{})
		assert.Equal(t, []Message{
			{Ordinal: 0, Role: "user", Text: "build it", Timestamp: inputAt(0)},
			{Ordinal: 1, Role: "assistant", Text: "Building.", Timestamp: inputAt(1)},
			{Ordinal: 1, Role: "tool", ToolName: "Bash", NoiseName: "bash", Text: `{"error":"boom","success":false}`, Timestamp: inputAt(1)},
			{Ordinal: 1, Role: "tool", ToolName: "Read", NoiseName: "read", Text: `{"error":"package a","success":true}`, CallIndex: 1, Timestamp: inputAt(1)},
			{Ordinal: 5, Role: "user", Text: "correct it", Timestamp: inputAt(5)},
			{Ordinal: 7, Role: "assistant", Text: "Fixed.", Timestamp: inputAt(7)},
		}, in.Messages)
		assert.Equal(t, "s1", in.SubjectID)
		assert.Equal(t, Dims{Agent: "claude"}, in.Dims)
		assert.Equal(t, []int{0, 5}, in.Patterns.UserOrdinals)
		assert.Equal(t, []Message{{Ordinal: 8, Role: "user", Text: "[Request interrupted by user]", Timestamp: inputAt(8)}}, in.Interruptions)
		assert.Equal(t, []int{4}, in.Patterns.CompactBoundaries)
		assert.Equal(t, []time.Time{inputAt(4)}, in.Patterns.BoundaryTimes)
		assert.Equal(t, []signals.ToolCallRow{bashFail.row(), readOK.row()}, in.Patterns.Calls)
		assert.Equal(t, []time.Time{inputAt(1), inputAt(1)}, in.Patterns.CallTimes)
		assert.Equal(t, 5, msgs[0].Ordinal)
		assert.Equal(t, "Read", calls[0].ToolName)
	})

	t.Run("call timestamps and dropped owners", func(t *testing.T) {
		withTime := bashFail
		withTime.Timestamp = inputAt(2)
		orphan := RawToolCall{MessageOrdinal: 2, ToolName: "Bash", Category: "Bash", EventStatus: "errored", ResultContent: "fatal: bad"}
		systemOwned := RawToolCall{MessageOrdinal: 3, ToolName: "Bash", Category: "Bash", EventStatus: "cancelled"}
		in := BuildSessionInput("s1", Dims{}, false, []RawMessage{
			{Ordinal: 1, Role: "assistant", Content: "a", Timestamp: inputAt(1)},
			{Ordinal: 3, Role: "assistant", IsSystem: true, Timestamp: inputAt(3)},
			{Ordinal: 4, Role: "user", Content: "u", Timestamp: inputAt(4)},
		}, []RawToolCall{systemOwned, orphan, readOK, withTime}, BuildOptions{})
		assert.Equal(t, []int{1, 1, 1, 2, 3, 4}, messageOrdinals(in.Messages))
		assert.Equal(t, []time.Time{inputAt(2), inputAt(1), time.Time{}, inputAt(3)}, in.Patterns.CallTimes)
		require.Len(t, in.Messages, 6)
		assert.Equal(t, inputAt(1), in.Messages[2].Timestamp)
		assert.Equal(t, "tool", in.Messages[3].Role)
		assert.True(t, in.Messages[3].Timestamp.IsZero())
		assert.Equal(t, inputAt(3), in.Messages[4].Timestamp)
	})

	t.Run("assistant text uses own calls", func(t *testing.T) {
		call := RawToolCall{MessageOrdinal: 1, ToolName: "TodoWrite", Category: "Other", InputJSON: `{"todos":[{"content":"x","status":"pending"}]}`}
		content := "[Thinking]\nleave it for now\n[/Thinking]\nDone.\n" + parser.ToolUseRendering(call.ToolName, call.InputJSON)
		in := BuildSessionInput("s1", Dims{}, false, []RawMessage{{Ordinal: 1, Role: "assistant", Content: content, ThinkingText: "leave it for now"}}, []RawToolCall{call}, BuildOptions{})
		require.Len(t, in.Messages, 2)
		assert.Equal(t, "Done.", in.Messages[0].Text)
	})

	t.Run("compaction and pressure", func(t *testing.T) {
		p := 0.95
		calls := []RawToolCall{
			{MessageOrdinal: 1, ToolName: "Read"}, {MessageOrdinal: 1, ToolName: "Edit"}, {MessageOrdinal: 1, ToolName: "Bash"},
			{MessageOrdinal: 3, ToolName: "Read"}, {MessageOrdinal: 3, ToolName: "Edit"},
		}
		in := BuildSessionInput("s1", Dims{}, false, []RawMessage{
			{Ordinal: 3, Role: "assistant", ContextTokens: 900, HasContextTokens: true, Timestamp: inputAt(3)},
			{Ordinal: 2, Role: "assistant", IsSystem: true, IsCompactBoundary: true, ContextTokens: 900, HasContextTokens: true, Timestamp: inputAt(2)},
			{Ordinal: 1, Role: "assistant", ContextTokens: 100, HasContextTokens: true, Timestamp: inputAt(1)},
			{Ordinal: 4, Role: "user", ContextTokens: 5000, HasContextTokens: true, Timestamp: inputAt(4)},
		}, calls, BuildOptions{PressureMax: &p})
		assert.Equal(t, 1, in.Patterns.MidTaskCompactions)
		assert.Equal(t, &p, in.Patterns.PressureMax)
		assert.Equal(t, inputAt(2), in.Patterns.PressureAt)
	})

	t.Run("empty and non-system interruption subtype", func(t *testing.T) {
		in := BuildSessionInput("s1", Dims{}, true, []RawMessage{{Ordinal: 1, Role: "user", Content: "keep", SourceSubtype: "interrupted"}}, nil, BuildOptions{})
		assert.Equal(t, []Message{{Ordinal: 1, Role: "user", Text: "keep"}}, in.Messages)
		assert.Empty(t, in.Interruptions)
		assert.True(t, in.IsSubAgent)
		assert.Nil(t, in.Patterns.PressureMax)
		assert.True(t, in.Patterns.PressureAt.IsZero())
	})

	t.Run("no rows and trailing calls", func(t *testing.T) {
		call := RawToolCall{MessageOrdinal: 9, ToolName: "Read", Category: "Read", EventStatus: "completed"}
		in := BuildSessionInput("s1", Dims{}, false, nil, []RawToolCall{call}, BuildOptions{})
		assert.Equal(t, []Message{{Ordinal: 9, Role: "tool", ToolName: "Read", NoiseName: "read", Text: `{"error":null,"success":true}`}}, in.Messages)
		assert.Equal(t, []time.Time{{}}, in.Patterns.CallTimes)
		assert.Empty(t, in.Patterns.UserOrdinals)
	})
}

func messageOrdinals(msgs []Message) []int {
	ordinals := make([]int, 0, len(msgs))
	for _, m := range msgs {
		ordinals = append(ordinals, m.Ordinal)
	}
	return ordinals
}

func TestToolEnvelope(t *testing.T) {
	tests := []struct {
		name string
		call RawToolCall
		want string
	}{
		{
			"errored status uses last event content",
			RawToolCall{ToolName: "Bash", Category: "Bash", EventStatus: "errored",
				ResultContent: "summary", LastEventContent: "boom"},
			`{"error":"boom","success":false}`,
		},
		{
			"cancelled status is a failure",
			RawToolCall{ToolName: "Bash", Category: "Bash", EventStatus: "cancelled",
				ResultContent: "interrupted"},
			`{"error":"interrupted","success":false}`,
		},
		{
			"event with empty content falls back to result content",
			RawToolCall{ToolName: "Bash", Category: "Bash", EventStatus: "errored",
				ResultContent: "summary"},
			`{"error":"summary","success":false}`,
		},
		{
			"event content ignored without an event status",
			RawToolCall{ToolName: "Bash", Category: "Bash",
				ResultContent: "bash: x: command not found", LastEventContent: "stale"},
			`{"error":"bash: x: command not found","success":false}`,
		},
		{
			"empty content is null",
			RawToolCall{ToolName: "Read", Category: "Read", EventStatus: "completed"},
			`{"error":null,"success":true}`,
		},
		{
			"content heuristic failure without status",
			RawToolCall{ToolName: "Bash", Category: "Bash",
				ResultContent: "bash: foo: command not found"},
			`{"error":"bash: foo: command not found","success":false}`,
		},
		{
			"precomputed content verdict wins over content scan",
			RawToolCall{ToolName: "Bash", Category: "Bash", ResultContent: "ok",
				ContentFailure: true, ContentFailureKnown: true},
			`{"error":"ok","success":false}`,
		},
		{
			"html and line separators are not escaped",
			RawToolCall{ToolName: "Bash", Category: "Bash", EventStatus: "errored",
				ResultContent: "a<b>&c\u2028d"},
			"{\"error\":\"a<b>&c\u2028d\",\"success\":false}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ToolEnvelope(tt.call))
		})
	}
}

func TestNoiseToolName(t *testing.T) {
	tests := []struct {
		name, tool, category, want string
	}{
		{"claude bash", "Bash", "Bash", "bash"},
		{"codex shell mapped by category", "exec_command", "Bash", "bash"},
		{"mode tool lowercased", "Mode", "Other", "mode"},
		{"other tool lowercased", "python_check", "Other", "python_check"},
		{"empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, NoiseToolName(tt.tool, tt.category))
		})
	}
}
