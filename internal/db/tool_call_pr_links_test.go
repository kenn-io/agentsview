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
		{ToolName: "spawn_agents", Category: "Task", InputJSON: `{"agents":[{"prompt":"Review https://github.com/example/repo/pull/52"},{"prompt":"Review https://github.com/example/repo/pull/53 and https://github.com/example/repo/pull/52","description":"https://github.com/example/repo/pull/44"}]}`},
		{ToolName: "Task", Category: "Task", InputJSON: `{"prompt":"Review #99"}`},
		{ToolName: "Task", Category: "Task", InputJSON: `{"message":"Review https://github.com/example/repo/pull/51"}`},
		{ToolName: "Task", Category: "Task", InputJSON: `{"task":"Review https://github.com/example/repo/pull/50"}`},
		{ToolName: "Task", Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/42", "description":"https://github.com/example/repo/pull/47"}`},
		{ToolName: "newTask", Category: "Task", InputJSON: `{"tool":"newTask","content":"Review https://github.com/example/repo/pull/48"}`},
		{ToolName: "Task", Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/49","content":[{"type":"text","text":"other"}]}`},
		{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":["codex","exec","Review https://github.com/example/repo/pull/43"]}`},
		{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"curl https://github.com/example/repo/pull/44"}`},
	}}}
	links := toolCallPRLinks(t, messages, "", "")
	if assert.Len(t, links, 7) {
		assert.Equal(t, 52, links[0].Number)
		assert.Equal(t, 53, links[1].Number)
		assert.Equal(t, 51, links[2].Number)
		assert.Equal(t, 50, links[3].Number)
		assert.Equal(t, 42, links[4].Number)
		assert.Equal(t, 48, links[5].Number)
		assert.Equal(t, 49, links[6].Number)
	}
}

func TestToolCallPRLinksContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := db.ToolCallPRLinks(ctx, []db.Message{{ToolCalls: []db.ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`}}}}, "", "")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestToolCallPRLinksOversizedInput(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, cwd, remote string
		want                             int
	}{
		{"skipped", "gh pr view 41 -R github.com/example/repo", "gh pr view 42 -R github.com/example/repo", "", "", 1},
		{"repository change", "git remote set-url origin https://github.com/example/other.git", "gh pr view 42", "/work/repo", "https://github.com/example/repo.git", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"command":"` + tc.first + `","unrelated":"` + strings.Repeat("x", 256*1024) + `"}`
			messages := []db.Message{{ToolCalls: []db.ToolCall{
				{ToolName: "Bash", Category: "Bash", InputJSON: input},
				{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + tc.second + `"}`},
			}}}
			links := toolCallPRLinks(t, messages, tc.cwd, tc.remote)
			require.Len(t, links, tc.want)
			if tc.want > 0 {
				assert.Equal(t, 42, links[0].Number)
			}
		})
	}
}

func toolCallPRLinks(t *testing.T, messages []db.Message, cwd, remote string) []db.PRLink {
	t.Helper()
	links, err := db.ToolCallPRLinks(t.Context(), messages, cwd, remote)
	require.NoError(t, err)
	return links
}

func TestToolCallPRLinksDirectFormats(t *testing.T) {
	for _, tc := range []struct{ tool, input string }{
		{"exec_command", `{"commands":["gh pr view 42 -R example/repo"]}`},
		{"exec_command", `{"command":["gh","pr","view","42","-R","example/repo"]}`},
		{"exec_command", `{"cmd":"gh pr view 42 -R example/repo"}`},
		{"run_command", `{"commandLine":"gh pr view 42 -R github.com/example/repo"}`},
		{"shell_command", `gh pr view 42 -R github.com/example/repo`},
	} {
		t.Run(tc.input, func(t *testing.T) {
			links := toolCallPRLinks(t, []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: tc.tool, InputJSON: tc.input}}}}, "", "https://github.com/example/repo.git")
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
		{`{"cmd":"gh pr view 42","workdir":"/work/./repo"}`, 1},
	} {
		t.Run(tc.input, func(t *testing.T) {
			links := toolCallPRLinks(t, []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "exec_command", InputJSON: tc.input}}}}, "/work/repo", "https://github.com/example/repo.git")
			assert.Len(t, links, tc.want)
		})
	}
}

func TestToolCallPRLinksDirectoryChangePreservesHost(t *testing.T) {
	for _, tc := range []struct {
		name  string
		calls []db.ToolCall
		want  string
	}{
		{"host after cd", []db.ToolCall{{Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"cd /work/other"}`}, {Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"gh pr view 42 -R example/repo"}`}}, "https://git.example/example/repo/pull/42"},
		{"commands array", []db.ToolCall{{Category: "Bash", ToolName: "Bash", InputJSON: `{"commands":["cd /work/other","gh pr view 42 -R example/repo"]}`}}, "https://git.example/example/repo/pull/42"},
		{"bare after cd", []db.ToolCall{{Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"cd /work/other"}`}, {Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"gh pr merge 60"}`}}, ""},
		{"bare after push", []db.ToolCall{{Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"git push"}`}, {Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"gh pr merge 60"}`}}, "https://git.example/example/repo/pull/60"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			links := toolCallPRLinks(t, []db.Message{{ToolCalls: tc.calls}}, "/work/repo", "https://git.example/example/repo.git")
			if tc.want == "" {
				assert.Empty(t, links)
				return
			}
			require.Len(t, links, 1)
			assert.Equal(t, tc.want, links[0].URL)
		})
	}
}
