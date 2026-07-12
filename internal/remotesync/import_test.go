package remotesync

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestPreparedHTTPSyncRebuildContributor(t *testing.T) {
	const (
		sessionID   = "session"
		remoteDir   = "/remote"
		remoteFile  = "/remote/path/session.jsonl"
		skippedDir  = "/remote-skips"
		skippedFile = "/remote-skips/path/intentionally-empty.jsonl"
	)
	sessionBody := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID(
		"2024-01-01T00:00:00Z", "remote rebuild searchable", sessionID,
	).String()
	usageBody := testjsonl.ClaudeUserJSON(
		"<command-name>/usage</command-name>\n"+
			"<command-message>usage</command-message>\n"+
			"<command-args></command-args>",
		"2024-01-01T00:01:00Z",
	)
	files := map[string]string{
		remoteFile:  sessionBody,
		skippedFile: usageBody,
	}
	mtime := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	manifest := Manifest{Files: []ManifestEntry{
		{Path: remoteFile, Size: int64(len(sessionBody)), MtimeNS: mtime.UnixNano()},
		{Path: skippedFile, Size: int64(len(usageBody)), MtimeNS: mtime.UnixNano()},
	}}
	archive := func(t *testing.T) []byte {
		t.Helper()
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, path := range []string{remoteFile, skippedFile} {
			name, err := safeRemotePathArchiveName(path)
			require.NoError(t, err)
			body := files[path]
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: mtime,
			}))
			_, err = tw.Write([]byte(body))
			require.NoError(t, err)
		}
		require.NoError(t, tw.Close())
		return buf.Bytes()
	}(t)
	targets := TargetSet{Dirs: map[parser.AgentType][]string{
		parser.AgentClaude: {remoteDir, skippedDir},
	}}
	targetsJSON, err := json.Marshal(targets)
	require.NoError(t, err)
	manifestJSON, err := json.Marshal(manifest)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(targetsJSON)
		case "/api/v1/remote-sync/manifest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(manifestJSON)
		case "/api/v1/remote-sync/archive":
			w.Header().Set("Content-Type", "application/x-tar")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	database, err := db.Open(filepath.Join(t.TempDir(), "active.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.NoError(t, database.ReplaceRemoteSkippedFiles("devbox", map[string]int64{
		remoteFile: mtime.UnixNano(),
	}))
	hs := HTTPSync{
		Host: "devbox", URL: server.URL, DataDir: t.TempDir(), DB: database,
	}
	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	assert.FileExists(t, remappedRemotePath(prepared.Root(), remoteFile))
	contributor, err := prepared.RebuildContributor()
	require.NoError(t, err)

	localEngine := syncpkg.NewEngine(database, syncpkg.EngineConfig{})
	t.Cleanup(localEngine.Close)
	stats, err := localEngine.ResyncAllWithOptions(
		context.Background(), nil,
		syncpkg.RebuildOptions{Contributors: []syncpkg.RebuildContributor{contributor}},
	)
	require.NoError(t, err)
	assert.False(t, stats.Aborted)

	full, err := database.GetSessionFull(context.Background(), "devbox~"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, full)
	assert.Equal(t, "devbox", full.Machine)
	require.NotNil(t, full.FilePath)
	assert.Equal(t, "devbox:"+remoteFile, *full.FilePath)
	messages, err := database.GetMessages(
		context.Background(), "devbox~"+sessionID, 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "remote rebuild searchable", messages[0].Content)
	search, err := database.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "searchable", Limit: 10, IncludeOneShot: true,
	})
	require.NoError(t, err)
	require.Len(t, search.Matches, 1)
	assert.Equal(t, "devbox~"+sessionID, search.Matches[0].SessionID)
	remoteCache, err := database.LoadRemoteSkippedFiles("devbox")
	require.NoError(t, err)
	require.Len(t, remoteCache, 1)
	for cachedPath := range remoteCache {
		assert.True(t, strings.HasPrefix(
			cachedPath, skippedFile+"?source_hash=",
		), "rowless Claude skips must persist their content hash")
	}
	assert.NotContains(t, remoteCache, remoteFile,
		"a full contributor must not load the active database's stale host cache")

	prepared.targets = TargetSet{Dirs: map[parser.AgentType][]string{
		parser.AgentClaude: {skippedDir},
	}}
	activeStats, err := prepared.ImportActive(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, activeStats.Failed)
	assert.Equal(t, 1, activeStats.Skipped)
}

func TestPreparedHTTPSyncImportActiveImportsPreparedRoot(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "session.jsonl",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "prepared import")
	database, hs := newMirrorSync(t, remote, t.TempDir())

	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	before, err := database.ListSessions(context.Background(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, before.Sessions, "Prepare must not import active sessions")

	stats, err := prepared.ImportActive(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	after, err := database.ListSessions(context.Background(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, after.Sessions, 1)
	assert.Equal(t, "devbox", after.Sessions[0].Machine)
}

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
