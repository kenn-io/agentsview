package archivesource

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawarchive"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/docbank/docbank"
)

func TestAttachedRestoredDirectory(t *testing.T) {
	for _, layout := range []string{"agentsview", "docbank"} {
		t.Run(layout, func(t *testing.T) { testAttachedRestoredDirectory(t, layout) })
	}
}

func testAttachedRestoredDirectory(t *testing.T, layout string) {
	t.Helper()
	source, inputs := archiveFixture(t)
	directory := filepath.Join(t.TempDir(), "restored")
	application := ""
	if layout == "docbank" {
		repo, err := docbank.OpenBackupRepository(source.Repository)
		require.NoError(t, err)
		_, err = repo.Restore(t.Context(), docbank.BackupRestoreOptions{SnapshotID: source.Snapshot, Target: directory})
		require.NoError(t, err)
		application = "application"
	} else {
		_, err := rawarchive.Restore(t.Context(), source.Repository, source.Snapshot, directory, nil)
		require.NoError(t, err)
	}
	require.NoError(t, os.RemoveAll(source.Repository))
	require.NoError(t, os.RemoveAll(inputs))
	source = config.ArchiveSource{
		Name: "history", Directory: directory,
		MachineName: "Previous workstation",
	}
	saved, err := os.ReadFile(filepath.Join(directory, application, "sessions.db"))
	require.NoError(t, err)
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}, ArchiveContent: config.ArchiveContentFull}
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	manager, err := NewManager(t.Context(), cfg, receiver, engine, nil)
	require.NoError(t, err)
	require.NoError(t, drainError(manager, t.Context(), nil))
	manager.Close()
	for _, pair := range []struct{ native, content string }{
		{"019eb791-cf7d-75c1-8439-9ed74c122e02", "saved Claude question"},
		{"codex:019eb791-cf7d-75c1-8439-9ed74c122e02", "saved Codex question"},
	} {
		session, err := receiver.GetSessionFull(t.Context(), sourceDevice+"~"+pair.native)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, sourceDevice, session.Machine)
		messages, err := receiver.GetAllMessages(t.Context(), session.ID)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		assert.Equal(t, pair.content, messages[0].Content)
	}
	labels, err := receiver.GetMachineLabels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Previous workstation", labels[sourceDevice])
	// Renaming and moving the archive change neither ownership nor completion.
	moved := filepath.Join(t.TempDir(), "moved")
	require.NoError(t, os.Rename(directory, moved))
	cfg.ArchiveSources[0].Directory = moved
	cfg.ArchiveSources[0].MachineName = "Saved workstation"
	copies := 0
	manager, err = NewManager(t.Context(), cfg, receiver, engine, func(context.Context) error { copies++; return nil })
	require.NoError(t, err)
	require.NoError(t, drainError(manager, t.Context(), nil))
	manager.Close()
	assert.Zero(t, copies)
	labels, err = receiver.GetMachineLabels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Saved workstation", labels[sourceDevice])
	cfg.ArchiveSources[0].MachineName = ""
	manager, err = NewManager(t.Context(), cfg, receiver, engine, nil)
	require.NoError(t, err)
	require.NoError(t, drainError(manager, t.Context(), nil))
	manager.Close()
	labels, err = receiver.GetMachineLabels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "retired-desktop", labels[sourceDevice], "omitting machine_name restores the captured name")
	options, err := RebuildContributors(t.Context(), cfg)
	require.NoError(t, err)
	stats, err := engine.ResyncAllWithOptions(t.Context(), nil, options)
	require.NoError(t, err)
	assert.False(t, stats.Aborted)
	assert.Equal(t, 2, stats.Synced)
	after, err := os.ReadFile(filepath.Join(moved, application, "sessions.db"))
	require.NoError(t, err)
	assert.Equal(t, sha256.Sum256(saved), sha256.Sum256(after), "source database is never migrated or rewritten")
}

func TestAttachedDirectoryRefusesActiveWriter(t *testing.T) {
	source, inputs := archiveFixture(t)
	source = config.ArchiveSource{Name: source.Name, Directory: filepath.Join(inputs, "seed")}
	lock := flock.New(filepath.Join(source.Directory, "db.write.lock"))
	locked, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	defer lock.Close()
	_, err = OpenIndex(t.Context(), source, t.TempDir(), nil)
	require.ErrorContains(t, err, "in use")
}

func TestAttachedDirectoryMachineNameRequiresOneMachine(t *testing.T) {
	_, inputs := archiveFixture(t)
	directory := filepath.Join(inputs, "seed")
	saved, err := db.OpenIsolatedContext(t.Context(), filepath.Join(directory, "sessions.db"))
	require.NoError(t, err)
	const otherDevice = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	require.NoError(t, saved.RegisterRawArchiveDevice(t.Context(), otherDevice, "other-desktop", nil))
	require.NoError(t, saved.RegisterRawArchiveRoot(t.Context(), db.RawArchiveRoot{
		ID: "other-root", DeviceID: otherDevice, Machine: "other-desktop", Provider: "files",
		OriginalPath: "/archive/other", ConfiguredRootID: "other-root",
	}))
	require.NoError(t, saved.Close())
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{{
		Name: "history", Directory: directory, MachineName: "Previous workstation",
	}}}
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	manager, err := NewManager(t.Context(), cfg, receiver, engine, nil)
	require.NoError(t, err)
	require.ErrorContains(t, drainError(manager, t.Context(), nil), "machine_name requires a single-machine archive")
	manager.Close()
	cfg.ArchiveSources[0].MachineName = ""
	manager, err = NewManager(t.Context(), cfg, receiver, engine, nil)
	require.NoError(t, err)
	defer manager.Close()
	require.NoError(t, drainError(manager, t.Context(), nil))
	labels, err := receiver.GetMachineLabels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "retired-desktop", labels[sourceDevice])
}

func TestArchiveSwitchStorageKeepsIdentityAndCuration(t *testing.T) {
	source, inputs := archiveFixture(t)
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}}
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	manager, err := NewManager(t.Context(), cfg, receiver, engine, nil)
	require.NoError(t, err)
	require.NoError(t, drainError(manager, t.Context(), nil))
	manager.Close()
	const id = sourceDevice + "~codex:019eb791-cf7d-75c1-8439-9ed74c122e02"
	require.NoError(t, receiver.RenameSession(t.Context(), id, new("Keep this title")))
	before, err := receiver.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	cfg.ArchiveSources = []config.ArchiveSource{{Name: "history", Directory: filepath.Join(inputs, "seed")}}
	require.NoError(t, os.RemoveAll(source.Repository))
	manager, err = NewManager(t.Context(), cfg, receiver, engine, nil)
	require.NoError(t, err)
	defer manager.Close()
	require.NoError(t, drainError(manager, t.Context(), nil))
	after, err := receiver.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.Len(t, after, 1)
	assert.Equal(t, before[0].Content, after[0].Content)
	session, err := receiver.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, new("Keep this title"), session.DisplayName)
	var sessions int
	require.NoError(t, receiver.Update(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&sessions)
	}))
	assert.Equal(t, 2, sessions, "changing storage must not create another copy of either session")
}

func TestArchiveDrainRemovesAbandonedMaterialization(t *testing.T) {
	source, _ := archiveFixture(t)
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}}
	scratch := filepath.Join(cfg.DataDir, "archive-sources", "scratch")
	abandoned := filepath.Join(scratch, "agentsview-raw-interrupted")
	dbtest.WriteTestFile(t, filepath.Join(abandoned, "nested", "transcript"), []byte("interrupted copy"))
	require.NoError(t, os.Chmod(filepath.Join(abandoned, "nested"), 0o500))
	require.NoError(t, os.Chmod(abandoned, 0o500))
	dbtest.WriteTestFile(t, filepath.Join(scratch, "unrelated"), []byte("keep"))
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	manager, err := NewManager(t.Context(), cfg, receiver, engine, nil)
	require.NoError(t, err)
	defer manager.Close()
	require.NoError(t, drainError(manager, t.Context(), nil))
	assert.NoDirExists(t, abandoned)
	assert.FileExists(t, filepath.Join(scratch, "unrelated"))
}

func TestAttachedDirectoryCatalogChangeKeepsCompletion(t *testing.T) {
	_, inputs := archiveFixture(t)
	directory := filepath.Join(inputs, "seed")
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{{Name: "history", Directory: directory}}}
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	manager, err := NewManager(t.Context(), cfg, receiver, engine, nil)
	require.NoError(t, err)
	require.NoError(t, drainError(manager, t.Context(), nil))
	manager.Close()

	// A later supplemental capture changes the catalog but no accepted source.
	saved, err := sql.Open("sqlite3", filepath.Join(directory, "sessions.db"))
	require.NoError(t, err)
	_, err = saved.ExecContext(t.Context(), `INSERT INTO raw_archive_files(root_id,path,sha256,size,mod_time_ns)
		SELECT id,'later.txt',?,1,0 FROM raw_archive_roots LIMIT 1`, fmt.Sprintf("%x", sha256.Sum256([]byte("later"))))
	require.NoError(t, err)
	require.NoError(t, saved.Close())
	copies := 0
	manager, err = NewManager(t.Context(), cfg, receiver, engine, func(context.Context) error { copies++; return nil })
	require.NoError(t, err)
	defer manager.Close()
	require.NoError(t, drainError(manager, t.Context(), nil))
	assert.Zero(t, copies, "completed sources stay complete under the same attachment")

	var attachments int
	require.NoError(t, receiver.Reader().QueryRow(t.Context(), `SELECT count(*) FROM archive_attachments`).Scan(&attachments))
	assert.Equal(t, 1, attachments)
	entries, err := os.ReadDir(filepath.Join(cfg.DataDir, "archive-sources"))
	require.NoError(t, err)
	var catalogs int
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "directory-") {
			catalogs++
		}
	}
	assert.Equal(t, 1, catalogs, "the replaced catalog is removed")
}
