package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
