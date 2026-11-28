//go:build !windows

package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestRemoteSyncArchiveHTTP_SkipsUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}

	claudeRoot := t.TempDir()
	healthy := filepath.Join(claudeRoot, "-proj-a", "session.jsonl")
	unreadable := filepath.Join(claudeRoot, "-proj-b", "memory", "dysync-troubleshooting.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(healthy), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(unreadable), 0o755))
	require.NoError(t, os.WriteFile(healthy, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-08-24T12:00:00Z", "healthy remote session").String()), 0o644))
	require.NoError(t, os.WriteFile(unreadable, []byte("root-only note"), 0o644))
	require.NoError(t, os.Chmod(unreadable, 0o000))
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	assertRemoteSyncSkipsUnreadableFileOverHTTP(t, claudeRoot, healthy, unreadable)
}
