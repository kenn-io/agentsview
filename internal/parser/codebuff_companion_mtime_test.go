package parser

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCodebuffCompanionMtime verifies the engine's warm-side derivation
// matches the cold-write fingerprint's max(chat, run-state, chat-meta)
// rule. Each case pins a different component as the floor so a future
// refactor that drops a sibling from the walk surfaces immediately.
// All mtimes share one base time so the relative ordering is obvious.
func TestCodebuffCompanionMtime(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("MaxIsMeta", func(t *testing.T) {
		dir := t.TempDir()
		chatPath := writeAtTime(t, dir, "chat-messages.json", "[]", base)
		writeAtTime(t, dir, "run-state.json", "{}", base.Add(2*time.Hour))
		writeAtTime(t, dir, "chat-meta.json", "{}", base.Add(4*time.Hour))
		got := callHelper(t, chatPath)
		require.Equal(t, base.Add(4*time.Hour).UnixNano(), got)
	})

	t.Run("MaxIsRunState", func(t *testing.T) {
		dir := t.TempDir()
		chatPath := writeAtTime(t, dir, "chat-messages.json", "[]", base)
		writeAtTime(t, dir, "run-state.json", "{}", base.Add(6*time.Hour))
		writeAtTime(t, dir, "chat-meta.json", "{}", base.Add(4*time.Hour))
		got := callHelper(t, chatPath)
		require.Equal(t, base.Add(6*time.Hour).UnixNano(), got)
	})

	t.Run("MaxIsChat", func(t *testing.T) {
		dir := t.TempDir()
		chatPath := writeAtTime(t, dir, "chat-messages.json", "[]", base.Add(8*time.Hour))
		writeAtTime(t, dir, "run-state.json", "{}", base.Add(1*time.Hour))
		writeAtTime(t, dir, "chat-meta.json", "{}", base.Add(4*time.Hour))
		got := callHelper(t, chatPath)
		require.Equal(t, base.Add(8*time.Hour).UnixNano(), got)
	})

	t.Run("MissingCompanionsFallBackToChat", func(t *testing.T) {
		dir := t.TempDir()
		chatPath := writeAtTime(t, dir, "chat-messages.json", "[]", base)
		got := callHelper(t, chatPath)
		chatInfo, err := os.Stat(chatPath)
		require.NoError(t, err)
		require.Equal(t, chatInfo.ModTime().UnixNano(), got)
	})

	t.Run("PartialCompanionsStillIncludeMax", func(t *testing.T) {
		dir := t.TempDir()
		chatPath := writeAtTime(t, dir, "chat-messages.json", "[]", base)
		// Only run-state.json present, newer than chat.
		writeAtTime(t, dir, "run-state.json", "{}", base.Add(10*time.Hour))
		got := callHelper(t, chatPath)
		require.Equal(t, base.Add(10*time.Hour).UnixNano(), got)
	})
}

// writeAtTime writes a file under dir with body and sets atime/mtime
// to the supplied mtime. Both fields are set to the same value so
// os.Stat returns the intended mtime.
func writeAtTime(t *testing.T, dir, name, body string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	return path
}

// callHelper is a small wrapper so each subtest reads as a single
// assertion; the os.Stat pair would otherwise dominate every line.
func callHelper(t *testing.T, chatPath string) int64 {
	t.Helper()
	chatInfo, err := os.Stat(chatPath)
	require.NoError(t, err)
	return CodebuffCompanionMtime(chatPath, chatInfo)
}
