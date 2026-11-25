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
	bash := func(tool, input string) db.ToolCall {
		return db.ToolCall{ToolName: tool, Category: "Bash", InputJSON: input}
	}
	oversized := func(command string) string {
		return `{"command":"` + command + `","unrelated":"` + strings.Repeat("x", 256*1024) + `"}`
	}
	for _, tc := range []struct {
		name  string
		calls []db.ToolCall
		want  []int
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
		}, []int{52, 53, 51, 50, 42, 48, 49}},
		{"command", []db.ToolCall{bash("Bash", `{"command":"gh pr view 42 -R example/repo"}`)}, []int{42}},
		{"argv", []db.ToolCall{bash("shell", `{"command":["gh","pr","view","43","-R","example/repo"]}`)}, []int{43}},
		{"commands", []db.ToolCall{bash("execute_command", `{"commands":["gh pr view 42 -R example/repo","gh pr view 43 -R example/repo"]}`)}, []int{42, 43}},
		{"CommandLine", []db.ToolCall{bash("run_command", `{"CommandLine":"gh pr view 42 -R example/repo"}`)}, []int{42}},
		{"plain text", []db.ToolCall{bash("shell", `gh pr view 42 -R example/repo`)}, []int{42}},
		{"oversized skipped", []db.ToolCall{bash("Bash", oversized("gh pr view 41 -R github.com/example/repo")), bash("Bash", `{"command":"gh pr view 42 -R github.com/example/repo"}`)}, []int{42}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			links, err := db.ToolCallPRLinks(t.Context(), []db.Message{{ToolCalls: tc.calls}})
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

func TestToolCallPRLinksRawShellGroups(t *testing.T) {
	for _, input := range []string{
		`{ gh pr view 42 -R example/repo; }`,
		`[ -n "$VALUE" ] && gh pr view 42 -R example/repo`,
	} {
		t.Run(input, func(t *testing.T) {
			links, err := db.ToolCallPRLinks(t.Context(), []db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", InputJSON: input}}}})
			require.NoError(t, err)
			require.Len(t, links, 1)
			assert.Equal(t, "https://github.com/example/repo/pull/42", links[0].URL)
		})
	}
}
