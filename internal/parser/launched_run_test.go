package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLaunchedRunID(t *testing.T) {
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
		{"setup before launch", "cd project && codex exec hello", startup, "codex:" + id},
		{"unrelated setup output", "cat old.log && codex exec hello", startup, ""},
		{"claude argument", "claude -p --session-id " + id, "", id},
		{"wrapped", "env X=1 bash -lc 'codex.exe exec --json hello'", `{"type":"thread.started","thread_id":"` + id + `"}`, "codex:" + id},
		{"json in human output", "codex exec hello", `{"type":"thread.started","thread_id":"` + id + `"}`, ""},
		{"json flag as model", "codex exec --model --json hello", `{"type":"thread.started","thread_id":"` + id + `"}`, ""},
		{"powershell", `& 'C:\Program Files\claude.exe' -p --session-id ` + id, "", id},
		{"resume", "codex exec resume " + id, startup, ""},
		{"resume after option", "codex exec --model example resume " + id, startup, ""},
		{"resume after global option value", "codex -m exec exec resume " + id, startup, ""},
		{"option value resume", "codex exec --model resume review", startup, "codex:" + id},
		{"resume prompt", "claude -p resume the review --session-id " + id, "", id},
		{"resume short", "claude -p -r " + id, `{"type":"result","session_id":"` + id + `"}`, ""},
		{"continue short", "claude -c -p hello --session-id " + id, "", ""},
		{"conflicting assistant JSON", "claude -p --session-id " + id, `{"type":"result","session_id":"` + other + `"}`, ""},
		{"duplicate flags", "claude -p --session-id " + id + " --session-id " + other, "", ""},
		{"unrelated json", "claude -p hello", `{"type":"message","session_id":"` + id + `"}`, ""},
		{"quoted text", `echo 'codex exec hello'`, startup, ""},
		{"two launches", "claude -p --session-id " + id + "; claude -p --session-id " + other, "", ""},
		{"unrelated log", "codex exec hello >run.log 2>&1; cat old.log", startup, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, launchedRunID(t.Context(), tt.command, tt.output))
		})
	}
}
