package remotesync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestImporterImportsExtractedRemoteFiles(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	tmp := t.TempDir()
	remoteDir := "/home/wes/.claude/projects"
	localDir := filepath.Join(tmp, "home", "wes", ".claude", "projects", "test-project")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	sessionPath := filepath.Join(localDir, "session.jsonl")
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "remote import").
			String(),
	), 0o644))

	stats, err := Importer{
		Host: "devbox",
		DB:   database,
	}.ImportExtracted(context.Background(), TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {remoteDir},
		},
	}, tmp)

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	page, err := database.ListSessions(context.Background(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "devbox", page.Sessions[0].Machine)
	full, err := database.GetSessionFull(context.Background(), page.Sessions[0].ID)
	require.NoError(t, err)
	require.NotNil(t, full)
	require.NotNil(t, full.FilePath)
	assert.Contains(t, *full.FilePath, "devbox:/home/wes/.claude/projects/test-project/session.jsonl")
}

func TestRemotePathMappingHandlesWindowsDrivePath(t *testing.T) {
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := `C:\Users\wes\.codex\sessions`
	remoteFile := `C:\Users\wes\.codex\sessions\2026\session.jsonl`
	wantLocalDir := filepath.Join(
		tempDir, "__drive_C", "Users", "wes", ".codex", "sessions",
	)
	wantLocalFile := filepath.Join(
		tempDir, "__drive_C", "Users", "wes", ".codex", "sessions",
		"2026", "session.jsonl",
	)

	assert.Equal(t, wantLocalDir, RemappedDir(tempDir, remoteDir))
	assert.Equal(t, wantLocalFile, remappedRemotePath(tempDir, remoteFile))
	assert.Equal(t,
		remoteFile,
		RemapToRemotePath(tempDir, remoteDir, wantLocalFile),
	)
}

func TestRemotePathMappingHandlesForwardSlashUNCPath(t *testing.T) {
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := `//server/share/.codex/sessions`
	remoteFile := `//server/share/.codex/sessions/2026/session.jsonl`
	wantLocalDir := filepath.Join(
		tempDir, "__unc", "server", "share", ".codex", "sessions",
	)
	wantLocalFile := filepath.Join(
		tempDir, "__unc", "server", "share", ".codex", "sessions",
		"2026", "session.jsonl",
	)

	assert.Equal(t, wantLocalDir, RemappedDir(tempDir, remoteDir))
	assert.Equal(t, wantLocalFile, remappedRemotePath(tempDir, remoteFile))
	assert.Equal(t,
		remoteFile,
		RemapToRemotePath(tempDir, remoteDir, wantLocalFile),
	)
}

func TestImporterRejectsEscapingRemoteTargets(t *testing.T) {
	stats, err := Importer{
		Host: "devbox",
	}.ImportExtracted(context.Background(), TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"../../outside"},
		},
	}, t.TempDir())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe remote path")
	assert.Zero(t, stats)
}

func TestLocalArchivePathRejectsDotDotComponents(t *testing.T) {
	_, err := safeLocalArchivePath(t.TempDir(), "safe/../escape.jsonl")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe archive path")
}

func TestRemoteSkipCacheUsesArchivePathMapping(t *testing.T) {
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := `C:\Users\wes\.codex\sessions`
	remoteFile := `C:\Users\wes\.codex\sessions\2026\session.jsonl`
	tempRoot := RemappedDir(tempDir, remoteDir)
	tempFile := filepath.Join(tempRoot, "2026", "session.jsonl")

	translated := translateRemoteCacheToTemp(
		map[string]int64{remoteFile: 123},
		[]string{remoteDir},
		[]string{tempRoot},
	)
	assert.Equal(t, map[string]int64{tempFile: 123}, translated)

	got, ok := tempPathToRemotePath(
		tempFile,
		[]string{remoteDir},
		[]string{tempRoot},
	)
	require.True(t, ok)
	assert.Equal(t, remoteFile, got)
}
