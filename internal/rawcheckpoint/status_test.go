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
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	base := t.TempDir()
	store, err := OpenWithOptions(t.Context(), filepath.Join(base, "checkpoint.db"), Options{
		SpoolDir:       filepath.Join(base, "spool"),
		MaxOutboxBytes: 1 << 20, Now: func() time.Time { return now },
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	root, err := store.ResolveConfiguredRoot(t.Context(), "claude", t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(t, err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(t, store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(t, err)
	require.True(t, ok)
	retryAt := now.Add(5 * time.Minute)
	require.NoError(t, store.RecordGenerationFailure(
		t.Context(), "device-a", generation.CaptureID,
		GenerationFailureTransient, retryAt,
	))

	status, err := store.ClientStatus(t.Context())

	require.NoError(t, err)
	assert.Equal(t, "device-a", status.DeviceID)
	assert.Equal(t, 1, status.PendingGenerations)
	assert.Equal(t, 1, status.PendingObjects)
	assert.Equal(t, int64(3), status.PendingObjectBytes)
	assert.Equal(t, int64(1795), status.Outbox.UsedBytes)
	require.NotNil(t, status.RetryAt)
	assert.Equal(t, retryAt, *status.RetryAt)
	require.Len(t, status.Sources, 1)
	assert.Equal(t, generation.Source.Provider, status.Sources[0].Provider)
	assert.Equal(t, generation.Source.ConfiguredRootID,
		status.Sources[0].ConfiguredRootID)
	assert.NotEmpty(t, status.Sources[0].SourceID)
	assert.NotContains(t, status.Sources[0].SourceID, generation.Source.SourceKey)
	assert.Equal(t, generation.CaptureID, status.Sources[0].LatestCaptureID)
	assert.Empty(t, status.Sources[0].Head.Receipt)
	require.Len(t, status.Coverage, 1)
	assert.Equal(t, CoverageComplete, status.Coverage[0].State)
	assert.Zero(t, status.PermanentFailures)
}

func TestClientStatusKeepsLastCaptureAfterAcknowledgement(t *testing.T) {
	now := time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)
	base := t.TempDir()
	path := filepath.Join(base, "checkpoint.db")
	store, err := OpenWithOptions(t.Context(), path, Options{
		SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		Now: func() time.Time { return now },
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	root, err := store.ResolveConfiguredRoot(t.Context(), "claude", t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(t, err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(t, store.CommitCapture(t.Context(), reservation.ID, generation))
	manifest, found, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(t, err)
	require.True(t, found)
	commit := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(90), Receipt: validCheckpointDigest(91),
		Generation: 1, Created: true,
	}
	assert.Equal(t, generation.CaptureID, manifest.CaptureID)
	require.NoError(t, store.BindFinalizedCommit(
		t.Context(), "device-a", generation.CaptureID, commit,
	))
	_, err = store.AcknowledgeGeneration(
		t.Context(), "device-a", generation.CaptureID, commit,
	)
	require.NoError(t, err)

	status, err := store.ClientStatus(t.Context())

	require.NoError(t, err)
	assert.Zero(t, status.PendingGenerations)
	require.NotNil(t, status.LastCaptureAt)
	assert.Equal(t, now, *status.LastCaptureAt)
}

func TestOpenReadOnlyReadsWhileWriterOwnsCheckpoint(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "checkpoint.db")
	writer, err := OpenWithOptions(t.Context(), path, Options{
		SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	require.NoError(t, writer.SetDevice(t.Context(), "device-a"))

	reader, err := OpenReadOnly(t.Context(), path)

	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	status, err := reader.ClientStatus(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "device-a", status.DeviceID)
	assert.Equal(t, int64(1<<20), status.Outbox.LimitBytes)
}

func TestOpenReadOnlyStatusSupportsVersionOneCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	db, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(t, err)
	for _, statement := range versionOneSchemaStatements {
		_, err = db.Exec(statement)
		require.NoError(t, err)
	}
	updatedAt := time.Date(2026, 8, 1, 12, 30, 0, 0, time.UTC)
	_, err = db.Exec(`INSERT INTO device_config (id, device_id, created_at)
		VALUES (1, ?, ?)`, "legacy-device", checkpointTimestamp(updatedAt))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO raw_sources (
		provider, configured_root_id, source_key, head_manifest_id,
		head_receipt, head_generation, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`, "claude", "legacy-root", "private-source-key",
		validCheckpointDigest(1), validCheckpointDigest(2), 7,
		checkpointTimestamp(updatedAt))
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA user_version = 1`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	reader, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	status, err := reader.ClientStatus(t.Context())

	require.NoError(t, err)
	assert.Equal(t, "legacy-device", status.DeviceID)
	assert.Nil(t, status.LastCaptureAt)
	assert.Zero(t, status.PendingGenerations)
	assert.Zero(t, status.PendingObjects)
	assert.Zero(t, status.PendingObjectBytes)
	assert.Zero(t, status.Outbox.UsedBytes)
	assert.Zero(t, status.Outbox.ReservedBytes)
	assert.Equal(t, defaultMaxOutboxBytes, status.Outbox.LimitBytes)
	assert.Nil(t, status.RetryAt)
	assert.Zero(t, status.PermanentFailures)
	require.Len(t, status.Sources, 1)
	assert.Equal(t, "claude", string(status.Sources[0].Provider))
	assert.Equal(t, "legacy-root", status.Sources[0].ConfiguredRootID)
	assert.NotEmpty(t, status.Sources[0].SourceID)
	assert.NotContains(t, status.Sources[0].SourceID, "private-source-key")
	assert.Empty(t, status.Sources[0].LatestCaptureID)
	assert.Equal(t, validCheckpointDigest(1), status.Sources[0].Head.ManifestID)
	assert.Equal(t, validCheckpointDigest(2), status.Sources[0].Head.Receipt)
	assert.Equal(t, int64(7), status.Sources[0].Head.Generation)
	assert.Equal(t, updatedAt, status.Sources[0].UpdatedAt)
	assert.Equal(t, updatedAt, status.Sources[0].Head.UpdatedAt)
	assert.Empty(t, status.Coverage)
	inspection, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspection.Close()) })
	var version int
	require.NoError(t, inspection.QueryRow(`PRAGMA user_version`).Scan(&version))
	assert.Equal(t, 1, version, "read-only status must not migrate the checkpoint")
}

func TestOpenReadOnlyStatusSupportsVersionTwoCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	db, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(t, err)
	for _, statements := range [][]string{
		versionOneSchemaStatements,
		versionTwoMigrationStatements,
	} {
		for _, statement := range statements {
			_, err = db.Exec(statement)
			require.NoError(t, err)
		}
	}
	_, err = db.Exec(`INSERT INTO outbox_config (id, spool_path) VALUES (1, 'spool')`)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA user_version = 2`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	reader, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	status, err := reader.ClientStatus(t.Context())

	require.NoError(t, err)
	assert.Equal(t, defaultMaxOutboxBytes, status.Outbox.LimitBytes)
	assert.Zero(t, status.PendingGenerations)
	assert.Nil(t, status.RetryAt)
	assert.Zero(t, status.PermanentFailures)
	inspection, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspection.Close()) })
	var version int
	require.NoError(t, inspection.QueryRow(`PRAGMA user_version`).Scan(&version))
	assert.Equal(t, 2, version, "read-only status must not migrate the checkpoint")
}

func TestOpenReadOnlyStatusSupportsVersionFiveCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	db, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(t, err)
	for _, statements := range [][]string{
		versionOneSchemaStatements,
		versionTwoMigrationStatements,
		versionThreeMigrationStatements,
		versionFourMigrationStatements,
		versionFiveMigrationStatements,
	} {
		for _, statement := range statements {
			_, err = db.Exec(statement)
			require.NoError(t, err)
		}
	}
	_, err = db.Exec(`INSERT INTO outbox_config (id, spool_path) VALUES (1, 'spool')`)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA user_version = 5`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	reader, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	status, err := reader.ClientStatus(t.Context())

	require.NoError(t, err)
	assert.Equal(t, defaultMaxOutboxBytes, status.Outbox.LimitBytes)
}
