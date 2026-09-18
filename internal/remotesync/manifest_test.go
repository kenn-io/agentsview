package remotesync

import (
	"archive/tar"
	"bytes"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestBuildManifestListsRegularFilesWithSizeAndMtime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	sub := filepath.Join(dir, "proj")
	require.NoError(os.MkdirAll(sub, 0o755))
	a := filepath.Join(sub, "a.jsonl")
	require.NoError(os.WriteFile(a, []byte("aaaa"), 0o644))
	mtime := time.Date(2026, 7, 8, 9, 0, 0, 987654321, time.UTC)
	require.NoError(os.Chtimes(a, mtime, mtime))
	require.NoError(os.Symlink(a, filepath.Join(sub, "link.jsonl")))
	extra := filepath.Join(dir, "index.jsonl")
	require.NoError(os.WriteFile(extra, []byte("x"), 0o644))

	m, err := BuildManifest(TargetSet{
		Dirs:       map[parser.AgentType][]string{parser.AgentClaude: {sub}},
		ExtraFiles: []string{extra},
	})
	require.NoError(err)

	// Sorted by path: <tmp>/index.jsonl precedes <tmp>/proj/a.jsonl.
	require.Len(m.Files, 2)
	assert.Equal(extra, m.Files[0].Path)
	assert.Equal(a, m.Files[1].Path)
	assert.Equal(int64(4), m.Files[1].Size)
	info, err := os.Stat(a)
	require.NoError(err)
	assert.Equal(info.ModTime().UnixNano(), m.Files[1].MtimeNS)
}

func TestBuildManifestToleratesMissingRootsAndExtraFiles(t *testing.T) {
	dir := t.TempDir()
	m, err := BuildManifest(TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {filepath.Join(dir, "gone")},
		},
		ExtraFiles: []string{filepath.Join(dir, "gone.jsonl")},
	})
	require.NoError(t, err)
	assert.Empty(t, m.Files)
}

func TestBuildManifestPrunesForbiddenRootNestedInAllowedRoot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	allowed := filepath.Join(root, "sessions")
	forbidden := filepath.Join(allowed, ".forbidden-provider")
	keep := filepath.Join(allowed, "session.jsonl")
	secret := filepath.Join(forbidden, "chat.db")
	require.NoError(os.MkdirAll(forbidden, 0o755))
	require.NoError(os.WriteFile(keep, []byte("session"), 0o644))
	require.NoError(os.WriteFile(secret, []byte("authentication state"), 0o600))

	manifest, err := BuildManifest(TargetSet{
		Dirs:           map[parser.AgentType][]string{parser.AgentClaude: {allowed}},
		ForbiddenRoots: []string{forbidden},
	})

	require.NoError(err)
	require.Len(manifest.Files, 1)
	assert.Equal(keep, manifest.Files[0].Path)
	assert.NotEqual(secret, manifest.Files[0].Path,
		"the manifest must never advertise a file under a forbidden root")
}

func TestBuildManifestRejectsFileScopedAgents(t *testing.T) {
	_, err := BuildManifest(TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentWindsurf: {"/srv/Windsurf/User"}},
		Files: map[parser.AgentType][]string{
			parser.AgentWindsurf: {"/srv/Windsurf/User/workspaceStorage/a/state.vscdb"},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file-scoped")
}

func TestHermesManifestMatchesStandaloneDatabaseSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	stateDB := filepath.Join(t.TempDir(), "profile", "state.db")
	require.NoError(os.MkdirAll(filepath.Dir(stateDB), 0o755))
	sessionsDir := filepath.Join(filepath.Dir(stateDB), "sessions")
	require.NoError(os.MkdirAll(sessionsDir, 0o755))
	writeHermesImportStateDB(t, stateDB)
	writer, err := sql.Open("sqlite3", stateDB)
	require.NoError(err)
	t.Cleanup(func() { _ = writer.Close() })
	var journalMode string
	require.NoError(writer.QueryRowContext(t.Context(), `PRAGMA journal_mode = WAL`).Scan(&journalMode))
	assert.Equal("wal", journalMode)
	_, err = writer.ExecContext(t.Context(), `PRAGMA wal_autocheckpoint = 0`)
	require.NoError(err)
	_, err = writer.ExecContext(t.Context(), `
		UPDATE sessions
		SET title = 'Manifest includes WAL commit'
		WHERE id = 'database-only'
	`)
	require.NoError(err)
	wal := stateDB + "-wal"
	require.FileExists(wal)
	walTime := time.Now().Add(2 * time.Second)
	require.NoError(os.Chtimes(wal, walTime, walTime))
	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		ExtraFiles: append([]string{stateDB}, hermesTestSidecars(stateDB)...),
	}

	manifest, err := BuildManifest(targets)
	require.NoError(err)
	require.Len(manifest.Files, 1)
	assert.Equal(stateDB, manifest.Files[0].Path)

	var archive bytes.Buffer
	require.NoError(WriteArchive(t.Context(), &archive, targets))
	extracted := t.TempDir()
	_, err = ExtractTarStream(t.Context(), &archive, extracted)
	require.NoError(err)
	extractedDB, err := safeRemappedRemotePath(extracted, stateDB)
	require.NoError(err)
	info, err := os.Stat(extractedDB)
	require.NoError(err)
	assert.Equal(manifest.Files[0].Size, info.Size())
	assert.Equal(manifest.Files[0].MtimeNS, info.ModTime().UnixNano())
}

func TestInvalidHermesStateDBDoesNotBlockTranscriptArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	profile := t.TempDir()
	sessionsDir := filepath.Join(profile, "sessions")
	require.NoError(os.MkdirAll(sessionsDir, 0o755))
	transcript := filepath.Join(sessionsDir, "session.json")
	require.NoError(os.WriteFile(transcript, []byte(`{"id":"session"}`), 0o644))
	stateDB := filepath.Join(profile, "state.db")
	require.NoError(os.WriteFile(stateDB, []byte("not sqlite"), 0o644))
	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		ExtraFiles: append([]string{stateDB}, hermesTestSidecars(stateDB)...),
	}

	manifest, err := BuildManifest(targets)
	require.NoError(err)
	require.Len(manifest.Files, 1)
	assert.Equal(transcript, manifest.Files[0].Path)

	var archive bytes.Buffer
	require.NoError(WriteArchive(t.Context(), &archive, targets))
	tr := tar.NewReader(&archive)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(err)
		names = append(names, hdr.Name)
	}
	assert.Contains(names, archiveNameForTest(t, transcript))
	assert.NotContains(names, archiveNameForTest(t, stateDB))
}
