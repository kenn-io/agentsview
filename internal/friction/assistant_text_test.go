package friction

import (
	"fmt"
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
			"transcript-only rendering found by its stored path",
			"hello\n" + readR, "",
			[]RawToolCall{{ToolName: "Read", Category: "Read", FilePath: "/home/user/app/a.go"}},
			true,
			"hello",
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
			if agent != "" && len(tt.calls) == 0 {
				continue
			}
			t.Run(agent+"/"+tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want,
					AssistantText(agent, tt.content, tt.thinking, tt.calls, tt.redacted))
			})
		}
	}
}

func TestAssistantTextCodexToolMessagesHaveNoProse(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		call          RawToolCall
	}{
		{"rebuilt", "[Bash]\n$ echo for now", RawToolCall{ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"echo for now"}`}},
		{"summary header", "[Bash: Check TODO]\n$ rg TODO", RawToolCall{ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"rg TODO"}`}},
		{"summary body", "[Tool: update_plan]\nFix the TODO for now", RawToolCall{ToolName: "update_plan", Category: "Other", InputJSON: `{"plan":[]}`}},
		{"raw patch", "[Edit: TODO.md]", RawToolCall{ToolName: "apply_patch", Category: "Edit", InputJSON: "*** Begin Patch\n*** Add File: TODO.md\n+x\n*** End Patch\n", FilePath: "TODO.md"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, redacted := range []bool{false, true} {
				assert.Empty(t, AssistantText(string(parser.AgentCodex), tt.content, "", []RawToolCall{tt.call}, redacted),
					"Codex emits each tool call as its own message holding only the rendering")
			}
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

// Removing one rendering can join text that belongs to another rendering.
// Calls need not occur in the same order as their visible text.
func TestAssistantTextMultipleRenderings(t *testing.T) {
	read := RawToolCall{ToolName: "Read", InputJSON: `{"file_path":"a.go"}`}
	write := RawToolCall{ToolName: "Write", InputJSON: `{"file_path":"b.go","content":"x"}`}
	bash := RawToolCall{ToolName: "Bash", InputJSON: `{"command":"echo for now"}`}
	for _, tt := range []struct {
		name, content, want string
		calls               []RawToolCall
		redacted            bool
	}{
		{"reverse order", "Before.\n[Write: b.go]\nBetween.\n[Read: a.go]\nAfter.", "Before.\nBetween.\nAfter.", []RawToolCall{read, write}, false},
		{"joined rendering", "Before.\n[Re[Write: b.go]ad: a.go]\nAfter.", "Before.\nAfter.", []RawToolCall{write, read}, false},
		{"newline across join", "Before.\n\n[Read: a.go][Write: b.go]\nAfter.", "Before.\nAfter.", []RawToolCall{read, write}, false},
		{"duplicate limit", "[Read: a.go]\n[Read: a.go]\n[Read: a.go]", "[Read: a.go]", []RawToolCall{read, read}, false},
		{"redacted selection keeps first occurrence", "[Bash]\n$ echo for now\n[Bash]", "$ echo for now\n[Bash]", []RawToolCall{bash}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, AssistantText("claude", tt.content, "", tt.calls, tt.redacted))
		})
	}
}

// Keep total text fixed while increasing calls. Copying the remaining message
// for each removed rendering makes allocated bytes grow with the call count.
func BenchmarkAssistantTextManyCalls(b *testing.B) {
	for _, shape := range []string{"tool arguments", "leading prose"} {
		for _, count := range []int{16, 128} {
			b.Run(fmt.Sprintf("%s/4MiB/%dcalls", shape, count), func(b *testing.B) {
				var content strings.Builder
				prose := "Before."
				if shape == "leading prose" {
					prose = strings.Repeat("x", 4<<20)
				}
				content.WriteString(prose + "\n")
				calls := make([]RawToolCall, count)
				for i := range calls {
					command := "echo done"
					if shape == "tool arguments" {
						command = strings.Repeat("x", (4<<20)/count)
					}
					calls[i] = RawToolCall{ToolName: "Bash", InputJSON: `{"command":"` + command + `"}`}
					content.WriteString("[Bash]\n$ " + command + "\n")
				}
				content.WriteString("After.")
				text, want := content.String(), prose+"\nAfter."
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					require.Equal(b, want, AssistantText("claude", text, "", calls, false))
				}
			})
		}
	}
}

func TestAssistantTextRebuildsTranscriptRenderingFromPath(t *testing.T) {
	rendering := parser.ToolUseRendering("create_file", `{"path":"TODO.md","content":"x"}`)
	require.Equal(t, "[Write: TODO.md]", rendering)
	call := RawToolCall{ToolName: "create_file", Category: "Write", FilePath: "TODO.md"}
	assert.Equal(t, "Done.",
		AssistantText(string(parser.AgentAmp), "Done.\n"+rendering, "", []RawToolCall{call}, true),
		"a path-only call still removes a rendering whose renderer reads another key")
}

func TestAssistantTextRemovesHeaderWhoseArgumentWasDropped(t *testing.T) {
	call := RawToolCall{ToolName: "list_directory", Category: "Read"}
	assert.Equal(t, "Looked around.",
		AssistantText(string(parser.AgentGemini), "Looked around.\n[List: TODO]", "", []RawToolCall{call}, true),
		"transcript-only archives drop dir_path, but the header is still a tool rendering")
}
