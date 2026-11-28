package parser

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchedRunID(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	const other = "abcdefab-1234-1234-1234-123456789abc"
	startup := "OpenAI Codex v0.1.0\n--------\nworkdir: /tmp\nsession id: " + id + "\n--------\nuser\nhello\n"
	tests := []struct {
		name, command, output, want string
		failed                      bool
	}{
		{"Codex text block output", "codex exec hello", `[{"type":"input_text","text":` + strconv.Quote(startup) + `}]`, "codex:" + id, false},
		{"codex output", "codex exec hello", strings.ReplaceAll(startup, "\n", "\r\n"), "codex:" + id, false},
		{"option value is not a flag", "claude -p --model --session-id " + id, "", "", false},
		{"assistant text is not a startup ID", "codex exec hello", "codex\nsession id: " + id, "", false},
		{"truncated startup", "codex exec hello", "OpenAI Codex v0.1.0\n--------\nsession id: " + id + "\n", "", false},
		{"startup ID before child output", "codex exec hello", startup + "codex\nsession id: " + other, "codex:" + id, false},
		{"conflicting startup banners", "for i in 1 2; do codex exec hello; done", startup + strings.ReplaceAll(startup, id, other), "", false},
		{"conflicting thread events", "for i in 1 2; do codex exec --json hello; done", `{"type":"thread.started","thread_id":"` + id + `"}` + "\n" + `{"type":"thread.started","thread_id":"` + other + `"}`, "", false},
		{"repeated same thread event", "codex exec --json hello", `{"type":"thread.started","thread_id":"` + id + `"}` + "\n" + `{"type":"thread.started","thread_id":"` + id + `"}`, "codex:" + id, false},
		{"setup before launch", "cd project && codex exec hello", startup, "codex:" + id, false},
		{"unrelated setup output", "cat old.log && codex exec hello", startup, "", false},
		{"claude argument", "claude -p --session-id " + id, "", id, false},
		{"quoted command substitution prompt", `claude -p "$(cat review.txt)" --session-id ` + id, "", "", false},
		{"trailing command substitution prompt", `claude -p --session-id ` + id + ` "$(cat p)"`, "", id, false},
		{"quoted command substitution option", `claude "$(cat flags)" -p --output-format json`, `{"type":"result","session_id":"` + id + `"}`, "", false},
		{"quoted substitution after option value delimiter", `claude --model -- "$(cat flags)" -p --output-format json`, `{"type":"result","session_id":"` + id + `"}`, "", false},
		{"quoted substitution as option value", `claude -p --model "$(cat model)" --session-id ` + id, "", id, false},
		{"shell JSON prompt mentions tool", `{"command":"claude -p --session-id ` + id + ` 'see tools.exec_command'"}`, "", id, false},
		{"wrapped", "env X=1 bash -lc 'codex.exe exec --json hello'", `{"type":"thread.started","thread_id":"` + id + `"}`, "codex:" + id, false},
		{"json in human output", "codex exec hello", `{"type":"thread.started","thread_id":"` + id + `"}`, "", false},
		{"json flag as model", "codex exec --model --json hello", `{"type":"thread.started","thread_id":"` + id + `"}`, "", false},
		{"powershell", `& 'C:\Program Files\claude.exe' -p --session-id ` + id, "", id, false},
		{"resume", "codex exec resume " + id, startup, "", false},
		{"resume prompt", "claude -p resume the review --session-id " + id, "", id, false},
		{"resume short", "claude -p --output-format json -r " + id, `{"type":"result","session_id":"` + id + `"}`, "", false},
		{"continue short", "claude -c -p hello --session-id " + id, "", "", false},
		{"resume from PR", "claude -p --output-format json --from-pr 12", `{"type":"result","session_id":"` + id + `"}`, "", false},
		{"resume teleport", "claude -p --output-format json --teleport 12", `{"type":"result","session_id":"` + id + `"}`, "", false},
		{"conflicting assistant JSON", "claude -p --output-format json --session-id " + id, `{"type":"result","session_id":"` + other + `"}`, "", false},
		{"plain Claude output", "claude -p hello", `{"type":"result","session_id":"` + id + `"}`, "", false},
		{"Claude JSON array", "claude -p --output-format json hello", `[{"type":"result","session_id":"` + id + `"}]`, id, false},
		{"conflicting Claude JSON array", "claude -p --output-format json hello", `[{"type":"system","session_id":"` + id + `"},{"type":"result","session_id":"` + other + `"}]`, "", false},
		{"wrapped Codex banner", "codex exec hello", "Chunk ID: abc\nWall time: 1 seconds\nProcess exited with code 0\nOutput:\n" + startup, "codex:" + id, false},
		{"wrapped Codex JSON", "codex exec --json hello", "Wall time: 1 seconds\nExit code: 0\nOutput:\n" + `{"type":"thread.started","thread_id":"` + id + `"}`, "codex:" + id, false},
		{"code mode", `const r = await tools.exec_command({cmd: "codex exec hello"}); text(r.output);`, "Script completed\nWall time 1 seconds\nOutput:\n" + startup, "codex:" + id, false},
		{"code mode session ID", `const r = await tools.exec_command({cmd: "claude -p --session-id ` + id + `"}); text(r.output);`, "", id, false},
		{"code mode unrelated output", `const r = await tools.exec_command({cmd: "codex exec hello"}); text("unrelated");`, "Script completed\nWall time 1 seconds\nOutput:\n" + startup, "", false},
		{"code mode uncalled function", `function f() { return tools.exec_command({cmd: "claude -p --session-id ` + id + `"}); } text("done");`, "", "", false},
		{"code mode another call", `const r = await tools.exec_command({cmd: "codex exec hello"}); text(r.output); text("other");`, "Script completed\nWall time 1 seconds\nOutput:\n" + startup, "", false},
		{"function definition", "f() { claude -p --session-id " + id + "; }", "", "", false},
		{"dynamic prompt", `claude -p --session-id ` + id + ` -- "$prompt"`, "", id, false},
		{"dynamic option", `claude -p "$option" --session-id ` + id, "", "", false},
		{"duplicate flags", "claude -p --session-id " + id + " --session-id " + other, "", "", false},
		{"unrelated json", "claude -p --output-format json hello", `{"type":"message","session_id":"` + id + `"}`, "", false},
		{"quoted text", `echo 'codex exec hello'`, startup, "", false},
		{"two launches", "claude -p --session-id " + id + "; claude -p --session-id " + other, "", "", false},
		{"failed no output ID", "claude -p --session-id " + id, "failed", "", true},
		{"failed Codex startup", "codex exec hello", startup, "codex:" + id, true},
		{"failed Claude system event", "claude -p --output-format json --session-id " + id, `{"type":"system","session_id":"` + id + `"}`, id, true},
		{"failed plain output matching Claude ID", "claude -p --session-id " + id, `{"type":"result","session_id":"` + id + `"}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, launchedRunID(t.Context(), tt.command, tt.output, tt.failed))
		})
	}
}

func TestAnnotateClaudeLaunchedRunFailedResult(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	output := `{"type":"system","session_id":"` + id + `"}`
	messages := []ParsedMessage{
		{ToolCalls: []ParsedToolCall{{Category: "Bash", ToolUseID: "call", InputJSON: `{"command":"claude -p --output-format json --session-id ` + id + `"}`}}},
		{ToolResults: []ParsedToolResult{{ToolUseID: "call", Status: "errored", ContentRaw: strconv.Quote(output)}}},
	}
	require.NoError(t, annotateClaudeLaunchedRuns(t.Context(), messages, false))
	assert.Equal(t, id, messages[0].ToolCalls[0].SubagentSessionID)
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ShellInputCommands(tc.input)
			assert.True(t, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}
