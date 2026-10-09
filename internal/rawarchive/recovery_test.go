package rawarchive

import (
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/docbank"
)

// The default crosses the recipe boundary. Run with
// AGENTSVIEW_RECOVERY_TEST_BYTES=4294967297 to exercise the former 4 GiB limit.
func TestRecoveryLargeApplicationExtra(t *testing.T) {
	ctx := t.Context()
	data := t.TempDir()
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, data, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte("019eb791cf7d75c184399ed74c122e04"))
	settings := RecoverySettings{LocalMachineName: "source-device"}
	target := filepath.Join(t.TempDir(), "backup")
	// An unsupported ordinary vault must be reported before creating a backup.
	require.NoError(t, os.Mkdir(filepath.Join(data, "artifacts"), 0o700))
	_, err = archive.Backup(ctx, target, settings, "test-build")
	require.ErrorContains(t, err, "ordinary artifact vault")
	assert.NoDirExists(t, target)
	require.NoError(t, os.Remove(filepath.Join(data, "artifacts")))
	size := int64(64<<20 + 1)
	if value := os.Getenv("AGENTSVIEW_RECOVERY_TEST_BYTES"); value != "" {
		size, err = strconv.ParseInt(value, 10, 64)
		require.NoError(t, err)
		require.Greater(t, size, int64(64<<20))
	}
	asset := filepath.Join(data, "assets", "large.bin")
	require.NoError(t, os.MkdirAll(filepath.Dir(asset), 0o700))
	f, err := os.Create(asset)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	_, err = f.WriteAt([]byte("end"), size-3)
	require.NoError(t, err)
	want := sha256.New()
	_, err = io.Copy(want, f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	recovery, err := archive.Backup(ctx, target, settings, "test-build")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, recovery.MinReaderVersion, 6)
	assert.Equal(t, "test-build", recovery.ReaderBuild)
	_, err = VerifyRecovery(ctx, target, recovery.SnapshotID)
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, database.Close())
	require.NoError(t, os.RemoveAll(data))
	restored := filepath.Join(t.TempDir(), "restored")
	_, err = Restore(ctx, target, "missing-snapshot", restored, nil)
	require.Error(t, err)
	assert.NoDirExists(t, restored)
	report, err := Restore(ctx, target, recovery.SnapshotID, restored, nil)
	require.NoError(t, err)
	assert.Equal(t, recovery.RepositoryID, report.RepositoryID)
	assert.Equal(t, recovery.SnapshotID, report.SnapshotID)
	assert.Equal(t, recovery.ReaderBuild, report.ReaderBuild)
	assert.Equal(t, recovery.MinReaderVersion, report.MinReaderVersion)
	f, err = os.Open(filepath.Join(restored, "assets", "large.bin"))
	require.NoError(t, err)
	defer f.Close()
	got := sha256.New()
	n, err := io.Copy(got, f)
	require.NoError(t, err)
	assert.Equal(t, size, n)
	assert.Equal(t, want.Sum(nil), got.Sum(nil))
}

func TestRecoveryRejectsRepositorySymlinkInsideSource(t *testing.T) {
	ctx := t.Context()
	data := t.TempDir()
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	defer database.Close()
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, data, nil)
	require.NoError(t, err)
	defer archive.Close()
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte("019eb791cf7d75c184399ed74c122e04"))
	target := filepath.Join(data, "backups")
	repository, err := docbank.InitBackupRepository(target)
	require.NoError(t, err)
	alias := filepath.Join(t.TempDir(), "backup-link")
	require.NoError(t, os.Symlink(target, alias))
	_, err = archive.Backup(ctx, alias, RecoverySettings{LocalMachineName: "source-device"}, "test")
	require.ErrorContains(t, err, "outside the archive data directory")
	snapshots, err := repository.Snapshots()
	require.NoError(t, err)
	assert.Empty(t, snapshots)
}

func TestRecoveryRejectsMissingReferencedAsset(t *testing.T) {
	ctx := t.Context()
	data := t.TempDir()
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	defer database.Close()
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, data, nil)
	require.NoError(t, err)
	defer archive.Close()
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte("019eb791cf7d75c184399ed74c122e04"))
	ref, _, err := assets.Put(filepath.Join(data, "assets"), "image/png", []byte("synthetic image"))
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(ctx, db.Session{ID: "example", Agent: "chatgpt", Project: "example", Machine: "source-device"}))
	require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: "example", Ordinal: 0, Role: "user", Content: "![image](" + ref + ")"}}))
	require.NoError(t, os.Remove(filepath.Join(data, "assets", strings.TrimPrefix(ref, "asset://"))))
	settings := RecoverySettings{LocalMachineName: "source-device"}
	_, err = archive.Backup(ctx, filepath.Join(t.TempDir(), "backup"), settings, "test")
	require.ErrorContains(t, err, "asset")
	require.NoError(t, archive.Close())
	require.NoError(t, database.Close())
	_, err = verifyRestoredArchive(ctx, data, settings, nil)
	assert.ErrorContains(t, err, "asset")
}
