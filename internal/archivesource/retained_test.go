package archivesource

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gofrs/flock"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/testjsonl"
	"go.kenn.io/docbank/docbank"
)

func TestRetainedClaudeOriginalAndDirectory(t *testing.T) {
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	ctx := t.Context()
	inputs := t.TempDir()
	projects := filepath.Join(inputs, "claude", "projects")
	original := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "retained question", id).String()
	dbtest.WriteTestFile(t, filepath.Join(projects, "project", id+".jsonl"), []byte(original))
	dbtest.WriteTestFile(t, filepath.Join(projects, "project", id, "tool-results", "tool.txt"), []byte("tool result"))
	backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "claude", Path: projects}})
	seed := filepath.Join(inputs, "seed")
	// Seed imports accepted custody before local reparse creates session bindings.
	saved, err := os.ReadFile(filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	_, err = archive.Reparse(ctx, rawarchive.ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err)
	cfg := config.Config{DataDir: seed}
	_, _, _, err = Retained(ctx, cfg, database, id, sourceDevice, id, "claude", "", nil, 1<<20, nil)
	require.ErrorIs(t, err, docbank.ErrVaultLocked)
	require.NoError(t, archive.Close())
	tree, primary, source, err := Retained(ctx, cfg, database, id, sourceDevice, id, "claude", "", nil, 1<<20, nil)
	require.NoError(t, err)
	assert.Contains(t, source, "retained:")
	name, err := tree.EntryPath(primary)
	require.NoError(t, err)
	got, err := os.ReadFile(name)
	require.NoError(t, err)
	assert.Equal(t, original, string(got))
	companion, err := tree.EntryPath(filepath.ToSlash(filepath.Join(filepath.Dir(primary), id, "tool-results", "tool.txt")))
	require.NoError(t, err)
	got, err = os.ReadFile(companion)
	require.NoError(t, err)
	assert.Equal(t, "tool result", string(got))
	require.NoError(t, tree.Cleanup())
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	t.Setenv("TMP", scratch)
	t.Setenv("TEMP", scratch)
	_, _, _, err = Retained(ctx, cfg, database, id, "wrong-device", id, "claude", "", nil, 1<<20, nil)
	require.ErrorContains(t, err, "original transcript or owning source unavailable")
	_, _, _, err = Retained(ctx, cfg, database, id, sourceDevice, id, "claude", "", nil, 1, nil)
	require.ErrorIs(t, err, rawderive.ErrSizeLimit)
	_, _, _, err = Retained(ctx, cfg, database, id, sourceDevice, id, "claude", "archive://wrong/source", nil, 1<<20, nil)
	require.ErrorContains(t, err, "binding")
	entries, err := os.ReadDir(scratch)
	require.NoError(t, err)
	assert.Empty(t, entries)
	archiveID, err := database.GetArchiveID(ctx)
	require.NoError(t, err)
	var owner db.ArchiveSessionProvenance
	owner.NativeSessionID = id
	owner.Source.ArchiveID = archiveID
	err = database.Reader().QueryRowContext(ctx, `SELECT b.device_id,b.root_id,b.source_key,s.manifest_id FROM raw_archive_sessions b JOIN raw_archive_sources s ON s.root_id=b.root_id AND s.source_key=b.source_key WHERE b.session_id=?`, id).Scan(&owner.Source.DeviceID, &owner.Source.RootID, &owner.Source.SourceKey, &owner.ManifestID)
	require.NoError(t, err)
	owner.Attachment = db.ArchiveAttachmentKey{RepositoryID: "archive:" + archiveID, SnapshotID: "directory:" + archiveID}
	require.NoError(t, database.Close())
	prepareDirectoryOwnerLock(t, seed)
	directoryCfg := config.Config{ArchiveSources: []config.ArchiveSource{{Directory: seed}}}
	tree, primary, source, err = Retained(ctx, directoryCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.NoError(t, err)
	assert.Equal(t, "directory:"+archiveID, source)
	name, err = tree.EntryPath(primary)
	require.NoError(t, err)
	got, err = os.ReadFile(name)
	require.NoError(t, err)
	assert.Equal(t, original, string(got))
	require.NoError(t, tree.Cleanup())
	alternate := t.TempDir()
	require.NoError(t, os.CopyFS(alternate, os.DirFS(filepath.Join(seed, rawarchive.Directory, "artifacts"))))
	require.NoError(t, os.Mkdir(filepath.Join(alternate, "application"), 0o700))
	alternateDB := filepath.Join(alternate, "application", "sessions.db")
	require.NoError(t, os.WriteFile(alternateDB, saved, 0o600))
	alternateCfg := config.Config{ArchiveSources: []config.ArchiveSource{{Directory: filepath.Join(t.TempDir(), "unprepared")}, {Directory: alternate}}}
	_, _, _, err = Retained(ctx, alternateCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.ErrorContains(t, err, "attach the archive directory or run AgentsView there once, then retry")
	_, err = os.Stat(filepath.Join(alternate, "application", "db.write.lock"))
	require.ErrorIs(t, err, os.ErrNotExist)
	prepareDirectoryOwnerLock(t, filepath.Join(alternate, "application"))
	before := retainedTree(t, alternate)
	tree, _, _, err = Retained(ctx, alternateCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.NoError(t, err)
	require.NoError(t, tree.Cleanup())
	assert.Equal(t, before, retainedTree(t, alternate))
	lockPath := filepath.Join(alternate, "application", "db.write.lock")
	exclusive := flock.New(lockPath)
	t.Cleanup(func() { _ = exclusive.Close() })
	locked, err := exclusive.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	_, _, _, err = Retained(ctx, alternateCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.ErrorContains(t, err, "archive is busy")
	require.NoError(t, exclusive.Close())
	shared := flock.New(lockPath, flock.SetFlag(os.O_RDONLY))
	t.Cleanup(func() { _ = shared.Close() })
	locked, err = shared.TryRLock()
	require.NoError(t, err)
	require.True(t, locked)
	tree, _, _, err = Retained(ctx, alternateCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.NoError(t, err)
	require.NoError(t, tree.Cleanup())
	require.NoError(t, shared.Close())
	pending := filepath.Join(alternate, "application", "compact-recovery.json")
	require.NoError(t, os.WriteFile(pending, []byte("pending"), 0o600))
	_, _, _, err = Retained(ctx, alternateCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.ErrorContains(t, err, "compaction recovery is pending")
	require.NoError(t, os.Remove(pending))
	require.NoError(t, os.Remove(lockPath))
	require.NoError(t, os.Mkdir(lockPath, 0o700))
	_, _, _, err = Retained(ctx, alternateCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.ErrorContains(t, err, "attach the archive directory or run AgentsView there once, then retry")
	require.NoError(t, os.Remove(lockPath))
	if err := os.Symlink(alternateDB, lockPath); err == nil {
		_, _, _, err = Retained(ctx, alternateCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
		require.ErrorContains(t, err, "attach the archive directory or run AgentsView there once, then retry")
		require.NoError(t, os.Remove(lockPath))
	} else if runtime.GOOS != "windows" {
		require.NoError(t, err)
	}
	prepareDirectoryOwnerLock(t, filepath.Join(alternate, "application"))
	wal := filepath.Join(alternate, "application", "sessions.db-wal")
	require.NoError(t, os.WriteFile(wal, []byte("pending"), 0o600))
	_, _, _, err = Retained(ctx, alternateCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.ErrorContains(t, err, "close the archive writer and retry")
	require.NoError(t, os.Remove(wal))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, _, err = Retained(cancelled, directoryCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), "owning directory is not configured or unavailable")
	tree, _, _, err = Retained(ctx, directoryCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.NoError(t, err)
	require.NoError(t, tree.Cleanup())

	// Damage the selected synthetic raw object, not its catalog binding.
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(original)))
	blob := filepath.Join(seed, rawarchive.Directory, "artifacts", "blobs", hash[:2], hash)
	damaged, err := os.ReadFile(blob)
	require.NoError(t, err)
	require.NotEmpty(t, damaged)
	damaged[0] ^= 1
	require.NoError(t, os.WriteFile(blob, damaged, 0o600))
	tree, _, _, err = Retained(ctx, directoryCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.ErrorIs(t, err, artifact.ErrArtifactCorrupt)
	assert.Nil(t, tree)
}

func TestRetainedCodexCompressedAndAncestor(t *testing.T) {
	const thread = "22222222-2222-4222-8222-222222222222"
	const parent = "11111111-1111-4111-8111-111111111111"
	ctx := t.Context()
	inputs := t.TempDir()
	home := filepath.Join(inputs, "codex")
	live := filepath.Join(home, "sessions")
	parentName := "rollout-2024-01-15T00-00-00-" + parent + ".jsonl"
	pageName := "rollout-2026-01-01T00-00-00-" + thread + ".jsonl.zst"
	ancestor := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(parent, "/work", "codex_cli_rs", "2024-01-15T00:00:00Z", map[string]any{"history_mode": "paginated"}), testjsonl.CodexMsgJSON("user", "ancestor", "2024-01-15T00:00:01Z"))
	page := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(thread, "/work", "codex_cli_rs", "2026-01-01T00:00:00Z", map[string]any{"history_mode": "paginated", "history_base": map[string]any{"thread_id": parent, "end_ordinal_exclusive": 2, "end_byte_offset": len(ancestor)}}), testjsonl.CodexMsgJSON("user", "question", "2026-01-01T00:00:01Z"))
	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	compressed := encoder.EncodeAll([]byte(page), nil)
	encoder.Close()
	dbtest.WriteTestFile(t, filepath.Join(live, "2026", "01", "01", pageName), compressed)
	dbtest.WriteTestFile(t, filepath.Join(home, "archived_sessions", parentName), []byte(ancestor))
	backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "codex", Path: home}})
	seed := filepath.Join(inputs, "seed")
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	defer database.Close()
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	_, err = archive.Reparse(ctx, rawarchive.ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	cancelCtx, cancel := context.WithCancel(ctx)
	_, _, _, err = Retained(cancelCtx, config.Config{DataDir: seed}, database, "codex:"+thread, sourceDevice, "codex:"+thread, "codex", "", nil, 1<<20, func(_ string, _, _ []string, _ func(context.Context, string, int64) ([]byte, error), _ CodexAncestry) error {
		cancel()
		return cancelCtx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, sql.ErrTxDone)
	var seen []string
	_, _, source, err := Retained(ctx, config.Config{DataDir: seed}, database, "codex:"+thread, sourceDevice, "codex:"+thread, "codex", "", nil, 1<<20, func(_ string, names, _ []string, read func(context.Context, string, int64) ([]byte, error), _ CodexAncestry) error {
		for _, name := range names {
			got, err := read(ctx, name, 1<<20)
			require.NoError(t, err)
			switch filepath.Base(name) {
			case parentName:
				assert.Equal(t, []byte(ancestor), got)
				seen = append(seen, parentName)
			case pageName:
				assert.Equal(t, compressed, got)
				seen = append(seen, pageName)
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Contains(t, source, "retained:")
	assert.ElementsMatch(t, []string{parentName, pageName}, seen)
}

func TestRetainedMissingVaultNeedsOwner(t *testing.T) {
	data := t.TempDir()
	_, _, _, err := Retained(t.Context(), config.Config{DataDir: data}, nil, "session", "device", "native", "claude", "", nil, 1024, nil)
	require.ErrorContains(t, err, "requires the local archive database")
	database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	defer database.Close()
	_, _, _, err = Retained(t.Context(), config.Config{DataDir: data}, database, "session", "device", "native", "claude", "", nil, 1024, nil)
	require.ErrorIs(t, err, docbank.ErrVaultNeedsOwner)
	_, err = os.Stat(filepath.Join(data, rawarchive.Directory, "artifacts"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// retainedTree records every entry and regular-file byte digest, including sidecars.
func retainedTree(t *testing.T, root string) map[string]string {
	t.Helper()
	entries := make(map[string]string)
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, name)
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			entries[rel] = fmt.Sprintf("%x", sha256.Sum256(data))
		} else {
			entries[rel] = entry.Type().String()
		}
		return nil
	})
	require.NoError(t, err)
	return entries
}

func TestRetainedDirectoryReadCreatesNoSidecars(t *testing.T) {
	const thread = "22222222-2222-4222-8222-222222222222"
	ctx := t.Context()
	inputs := t.TempDir()
	home := filepath.Join(inputs, "codex")
	name := "rollout-2026-01-01T00-00-00-" + thread + ".jsonl"
	page := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(thread, "/work", "codex_cli_rs", "2026-01-01T00:00:00Z", map[string]any{"history_mode": "paginated"}), testjsonl.CodexMsgJSON("user", "question", "2026-01-01T00:00:01Z"))
	dbtest.WriteTestFile(t, filepath.Join(home, "sessions", name), []byte(page))
	backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "codex", Path: home}})
	seed := filepath.Join(inputs, "seed")
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = archive.Close() })
	_, err = archive.Reparse(ctx, rawarchive.ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err)
	archiveID, err := database.GetArchiveID(ctx)
	require.NoError(t, err)
	var owner db.ArchiveSessionProvenance
	owner.NativeSessionID = "codex:" + thread
	owner.Source.ArchiveID = archiveID
	err = database.Reader().QueryRowContext(ctx, `SELECT b.device_id,b.root_id,b.source_key,s.manifest_id FROM raw_archive_sessions b JOIN raw_archive_sources s ON s.root_id=b.root_id AND s.source_key=b.source_key WHERE b.session_id=?`, owner.NativeSessionID).Scan(&owner.Source.DeviceID, &owner.Source.RootID, &owner.Source.SourceKey, &owner.ManifestID)
	require.NoError(t, err)
	owner.Attachment = db.ArchiveAttachmentKey{RepositoryID: "archive:" + archiveID, SnapshotID: "directory:" + archiveID}
	require.NoError(t, archive.Close())
	require.NoError(t, database.Close())
	// The normal directory attach path creates the application owner lock.
	index, err := openDirectoryIndex(ctx, config.ArchiveSource{Directory: seed}, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = index.Close() })
	require.NoError(t, index.Close())
	dbPath := filepath.Join(seed, "sessions.db")
	// A final writable owner checkpoints and closes after attachment.
	checkpointed, err := db.OpenIsolatedContext(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = checkpointed.Close() })
	require.NoError(t, checkpointed.Close())
	before, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(before), 20)
	assert.Equal(t, []byte{2, 2}, before[18:20])
	info, err := os.Stat(dbPath)
	require.NoError(t, err)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err := os.Stat(dbPath + suffix)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	listing, err := os.ReadDir(seed)
	require.NoError(t, err)
	var directoryMode fs.FileMode
	check := func() {
		t.Helper()
		after, err := os.ReadDir(seed)
		require.NoError(t, err)
		var gotNames, wantNames []string
		for _, entry := range after {
			gotNames = append(gotNames, entry.Name())
		}
		for _, entry := range listing {
			wantNames = append(wantNames, entry.Name())
		}
		assert.Equal(t, wantNames, gotNames)
		bytes, err := os.ReadFile(dbPath)
		require.NoError(t, err)
		assert.Equal(t, before, bytes)
		current, err := os.Stat(dbPath)
		require.NoError(t, err)
		assert.Equal(t, info.ModTime(), current.ModTime())
		assert.Equal(t, info.Mode(), current.Mode())
		currentDir, err := os.Stat(seed)
		require.NoError(t, err)
		assert.Equal(t, directoryMode, currentDir.Mode())
	}
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Chmod(seed, 0o555))
		t.Cleanup(func() { _ = os.Chmod(seed, 0o700) })
	}
	directoryInfo, err := os.Stat(seed)
	require.NoError(t, err)
	directoryMode = directoryInfo.Mode()
	vaultPath := filepath.Join(seed, rawarchive.Directory, "artifacts")
	checkReleased := func() {
		t.Helper()
		lock := flock.New(filepath.Join(seed, "db.write.lock"), flock.SetFlag(os.O_RDONLY))
		locked, err := lock.TryLock()
		require.NoError(t, err)
		require.True(t, locked)
		require.NoError(t, lock.Close())
		vault, err := docbank.New(ctx, docbank.Config{Root: vaultPath})
		require.NoError(t, err)
		require.NoError(t, vault.Close())
		check()
	}
	called := false
	_, _, _, err = Retained(ctx, config.Config{ArchiveSources: []config.ArchiveSource{{Directory: seed}}}, nil, owner.NativeSessionID, sourceDevice, owner.NativeSessionID, "codex", "", []db.ArchiveSessionProvenance{owner}, 1<<20, func(_ string, names, _ []string, read func(context.Context, string, int64) ([]byte, error), _ CodexAncestry) error {
		called = true
		require.Len(t, names, 1)
		assert.Equal(t, name, filepath.Base(names[0]))
		got, err := read(ctx, names[0], 1<<20)
		require.NoError(t, err)
		assert.Equal(t, page, string(got))
		lock := flock.New(filepath.Join(seed, "db.write.lock"), flock.SetFlag(os.O_RDONLY))
		locked, err := lock.TryLock()
		require.NoError(t, err)
		assert.False(t, locked)
		require.NoError(t, lock.Close())
		vault, err := docbank.New(ctx, docbank.Config{Root: vaultPath})
		if vault != nil {
			t.Cleanup(func() { _ = vault.Close() })
		}
		require.ErrorIs(t, err, docbank.ErrVaultLocked)
		check()
		return nil
	})
	require.NoError(t, err)
	assert.True(t, called)
	checkReleased()
	callbackErr := errors.New("callback failed")
	_, _, _, err = Retained(ctx, config.Config{ArchiveSources: []config.ArchiveSource{{Directory: seed}}}, nil, owner.NativeSessionID, sourceDevice, owner.NativeSessionID, "codex", "", []db.ArchiveSessionProvenance{owner}, 1<<20, func(_ string, _, _ []string, _ func(context.Context, string, int64) ([]byte, error), _ CodexAncestry) error {
		return callbackErr
	})
	require.ErrorIs(t, err, callbackErr)
	checkReleased()
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, _, _, err = Retained(cancelCtx, config.Config{ArchiveSources: []config.ArchiveSource{{Directory: seed}}}, nil, owner.NativeSessionID, sourceDevice, owner.NativeSessionID, "codex", "", []db.ArchiveSessionProvenance{owner}, 1<<20, func(_ string, _, _ []string, _ func(context.Context, string, int64) ([]byte, error), _ CodexAncestry) error {
		cancel()
		return cancelCtx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	checkReleased()
}

// prepareDirectoryOwnerLock uses the same lock creation as an application owner.
func prepareDirectoryOwnerLock(t *testing.T, application string) {
	t.Helper()
	lock := flock.New(filepath.Join(application, "db.write.lock"))
	t.Cleanup(func() { _ = lock.Close() })
	locked, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	require.NoError(t, lock.Close())
}
