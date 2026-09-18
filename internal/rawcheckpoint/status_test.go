package rawcheckpoint

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/rawsync"
)

func TestClientStatusReportsPendingRetryAndSourceHeadWithoutPaths(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	base := t.TempDir()
	store, err := OpenWithOptions(t.Context(), filepath.Join(base, "checkpoint.db"), Options{
		SpoolDir:       filepath.Join(base, "spool"),
		MaxOutboxBytes: 1 << 20, Now: func() time.Time { return now },
	})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })
	root, err := store.ResolveConfiguredRoot(t.Context(), "claude", t.TempDir())
	require.NoError(err)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	retryAt := now.Add(5 * time.Minute)
	require.NoError(store.RecordGenerationFailure(
		t.Context(), "device-a", generation.CaptureID,
		GenerationFailureTransient, retryAt,
	))

	status, err := store.ClientStatus(t.Context())

	require.NoError(err)
	assert.Equal("device-a", status.DeviceID)
	assert.Equal(1, status.PendingGenerations)
	assert.Equal(1, status.PendingObjects)
	assert.Equal(int64(3), status.PendingObjectBytes)
	assert.Equal(int64(1795), status.Outbox.UsedBytes)
	require.NotNil(status.RetryAt)
	assert.Equal(retryAt, *status.RetryAt)
	require.Len(status.Sources, 1)
	assert.Equal(generation.Source.Provider, status.Sources[0].Provider)
	assert.Equal(generation.Source.ConfiguredRootID,
		status.Sources[0].ConfiguredRootID)
	assert.NotEmpty(status.Sources[0].SourceID)
	assert.NotContains(status.Sources[0].SourceID, generation.Source.SourceKey)
	assert.Equal(generation.CaptureID, status.Sources[0].LatestCaptureID)
	assert.Empty(status.Sources[0].Head.Receipt)
	require.Len(status.Coverage, 1)
	assert.Equal(CoverageComplete, status.Coverage[0].State)
	assert.Zero(status.PermanentFailures)
}

func TestClientStatusKeepsLastCaptureAfterAcknowledgement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)
	base := t.TempDir()
	path := filepath.Join(base, "checkpoint.db")
	store, err := OpenWithOptions(t.Context(), path, Options{
		SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		Now: func() time.Time { return now },
	})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })
	root, err := store.ResolveConfiguredRoot(t.Context(), "claude", t.TempDir())
	require.NoError(err)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	manifest, found, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(found)
	commit := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(90), Receipt: validCheckpointDigest(91),
		Generation: 1, Created: true,
	}
	assert.Equal(generation.CaptureID, manifest.CaptureID)
	require.NoError(store.BindFinalizedCommit(
		t.Context(), "device-a", generation.CaptureID, commit,
	))
	_, err = store.AcknowledgeGeneration(
		t.Context(), "device-a", generation.CaptureID, commit,
	)
	require.NoError(err)

	status, err := store.ClientStatus(t.Context())

	require.NoError(err)
	assert.Zero(status.PendingGenerations)
	require.NotNil(status.LastCaptureAt)
	assert.Equal(now, *status.LastCaptureAt)
}

func TestOpenReadOnlyReadsWhileWriterOwnsCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	base := t.TempDir()
	path := filepath.Join(base, "checkpoint.db")
	writer, err := OpenWithOptions(t.Context(), path, Options{
		SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
	})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(writer.Close()) })
	require.NoError(writer.SetDevice(t.Context(), "device-a"))

	reader, err := OpenReadOnly(t.Context(), path)

	require.NoError(err)
	t.Cleanup(func() { require.NoError(reader.Close()) })
	status, err := reader.ClientStatus(t.Context())
	require.NoError(err)
	assert.Equal("device-a", status.DeviceID)
	assert.Equal(int64(1<<20), status.Outbox.LimitBytes)
}

func TestOpenReadOnlyStatusSupportsVersionOneCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "checkpoint.db")
	db, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(err)
	for _, statement := range versionOneSchemaStatements {
		_, err = db.ExecContext(t.Context(), statement)
		require.NoError(err)
	}
	updatedAt := time.Date(2026, 8, 1, 12, 30, 0, 0, time.UTC)
	_, err = db.ExecContext(t.Context(), `INSERT INTO device_config (id, device_id, created_at)
		VALUES (1, ?, ?)`, "legacy-device", checkpointTimestamp(updatedAt))
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO raw_sources (
		provider, configured_root_id, source_key, head_manifest_id,
		head_receipt, head_generation, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`, "claude", "legacy-root", "private-source-key",
		validCheckpointDigest(1), validCheckpointDigest(2), 7,
		checkpointTimestamp(updatedAt))
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `PRAGMA user_version = 1`)
	require.NoError(err)
	require.NoError(db.Close())

	reader, err := OpenReadOnly(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reader.Close()) })
	status, err := reader.ClientStatus(t.Context())

	require.NoError(err)
	assert.Equal("legacy-device", status.DeviceID)
	assert.Nil(status.LastCaptureAt)
	assert.Zero(status.PendingGenerations)
	assert.Zero(status.PendingObjects)
	assert.Zero(status.PendingObjectBytes)
	assert.Zero(status.Outbox.UsedBytes)
	assert.Zero(status.Outbox.ReservedBytes)
	assert.Equal(defaultMaxOutboxBytes, status.Outbox.LimitBytes)
	assert.Nil(status.RetryAt)
	assert.Zero(status.PermanentFailures)
	require.Len(status.Sources, 1)
	assert.Equal("claude", string(status.Sources[0].Provider))
	assert.Equal("legacy-root", status.Sources[0].ConfiguredRootID)
	assert.NotEmpty(status.Sources[0].SourceID)
	assert.NotContains(status.Sources[0].SourceID, "private-source-key")
	assert.Empty(status.Sources[0].LatestCaptureID)
	assert.Equal(validCheckpointDigest(1), status.Sources[0].Head.ManifestID)
	assert.Equal(validCheckpointDigest(2), status.Sources[0].Head.Receipt)
	assert.Equal(int64(7), status.Sources[0].Head.Generation)
	assert.Equal(updatedAt, status.Sources[0].UpdatedAt)
	assert.Equal(updatedAt, status.Sources[0].Head.UpdatedAt)
	assert.Empty(status.Coverage)
	inspection, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(inspection.Close()) })
	var version int
	require.NoError(inspection.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version))
	assert.Equal(1, version, "read-only status must not migrate the checkpoint")
}

func TestOpenReadOnlyStatusSupportsVersionTwoCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "checkpoint.db")
	db, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(err)
	for _, statements := range [][]string{
		versionOneSchemaStatements,
		versionTwoMigrationStatements,
	} {
		for _, statement := range statements {
			_, err = db.ExecContext(t.Context(), statement)
			require.NoError(err)
		}
	}
	_, err = db.ExecContext(t.Context(), `INSERT INTO outbox_config (id, spool_path) VALUES (1, 'spool')`)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `PRAGMA user_version = 2`)
	require.NoError(err)
	require.NoError(db.Close())

	reader, err := OpenReadOnly(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reader.Close()) })
	status, err := reader.ClientStatus(t.Context())

	require.NoError(err)
	assert.Equal(defaultMaxOutboxBytes, status.Outbox.LimitBytes)
	assert.Zero(status.PendingGenerations)
	assert.Nil(status.RetryAt)
	assert.Zero(status.PermanentFailures)
	inspection, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(inspection.Close()) })
	var version int
	require.NoError(inspection.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version))
	assert.Equal(2, version, "read-only status must not migrate the checkpoint")
}

func TestOpenReadOnlyStatusSupportsVersionFiveCheckpoint(t *testing.T) {
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "checkpoint.db")
	db, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(err)
	for _, statements := range [][]string{
		versionOneSchemaStatements,
		versionTwoMigrationStatements,
		versionThreeMigrationStatements,
		versionFourMigrationStatements,
		versionFiveMigrationStatements,
	} {
		for _, statement := range statements {
			_, err = db.ExecContext(t.Context(), statement)
			require.NoError(err)
		}
	}
	_, err = db.ExecContext(t.Context(), `INSERT INTO outbox_config (id, spool_path) VALUES (1, 'spool')`)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `PRAGMA user_version = 5`)
	require.NoError(err)
	require.NoError(db.Close())

	reader, err := OpenReadOnly(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reader.Close()) })
	status, err := reader.ClientStatus(t.Context())

	require.NoError(err)
	assert.Equal(t, defaultMaxOutboxBytes, status.Outbox.LimitBytes)
}
