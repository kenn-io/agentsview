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
		{ToolName: "exec", Category: "Bash", InputJSON: `tools.exec_command({cmd:'gh pr view 99 -R example/repo'})`},
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
	input := `{"command":"gh pr view 41 -R example/repo","unrelated":"` + strings.Repeat("x", 256*1024) + `"}`
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
		`text(await tools.exec_command({cmd: "gh pr view 42 -R example/repo"}))`,
	} {
		t.Run(input, func(t *testing.T) {
			links := toolCallPRLinks(t, []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "exec", InputJSON: input}}}}, "", "https://github.com/example/repo.git")
			require.Len(t, links, 1)
			assert.Equal(t, 42, links[0].Number)
		})
	}
}

func TestToolCallPRLinksDynamicCode(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "exec", InputJSON: `const cmd = "gh pr view 42"; tools.exec_command({cmd})`}}}}
	assert.Empty(t, toolCallPRLinks(t, messages, "", "https://github.com/example/repo.git"))
}

func TestToolCallPRLinksWorkingDirectory(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  int
	}{
		{`{"cmd":"gh pr view 42","workdir":"/work/other"}`, 0},
		{`tools.exec_command({cmd:"gh pr view 42",workdir:"/work/other"})`, 0},
		{`tools.exec_command({cmd:"gh pr view 42",workdir:dir})`, 0},
		{`{"cmd":"gh pr view 42","workdir":"/work/./repo"}`, 1},
		{`{"cmd":"gh pr view 42 -R github.com/example/repo","workdir":"/work/other"}`, 1},
	} {
		t.Run(tc.input, func(t *testing.T) {
			links := toolCallPRLinks(t, []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", ToolName: "exec", InputJSON: tc.input}}}}, "/work/repo", "https://github.com/example/repo.git")
			assert.Len(t, links, tc.want)
		})
	}
}
