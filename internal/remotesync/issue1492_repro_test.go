package remotesync

import (
	"archive/tar"
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

func archiveEntries(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	entries := make(map[string][]byte)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries
		}
		require.NoError(t, err)
		body, err := io.ReadAll(tr)
		require.NoError(t, err)
		entries[hdr.Name] = body
	}
}

func manifestPaths(m Manifest) []string {
	paths := make([]string, 0, len(m.Files))
	for _, file := range m.Files {
		paths = append(paths, file.Path)
	}
	sort.Strings(paths)
	return paths
}

func TestIssue1492CuratesCursorAndVSCodeTargets(t *testing.T) {
	root := t.TempDir()
	id := "01234567-89ab-cdef-0123-456789abcdef"
	cursor := filepath.Join(root, "cursor", "project", "agent-transcripts", id+".jsonl")
	cursorDecoy := filepath.Join(root, "cursor", "project", "mcp_auth.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(cursor), 0o755))
	require.NoError(t, os.WriteFile(cursor, []byte("cursor"), 0o644))
	require.NoError(t, os.WriteFile(cursorDecoy, []byte("secret"), 0o644))

	vscodeRoot := filepath.Join(root, "vscode")
	workspaceDir := filepath.Join(vscodeRoot, "workspaceStorage", "workspace-hash")
	workspaceChat := filepath.Join(workspaceDir, "chatSessions", id+".json")
	workspaceManifest := filepath.Join(workspaceDir, "workspace.json")
	globalChat := filepath.Join(vscodeRoot, "globalStorage", "emptyWindowChatSessions", id+".jsonl")
	vscodeDecoy := filepath.Join(vscodeRoot, "globalStorage", "mcp_auth.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(workspaceChat), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(globalChat), 0o755))
	require.NoError(t, os.WriteFile(workspaceChat, []byte("workspace chat"), 0o644))
	require.NoError(t, os.WriteFile(workspaceManifest, []byte(`{"folder":"/repo"}`), 0o644))
	require.NoError(t, os.WriteFile(globalChat, []byte("global chat"), 0o644))
	require.NoError(t, os.WriteFile(vscodeDecoy, []byte("secret"), 0o644))

	targets := ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCursor:        {filepath.Join(root, "cursor")},
		parser.AgentVSCodeCopilot: {vscodeRoot},
	}})
	require.Len(t, targets.Files[parser.AgentCursor], 1)
	require.Len(t, targets.Files[parser.AgentVSCodeCopilot], 3)
	assert.Contains(t, targets.Files[parser.AgentVSCodeCopilot], workspaceManifest)
	assert.NotContains(t, strings.Join(targets.Files[parser.AgentCursor], "\n"), "mcp_auth")

	manifest, err := BuildManifest(targets)
	require.NoError(t, err)
	var archive bytes.Buffer
	require.NoError(t, WriteArchive(&archive, targets))
	paths := strings.Join(manifestPaths(manifest), "\n")
	entries := archiveEntries(t, archive.Bytes())
	assert.NotContains(t, paths, "mcp_auth.json")
	for name := range entries {
		assert.NotContains(t, name, "mcp_auth.json")
	}
	assert.Contains(t, paths, "workspace.json")
}

func TestIssue1492ZedActiveWALIsOneStableStandaloneDatabase(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, parser.ZedThreadsDBRelPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	defer writer.Close()
	_, err = writer.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE threads (id TEXT PRIMARY KEY); INSERT INTO threads VALUES ('wal-thread');`)
	require.NoError(t, err)
	walInfo, err := os.Stat(dbPath + "-wal")
	require.NoError(t, err)
	assert.Greater(t, walInfo.Size(), int64(32))

	targets := ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentZed: {root},
	}})
	require.Equal(t, []string{dbPath}, targets.Files[parser.AgentZed])
	manifest, err := BuildManifest(targets)
	require.NoError(t, err)
	require.Len(t, manifest.Files, 1)
	manifestEntry := manifest.Files[0]
	var archive bytes.Buffer
	require.NoError(t, WriteArchive(&archive, targets))
	entries := archiveEntries(t, archive.Bytes())
	require.Len(t, entries, 1)
	archiveBody, ok := entries[filepath.ToSlash(dbPath)]
	if !ok {
		for name, body := range entries {
			if strings.HasSuffix(name, "/threads/threads.db") {
				archiveBody, ok = body, true
			}
		}
	}
	require.True(t, ok)
	assert.NotContains(t, strings.Join(keys(entries), "\n"), "-wal")

	snapshot := filepath.Join(t.TempDir(), "threads.db")
	require.NoError(t, os.WriteFile(snapshot, archiveBody, 0o600))
	reader, err := sql.Open("sqlite3", snapshot)
	require.NoError(t, err)
	var id string
	require.NoError(t, reader.QueryRow("SELECT id FROM threads").Scan(&id))
	assert.Equal(t, "wal-thread", id)
	require.NoError(t, reader.Close())
	require.NoError(t, writer.Close())

	for _, entry := range manifest.Files {
		assert.Equal(t, manifestEntry.Size, entry.Size)
		assert.Equal(t, manifestEntry.MtimeNS, entry.MtimeNS)
	}
}

func TestIssue1492VanishedCuratedFileIsOmittedAndDeltaIsConfined(t *testing.T) {
	root := t.TempDir()
	id := "01234567-89ab-cdef-0123-456789abcdef"
	selected := filepath.Join(root, "project", "agent-transcripts", id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(selected), 0o755))
	require.NoError(t, os.WriteFile(selected, []byte("session"), 0o644))
	targets := ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCursor: {root},
	}})
	require.NoError(t, os.Remove(selected))
	manifest, err := BuildManifest(targets)
	require.NoError(t, err)
	assert.Empty(t, manifest.Files)
	var archive bytes.Buffer
	require.NoError(t, WriteArchive(&archive, targets))
	assert.Empty(t, archiveEntries(t, archive.Bytes()))

	_, ok := SelectAllowedFiles(targets, []string{filepath.Join(root, "project", "mcp_auth.json")})
	assert.False(t, ok)
}

func TestIssue1492VanishedCursorAndVSCodeFilesRemainAuthorized(t *testing.T) {
	id := "01234567-89ab-cdef-0123-456789abcdef"
	tests := []struct {
		name  string
		agent parser.AgentType
		path  func(string) string
	}{
		{
			name:  "cursor",
			agent: parser.AgentCursor,
			path: func(root string) string {
				return filepath.Join(root, "project", "agent-transcripts", id+".jsonl")
			},
		},
		{
			name:  "vscode",
			agent: parser.AgentVSCodeCopilot,
			path: func(root string) string {
				return filepath.Join(root, "workspaceStorage", "hash", "chatSessions", id+".json")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := tt.path(root)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte("session"), 0o644))
			cfg := config.Config{AgentDirs: map[parser.AgentType][]string{tt.agent: {root}}}
			stale := ResolveTargets(cfg)
			require.Contains(t, stale.Files[tt.agent], path)
			require.NoError(t, os.Remove(path))

			fresh := ResolveTargets(cfg)
			require.Contains(t, fresh.Dirs[tt.agent], root)
			files, ok := SelectAllowedFiles(fresh, []string{path})
			require.True(t, ok, "vanished %s file must remain authorized", tt.name)
			var delta bytes.Buffer
			require.NoError(t, WriteArchiveFiles(&delta, fresh, files))
			assert.Empty(t, archiveEntries(t, delta.Bytes()))
		})
	}
}

func TestIssue1492EmptyEditorRootsRemainWithoutEmptyFileTargets(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{filepath.Join(root, "cursor"), filepath.Join(root, "vscode")} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	targets := ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCursor:        {filepath.Join(root, "cursor")},
		parser.AgentVSCodeCopilot: {filepath.Join(root, "vscode")},
	}})

	assert.Contains(t, targets.Dirs[parser.AgentCursor], filepath.Join(root, "cursor"))
	assert.Contains(t, targets.Dirs[parser.AgentVSCodeCopilot], filepath.Join(root, "vscode"))
	assert.NotContains(t, targets.Files, parser.AgentCursor)
	assert.NotContains(t, targets.Files, parser.AgentVSCodeCopilot)
}

func TestIssue1492VanishedZedDatabaseRemainsEvictable(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, parser.ZedThreadsDBRelPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	require.NoError(t, os.WriteFile(dbPath, []byte("selected"), 0o644))
	configured := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentZed: {root},
	}}
	initial := ResolveTargets(configured)
	require.Equal(t, []string{dbPath}, initial.Files[parser.AgentZed])
	require.NoError(t, os.Remove(dbPath))

	fresh := ResolveTargets(configured)
	assert.Equal(t, []string{root}, fresh.Dirs[parser.AgentZed])
	assert.Equal(t, []string{dbPath}, fresh.Files[parser.AgentZed])
	manifest, err := BuildManifest(fresh)
	require.NoError(t, err)
	assert.Empty(t, manifest.Files)

	selected, ok := SelectAllowedFiles(fresh, []string{dbPath})
	require.True(t, ok)
	var delta bytes.Buffer
	require.NoError(t, WriteArchiveFiles(&delta, fresh, selected))
	assert.Empty(t, archiveEntries(t, delta.Bytes()))

	mirror := t.TempDir()
	mirrorPath, err := safeRemappedRemotePath(mirror, dbPath)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorPath), 0o755))
	require.NoError(t, os.WriteFile(mirrorPath, []byte("stale"), 0o644))
	diff, err := MirrorDiff(mirror, manifest)
	require.NoError(t, err)
	assert.Equal(t, []string{mirrorPath}, diff.Deletions)
}

func TestIssue1492ZedSnapshotDeltaUsesOnlineBackup(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, parser.ZedThreadsDBRelPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	_, err = writer.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE threads (id TEXT PRIMARY KEY); INSERT INTO threads VALUES ('delta-thread')`)
	require.NoError(t, err)
	targets := ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentZed: {root},
	}})
	files, ok := SelectAllowedFiles(targets, []string{dbPath})
	require.True(t, ok, "Zed snapshot must be authorized as a delta file")
	var delta bytes.Buffer
	require.NoError(t, WriteArchiveFiles(&delta, targets, files))
	entries := archiveEntries(t, delta.Bytes())
	require.Len(t, entries, 1)
	var snapshot []byte
	for name, body := range entries {
		if strings.HasSuffix(name, "/threads/threads.db") {
			snapshot = body
		}
	}
	require.NotEmpty(t, snapshot)
	snapshotPath := filepath.Join(t.TempDir(), "threads.db")
	require.NoError(t, os.WriteFile(snapshotPath, snapshot, 0o600))
	reader, err := sql.Open("sqlite3", snapshotPath)
	require.NoError(t, err)
	var id string
	require.NoError(t, reader.QueryRow("SELECT id FROM threads").Scan(&id))
	assert.Equal(t, "delta-thread", id)
	require.NoError(t, reader.Close())
}

func keys(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
