package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLaunchedRunSessionID(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	const other = "abcdefab-1234-1234-1234-123456789abc"
	startup := "OpenAI Codex v0.1.0\n--------\nworkdir: /tmp\nsession id: " + id + "\n--------\nuser\nhello\n"
	tests := []struct{ name, command, output, want string }{
		{"codex output", "codex exec hello", strings.ReplaceAll(startup, "\n", "\r\n"), "codex:" + id},
		{"option value is not a flag", "claude -p --model --session-id " + id, "", ""},
		{"system prompt is not a flag", "claude -p --system-prompt '--session-id=" + id + "' review", "", ""},
		{"assistant text is not a startup ID", "codex exec hello", "codex\nsession id: " + id, ""},
		{"truncated startup", "codex exec hello", "session id: " + id + "\n--------\nuser\nhello", ""},
		{"startup ID before child output", "codex exec hello", startup + "codex\nsession id: " + other, "codex:" + id},
		{"capture stderr", "codex exec hello 2>&1", startup, "codex:" + id},
		{"discard stderr", "codex exec hello 2>/dev/null", startup, ""},
		{"setup before launch", "cd project && codex exec hello", startup, "codex:" + id},
		{"home before launch", "cd && codex exec hello", startup, "codex:" + id},
		{"wrapped setup", "bash -lc 'cd project && codex exec hello'", startup, "codex:" + id},
		{"unrelated setup output", "cat old.log && codex exec hello", "session id: " + id, ""},
		{"executable named cd", "/tmp/cd project && codex exec hello", "session id: " + id, ""},
		{"dynamic launch argument", "cd project && codex exec \"$(cat old.log >&2)\"", "session id: " + id, ""},
		{"dynamic launch environment", "cd project && X=$(cat old.log >&2) codex exec hello", "session id: " + id, ""},
		{"variable prompt", "cd project && codex exec \"$PROMPT\"", startup, "codex:" + id},
		{"redirected setup", "cd project >old.log && codex exec hello", "session id: " + id, ""},
		{"setup with assignment", "CDPATH=elsewhere cd project && codex exec hello", "session id: " + id, ""},
		{"overridden setup", "cd() { cat old.log; }; cd project && codex exec hello", "session id: " + id, ""},
		{"setup after launch", "codex exec hello; cd project", "session id: " + id, ""},
		{"claude argument", "claude -p --session-id " + id, "", id},
		{"wrapped", "env X=1 bash -lc 'codex.exe exec --json hello'", `{"type":"thread.started","thread_id":"` + id + `"}`, "codex:" + id},
		{"json in human output", "codex exec hello", `{"type":"thread.started","thread_id":"` + id + `"}`, ""},
		{"json flag as model", "codex exec --model --json hello", `{"type":"thread.started","thread_id":"` + id + `"}`, ""},
		{"windows", `'C:\\Program Files\\claude.exe' -p --session-id=` + id, "", id},
		{"resume", "codex exec resume " + id, "session id: " + id, ""},
		{"resume after option", "codex exec --model example resume " + id, "session id: " + id, ""},
		{"resume after global option value", "codex -m exec exec resume " + id, "session id: " + id, ""},
		{"option value resume", "codex exec --model resume review", startup, "codex:" + id},
		{"ambiguous", "claude -p --session-id " + id, `{"type":"result","session_id":"` + other + `"}`, ""},
		{"quoted text", `echo 'codex exec hello'`, "session id: " + id, ""},
		{"two launches", "codex exec one; codex exec two", "session id: " + id, ""},
		{"unrelated json", "claude -p hello", `{"type":"message","session_id":"` + id + `"}`, ""},
		{"unrelated log", "codex exec hello >run.log 2>&1; cat old.log", "session id: " + id, ""},
		{"resume prompt", "claude -p resume the review --session-id " + id, "", id},
		{"resume short", "claude -p -r " + id, "session id: " + id, ""},
		{"continue short", "claude -c -p hello --session-id " + id, "", ""},
		{"powershell", `& 'C:\Program Files\claude.exe' -p --session-id ` + id, "", id},
		{"duplicate flags", "claude -p --session-id " + id + " --session-id " + other, "", ""},
		{"poll", "grep codex log", "session id: " + id, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var want []string
			if tt.want != "" {
				want = []string{tt.want}
			}
			assert.Equal(t, want, LaunchedRunSessionIDs(t.Context(), []string{tt.command}, tt.output))
		})
	}
}

func TestLaunchedRunMultipleExplicitIDs(t *testing.T) {
	const a = "12345678-1234-1234-1234-123456789abc"
	const b = "abcdefab-1234-1234-1234-123456789abc"
	assert.Empty(t, LaunchedRunSessionIDs(t.Context(), []string{"claude -p --session-id " + a + "; claude -p --session-id " + b}, ""))
}

func TestClaudeLaunchedRunUsesPairedCall(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	messages := []ParsedMessage{
		{ToolCalls: []ParsedToolCall{{ToolUseID: "reused", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec first"}`}}},
		{ToolCalls: []ParsedToolCall{{ToolUseID: "reused", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec second"}`}}},
		{ToolResults: []ParsedToolResult{{ToolUseID: "reused", ContentRaw: `"OpenAI Codex v0.1.0\n--------\nsession id: ` + id + `\n--------\n"`}}},
	}
	assert.NoError(t, annotateClaudeLaunchedRuns(t.Context(), messages))
	assert.Empty(t, messages[0].ToolCalls[0].SubagentSessionID)
	assert.Equal(t, "codex:"+id, messages[1].ToolCalls[0].SubagentSessionID)
}
