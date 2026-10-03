package friction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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
	const quotedThinking = "for now\n[Bash]\n$ echo done\nmore thought"
	parserText, _, _, _, parserCalls, _ := parser.ExtractTextContent(
		t.Context(), gjson.Parse(`[
			{"type":"thinking","thinking":"for now\n[Bash]\n$ echo done\nmore thought"},
			{"type":"tool_use","name":"Bash","input":{"command":"echo done"}},
			{"type":"text","text":"Answer."}
		]`),
	)
	require.Len(t, parserCalls, 1)

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
			"hello\n" + readR + "\nworld", "",
			[]RawToolCall{read},
			false,
			"hello\nworld",
		},
		{
			"todo rendering removed",
			"Let me update the plan.\n" + todoR, "",
			[]RawToolCall{todo},
			false,
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
			"quoted tool rendering in parser thinking is removed",
			parserText, quotedThinking,
			[]RawToolCall{{
				ToolName:  parserCalls[0].ToolName,
				Category:  parserCalls[0].Category,
				InputJSON: parserCalls[0].InputJSON,
			}},
			false, "Answer.",
		},
		{
			"two identical calls both removed",
			"Running twice.\n" + bashR + "\n" + bashR + "\nBoth passed.",
			"",
			[]RawToolCall{bash, bash},
			false,
			"Running twice.\nBoth passed.",
		},
		{
			"redacted rendering removed",
			"Run it.\n" + bashRedacted, "",
			[]RawToolCall{bash},
			true,
			"Run it.",
		},
		{
			"full rendering fallback under redacting policy",
			"Run it.\n" + bashR, "",
			[]RawToolCall{bash},
			true,
			"Run it.",
		},
		{
			"redacted prefix does not leave full rendering arguments",
			parser.ToolUseRendering(
				"Bash", `{"command":"echo for now"}`,
			), "",
			[]RawToolCall{{
				ToolName: "Bash", Category: "Bash",
				InputJSON: `{"command":"echo for now"}`,
			}},
			true, "",
		},
		{
			"standalone redacted header is removed",
			parser.RedactedToolUseRendering(
				"Bash", `{"command":"echo for now"}`,
			), "",
			[]RawToolCall{{
				ToolName: "Bash", Category: "Bash",
				InputJSON: `{"command":"echo for now"}`,
			}},
			true, "",
		},
		{
			"redacted preferred when both forms occur",
			"Run it.\n" + bashRedacted + "\n" + bashR, "",
			[]RawToolCall{bash},
			true,
			"Run it.\n" + bashR,
		},
		{
			"full preferred when both forms occur",
			"Run it.\n" + bashRedacted + "\n" + bashR, "",
			[]RawToolCall{bash},
			false,
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
	for _, agent := range []string{"", "claude", "openclaude", "cowork"} {
		for _, tt := range tests {
			t.Run(agent+"/"+tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want,
					AssistantText(agent, tt.content, tt.thinking, tt.calls, tt.redacted))
			})
		}
	}
}

func TestAssistantTextCodex(t *testing.T) {
	call := RawToolCall{ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"echo for now"}`}
	for _, tt := range []struct {
		name, content, want string
		redacted            bool
	}{
		{"full", "Before.\n[Bash]\n$ echo for now\nAfter.", "Before.\nAfter.", false},
		{"redacted", "Before.\n[Bash]\nAfter.", "Before.\nAfter.", true},
		{"full under transcript policy", "Before.\n[Bash]\n$ echo for now\nAfter.", "Before.\nAfter.", true},
		{"redacted preferred", "[Bash]\n[Bash]\n$ echo for now", "[Bash]\n$ echo for now", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, AssistantText("codex", tt.content, "", []RawToolCall{call}, tt.redacted))
		})
	}
}

// Large Write arguments are not part of the short label stored in assistant
// text. Reconstructing every provider's rendering used to copy them repeatedly.
func BenchmarkAssistantTextToolArguments(b *testing.B) {
	for _, size := range []struct {
		name  string
		bytes int
	}{{"1KiB", 1 << 10}, {"64KiB", 64 << 10}} {
		b.Run(size.name, func(b *testing.B) {
			calls := []RawToolCall{{
				ToolName: "Write", Category: "Write",
				InputJSON: `{"file_path":"fixture.go","content":"` + strings.Repeat("x", size.bytes) + `"}`,
			}}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				text := AssistantText("claude", "Before.\n[Write: fixture.go]\nAfter.", "", calls, false)
				require.Equal(b, "Before.\nAfter.", text)
			}
		})
	}
}
