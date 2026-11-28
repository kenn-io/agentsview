package sessionmaterialize

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/capture"
)

func TestClearClaudeWorktree(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	tests := []struct {
		name, input, suffix, sourceID string
		noPrefix, invalid             bool
	}{
		{name: "bound", input: `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":{"worktreePath":"/old"}}`, suffix: "\n" + `{"type":"worktree-state","worktreeSession":null,"sessionId":"` + id + `"}` + "\n"},
		{name: "bound with newline", input: `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":{"worktreePath":"/old"}}` + "\n", suffix: `{"type":"worktree-state","worktreeSession":null,"sessionId":"` + id + `"}` + "\n"},
		{name: "already cleared", input: `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":null}`},
		{name: "last clears", input: `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":{"worktreePath":"/old"}}` + "\n" + `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":null}`},
		{name: "no state", input: `{"type":"user","message":"hello"}`},
		{name: "other session", input: `{"type":"worktree-state","sessionId":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","worktreeSession":{"worktreePath":"/old"}}`},
		{name: "missing id", input: `{"type":"worktree-state","worktreeSession":{"worktreePath":"/old"}}`, invalid: true},
		{name: "invalid id", input: `{"type":"worktree-state","sessionId":"other","worktreeSession":{"worktreePath":"/old"}}`, invalid: true},
		{name: "whitespace", input: " \n", noPrefix: true, invalid: true},
		{name: "metadata only", input: `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":null}`, noPrefix: true, invalid: true},
		{name: "other conversation", input: `{"type":"user","sessionId":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","message":{"role":"user","content":"other"}}`, noPrefix: true, invalid: true},
		{name: "parent", input: `{"type":"user","sessionId":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","message":{"content":"parent"}}` + "\n", sourceID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", noPrefix: true},
		{name: "wrong parent", input: `{"type":"user","sessionId":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","message":{"content":"parent"}}` + "\n", sourceID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", noPrefix: true, invalid: true},
	}
	prefix := `{"type":"user","sessionId":"` + id + `","message":{"role":"user","content":"hello"}}` + "\n"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := tt.input
			if !tt.noPrefix {
				input = prefix + input
			}
			got, err := clearClaudeWorktree([]byte(input), id, tt.sourceID)
			if tt.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, input+tt.suffix, string(got))
		})
	}
}

func TestClaudeDestination(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "project-1")
	const id = "12345678-1234-1234-1234-123456789abc"
	cwd := t.TempDir()
	got, _, _, err := claudeDestination(cwd, id)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "projects", "project-1", id+".jsonl"), got)
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "CON")
	got, raw, project, err := claudeDestination(cwd, id)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "projects", capture.ClaudeProjectDir(cwd), id+".jsonl"), got)
	assert.Equal(t, home, raw)
	assert.Empty(t, project)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "project-1")
	got, raw, project, err = claudeDestination(cwd, id)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".claude", "projects", capture.ClaudeProjectDir(cwd), id+".jsonl"), got)
	assert.Empty(t, raw)
	assert.Empty(t, project)
	t.Setenv("CLAUDE_CONFIG_DIR", "relative-config")
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "project-1")
	_, _, _, err = claudeDestination(t.TempDir(), id)
	require.ErrorContains(t, err, "must be absolute")
	t.Setenv("CLAUDE_CONFIG_DIR", home+string(filepath.Separator))
	_, raw, project, err = claudeDestination(t.TempDir(), id)
	require.NoError(t, err)
	assert.Equal(t, home+string(filepath.Separator), raw)
	assert.Equal(t, "project-1", project)
	decomposed := filepath.Join(home, "e\u0301")
	t.Setenv("CLAUDE_CONFIG_DIR", decomposed)
	got, raw, project, err = claudeDestination(t.TempDir(), id)
	require.NoError(t, err)
	assert.Equal(t, decomposed, raw)
	assert.Equal(t, filepath.Join(home, "é", "projects", project, id+".jsonl"), got)
}
