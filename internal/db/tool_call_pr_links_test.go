package db_test

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestToolCallPRLinks(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{
		{Category: "Task", InputJSON: `{"message":"Review https://github.com/example/repo/pull/51"}`},
		{Category: "Task", InputJSON: `{"task":"Review https://github.com/example/repo/pull/50"}`},
		{Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/42", "description":"https://github.com/example/repo/pull/47"}`},
		{Category: "Task", InputJSON: `{"tool":"newTask","content":"Review https://github.com/example/repo/pull/48"}`},
		{Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/49","content":[{"type":"text","text":"other"}]}`},
		{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":["codex","exec","Review https://github.com/example/repo/pull/43"]}`},
		{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"curl https://github.com/example/repo/pull/44"}`},
	}}}
	links := toolCallPRLinks(t, messages, "", "")
	if assert.Len(t, links, 6) {
		assert.Equal(t, 51, links[0].Number)
		assert.Equal(t, 50, links[1].Number)
		assert.Equal(t, 42, links[2].Number)
		assert.Equal(t, 48, links[3].Number)
		assert.Equal(t, 49, links[4].Number)
		assert.Equal(t, 43, links[5].Number)
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
	for _, tc := range []struct {
		name, tool, input, cwd string
		want                   bool
	}{
		{"workdir", "Bash", `{"command":"gh pr view 42","workdir":"/work/other"}`, "/work/repo", false},
		{"working_directory", "Bash", `{"command":"gh pr view 42","working_directory":"/work/other"}`, "/work/repo", false},
		{"same", "exec_command", `{"cmd":"gh pr view 42","workdir":"C:\\work\\repo"}`, `C:\work\repo`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := []db.Message{{ToolCalls: []db.ToolCall{{ToolName: tc.tool, Category: "Bash", InputJSON: tc.input}}}}
			links := toolCallPRLinks(t, messages, tc.cwd, "https://github.com/example/repo.git")
			if tc.want {
				require.Len(t, links, 1)
				assert.Equal(t, 42, links[0].Number)
			} else {
				assert.Empty(t, links)
			}
		})
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
		{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"gh pr view 42 -R github.com/example/repo"}`},
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
			key := "command"
			if tc.tool == "exec_command" {
				key = "cmd"
			}
			messages := []db.Message{{ToolCalls: []db.ToolCall{
				{ToolName: tc.tool, Category: "Bash", InputJSON: `{"` + key + `":"gh pr view 41"}`},
				{ToolName: tc.tool, Category: "Bash", InputJSON: `{"` + key + `":"cd /other"}`},
				{ToolName: tc.tool, Category: "Bash", InputJSON: `{"` + key + `":"gh pr view 42"}`},
			}}}
			links := toolCallPRLinks(t, messages, "/repo", "https://github.com/example/repo.git")
			require.Len(t, links, len(tc.want))
			for i, number := range tc.want {
				assert.Equal(t, number, links[i].Number)
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

func TestCodeModePRLinks(t *testing.T) {
	for _, tc := range []struct {
		name, input, cwd string
		want             []string
	}{
		{"ignored literals and comments", "const pattern = /tools.exec_command({cmd:\"gh pr view 99\"})/;\n" +
			"const literal = 'tools.exec_command({cmd:\"gh pr view 95\"})';\n" +
			"// tools.exec_command({cmd:'gh pr view 98'})\n" +
			"/* tools.exec_command({cmd:'gh pr view 97'}) */\n" +
			"const example = `tools.exec_command({cmd:'gh pr view 96'})`;\n" +
			"tools.exec_command({cmd:`gh pr view ${number}`});", "/repo", nil},
		{"workdir mismatch", `text(await tools.exec_command({cmd:'gh pr view 41',workdir:'/repo'})); text(await tools.exec_command({cmd:'gh pr co 42 -R example/other',workdir:'/other'})); text(await tools.exec_command({cmd:'gh pr view 43',workdir:'/other'}));`, "/repo", []string{"https://github.com/example/repo/pull/41", "https://github.com/example/other/pull/42"}},
		{"nested call", `tools.exec_command({cmd:tools.exec_command({cmd:'gh pr view 44'})});`, "/repo", []string{"https://github.com/example/repo/pull/44"}},
		{"spread", `tools.exec_command({cmd:'gh pr view 42 -R example/repo', ...params})`, "/repo", nil},
		{"computed", `tools.exec_command({cmd:'gh pr view 42 -R example/repo', ['cmd']: other})`, "/repo", nil},
		{"functions", `function f() { tools.exec_command({cmd:'gh pr view 41'}); } const g = () => tools.exec_command({cmd:'gh pr view 42'}); const o = { f() { tools.exec_command({cmd:'gh pr view 43'}); } };`, "/repo", nil},
		{"windows workdir", `tools.exec_command({cmd:"gh pr view 42",workdir:"C:\\work\\repo"})`, `C:\work\repo`, []string{"https://github.com/example/repo/pull/42"}},
		{"raw shell", `gh pr view 42 -R example/repo`, "/repo", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := []db.Message{{ToolCalls: []db.ToolCall{{ToolName: "exec", Category: "Bash", InputJSON: tc.input}}}}
			links := toolCallPRLinks(t, messages, tc.cwd, "https://github.com/example/repo.git")
			var urls []string
			for _, link := range links {
				urls = append(urls, link.URL)
			}
			assert.Equal(t, tc.want, urls)
		})
	}
}

func TestCodeModeRemoteChange(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{
		{ToolName: "exec", Category: "Bash", InputJSON: `await tools.exec_command({cmd:'git remote set-url origin https://github.com/example/other.git'})`},
		{ToolName: "exec", Category: "Bash", InputJSON: `await tools.exec_command({cmd:'gh pr view 42'}); await tools.exec_command({cmd:'gh pr view 43 -R example/other'})`},
	}}}
	links := toolCallPRLinks(t, messages, "/repo", "https://github.com/example/repo.git")
	require.Len(t, links, 1)
	assert.Equal(t, "https://github.com/example/other/pull/43", links[0].URL)
}

func TestCodeModeMalformedWarning(t *testing.T) {
	const marker = "private-command-marker"
	if os.Getenv("AGENTSVIEW_TEST_MALFORMED_CODE") == "1" {
		messages := []db.Message{{ToolCalls: []db.ToolCall{{ToolName: "exec", Category: "Bash", InputJSON: `tools.exec_command({cmd:'` + marker + `';`}}}}
		for range 2 {
			links, err := db.ToolCallPRLinks(t.Context(), messages, "", "")
			require.NoError(t, err)
			assert.Empty(t, links)
		}
		return
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCodeModeMalformedWarning$")
	cmd.Env = append(os.Environ(), "AGENTSVIEW_TEST_MALFORMED_CODE=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	warnings := regexp.MustCompile(`(?i)code[- ]mode|javascript`).FindAll(output, -1)
	assert.Len(t, warnings, 1, "one generic warning should report skipped code-mode input")
	assert.NotContains(t, string(output), marker)
}
