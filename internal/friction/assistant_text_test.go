package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/parser"
)

func TestAssistantText(t *testing.T) {
	todo := RawToolCall{
		ToolName: "TodoWrite", Category: "Other",
		InputJSON: `{"todos":[{"content":"Fix the hack for now","status":"pending"}]}`,
	}
	bash := RawToolCall{
		ToolName: "Bash", Category: "Bash",
		InputJSON: `{"command":"go test ./...","description":"Run tests"}`,
	}
	read := RawToolCall{ToolName: "Read", Category: "Read", InputJSON: `{"file_path":"/home/user/app/a.go"}`}
	todoR := parser.ToolUseRendering(todo.ToolName, todo.InputJSON)
	bashR := parser.ToolUseRendering(bash.ToolName, bash.InputJSON)
	bashRedacted := parser.RedactedToolUseRendering(bash.ToolName, bash.InputJSON)
	readR := parser.ToolUseRendering(read.ToolName, read.InputJSON)

	tests := []struct {
		name     string
		content  string
		thinking string
		calls    []RawToolCall
		redacted bool
		want     string
	}{
		{"plain text unchanged", "hello\nworld", "", nil, false, "hello\nworld"},
		{
			"tool rendering between text blocks",
			"hello\n" + readR + "\nworld", "", []RawToolCall{read}, false,
			"hello\nworld",
		},
		{
			"todo rendering removed",
			"Let me update the plan.\n" + todoR, "", []RawToolCall{todo}, false,
			"Let me update the plan.",
		},
		{
			"single thinking block removed",
			"[Thinking]\nI will do this for now\n[/Thinking]\nDone.",
			"I will do this for now", nil, false,
			"Done.",
		},
		{
			"multiple thinking blocks removed",
			"[Thinking]\nfirst for now\n[/Thinking]\nText one.\n" +
				"[Thinking]\nsecond: I'll come back to this\n[/Thinking]\nText two.",
			"first for now\n\nsecond: I'll come back to this", nil, false,
			"Text one.\nText two.",
		},
		{
			"thinking containing close marker removed whole",
			"[Thinking]\nquote:\n[/Thinking]\nstill thinking for now\n[/Thinking]\nAnswer.",
			"quote:\n[/Thinking]\nstill thinking for now", nil, false,
			"Answer.",
		},
		{
			"two identical calls both removed",
			"Running twice.\n" + bashR + "\n" + bashR + "\nBoth passed.",
			"", []RawToolCall{bash, bash}, false,
			"Running twice.\nBoth passed.",
		},
		{
			"redacted rendering removed",
			"Run it.\n" + bashRedacted, "", []RawToolCall{bash}, true,
			"Run it.",
		},
		{
			"full rendering fallback under redacting policy",
			"Run it.\n" + bashR, "", []RawToolCall{bash}, true,
			"Run it.",
		},
		{
			"redacted preferred when both forms occur",
			"Run it.\n" + bashRedacted + "\n" + bashR, "", []RawToolCall{bash}, true,
			"Run it.\n" + bashR,
		},
		{
			"full preferred when both forms occur",
			"Run it.\n" + bashRedacted + "\n" + bashR, "", []RawToolCall{bash}, false,
			"Run it.\n" + bashRedacted,
		},
		{
			"inline marker kept",
			"I wrote [Thinking] in the docs for now.", "", nil, false,
			"I wrote [Thinking] in the docs for now.",
		},
		{
			"unclosed thinking marker kept",
			"[Thinking]\nfor now\nAnswer.", "for now", nil, false,
			"[Thinking]\nfor now\nAnswer.",
		},
		{
			"thinking block with unmatched body kept",
			"Before.\n[Thinking]\nprivate draft\n[/Thinking]\nAfter.",
			"different thought", nil, false,
			"Before.\n[Thinking]\nprivate draft\n[/Thinking]\nAfter.",
		},
		{"only rendering leaves empty text", readR, "", []RawToolCall{read}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				AssistantText(tt.content, tt.thinking, tt.calls, tt.redacted))
		})
	}
}
