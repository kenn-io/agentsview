package db_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestToolCallPRLinks(t *testing.T) {
	remote := "https://github.com/example/repo.git"
	bash := func(tool, input string) db.ToolCall {
		return db.ToolCall{ToolName: tool, Category: "Bash", InputJSON: input}
	}
	oversized := func(command string) string {
		return `{"command":"` + command + `","unrelated":"` + strings.Repeat("x", 256*1024) + `"}`
	}
	for _, tc := range []struct {
		name        string
		calls       []db.ToolCall
		cwd, remote string
		want        []int
	}{
		{"child prompts", []db.ToolCall{
			{ToolName: "spawn_agents", Category: "Task", InputJSON: `{"agents":[{"prompt":"Review https://github.com/example/repo/pull/52"},{"prompt":"Review https://github.com/example/repo/pull/53 and https://github.com/example/repo/pull/52","description":"https://github.com/example/repo/pull/44"}]}`},
			{ToolName: "Task", Category: "Task", InputJSON: `{"prompt":"Review #99"}`},
			{ToolName: "Task", Category: "Task", InputJSON: `{"message":"Review https://github.com/example/repo/pull/51"}`},
			{ToolName: "Task", Category: "Task", InputJSON: `{"task":"Review https://github.com/example/repo/pull/50"}`},
			{ToolName: "Task", Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/42", "description":"https://github.com/example/repo/pull/47"}`},
			{ToolName: "newTask", Category: "Task", InputJSON: `{"tool":"newTask","content":"Review https://github.com/example/repo/pull/48"}`},
			{ToolName: "Task", Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/49","content":[{"type":"text","text":"other"}]}`},
			bash("Bash", `{"command":["codex","exec","Review https://github.com/example/repo/pull/43"]}`),
			bash("Bash", `{"command":"curl https://github.com/example/repo/pull/44"}`),
		}, "", "", []int{52, 53, 51, 50, 42, 48, 49}},
		{"command", []db.ToolCall{bash("Bash", `{"command":"gh pr view 42 -R example/repo"}`)}, "/work/repo", remote, []int{42}},
		{"argv", []db.ToolCall{bash("shell", `{"command":["bash","-lc","gh pr view 43 -R example/repo"]}`)}, "/work/repo", remote, []int{43}},
		{"commands", []db.ToolCall{bash("execute_command", `{"commands":["gh pr view 42 -R example/repo","gh pr view 43 -R example/repo"]}`)}, "/work/repo", remote, []int{42, 43}},
		{"CommandLine", []db.ToolCall{bash("run_command", `{"CommandLine":"gh pr view 42 -R example/repo"}`)}, "/work/repo", remote, []int{42}},
		{"plain text", []db.ToolCall{bash("shell", `gh pr view 42`)}, "/work/repo", remote, []int{42}},
		{"code mode", []db.ToolCall{bash("exec", `text(await tools.exec_command({cmd:"gh pr view 42 -R example/repo"})); text(await tools.exec_command({cmd:"gh pr view 43 -R example/repo"}))`)}, "/work/repo", remote, []int{42, 43}},
		{"other", []db.ToolCall{bash("exec_command", `{"cmd":"gh pr view 42","workdir":"/work/other"}`)}, "/work/repo", remote, nil},
		{"matching", []db.ToolCall{bash("exec_command", `{"cmd":"gh pr view 42","workdir":"/work/./repo"}`)}, "/work/repo", remote, []int{42}},
		{"relative matching", []db.ToolCall{bash("exec_command", `{"cmd":"gh pr view 42","workdir":"."}`)}, "/work/repo", remote, []int{42}},
		{"relative other", []db.ToolCall{bash("exec_command", `{"cmd":"gh pr view 42","workdir":"../other"}`)}, "/work/repo", remote, nil},
		{"code mode relative matching", []db.ToolCall{bash("exec", `text(await tools.exec_command({cmd:"gh pr view 42",workdir:"."}))`)}, "/work/repo", remote, []int{42}},
		{"code mode batch", []db.ToolCall{bash("exec", `text(await tools.exec_command({cmd:"gh pr view 41",workdir:"/other"})); text(await tools.exec_command({cmd:"gh pr view 42",workdir:"/work/repo"}))`)}, "/work/repo", remote, []int{42}},
		{"code mode commit directory", []db.ToolCall{bash("exec", `text(await tools.exec_command({cmd:"git -C /other commit -m x; gh pr view 42",workdir:"/work/repo"}))`)}, "/work/repo", remote, nil},
		{"earlier directory", []db.ToolCall{bash("Bash", `{"command":"cd /work/other"}`), bash("Bash", `{"command":"gh pr view 42"}`), bash("Bash", `{"command":"gh pr view 61 -R github.com/example/repo"}`)}, "/work/repo", remote, []int{42, 61}},
		{"earlier repository", []db.ToolCall{bash("Bash", `{"command":"git remote set-url origin https://github.com/user-b/project-b.git"}`), bash("Bash", `{"command":"gh pr view 42"}`), bash("Bash", `{"command":"gh pr view 61 -R github.com/example/repo"}`)}, "/work/repo", remote, []int{42, 61}},
		{"oversized skipped", []db.ToolCall{bash("Bash", oversized("gh pr view 41 -R github.com/example/repo")), bash("Bash", `{"command":"gh pr view 42 -R github.com/example/repo"}`)}, "", "", []int{42}},
		{"oversized input leaves later call independent", []db.ToolCall{bash("Bash", oversized("echo")), bash("Bash", `{"command":"gh pr view 42"}`)}, "/work/repo", remote, []int{42}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			links, err := db.ToolCallPRLinks(t.Context(), []db.Message{{ToolCalls: tc.calls}}, tc.cwd, tc.remote)
			require.NoError(t, err)
			var numbers []int
			for _, link := range links {
				numbers = append(numbers, link.Number)
				assert.Equal(t, "https://github.com/example/repo/pull/"+strconv.Itoa(link.Number), link.URL)
			}
			assert.Equal(t, tc.want, numbers)
		})
	}
}
