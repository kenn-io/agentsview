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
		{"assistant text is not a startup ID", "codex exec hello", "codex\nsession id: " + id, ""},
		{"truncated startup", "codex exec hello", "OpenAI Codex v0.1.0\n--------\nsession id: " + id + "\n", ""},
		{"startup ID before child output", "codex exec hello", startup + "codex\nsession id: " + other, "codex:" + id},
		{"capture stderr", "codex exec hello 2>&1", startup, "codex:" + id},
		{"setup before launch", "cd project && codex exec hello", startup, "codex:" + id},
		{"unrelated setup output", "cat old.log && codex exec hello", startup, ""},
		{"claude argument", "claude -p --session-id " + id, "", id},
		{"shell JSON prompt mentions tool", `{"command":"claude -p --session-id ` + id + ` 'see tools.exec_command'"}`, "", id},
		{"wrapped", "env X=1 bash -lc 'codex.exe exec --json hello'", `{"type":"thread.started","thread_id":"` + id + `"}`, "codex:" + id},
		{"json in human output", "codex exec hello", `{"type":"thread.started","thread_id":"` + id + `"}`, ""},
		{"json flag as model", "codex exec --model --json hello", `{"type":"thread.started","thread_id":"` + id + `"}`, ""},
		{"powershell", `& 'C:\Program Files\claude.exe' -p --session-id ` + id, "", id},
		{"resume", "codex exec resume " + id, startup, ""},
		{"resume prompt", "claude -p resume the review --session-id " + id, "", id},
		{"resume short", "claude -p --output-format json -r " + id, `{"type":"result","session_id":"` + id + `"}`, ""},
		{"continue short", "claude -c -p hello --session-id " + id, "", ""},
		{"resume from PR", "claude -p --output-format json --from-pr 12", `{"type":"result","session_id":"` + id + `"}`, ""},
		{"resume teleport", "claude -p --output-format json --teleport 12", `{"type":"result","session_id":"` + id + `"}`, ""},
		{"conflicting assistant JSON", "claude -p --output-format json --session-id " + id, `{"type":"result","session_id":"` + other + `"}`, ""},
		{"plain Claude output", "claude -p hello", `{"type":"result","session_id":"` + id + `"}`, ""},
		{"Claude JSON array", "claude -p --output-format json hello", `[{"type":"result","session_id":"` + id + `"}]`, id},
		{"conflicting Claude JSON array", "claude -p --output-format json hello", `[{"type":"system","session_id":"` + id + `"},{"type":"result","session_id":"` + other + `"}]`, ""},
		{"wrapped Codex banner", "codex exec hello", "Chunk ID: abc\nWall time: 1 seconds\nProcess exited with code 0\nOutput:\n" + startup, "codex:" + id},
		{"wrapped Codex JSON", "codex exec --json hello", "Wall time: 1 seconds\nExit code: 0\nOutput:\n" + `{"type":"thread.started","thread_id":"` + id + `"}`, "codex:" + id},
		{"code mode", `const r = await tools.exec_command({cmd: "codex exec hello"}); text(r.output);`, "Script completed\nWall time 1 seconds\nOutput:\n" + startup, "codex:" + id},
		{"code mode session ID", `const r = await tools.exec_command({cmd: "claude -p --session-id ` + id + `"}); text(r.output);`, "", id},
		{"code mode unrelated output", `const r = await tools.exec_command({cmd: "codex exec hello"}); text("unrelated");`, "Script completed\nWall time 1 seconds\nOutput:\n" + startup, ""},
		{"code mode uncalled function", `function f() { return tools.exec_command({cmd: "claude -p --session-id ` + id + `"}); } text("done");`, "", ""},
		{"code mode another call", `const r = await tools.exec_command({cmd: "codex exec hello"}); text(r.output); text("other");`, "Script completed\nWall time 1 seconds\nOutput:\n" + startup, ""},
		{"function definition", "f() { claude -p --session-id " + id + "; }", "", ""},
		{"dynamic prompt", `claude -p --session-id ` + id + ` -- "$prompt"`, "", id},
		{"dynamic option", `claude -p "$option" --session-id ` + id, "", ""},
		{"duplicate flags", "claude -p --session-id " + id + " --session-id " + other, "", ""},
		{"unrelated json", "claude -p --output-format json hello", `{"type":"message","session_id":"` + id + `"}`, ""},
		{"quoted text", `echo 'codex exec hello'`, startup, ""},
		{"two launches", "claude -p --session-id " + id + "; claude -p --session-id " + other, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, launchedRunID(t.Context(), tt.command, tt.output))
		})
	}
}

func TestHasLaunchedOutputIDArray(t *testing.T) {
	assert.True(t, hasLaunchedOutputID(`[{"type":"input_text","text":"Script completed\nWall time 1 seconds\nOutput:\n"},{"type":"input_text","text":"OpenAI Codex v0.1.0\n--------\nsession id: 12345678-1234-1234-1234-123456789abc\n--------\n"}]`))
	assert.False(t, hasLaunchedOutputID(`{"type":"result","session_id":"invalid"}`))
}

func TestOneShotRunArgs(t *testing.T) {
	for _, tc := range []struct {
		name, agent string
		args        []string
		want        bool
	}{
		{"codex alias", "codex", []string{"codex", "e", "x"}, true},
		{"codex option before exec", "codex", []string{"codex", "-m", "model", "exec"}, true},
		{"claude continue before print", "claude", []string{"claude", "-c", "-p"}, true},
		{"codex directory named exec", "codex", []string{"codex", "-C", "exec"}, false},
		{"claude prompt flag value", "claude", []string{"claude", "--append-system-prompt", "-p"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, OneShotRunArgs(tc.agent, tc.args)) })
	}
}

func TestShellInputCommands(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        []string
	}{
		{"argv", `{"command":["gh","pr","view","43","-R","example/repo"]}`, []string{"gh pr view 43 -R example/repo"}},
		{"commands", `{"commands":["gh pr view 42 -R example/repo","gh pr view 43 -R example/repo"]}`, []string{"gh pr view 42 -R example/repo", "gh pr view 43 -R example/repo"}},
		{"CommandLine", `{"CommandLine":"gh pr view 42 -R example/repo"}`, []string{"gh pr view 42 -R example/repo"}},
		{"plain text", `gh pr view 42 -R example/repo`, []string{"gh pr view 42 -R example/repo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ShellInputCommands(tc.input)
			assert.True(t, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}
