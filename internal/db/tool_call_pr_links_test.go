package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestToolCallPRLinks(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{
		{Category: "Task", InputJSON: `{"prompt":"Review #99"}`},
		{Category: "Task", InputJSON: `{"message":"Review https://github.com/example/repo/pull/51"}`},
		{Category: "Task", InputJSON: `{"task":"Review https://github.com/example/repo/pull/50"}`},
		{Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/42", "description":"https://github.com/example/repo/pull/47"}`},
		{Category: "Task", InputJSON: `{"tool":"newTask","content":"Review https://github.com/example/repo/pull/48"}`},
		{Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/49","content":[{"type":"text","text":"other"}]}`},
		{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":["codex","exec","Review https://github.com/example/repo/pull/43"]}`},
		{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"curl https://github.com/example/repo/pull/44"}`},
	}}}
	links := toolCallPRLinks(t, messages, "", "")
	if assert.Len(t, links, 5) {
		assert.Equal(t, 51, links[0].Number)
		assert.Equal(t, 50, links[1].Number)
		assert.Equal(t, 42, links[2].Number)
		assert.Equal(t, 48, links[3].Number)
		assert.Equal(t, 49, links[4].Number)
	}
}

func TestToolCallPRLinksContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := db.ToolCallPRLinks(ctx, []db.Message{{ToolCalls: []db.ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`}}}}, "", "")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestToolCallPRLinksSkipsOversizedInput(t *testing.T) {
	input := `{"command":"gh pr view 41 -R github.com/example/repo","unrelated":"` + strings.Repeat("x", 256*1024) + `"}`
	messages := []db.Message{{ToolCalls: []db.ToolCall{
		{ToolName: "Bash", Category: "Bash", InputJSON: input},
		{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"GH_HOST=github.com gh pr view 42 -R example/repo"}`},
	}}}
	links := toolCallPRLinks(t, messages, "", "")
	require.Len(t, links, 1)
	assert.Equal(t, 42, links[0].Number)
}

func toolCallPRLinks(t *testing.T, messages []db.Message, cwd, remote string) []db.PRLink {
	t.Helper()
	links, err := db.ToolCallPRLinks(t.Context(), messages, cwd, remote)
	require.NoError(t, err)
	return links
}

func TestToolCallPRLinksDirectFormats(t *testing.T) {
	for _, input := range []string{
		`{"commands":["gh pr view 42 -R example/repo"]}`,
		`{"command":["gh","pr","view","42","-R","example/repo"]}`,
		`{"cmd":"gh pr view 42 -R example/repo"}`,
	} {
		t.Run(input, func(t *testing.T) {
			links := toolCallPRLinks(t, []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "exec_command", InputJSON: input}}}}, "", "https://github.com/example/repo.git")
			require.Len(t, links, 1)
			assert.Equal(t, 42, links[0].Number)
		})
	}
}

func TestToolCallPRLinksWorkingDirectory(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  int
	}{
		{`{"cmd":"gh pr view 42","workdir":"/work/other"}`, 0},
		{`{"cmd":"gh pr view 42 -R example/repo","workdir":"/work/other"}`, 1},
		{`{"cmd":"GH_HOST=git.example gh pr view 42 -R example/repo","workdir":"/work/other"}`, 1},
		{`{"cmd":"gh pr view 42","workdir":"/work/./repo"}`, 1},
		{`{"cmd":"gh pr view 42 -R github.com/example/repo","workdir":"/work/other"}`, 1},
		{`{"cmd":"cd /work/repo && gh pr view 42"}`, 1},
		{`{"cmd":"cd . && gh pr view 42"}`, 1},
		{`{"cmd":"cd ../repo && gh pr view 42"}`, 1},
		{`{"cmd":"cd ../other && gh pr view 42"}`, 0},
		{`{"cmd":"cd /work/other && gh pr view 42"}`, 0},
		{`{"cmd":"gh pr view https://github.com/example/repo/pull/42","workdir":"/work/other"}`, 1},
	} {
		t.Run(tc.input, func(t *testing.T) {
			links := toolCallPRLinks(t, []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "exec_command", InputJSON: tc.input}}}}, "/work/repo", "https://github.com/example/repo.git")
			assert.Len(t, links, tc.want)
		})
	}
}

func TestToolCallPRLinksSharedShellDirectoryChange(t *testing.T) {
	for _, command := range []string{`cd /work/other`, `if true; then cd /work/other; fi`, `cd $REPO`, `cd -P /work/other`, `cd -- /work/other`, `pushd /work/other`} {
		messages := []db.Message{{ToolCalls: []db.ToolCall{
			{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + command + `"}`},
			{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr merge 60"}`},
		}}}
		assert.Empty(t, toolCallPRLinks(t, messages, "/work/repo", "https://github.com/example/repo.git"))
	}
	for _, command := range []string{`git push`, `git config --get remote.origin.url`, `git remote -v`, `echo $GH_REPO`, `gh repo set-default --view`, `change_repo() { export GH_REPO=example/other; }`} {
		messages := []db.Message{{ToolCalls: []db.ToolCall{
			{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + command + `"}`},
			{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr merge 60"}`},
		}}}
		assert.Len(t, toolCallPRLinks(t, messages, "/work/repo", "https://github.com/example/repo.git"), 1, command)
	}
	for _, command := range []string{`change_repo() { export GH_REPO=example/other; }; change_repo`, `if true; then export GH_REPO=example/other; fi`} {
		messages := []db.Message{{ToolCalls: []db.ToolCall{
			{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + command + `"}`},
			{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr merge 60"}`},
		}}}
		assert.Empty(t, toolCallPRLinks(t, messages, "/work/repo", "https://github.com/example/repo.git"), command)
	}
}

func TestToolCallPRLinksPlainInputAndRepositoryChanges(t *testing.T) {
	unknown := []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "custom_shell", InputJSON: `gh pr view 42 -R github.com/example/repo`}}}}
	assert.Len(t, toolCallPRLinks(t, unknown, "", ""), 1)
	for _, tool := range []string{"shell_command", "exec_command"} {
		t.Run(tool, func(t *testing.T) {
			call := func(input string) db.ToolCall { return db.ToolCall{Category: "Bash", ToolName: tool, InputJSON: input} }
			links := toolCallPRLinks(t, []db.Message{{ToolCalls: []db.ToolCall{call(`gh pr view 42 -R github.com/example/repo`)}}}, "", "")
			assert.Len(t, links, 1)
			messages := []db.Message{{ToolCalls: []db.ToolCall{call(`{"command":"git remote set-url origin https://github.com/example/other.git"}`), call(`{"command":"gh pr view 42"}`)}}}
			assert.Empty(t, toolCallPRLinks(t, messages, "/work/repo", "https://github.com/example/repo.git"))
		})
	}
}

func TestToolCallPRLinksExecInputs(t *testing.T) {
	for _, input := range []string{`tools.exec_command({cmd:"gh pr view 42 -R example/repo"})`, `{"code":"tools.exec_command({cmd:'gh pr view 42 -R example/repo'})"}`} {
		messages := []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "exec", InputJSON: input}}}}
		assert.Empty(t, toolCallPRLinks(t, messages, "", ""))
	}
	for _, input := range []string{`{"command":"gh pr view 42 -R example/repo"}`, `{"cmd":"gh pr view 42 -R example/repo"}`} {
		messages := []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "exec", InputJSON: input}}}}
		assert.Len(t, toolCallPRLinks(t, messages, "", ""), 1)
	}
}
