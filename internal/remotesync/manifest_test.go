package remotesync

import (
	"archive/tar"
	"bytes"
	"context"
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
	dir := t.TempDir()
	sub := filepath.Join(dir, "proj")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	a := filepath.Join(sub, "a.jsonl")
	require.NoError(t, os.WriteFile(a, []byte("aaaa"), 0o644))
	mtime := time.Date(2026, 7, 8, 9, 0, 0, 987654321, time.UTC)
	require.NoError(t, os.Chtimes(a, mtime, mtime))
	require.NoError(t, os.Symlink(a, filepath.Join(sub, "link.jsonl")))
	extra := filepath.Join(dir, "index.jsonl")
	require.NoError(t, os.WriteFile(extra, []byte("x"), 0o644))

	m, err := BuildManifest(TargetSet{
		Dirs:       map[parser.AgentType][]string{parser.AgentClaude: {sub}},
		ExtraFiles: []string{extra},
	})
	require.NoError(t, err)

	// Sorted by path: <tmp>/index.jsonl precedes <tmp>/proj/a.jsonl.
	require.Len(t, m.Files, 2)
	assert.Equal(t, extra, m.Files[0].Path)
	assert.Equal(t, a, m.Files[1].Path)
	assert.Equal(t, int64(4), m.Files[1].Size)
	info, err := os.Stat(a)
	require.NoError(t, err)
	assert.Equal(t, info.ModTime().UnixNano(), m.Files[1].MtimeNS)
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
	stateDB := filepath.Join(t.TempDir(), "profile", "state.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(stateDB), 0o755))
	sessionsDir := filepath.Join(filepath.Dir(stateDB), "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	writeHermesImportStateDB(t, stateDB)
	writer, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	var journalMode string
	require.NoError(t,
		writer.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode))
	assert.Equal(t, "wal", journalMode)
	_, err = writer.Exec(`PRAGMA wal_autocheckpoint = 0`)
	require.NoError(t, err)
	_, err = writer.Exec(`
		UPDATE sessions
		SET title = 'Manifest includes WAL commit'
		WHERE id = 'database-only'
	`)
	require.NoError(t, err)
	wal := stateDB + "-wal"
	require.FileExists(t, wal)
	walTime := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(wal, walTime, walTime))
	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		ExtraFiles: append([]string{stateDB}, hermesTestSidecars(stateDB)...),
	}

	manifest, err := BuildManifest(targets)
	require.NoError(t, err)
	require.Len(t, manifest.Files, 1)
	assert.Equal(t, stateDB, manifest.Files[0].Path)

	var archive bytes.Buffer
	require.NoError(t, WriteArchive(&archive, targets))
	extracted := t.TempDir()
	_, err = ExtractTarStream(context.Background(), &archive, extracted)
	require.NoError(t, err)
	extractedDB, err := safeRemappedRemotePath(extracted, stateDB)
	require.NoError(t, err)
	info, err := os.Stat(extractedDB)
	require.NoError(t, err)
	assert.Equal(t, manifest.Files[0].Size, info.Size())
	assert.Equal(t, manifest.Files[0].MtimeNS, info.ModTime().UnixNano())
}

func TestInvalidHermesStateDBDoesNotBlockTranscriptArchive(t *testing.T) {
	profile := t.TempDir()
	sessionsDir := filepath.Join(profile, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	transcript := filepath.Join(sessionsDir, "session.json")
	require.NoError(t, os.WriteFile(transcript, []byte(`{"id":"session"}`), 0o644))
	stateDB := filepath.Join(profile, "state.db")
	require.NoError(t, os.WriteFile(stateDB, []byte("not sqlite"), 0o644))
	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		ExtraFiles: append([]string{stateDB}, hermesTestSidecars(stateDB)...),
	}

	manifest, err := BuildManifest(targets)
	require.NoError(t, err)
	require.Len(t, manifest.Files, 1)
	assert.Equal(t, transcript, manifest.Files[0].Path)

	var archive bytes.Buffer
	require.NoError(t, WriteArchive(&archive, targets))
	tr := tar.NewReader(&archive)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
	}
	assert.Contains(t, names, archiveNameForTest(t, transcript))
	assert.NotContains(t, names, archiveNameForTest(t, stateDB))
}
