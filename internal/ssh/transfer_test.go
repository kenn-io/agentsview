package ssh

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/parser"
)

func TestBuildTarCommand(t *testing.T) {
	dirs := map[parser.AgentType][]string{
		parser.AgentClaude: {"/home/wes/.claude/projects"},
		parser.AgentCodex:  {"/home/wes/.codex/sessions"},
	}
	cmd := buildTarCommand(dirs, []string{"/home/wes/.codex/session_index.jsonl"})

	assert.True(t, strings.HasPrefix(cmd, "tar cf - -C / -- "), "bad prefix: %s", cmd)
	// Paths are shell-quoted.
	assert.Contains(t, cmd, "'home/wes/.claude/projects'")
	assert.Contains(t, cmd, "'home/wes/.codex/sessions'")
	// Extra files are included, shell-quoted, with no leading slash.
	assert.Contains(t, cmd, "'home/wes/.codex/session_index.jsonl'")
	// No leading slash in path args.
	assert.NotContains(t, cmd, "'/home/", "path has leading slash: %s", cmd)
}

func TestRemapPath(t *testing.T) {
	// Use filepath.Join so the local paths are OS-native.
	// remapToRemotePath always returns forward-slash paths.
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := "/home/wes/.claude"
	localPath := filepath.Join(
		"tmp", "sync-123", "home", "wes", ".claude", "foo.jsonl",
	)
	got := remapToRemotePath(tempDir, remoteDir, localPath)
	assert.Equal(t, "/home/wes/.claude/foo.jsonl", got)
}

func TestRemappedDir(t *testing.T) {
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := "/home/wes/.claude"
	got := remappedDir(tempDir, remoteDir)
	want := filepath.Join("tmp", "sync-123", "home", "wes", ".claude")
	assert.Equal(t, want, got)
}
