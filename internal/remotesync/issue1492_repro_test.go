package remotesync

import (
	"archive/tar"
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

func resolveTargetsForTest(t *testing.T, cfg config.Config) TargetSet {
	t.Helper()
	targets, err := ResolveTargets(cfg)
	require.NoError(t, err)
	return targets
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

	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
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

	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
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
	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
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
			stale := resolveTargetsForTest(t, cfg)
			require.Contains(t, stale.Files[tt.agent], path)
			require.NoError(t, os.Remove(path))

			fresh := resolveTargetsForTest(t, cfg)
			require.Contains(t, fresh.Dirs[tt.agent], root)
			files, ok := SelectAllowedFiles(fresh, []string{path})
			require.True(t, ok, "vanished %s file must remain authorized", tt.name)
			var delta bytes.Buffer
			require.NoError(t, WriteArchiveFiles(&delta, fresh, files))
			assert.Empty(t, archiveEntries(t, delta.Bytes()))
		})
	}
}

func TestIssue1492AllCuratedEditorFilesVanishedRemainAuthorized(t *testing.T) {
	id := "01234567-89ab-cdef-0123-456789abcdef"
	tests := []struct {
		name  string
		agent parser.AgentType
		path  string
	}{
		{"cursor", parser.AgentCursor, filepath.Join("project", "agent-transcripts", id+".jsonl")},
		{"vscode", parser.AgentVSCodeCopilot, filepath.Join("workspaceStorage", "hash", "chatSessions", id+".json")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tt.path)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte("session"), 0o644))
			cfg := config.Config{AgentDirs: map[parser.AgentType][]string{tt.agent: {root}}}
			stale := resolveTargetsForTest(t, cfg)
			require.NoError(t, os.Remove(path))
			fresh := resolveTargetsForTest(t, cfg)
			selected, ok := SelectAllowedTargets(fresh, stale)
			require.True(t, ok, "vanished %s target must remain authorized", tt.name)
			manifest, err := BuildManifest(selected)
			require.NoError(t, err)
			assert.Empty(t, manifest.Files)
			files, ok := SelectAllowedFiles(fresh, []string{path})
			require.True(t, ok, "vanished %s delta must remain authorized", tt.name)
			var delta bytes.Buffer
			require.NoError(t, WriteArchiveFiles(&delta, fresh, files))
			assert.Empty(t, archiveEntries(t, delta.Bytes()))
		})
	}
}

func TestIssue1492VanishedVSCodeWorkspaceIsEvictedFromMirror(t *testing.T) {
	root := t.TempDir()
	workspaceDir := filepath.Join(root, "workspaceStorage", "hash")
	workspace := filepath.Join(workspaceDir, "workspace.json")
	chat := filepath.Join(workspaceDir, "chatSessions", "01234567-89ab-cdef-0123-456789abcdef.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(chat), 0o755))
	require.NoError(t, os.WriteFile(workspace, []byte(`{"folder":"/repo"}`), 0o644))
	require.NoError(t, os.WriteFile(chat, []byte(`{"id":"chat"}`), 0o644))
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentVSCodeCopilot: {root},
	}}
	initial := resolveTargetsForTest(t, cfg)
	require.Contains(t, initial.Files[parser.AgentVSCodeCopilot], workspace)

	mirror := t.TempDir()
	mirrorWorkspace, err := safeRemappedRemotePath(mirror, workspace)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorWorkspace), 0o755))
	require.NoError(t, os.WriteFile(mirrorWorkspace, []byte("stale"), 0o644))
	require.NoError(t, os.Remove(workspace))
	fresh := resolveTargetsForTest(t, cfg)
	selected, ok := SelectAllowedFiles(fresh, []string{workspace})
	require.True(t, ok, "vanished workspace.json must remain authorized")
	var delta bytes.Buffer
	require.NoError(t, WriteArchiveFiles(&delta, fresh, selected))
	assert.Empty(t, archiveEntries(t, delta.Bytes()))

	manifest, err := BuildManifest(fresh)
	require.NoError(t, err)
	diff, err := MirrorDiff(mirror, manifest)
	require.NoError(t, err)
	assert.Equal(t, []string{mirrorWorkspace}, diff.Deletions)
}

func TestIssue1492VSCodeWorkspaceMetadataRequiresSelectedChat(t *testing.T) {
	root := t.TempDir()
	workspaceDir := filepath.Join(root, "workspaceStorage", "allowed")
	chat := filepath.Join(workspaceDir, "chatSessions", "01234567-89ab-cdef-0123-456789abcdef.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(chat), 0o755))
	require.NoError(t, os.WriteFile(chat, []byte(`{"id":"chat"}`), 0o644))
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentVSCodeCopilot: {root},
	}}
	allowed := resolveTargetsForTest(t, cfg)
	unauthorized := filepath.Join(root, "workspaceStorage", "other", "workspace.json")
	_, ok := SelectAllowedFiles(allowed, []string{unauthorized})
	assert.False(t, ok)

	requested := TargetSet{
		Dirs:  map[parser.AgentType][]string{parser.AgentVSCodeCopilot: {root}},
		Files: map[parser.AgentType][]string{parser.AgentVSCodeCopilot: {chat, unauthorized}},
	}
	_, ok = SelectAllowedTargets(allowed, requested)
	assert.False(t, ok)
}

func TestIssue1492EmptyEditorRootsRetainEmptyFileTargets(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{filepath.Join(root, "cursor"), filepath.Join(root, "vscode")} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCursor:        {filepath.Join(root, "cursor")},
		parser.AgentVSCodeCopilot: {filepath.Join(root, "vscode")},
	}})

	assert.Contains(t, targets.Dirs[parser.AgentCursor], filepath.Join(root, "cursor"))
	assert.Contains(t, targets.Dirs[parser.AgentVSCodeCopilot], filepath.Join(root, "vscode"))
	assert.Empty(t, targets.Files[parser.AgentCursor])
	assert.Empty(t, targets.Files[parser.AgentVSCodeCopilot])
	_, cursorMarker := targets.Files[parser.AgentCursor]
	_, vscodeMarker := targets.Files[parser.AgentVSCodeCopilot]
	assert.True(t, cursorMarker)
	assert.True(t, vscodeMarker)

	data, err := json.Marshal(targets)
	require.NoError(t, err)
	var roundTrip TargetSet
	require.NoError(t, json.Unmarshal(data, &roundTrip))
	_, cursorMarker = roundTrip.Files[parser.AgentCursor]
	_, vscodeMarker = roundTrip.Files[parser.AgentVSCodeCopilot]
	assert.True(t, cursorMarker)
	assert.True(t, vscodeMarker)

	allowedSibling := filepath.Join(root, "allowed")
	filtered := filterForbiddenTargets(TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentCursor: {allowedSibling, filepath.Join(root, "forbidden")},
		},
		Files:          map[parser.AgentType][]string{parser.AgentCursor: {}},
		ForbiddenRoots: []string{filepath.Join(root, "forbidden")},
	})
	_, marker := filtered.Files[parser.AgentCursor]
	assert.True(t, marker)
	filtered = filterForbiddenTargets(TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentCursor: {filepath.Join(root, "forbidden")},
		},
		Files:          map[parser.AgentType][]string{parser.AgentCursor: {}},
		ForbiddenRoots: []string{filepath.Join(root, "forbidden")},
	})
	_, marker = filtered.Files[parser.AgentCursor]
	assert.False(t, marker)
}

func TestIssue1492CuratedEditorRootsAccumulateFiles(t *testing.T) {
	root := t.TempDir()
	cursorRoots := []string{filepath.Join(root, "cursor-a"), filepath.Join(root, "cursor-b")}
	vscodeRoots := []string{filepath.Join(root, "vscode-a"), filepath.Join(root, "vscode-b")}
	for i, cursorRoot := range cursorRoots {
		path := filepath.Join(cursorRoot, "project", "agent-transcripts",
			fmt.Sprintf("01234567-89ab-cdef-0123-456789abcde%d.jsonl", i))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("cursor"), 0o644))
	}
	for i, vscodeRoot := range vscodeRoots {
		workspaceDir := filepath.Join(vscodeRoot, "workspaceStorage", fmt.Sprintf("hash-%d", i))
		chat := filepath.Join(workspaceDir, "chatSessions", "01234567-89ab-cdef-0123-456789abcdef.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(chat), 0o755))
		require.NoError(t, os.WriteFile(chat, []byte(`{"id":"chat"}`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(workspaceDir, "workspace.json"), []byte(`{"folder":"/repo"}`), 0o644))
	}
	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCursor:        cursorRoots,
		parser.AgentVSCodeCopilot: vscodeRoots,
	}})

	assert.ElementsMatch(t, cursorRoots, targets.Dirs[parser.AgentCursor])
	assert.Len(t, targets.Files[parser.AgentCursor], len(cursorRoots))
	assert.ElementsMatch(t, vscodeRoots, targets.Dirs[parser.AgentVSCodeCopilot])
	assert.Len(t, targets.Files[parser.AgentVSCodeCopilot], len(vscodeRoots)*2)
}

func TestArchiveRequestPreservesEmptyFiles(t *testing.T) {
	targetSet := TargetSet{
		Dirs:  map[parser.AgentType][]string{parser.AgentCursor: {"/root"}},
		Files: map[parser.AgentType][]string{parser.AgentCursor: {}},
	}
	request := ArchiveRequest{
		TargetSet:  targetSet,
		DeltaFiles: []string{},
	}
	data, err := json.Marshal(request)
	require.NoError(t, err)
	var decoded ArchiveRequest
	require.NoError(t, json.Unmarshal(data, &decoded))
	files, ok := decoded.Files[parser.AgentCursor]
	require.True(t, ok)
	assert.Empty(t, files)
	assert.NotNil(t, decoded.DeltaFiles)
}

func TestSelectAllowedTargetsRootOnlyCuratedRequestIsEmptyAndNoFilesystemAccess(t *testing.T) {
	root := t.TempDir()
	allowed := TargetSet{
		Dirs:  map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Files: map[parser.AgentType][]string{parser.AgentCursor: {}},
	}
	requested := TargetSet{
		Dirs:  map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Files: map[parser.AgentType][]string{parser.AgentCursor: {`/attacker/evil`}},
	}
	var touched []string
	orig := evalSymlinksFn
	evalSymlinksFn = func(path string) (string, error) {
		if strings.Contains(path, "attacker") {
			touched = append(touched, path)
		}
		return orig(path)
	}
	t.Cleanup(func() { evalSymlinksFn = orig })
	_, ok := SelectAllowedTargets(allowed, requested)
	assert.False(t, ok)
	assert.Empty(t, touched)

	forbiddenAllowed := TargetSet{
		Dirs:           map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Files:          map[parser.AgentType][]string{parser.AgentCursor: {}},
		ForbiddenRoots: []string{root},
	}
	stale := TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
		Files: map[parser.AgentType][]string{parser.AgentCursor: {
			filepath.Join(root, "project", "agent-transcripts", "01234567-89ab-cdef-0123-456789abcdef.jsonl"),
		}},
	}
	_, ok = SelectAllowedTargets(forbiddenAllowed, stale)
	assert.False(t, ok, "stale curated files under forbidden roots must stay rejected")
	_, ok = SelectAllowedFiles(forbiddenAllowed, stale.Files[parser.AgentCursor])
	assert.False(t, ok, "stale delta files under forbidden roots must stay rejected")

	selected, ok := SelectAllowedTargets(allowed, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentCursor: {root}},
	})
	require.True(t, ok)
	_, marker := selected.Files[parser.AgentCursor]
	assert.True(t, marker)
	assert.Empty(t, selected.Files[parser.AgentCursor])
}

func TestSelectAllowedTargetsDirectoryOnlyRequestRequiresCurrentCuratedFiles(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		agent parser.AgentType
		file  string
	}{
		{agent: parser.AgentCursor, file: filepath.Join(root, "cursor", "chat.jsonl")},
		{agent: parser.AgentVSCodeCopilot, file: filepath.Join(root, "vscode", "chat.json")},
		{agent: parser.AgentRooCode, file: filepath.Join(root, "roo", "session.json")},
		{agent: parser.AgentZed, file: filepath.Join(root, parser.ZedThreadsDBRelPath)},
	}
	for _, tc := range cases {
		allowed := TargetSet{
			Dirs:  map[parser.AgentType][]string{tc.agent: {root}},
			Files: map[parser.AgentType][]string{tc.agent: {tc.file}},
		}
		requested := TargetSet{
			Dirs: map[parser.AgentType][]string{tc.agent: {root}},
		}
		_, ok := SelectAllowedTargets(allowed, requested)
		assert.False(t, ok, "%s directory-only request must not discard current curated files", tc.agent)

		requestedEmpty := TargetSet{
			Dirs:  map[parser.AgentType][]string{tc.agent: {root}},
			Files: map[parser.AgentType][]string{tc.agent: {}},
		}
		_, ok = SelectAllowedTargets(allowed, requestedEmpty)
		assert.False(t, ok,
			"%s explicitly empty file request must not discard current curated files", tc.agent)
	}
}

func TestIssue1492EmptyCuratedRootsProduceEmptyArchives(t *testing.T) {
	root := t.TempDir()
	cursorRoot := filepath.Join(root, "cursor")
	vscodeRoot := filepath.Join(root, "vscode")
	for _, path := range []string{
		filepath.Join(cursorRoot, "mcp_auth.json"),
		filepath.Join(vscodeRoot, "User", "settings.json"),
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("credential decoy"), 0o600))
	}
	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCursor:        {cursorRoot},
		parser.AgentVSCodeCopilot: {vscodeRoot},
	}})

	for _, agent := range []parser.AgentType{
		parser.AgentCursor, parser.AgentVSCodeCopilot,
	} {
		files, ok := targets.Files[agent]
		require.True(t, ok, "%s must retain its file-scope marker", agent)
		assert.Empty(t, files)
	}
	manifest, err := BuildManifest(targets)
	require.NoError(t, err)
	assert.Empty(t, manifest.Files)
	var archive bytes.Buffer
	require.NoError(t, WriteArchive(&archive, targets))
	assert.Empty(t, archiveEntries(t, archive.Bytes()))
}

func TestIssue1492VanishedZedDatabaseRemainsEvictable(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, parser.ZedThreadsDBRelPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	require.NoError(t, os.WriteFile(dbPath, []byte("selected"), 0o644))
	configured := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentZed: {root},
	}}
	initial := resolveTargetsForTest(t, configured)
	require.Equal(t, []string{dbPath}, initial.Files[parser.AgentZed])
	require.NoError(t, os.Remove(dbPath))

	fresh := resolveTargetsForTest(t, configured)
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

// A resolution error must fail the whole resolution instead of
// silently dropping the agent: an agent absent from the advertised
// targets is indistinguishable from an uninstalled one, so the client
// would evict its entire mirror subtree over a transient read error.
func TestIssue1492UnreadableCuratedRootFailsResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-permission read failures are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	id := "01234567-89ab-cdef-0123-456789abcdef"

	t.Run("vscode discovery error", func(t *testing.T) {
		root := t.TempDir()
		chat := filepath.Join(root, "workspaceStorage", "hash", "chatSessions", id+".json")
		require.NoError(t, os.MkdirAll(filepath.Dir(chat), 0o755))
		require.NoError(t, os.WriteFile(chat, []byte(`{"id":"chat"}`), 0o644))
		cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCodeCopilot: {root},
		}}
		targets := resolveTargetsForTest(t, cfg)
		require.Contains(t, targets.Files[parser.AgentVSCodeCopilot], chat)

		storage := filepath.Join(root, "workspaceStorage")
		require.NoError(t, os.Chmod(storage, 0o000))
		t.Cleanup(func() { require.NoError(t, os.Chmod(storage, 0o755)) })
		_, err := ResolveTargets(cfg)
		require.Error(t, err,
			"an unreadable editor root must not resolve to an empty target set")
		assert.ErrorIs(t, err, os.ErrPermission)
	})

	t.Run("roocode root stat error", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "roo")
		task := filepath.Join(root, "tasks", "task-1")
		require.NoError(t, os.MkdirAll(task, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(task, "history_item.json"), []byte("{}"), 0o644))
		cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
			parser.AgentRooCode: {root},
		}}
		targets := resolveTargetsForTest(t, cfg)
		require.NotEmpty(t, targets.Files[parser.AgentRooCode])

		require.NoError(t, os.Chmod(parent, 0o000))
		t.Cleanup(func() { require.NoError(t, os.Chmod(parent, 0o755)) })
		_, err := ResolveTargets(cfg)
		require.Error(t, err,
			"an unreadable RooCode root must not resolve to an empty target set")
		assert.ErrorIs(t, err, os.ErrPermission)
	})

	t.Run("kilo legacy discovery error", func(t *testing.T) {
		root := t.TempDir()
		task := filepath.Join(root, "tasks", "task-1")
		require.NoError(t, os.MkdirAll(task, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(task, "task_metadata.json"), []byte("{}"), 0o644))
		cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
			parser.AgentKiloLegacy: {root},
		}}
		targets := resolveTargetsForTest(t, cfg)
		require.NotEmpty(t, targets.Files[parser.AgentKiloLegacy])

		tasksDir := filepath.Join(root, "tasks")
		require.NoError(t, os.Chmod(tasksDir, 0o000))
		t.Cleanup(func() { require.NoError(t, os.Chmod(tasksDir, 0o755)) })
		_, err := ResolveTargets(cfg)
		require.Error(t, err,
			"an unreadable Kilo Legacy tasks directory must not resolve to an empty target set")
		assert.ErrorIs(t, err, os.ErrPermission)
	})

	t.Run("zed root stat error", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "zed")
		require.NoError(t, os.MkdirAll(filepath.Join(root, "threads"), 0o755))
		cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
			parser.AgentZed: {root},
		}}
		targets := resolveTargetsForTest(t, cfg)
		require.Contains(t, targets.Dirs[parser.AgentZed], root)

		require.NoError(t, os.Chmod(parent, 0o000))
		t.Cleanup(func() { require.NoError(t, os.Chmod(parent, 0o755)) })
		_, err := ResolveTargets(cfg)
		require.Error(t, err,
			"an unreadable Zed root must not resolve to an empty target set")
		assert.ErrorIs(t, err, os.ErrPermission)
	})
}

// A curated file whose stat fails for any reason other than absence
// must fail resolution instead of being silently omitted, or the next
// manifest would evict the client's cached copy.
func TestIssue1492UnreadableCuratedFilePropagatesStatError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-permission read failures are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	path := filepath.Join(sub, "session.json")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	regular, err := regularCuratedFile(root, path)
	require.NoError(t, err)
	assert.True(t, regular)

	require.NoError(t, os.Chmod(sub, 0o000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(sub, 0o755)) })
	_, err = regularCuratedFile(root, path)
	require.Error(t, err, "an unreadable curated file must not be silently omitted")
	assert.ErrorIs(t, err, os.ErrPermission)
}

// A corrupt Zed database must not block the whole sync: the archive
// database preserves already-imported sessions, so the unreadable file
// degrades to a missing manifest entry (the mirror evicts its cached
// copy) while every other agent keeps syncing.
func TestIssue1492CorruptZedDatabaseDoesNotBlockOtherAgents(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, parser.ZedThreadsDBRelPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	require.NoError(t, os.WriteFile(dbPath, []byte("corrupt sqlite database"), 0o644))
	cursorRoot := filepath.Join(root, "cursor")
	cursorFile := filepath.Join(cursorRoot, "project", "agent-transcripts",
		"01234567-89ab-cdef-0123-456789abcdef.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(cursorFile), 0o755))
	require.NoError(t, os.WriteFile(cursorFile, []byte("cursor transcript"), 0o644))
	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentZed:    {root},
			parser.AgentCursor: {cursorRoot},
		},
		Files: map[parser.AgentType][]string{
			parser.AgentZed:    {dbPath},
			parser.AgentCursor: {cursorFile},
		},
	}
	manifest, err := BuildManifest(targets)
	require.NoError(t, err)
	assert.Equal(t, []string{cursorFile}, manifestPaths(manifest))

	var archive bytes.Buffer
	require.NoError(t, WriteArchive(&archive, targets))
	entries := archiveEntries(t, archive.Bytes())
	require.Len(t, entries, 1)
	assert.NotContains(t, strings.Join(keys(entries), "\n"), "threads.db")
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
	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
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
