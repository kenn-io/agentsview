package db_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestToolCallPRLinks(t *testing.T) {
	messages := []db.Message{{ToolCalls: []db.ToolCall{
		{Category: "Task", InputJSON: `{"prompt":"Review https://github.com/example/repo/pull/42", "description":"https://github.com/example/repo/pull/47"}`},
		{Category: "Bash", InputJSON: `{"command":["codex","exec","Review https://github.com/example/repo/pull/43"]}`},
		{Category: "Bash", InputJSON: `{"command":"claude -p \"Review https://github.com/example/repo/pull/46; then report\""}`},
		{Category: "Bash", InputJSON: `{"command":"curl https://github.com/example/repo/pull/44"}`},
		{Category: "Bash", InputJSON: `{"command":["gh","pr","merge","45","-R","example/repo"]}`},
	}}}
	links := db.ToolCallPRLinks(messages, "", "")
	if assert.Len(t, links, 4) {
		assert.Equal(t, 42, links[0].Number)
		assert.Equal(t, 43, links[1].Number)
		assert.Equal(t, 46, links[2].Number)
		assert.Equal(t, 45, links[3].Number)
	}
}

func TestToolCallPRLinksLauncherPrompts(t *testing.T) {
	tests := []struct {
		command string
		want    int
	}{
		{`claude --help https://github.com/example/repo/pull/1`, 0},
		{`claude --model "$MODEL" -p 'review https://github.com/example/repo/pull/11'`, 11},
		{`claude -p "$PROMPT"`, 0},
		{`codex -m "$MODEL" exec 'review https://github.com/example/repo/pull/12'`, 12},
		{`claude --model https://github.com/example/repo/pull/2 -p 'hello'`, 0},
		{`claude --print 'review https://github.com/example/repo/pull/3'`, 3},
		{`claude --resume https://github.com/example/repo/pull/10 -p hello`, 0},
		{`claude -p 'review https://github.com/example/repo/pull/4; report'`, 4},
		{`codex --help https://github.com/example/repo/pull/5`, 0},
		{`codex exec --help https://github.com/example/repo/pull/6`, 0},
		{`codex exec --output-schema https://github.com/example/repo/pull/7 'hello'`, 0},
		{`codex -m model exec -C /tmp 'review https://github.com/example/repo/pull/8'`, 8},
		{`codex exec resume https://github.com/example/repo/pull/9`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			input, err := json.Marshal(map[string]string{"command": tt.command})
			require.NoError(t, err)
			links := db.ToolCallPRLinks([]db.Message{{ToolCalls: []db.ToolCall{{Category: "Bash", InputJSON: string(input)}}}}, "", "")
			if tt.want == 0 {
				assert.Empty(t, links)
				return
			}
			if assert.Len(t, links, 1) {
				assert.Equal(t, tt.want, links[0].Number)
			}
		})
	}
}

func TestCallPRLinksWorkdirAndWrapper(t *testing.T) {
	tests := []struct {
		input, cwd, remote string
		want               int
	}{
		{`{"command":"gh pr view 42","workdir":"/work/other"}`, "/work/repo", "https://github.com/example/repo.git", 0},
		{`{"command":"gh pr view 42 -R example/repo","workdir":"/work/other"}`, "/work/repo", "https://github.com/other/repo.git", 42},
		{`{"command":"timeout 60 claude -p 'review https://github.com/example/repo/pull/43'"}`, "/work/repo", "", 43},
		{`{"command":"env FOO=bar nohup codex exec 'review https://github.com/example/repo/pull/44'"}`, "/work/repo", "", 44},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			links := db.CallPRLinks("Bash", tt.input, tt.cwd, tt.remote)
			if tt.want == 0 {
				assert.Empty(t, links)
				return
			}
			if assert.Len(t, links, 1) {
				assert.Equal(t, tt.want, links[0].Number)
			}
		})
	}
}

func TestCallPRLinksMixedShellCalls(t *testing.T) {
	input := `{"command":"GH_HOST=forge.example gh pr comment 42 -R example/alpha --body \"$(cat /tmp/pr-comment)\"; claude -p 'review https://github.com/example/repo/pull/43'"}`
	links := db.CallPRLinks("Bash", input, "", "")
	require.Len(t, links, 2)
	assert.Equal(t, "https://forge.example/example/alpha/pull/42", links[0].URL)
	assert.Equal(t, "https://github.com/example/repo/pull/43", links[1].URL)
}

func TestTaskPRLinksWithNonStringContent(t *testing.T) {
	links := db.CallPRLinks("Task", `{"prompt":"Review https://github.com/example/repo/pull/42","content":[{"type":"text","text":"other"}]}`, "", "")
	require.Len(t, links, 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", links[0].URL)
}
