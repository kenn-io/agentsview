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
		{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R github.com/example/repo"}`},
	}}}
	links := toolCallPRLinks(t, messages, "", "")
	require.Len(t, links, 1)
	assert.Equal(t, 42, links[0].Number)
}

func TestToolCallPRLinksOversizedRepositoryChange(t *testing.T) {
	input := `{"command":"git remote set-url origin https://github.com/example/other.git","unrelated":"` + strings.Repeat("x", 256*1024) + `"}`
	messages := []db.Message{{ToolCalls: []db.ToolCall{
		{ToolName: "Bash", Category: "Bash", InputJSON: input},
		{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42"}`},
	}}}
	assert.Empty(t, toolCallPRLinks(t, messages, "/work/repo", "https://github.com/example/repo.git"))
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
	messages := []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "exec", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`}}}}
	assert.Len(t, toolCallPRLinks(t, messages, "", "https://github.com/example/repo.git"), 1)
}

func TestToolCallPRLinksWorkingDirectory(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  int
	}{
		{`{"cmd":"gh pr view 42","workdir":"/work/other"}`, 0},
		{`{"cmd":"gh pr view 42","workdir":"/work/./repo"}`, 1},
	} {
		t.Run(tc.input, func(t *testing.T) {
			links := toolCallPRLinks(t, []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "exec_command", InputJSON: tc.input}}}}, "/work/repo", "https://github.com/example/repo.git")
			assert.Len(t, links, tc.want)
		})
	}
}

func TestToolCallPRLinksDirectoryChangePreservesHost(t *testing.T) {
	for _, calls := range [][]db.ToolCall{
		{{Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"cd /work/other"}`}, {Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`}},
		{{Category: "Bash", ToolName: "Bash", InputJSON: `{"commands":["cd /work/other","gh pr view 42 -R example/repo"]}`}},
	} {
		links := toolCallPRLinks(t, []db.Message{{ToolCalls: calls}}, "/work/repo", "https://git.example/example/repo.git")
		require.Len(t, links, 1)
		assert.Equal(t, "https://git.example/example/repo/pull/42", links[0].URL)
	}
}

func TestToolCallPRLinksSharedShellDirectoryChange(t *testing.T) {
	for _, command := range []string{`cd /work/other`} {
		messages := []db.Message{{ToolCalls: []db.ToolCall{
			{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + command + `"}`},
			{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr merge 60"}`},
		}}}
		assert.Empty(t, toolCallPRLinks(t, messages, "/work/repo", "https://github.com/example/repo.git"))
	}
	for _, command := range []string{`git push`} {
		messages := []db.Message{{ToolCalls: []db.ToolCall{
			{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + command + `"}`},
			{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr merge 60"}`},
		}}}
		assert.Len(t, toolCallPRLinks(t, messages, "/work/repo", "https://github.com/example/repo.git"), 1, command)
	}
	for _, command := range []string{`if true; then export GH_REPO=example/other; fi`} {
		messages := []db.Message{{ToolCalls: []db.ToolCall{
			{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + command + `"}`},
			{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr merge 60"}`},
		}}}
		assert.Empty(t, toolCallPRLinks(t, messages, "/work/repo", "https://github.com/example/repo.git"), command)
	}
}

func TestToolCallPRLinksPlainInput(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "shell_command", InputJSON: `gh pr view 42 -R github.com/example/repo`}}}}
	assert.Len(t, toolCallPRLinks(t, messages, "", ""), 1)
}

func TestToolCallPRLinksRepositoryChangesAcrossCommands(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "run_command", InputJSON: `{"commands":["git remote set-url origin https://github.com/example/other.git","gh pr view 42"]}`}}}}
	assert.Empty(t, toolCallPRLinks(t, messages, "/work/repo", "https://github.com/example/repo.git"))
}
