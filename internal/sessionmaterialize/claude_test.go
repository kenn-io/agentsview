package sessionmaterialize

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClearClaudeWorktree(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	tests := []struct {
		name, input, suffix string
		invalid             bool
	}{
		{"bound", `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":{"worktreePath":"/old"}}`, "\n" + `{"type":"worktree-state","worktreeSession":null,"sessionId":"` + id + `"}` + "\n", false},
		{"bound with newline", `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":{}}` + "\n", `{"type":"worktree-state","worktreeSession":null,"sessionId":"` + id + `"}` + "\n", false},
		{"already cleared", `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":null}`, "", false},
		{"last clears", `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":{}}` + "\n" + `{"type":"worktree-state","sessionId":"` + id + `","worktreeSession":null}`, "", false},
		{"no state", `{"type":"user","message":"hello"}`, "", false},
		{"other session", `{"type":"worktree-state","sessionId":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","worktreeSession":{}}`, "", false},
		{"missing id", `{"type":"worktree-state","worktreeSession":{}}`, "", true},
		{"invalid id", `{"type":"worktree-state","sessionId":"other","worktreeSession":{}}`, "", true},
		{"numeric id", `{"type":"worktree-state","sessionId":5,"worktreeSession":{}}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := clearClaudeWorktree([]byte(tt.input), id)
			if tt.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.input+tt.suffix, string(got))
		})
	}
}

func TestClaudeDestination(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "project-1")
	const id = "12345678-1234-1234-1234-123456789abc"
	got, err := claudeDestination(t.TempDir(), id)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "projects", "project-1", id+".jsonl"), got)
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "CON")
	got, err = claudeDestination(t.TempDir(), id)
	require.NoError(t, err)
	assert.NotEqual(t, filepath.Join(home, "projects", "CON", id+".jsonl"), got)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "project-1")
	got, err = claudeDestination(t.TempDir(), id)
	require.NoError(t, err)
	assert.NotEqual(t, filepath.Join(home, ".claude", "projects", "project-1", id+".jsonl"), got)
}
