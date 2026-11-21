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
		{Category: "Bash", InputJSON: `{"command":"claude -p \"Review https://github.com/example/repo/pull/46; then report\""}`},
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

func TestCallPRLinksWorkdir(t *testing.T) {
	links := db.CallPRLinks("Bash", `{"command":"gh pr view 42","workdir":"/work/other"}`, "/work/repo", "https://github.com/example/repo.git")
	assert.Empty(t, links)
}

func TestTaskPRLinksWithNonStringContent(t *testing.T) {
	links := db.CallPRLinks("Task", `{"prompt":"Review https://github.com/example/repo/pull/42","content":[{"type":"text","text":"other"}]}`, "", "")
	require.Len(t, links, 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", links[0].URL)
}

func TestCodeModePRLinksUseEachCommandWorkdir(t *testing.T) {
	remote := "https://github.com/example/repo.git"
	assert.Empty(t, db.CallPRLinks("Bash", `text(await tools.exec_command({cmd:"gh pr view 42",workdir:"/other"}))`, "/repo", remote))
	links := db.CallPRLinks("Bash", `text(await tools.exec_command({cmd:"gh pr view 41 -R example/alpha",workdir:"/one"})); text(await tools.exec_command({cmd:"gh pr view 42 -R example/beta",workdir:"/two"}))`, "/repo", remote)
	require.Len(t, links, 2)
	assert.Equal(t, "https://github.com/example/alpha/pull/41", links[0].URL)
	assert.Equal(t, "https://github.com/example/beta/pull/42", links[1].URL)
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
