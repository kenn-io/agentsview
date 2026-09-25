package friction

import "time"

// Test helpers mirror jilog detectors.rs:644-666 (assistant, user, tool)
// and :697-712 (tool_result_user). Ordinals are assigned by position.
func assistant(text string) Message { return Message{Role: "assistant", Text: text} }

func user(text string) Message { return Message{Role: "user", Text: text} }

func tool(name, text string) Message {
	return Message{Role: "tool", ToolName: name, Text: text}
}

func failedTool(name, text string) Message {
	m := tool(name, text)
	m.Failed = true
	return m
}

// toolResultUser is a user turn that carried a tool_result block. The
// adapter drops these rows; the flag keeps jilog's guard testable.
func toolResultUser(content string) Message {
	return Message{Role: "user", Text: content, HadToolResult: true}
}

func numbered(msgs ...Message) []Message {
	for i := range msgs {
		msgs[i].Ordinal = i
	}
	return msgs
}

func contexts(sigs []Signal) []string {
	out := make([]string, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, s.Text)
	}
	return out
}

func labels(sigs []Signal) []string {
	out := make([]string, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, s.Label)
	}
	return out
}

// fixtureBase is jilog health.rs tests' 2026-01-01 09:00 UTC.
var fixtureBase = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
