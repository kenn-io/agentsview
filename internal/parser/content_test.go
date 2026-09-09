package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveFilePathFromJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"file_path key", `{"file_path":"/a/b.go"}`, "/a/b.go"},
		{"path key", `{"path":"src/x.go"}`, "src/x.go"},
		{"filePath key", `{"filePath":"app.ts"}`, "app.ts"},
		{"file key", `{"file":"new.go"}`, "new.go"},
		{"precedence file_path wins", `{"path":"p","file_path":"fp"}`, "fp"},
		{"precedence path beats filePath", `{"path":"b","filePath":"c"}`, "b"},
		{"precedence filePath beats file", `{"filePath":"c","file":"d"}`, "c"},
		{"invalid json returns empty", "this is a raw diff, not json", ""},
		{"valid json no path key", `{"command":"ls"}`, ""},
		{"empty string", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ResolveFilePathFromJSON(tt.input))
		})
	}
}

func TestRedactedToolUseRenderingKeepsPathsAndDropsArguments(t *testing.T) {
	tests := []struct {
		name, tool, input, wantFull, wantRedacted string
	}{
		{
			name:         "bash keeps only the label",
			tool:         "Bash",
			input:        `{"command":"curl -H 'Authorization: token' https://example.com","description":"fetch the report"}`,
			wantFull:     "[Bash: fetch the report]\n$ curl -H 'Authorization: token' https://example.com",
			wantRedacted: "[Bash]",
		},
		{
			name: "read keeps its path", tool: "Read",
			input:        `{"file_path":"/repo/internal/db/db.go","limit":40}`,
			wantFull:     "[Read: /repo/internal/db/db.go]",
			wantRedacted: "[Read: /repo/internal/db/db.go]",
		},
		{
			name: "grep drops the pattern", tool: "Grep",
			input:        `{"pattern":"AKIA[0-9A-Z]{16}","path":"/repo"}`,
			wantFull:     "[Grep: AKIA[0-9A-Z]{16}]",
			wantRedacted: "[Grep]",
		},
		{
			name: "unknown tools already show only their name", tool: "mcp__jira__search",
			input:        `{"query":"secret project"}`,
			wantFull:     "[Tool: mcp__jira__search]",
			wantRedacted: "[Tool: mcp__jira__search]",
		},
		{
			name: "todo list drops its items", tool: "TodoWrite",
			input:        `{"todos":[{"status":"pending","content":"rotate the leaked key"}]}`,
			wantFull:     "[Todo List]\n  ○ rotate the leaked key",
			wantRedacted: "[Todo List]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantFull, ToolUseRendering(tt.tool, tt.input))
			assert.Equal(t, tt.wantRedacted,
				RedactedToolUseRendering(tt.tool, tt.input))
		})
	}
	assert.Empty(t, ToolUseRendering("", `{}`))
	assert.Empty(t, ToolUseRendering("Bash", `not json`))
}

func TestToolUseRenderingCandidatesRebuildGrokSummaries(t *testing.T) {
	pairs := ToolUseRenderingCandidates(
		"WebSearch", "web_search", `{"type":"search","query":"payroll export"}`,
	)
	var found bool
	for _, pair := range pairs {
		if pair.Full == "[backend web_search] search: payroll export" {
			found = true
			assert.Equal(t, "[backend web_search]", pair.Redacted)
		}
	}
	assert.True(t, found, "a copied Grok row can be redacted from its stored input")
}

func TestRedactToolUseRenderingHandlesProviderRenderers(t *testing.T) {
	tests := []struct {
		name, full, category, tool, input, want string
	}{
		{
			name: "generic bash", category: "Bash", tool: "Bash",
			input: `{"command":"cat /etc/shadow","description":"clean"}`,
			full:  "[Bash: clean]\n$ cat /etc/shadow", want: "[Bash]",
		},
		{
			name: "gemini keeps the file path", category: "Read", tool: "read_file",
			input: `{"file_path":"/repo/a.go"}`,
			full:  "[Read: /repo/a.go]", want: "[Read: /repo/a.go]",
		},
		{
			name: "gemini shell", category: "Bash", tool: "run_shell_command",
			input: `{"command":"cat ~/.netrc"}`,
			full:  "[Bash]\n$ cat ~/.netrc", want: "[Bash]",
		},
		{
			name: "antigravity header", category: "Bash", tool: "run_command",
			input: `{"CommandLine":"env | grep TOKEN"}`,
			full:  formatToolHeader("Bash", agyToolDetail("run_command", `{"CommandLine":"env | grep TOKEN"}`)),
			want:  "[Bash: run_command]",
		},
		{
			name: "codex with a summary the input cannot reproduce", category: "Bash", tool: "shell",
			input: `{"command":["bash","-lc","curl -u me:pw https://x"]}`,
			full:  "[Bash: fetch the thing]\n$ curl -u me:pw https://x", want: "[Bash]",
		},
		{
			name: "unknown multi-line rendering keeps only its label", category: "Other", tool: "custom",
			input: `{"q":"secret"}`,
			full:  "[Custom: secret]\nsecret preview", want: "[Custom]",
		},
		{
			name: "text that is not a header is dropped", category: "Other", tool: "custom",
			input: `{}`, full: "ran custom with secret", want: "",
		},
		{
			name: "openhands terminal", category: "Bash", tool: "terminal",
			input: `{"command":"cat /secret"}`,
			full:  "[Bash]\n$ cat /secret", want: "[Bash]",
		},
		{
			name: "vs code copilot terminal", category: "Bash", tool: "run_in_terminal",
			input: `{"command":"printenv TOKEN"}`,
			full:  "[Bash: run_in_terminal]\n$ printenv TOKEN", want: "[Bash: run_in_terminal]",
		},
		{
			name: "grok search keeps only the backend label", category: "WebSearch", tool: "web_search",
			input: `{"type":"search","query":"employee salaries 2026"}`,
			full:  "[backend web_search] search: employee salaries 2026",
			want:  "[backend web_search]",
		},
		{
			name: "grok code interpreter drops the code", category: "Other", tool: "code_interpreter",
			input: `{"code":"print(open('/etc/passwd').read())"}`,
			full:  "[backend code_interpreter] print(open('/etc/passwd').read())",
			want:  "[backend code_interpreter]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, RedactToolUseRendering(
				tt.full, tt.category, tt.tool, tt.input,
			))
		})
	}
}
