package db_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestToolCallPRLinks(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{
		{Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/42", "description":"https://github.com/example/repo/pull/47"}`},
		{Category: "Task", InputJSON: `{"tool":"newTask","content":"Review https://github.com/example/repo/pull/48"}`},
		{Category: "Bash", InputJSON: `{"command":["codex","exec","Review https://github.com/example/repo/pull/43"]}`},
		{Category: "Bash", InputJSON: `{"command":"claude -p \"Review https://github.com/example/repo/pull/46\""}`},
		{Category: "Bash", InputJSON: `{"command":"curl https://github.com/example/repo/pull/44"}`},
		{Category: "Bash", InputJSON: `{"command":["gh","pr","merge","45","-R","example/repo"]}`},
	}}}
	links := db.ToolCallPRLinks(messages, "", "")
	if assert.Len(t, links, 5) {
		assert.Equal(t, 42, links[0].Number)
		assert.Equal(t, 48, links[1].Number)
		assert.Equal(t, 43, links[2].Number)
		assert.Equal(t, 46, links[3].Number)
		assert.Equal(t, 45, links[4].Number)
	}
}

func TestToolCallPRLinksWorkdir(t *testing.T) {
	links := db.ToolCallPRLinks([]db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", InputJSON: `{"command":"gh pr view 42","workdir":"/work/other"}`}}}}, "/work/repo", "https://github.com/example/repo.git")
	assert.Empty(t, links)
}

func TestTaskPRLinksWithNonStringContent(t *testing.T) {
	links := db.ToolCallPRLinks([]db.Message{{ToolCalls: []db.ToolCall{{Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/42","content":[{"type":"text","text":"other"}]}`}}}}, "", "")
	require.Len(t, links, 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", links[0].URL)
}

func TestToolCallPRLinksForgetCheckoutAfterDirectoryChange(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{
		{Category: "Bash", InputJSON: `{"cmd":"gh pr view 41"}`},
		{Category: "Bash", InputJSON: `{"cmd":"cd /other"}`},
		{Category: "Bash", InputJSON: `{"cmd":"gh pr view 42"}`},
		{Category: "Bash", InputJSON: `{"cmd":"gh pr view 43 -R example/other"}`},
	}}}
	links := db.ToolCallPRLinks(messages, "/repo", "https://github.com/example/repo.git")
	require.Len(t, links, 2)
	assert.Equal(t, "https://github.com/example/repo/pull/41", links[0].URL)
	assert.Equal(t, "https://github.com/example/other/pull/43", links[1].URL)
}

func TestCodexSeparateShellWorkingDirectories(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{
		{ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"cd /other"}`},
		{ToolName: "exec_command", Category: "Bash", InputJSON: `{"cmd":"gh pr view 42"}`},
	}}}
	links := db.ToolCallPRLinks(messages, "/repo", "https://github.com/example/repo.git")
	require.Len(t, links, 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", links[0].URL)
}

func TestCodeModeExplicitSameDirectory(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{{ToolName: "exec", Category: "Bash", InputJSON: `text(await tools.exec_command({cmd:"gh pr view 42",workdir:"/repo"})); text(await tools.exec_command({cmd:"gh pr view 43",workdir:"/other"})); text(await tools.exec_command({cmd:"gh pr view 44",workdir:dir})); text(await tools.exec_command({cmd:"gh pr view 45",workdir:"/repo"+suffix}))`}}}}
	links := db.ToolCallPRLinks(messages, "/repo", "https://github.com/example/repo.git")
	require.Len(t, links, 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", links[0].URL)
}

func TestCodeModePRLinksKeepExplicitIdentities(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", InputJSON: `text(await tools.exec_command({cmd:"gh pr view 41",workdir:"/repo"})); text(await tools.exec_command({cmd:"gh pr view 42"})); text(await tools.exec_command({cmd:"gh pr view 43 -R example/other",workdir:"/other"})); text(await tools.exec_command({cmd:"cd /other"})); text(await tools.exec_command({cmd:"gh pr view 44"}))`}}}}
	links := db.ToolCallPRLinks(messages, "/repo", "https://github.com/example/repo.git")
	require.Len(t, links, 4)
	assert.Equal(t, "https://github.com/example/repo/pull/41", links[0].URL)
	assert.Equal(t, "https://github.com/example/repo/pull/42", links[1].URL)
	assert.Equal(t, "https://github.com/example/other/pull/43", links[2].URL)
	assert.Equal(t, "https://github.com/example/repo/pull/44", links[3].URL)
}
