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
		{Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/42", "description":"https://github.com/example/repo/pull/47"}`},
		{Category: "Task", InputJSON: `{"tool":"newTask","content":"Review https://github.com/example/repo/pull/48"}`},
		{Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/49","content":[{"type":"text","text":"other"}]}`},
		{Category: "Bash", InputJSON: `{"command":["codex","exec","Review https://github.com/example/repo/pull/43"]}`},
		{Category: "Bash", InputJSON: `{"command":"claude -p \"Review https://github.com/example/repo/pull/46\""}`},
		{Category: "Bash", InputJSON: `{"command":"curl https://github.com/example/repo/pull/44"}`},
		{Category: "Bash", InputJSON: `{"command":["gh","pr","merge","45","-R","example/repo"]}`},
	}}}
	links := toolCallPRLinks(t, messages, "", "")
	if assert.Len(t, links, 6) {
		assert.Equal(t, 42, links[0].Number)
		assert.Equal(t, 48, links[1].Number)
		assert.Equal(t, 49, links[2].Number)
		assert.Equal(t, 43, links[3].Number)
		assert.Equal(t, 46, links[4].Number)
		assert.Equal(t, 45, links[5].Number)
	}
}

func TestClineCommandArrayPRLinks(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{{ToolName: "execute_command", Category: "Bash", InputJSON: `{"commands":["gh pr view 41 -R example/other","gh pr view 42 -R example/repo"]}`}}}}
	links := toolCallPRLinks(t, messages, "", "")
	require.Len(t, links, 2)
	assert.Equal(t, "https://github.com/example/other/pull/41", links[0].URL)
	assert.Equal(t, "https://github.com/example/repo/pull/42", links[1].URL)
}

func TestToolCallPRLinksWorkdir(t *testing.T) {
	for _, key := range []string{"workdir", "working_directory"} {
		t.Run(key, func(t *testing.T) {
			input := `{"command":"gh pr view 42","` + key + `":"/work/other"}`
			links := toolCallPRLinks(t, []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", InputJSON: input}}}}, "/work/repo", "https://github.com/example/repo.git")
			assert.Empty(t, links)
		})
	}
}

func TestToolCallPRLinksContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := db.ToolCallPRLinks(ctx, []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", InputJSON: `{"cmd":"gh pr view 42 -R example/repo"}`}}}}, "", "")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestToolCallPRLinksSkipsOversizedInput(t *testing.T) {
	input := `{"cmd":"gh pr view 41 -R example/repo","unrelated":"` + strings.Repeat("x", 256*1024) + `"}`
	messages := []db.Message{{ToolCalls: []db.ToolCall{
		{Category: "Bash", InputJSON: input},
		{Category: "Bash", InputJSON: `{"cmd":"gh pr view 42 -R example/repo"}`},
	}}}
	links := toolCallPRLinks(t, messages, "", "")
	require.Len(t, links, 1)
	assert.Equal(t, 42, links[0].Number)
}

func TestToolCallPRLinksDirectoryChange(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		want       []int
	}{
		{"shared shell", "Bash", []int{41}},
		{"separate shell", "exec_command", []int{41, 42}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := []db.Message{{ToolCalls: []db.ToolCall{
				{ToolName: tc.tool, Category: "Bash", InputJSON: `{"cmd":"gh pr view 41"}`},
				{ToolName: tc.tool, Category: "Bash", InputJSON: `{"cmd":"cd /other"}`},
				{ToolName: tc.tool, Category: "Bash", InputJSON: `{"cmd":"gh pr view 42"}`},
			}}}
			links := toolCallPRLinks(t, messages, "/repo", "https://github.com/example/repo.git")
			require.Len(t, links, len(tc.want))
			for i, number := range tc.want {
				assert.Equal(t, number, links[i].Number)
			}
		})
	}
}

func TestCodeModePRLinksKeepExplicitIdentities(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", InputJSON: `text(await tools.exec_command({cmd:"gh pr view 41",workdir:"/repo"})); text(await tools.exec_command({cmd:"gh pr view 42"})); text(await tools.exec_command({cmd:"gh pr view 43 -R example/other",workdir:"/other"})); text(await tools.exec_command({cmd:"cd /other"})); text(await tools.exec_command({cmd:"gh pr view 44"})); text(await tools.exec_command({cmd:"gh pr view 45",workdir:dir})); text(await tools.exec_command({cmd:"gh pr view 46",workdir:"/repo"+suffix})); text(await tools.exec_command({cmd:"gh pr view 47",workdir:"/other"}))`}}}}
	links := toolCallPRLinks(t, messages, "/repo", "https://github.com/example/repo.git")
	require.Len(t, links, 4)
	assert.Equal(t, "https://github.com/example/repo/pull/41", links[0].URL)
	assert.Equal(t, "https://github.com/example/repo/pull/42", links[1].URL)
	assert.Equal(t, "https://github.com/example/other/pull/43", links[2].URL)
	assert.Equal(t, "https://github.com/example/repo/pull/44", links[3].URL)
}

func TestCodeModeDoesNotTreatRawJavaScriptAsShell(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{{ToolName: "exec", Category: "Bash", InputJSON: `gh pr view 42 -R example/repo`}}}}
	assert.Empty(t, toolCallPRLinks(t, messages, "", ""))
}

func toolCallPRLinks(t *testing.T, messages []db.Message, cwd, remote string) []db.PRLink {
	t.Helper()
	links, err := db.ToolCallPRLinks(t.Context(), messages, cwd, remote)
	require.NoError(t, err)
	return links
}
