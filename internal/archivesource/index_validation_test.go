package archivesource

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/docbank/docbank"
)

func TestArchiveSourceRejectsInvalidSavedIndex(t *testing.T) {
	for _, tc := range []struct{ fault, wantError string }{
		{"inventory version", "unsupported recovery inventory version"},
		{"missing table", "unsupported source schema"},
		{"missing column", "unsupported source schema"},
		{"wrong column type", "unsupported source schema"},
		{"archive marker", "archive-only"},
		{"version marker", "archive-only"},
		{"canonical manifest", "canonical"},
		{"missing object", "source object"},
		{"object size", "recorded size"},
		{"suppression policy", "suppression policy"},
		{"qualified native identity", "inconsistent saved archive references"},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			source, inputs := archiveFixture(t)
			broken := invalidIndexSnapshot(t, source, inputs, tc.fault)
			cache := t.TempDir()
			index, err := OpenIndex(t.Context(), broken, cache, nil)
			require.ErrorContains(t, err, tc.wantError)
			assert.Nil(t, index)
			files, err := os.ReadDir(cache)
			require.NoError(t, err)
			assert.Empty(t, files, "rejected input publishes no index and leaves no bootstrap files")
		})
	}
}

func invalidIndexSnapshot(t *testing.T, source config.ArchiveSource, inputs, fault string) config.ArchiveSource {
	t.Helper()
	ctx := t.Context()
	repository, err := docbank.OpenBackupRepository(source.Repository)
	require.NoError(t, err)
	reader, err := repository.OpenSnapshot(ctx, source.Snapshot)
	require.NoError(t, err)
	var extras []docbank.BackupExtraFile
	files := t.TempDir()
	for _, name := range []string{"sessions.db", "inventory.json", "recovery-settings.json", "telemetry-install-id"} {
		path := filepath.Join(files, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = reader.CopyExtra(ctx, "application/"+name, file)
		require.NoError(t, err)
		require.NoError(t, file.Close())
		extras = append(extras, docbank.BackupExtraFile{Path: path, RecordAs: "application/" + name})
	}
	require.NoError(t, reader.Close())
	if fault == "inventory version" {
		require.NoError(t, os.WriteFile(filepath.Join(files, "inventory.json"), []byte(`{"version":2}`), 0o600))
	} else {
		conn, err := sql.Open("sqlite3", filepath.Join(files, "sessions.db"))
		require.NoError(t, err)
		mutateSavedIndex(t, conn, fault)
		require.NoError(t, conn.Close())
	}
	// Keep real content membership; only the application-owned input is altered.
	vault, err := docbank.New(ctx, docbank.Config{Root: filepath.Join(inputs, "seed", "raw-archive", "artifacts")})
	require.NoError(t, err)
	snapshot, err := vault.CreateBackup(ctx, repository, docbank.BackupOptions{ExtraFiles: extras})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
	source.Snapshot = snapshot.ID
	return source
}

func mutateSavedIndex(t *testing.T, conn *sql.DB, fault string) {
	t.Helper()
	queries := map[string]string{
		"qualified native identity": `INSERT INTO raw_archive_sessions(device_id,provider,parser_id,session_id,root_id,source_key)
        SELECT r.device_id,r.provider,'foreign~native','foreign~native',r.id,s.source_key
        FROM raw_archive_sources s JOIN raw_archive_roots r ON r.id=s.root_id WHERE r.provider='claude' LIMIT 1`,
		"missing table":      `DROP TABLE raw_archive_root_aliases`,
		"missing column":     `ALTER TABLE raw_archive_files RENAME COLUMN covered TO unsupported`,
		"wrong column type":  `ALTER TABLE raw_archive_devices RENAME TO old_devices; CREATE TABLE raw_archive_devices(device_id TEXT,suppressions TEXT)`,
		"archive marker":     `UPDATE archive_metadata SET value='0' WHERE key='archive_only'`,
		"version marker":     `PRAGMA user_version=1`,
		"canonical manifest": `UPDATE raw_archive_sources SET canonical_json=x'7b7d'`,
		"suppression policy": `UPDATE raw_archive_devices SET suppressions=x'7b7d'`,
	}
	if query, ok := queries[fault]; ok {
		_, err := conn.ExecContext(t.Context(), query)
		require.NoError(t, err)
		return
	}
	var id, archiveID, deviceID string
	var canonical []byte
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT s.manifest_id,s.canonical_json,r.device_id,
		(SELECT value FROM archive_metadata WHERE key='archive_id')
		FROM raw_archive_sources s JOIN raw_archive_roots r ON r.id=s.root_id LIMIT 1`).Scan(&id, &canonical, &deviceID, &archiveID))
	identity, err := rawsync.NewAuthIdentity(archiveID, deviceID)
	require.NoError(t, err)
	manifest, err := rawsync.ParseCanonicalManifest(identity, id, canonical, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	entry := &manifest.Manifest.Entries[0]
	switch fault {
	case "missing object":
		entry.Objects[0].SHA256 = strings.Repeat("0", 64)
	case "object size":
		entry.Length++
		entry.Objects[0].Length++
	default:
		require.FailNow(t, "unknown fixture mutation", fault)
	}
	changed, err := rawsync.ValidateAndCanonicalize(identity, manifest.Manifest, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE raw_archive_sources SET manifest_id=?,canonical_json=? WHERE manifest_id=?`,
		changed.ManifestID, changed.CanonicalJSON, id)
	require.NoError(t, err)
}

func TestArchiveSourceBootstrapCancellationAndAbandonedFiles(t *testing.T) {
	source, inputs := archiveFixture(t)
	require.NoError(t, os.RemoveAll(inputs))
	cache := t.TempDir()
	abandoned := filepath.Join(cache, ".bootstrap-abandoned")
	require.NoError(t, os.Mkdir(abandoned, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(abandoned, "sessions.db"), []byte("partial"), 0o600))
	// A bootstrap in another process, such as a rebuild worker, holds its tree.
	active := filepath.Join(cache, ".bootstrap-active")
	require.NoError(t, os.Mkdir(active, 0o700))
	owner := flock.New(filepath.Join(active, bootstrapLockFile))
	locked, err := owner.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	index, err := OpenIndex(ctx, source, cache, func(string) { cancel() })
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, index)
	files, err := os.ReadDir(cache)
	require.NoError(t, err)
	require.Len(t, files, 1, "cleanup removes only abandoned bootstrap trees")
	assert.Equal(t, ".bootstrap-active", files[0].Name())
	require.NoError(t, owner.Close())
	index, err = OpenIndex(t.Context(), source, cache, nil)
	require.NoError(t, err)
	require.NoError(t, index.Close())
	_, err = os.Stat(active)
	assert.ErrorIs(t, err, os.ErrNotExist, "a released tree is abandoned")
}

func TestBootstrapScratchCreationExcludesCacheCleanup(t *testing.T) {
	cache := t.TempDir()
	var cleanupLocked, sawScratch bool
	bootstrapTestHookBeforeLock = func(scratch string) {
		// Another process's cleanup arrives while the new tree is still unlocked.
		_, err := os.Stat(scratch)
		sawScratch = err == nil
		other := flock.New(filepath.Clean(cache) + ".lock")
		locked, err := other.TryLock()
		require.NoError(t, err)
		cleanupLocked = locked
		require.NoError(t, other.Close())
	}
	defer func() { bootstrapTestHookBeforeLock = nil }()
	scratch, release, err := newBootstrapScratch(cache)
	require.NoError(t, err)
	require.True(t, sawScratch)
	assert.False(t, cleanupLocked, "cleanup cannot run before the new tree is locked")
	require.NoError(t, prepareCache(cache))
	assert.DirExists(t, scratch, "cleanup keeps a tree its owner locked during creation")
	require.NoError(t, release())
	assert.NoDirExists(t, scratch)
}
