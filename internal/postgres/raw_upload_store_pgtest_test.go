//go:build pgtest

package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawUploadStoreResumesAfterRestartAndRepairsCrashTail(t *testing.T) {
	pg, dataDir := newRawUploadTestDatabase(t)
	first, err := NewRawUploadStore(pg, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	identity := rawUploadIdentity(t, pg, "tenant-a", "device-a")
	record := rawUploadRecord(t, identity, "upl_AQEBAQEBAQEBAQEBAQEBAQ", now)
	created, wasCreated, err := first.Create(t.Context(), record)
	require.NoError(t, err)
	assert.True(t, wasCreated)
	assert.Equal(t, record, created)

	resumed, wasCreated, err := first.Create(t.Context(), rawUploadRecord(
		t, identity, "upl_AgICAgICAgICAgICAgICAg", now,
	))
	require.NoError(t, err)
	assert.False(t, wasCreated)
	assert.Equal(t, record.ID, resumed.ID)

	appended, err := first.Append(
		t.Context(), identity, record.ID, 0, []byte("hello"), now,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(5), appended.Offset)
	require.NoError(t, first.Close())

	stagePath := filepath.Join(dataDir, rawUploadSpoolDirectory, record.ID+".part")
	file, err := os.OpenFile(stagePath, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = file.Write([]byte("uncommitted-tail"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	restarted, err := NewRawUploadStore(pg, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	status, err := restarted.Status(t.Context(), identity, record.ID, now)
	require.NoError(t, err)
	assert.Equal(t, int64(5), status.Offset)

	_, err = restarted.Append(
		t.Context(), identity, record.ID, 0, []byte("wrong offset"), now,
	)
	var conflict *rawsync.UploadOffsetConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, int64(5), conflict.CurrentOffset)

	appended, err = restarted.Append(
		t.Context(), identity, record.ID, 5, []byte(" world"), now,
	)
	require.NoError(t, err)
	assert.Equal(t, record.Object.Length, appended.Offset)
	opened, reader, err := restarted.Open(t.Context(), identity, record.ID, now)
	require.NoError(t, err)
	assert.Equal(t, appended, opened)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, []byte("hello world"), data,
		"the uncommitted tail must be truncated before the resumed append")
}

func TestRawUploadStoreFencesTenantDeviceExpiryAndCompletion(t *testing.T) {
	pg, dataDir := newRawUploadTestDatabase(t)
	store, err := NewRawUploadStore(pg, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	identity := rawUploadIdentity(t, pg, "tenant-a", "device-a")
	otherIdentity := rawUploadIdentity(t, pg, "tenant-b", "device-b")
	record := rawUploadRecord(t, identity, "upl_AQEBAQEBAQEBAQEBAQEBAQ", now)
	_, _, err = store.Create(t.Context(), record)
	require.NoError(t, err)

	_, err = store.Status(t.Context(), otherIdentity, record.ID, now)
	assert.ErrorIs(t, err, rawsync.ErrNotFound)
	_, err = store.Append(
		t.Context(), otherIdentity, record.ID, 0, []byte("hello world"), now,
	)
	assert.ErrorIs(t, err, rawsync.ErrNotFound)

	full, err := store.Append(
		t.Context(), identity, record.ID, 0, []byte("hello world"), now,
	)
	require.NoError(t, err)
	assert.Equal(t, record.Object.Length, full.Offset)
	completed, err := store.Complete(t.Context(), identity, record.ID, now)
	require.NoError(t, err)
	assert.True(t, completed.Complete)
	assert.NoFileExists(t,
		filepath.Join(dataDir, rawUploadSpoolDirectory, record.ID+".part"))
	retried, err := store.Append(
		t.Context(), identity, record.ID, record.Object.Length, nil, now,
	)
	require.NoError(t, err)
	assert.True(t, retried.Complete)
	_, err = store.Status(t.Context(), identity, record.ID, record.ExpiresAt)
	assert.ErrorIs(t, err, rawsync.ErrNotFound)
	var retained int
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT count(*) FROM raw_upload_sessions WHERE upload_id = $1`,
		record.ID,
	).Scan(&retained))
	assert.Zero(t, retained)

	expiring := rawUploadRecord(
		t, identity, "upl_AgICAgICAgICAgICAgICAg", now,
	)
	_, _, err = store.Create(t.Context(), expiring)
	require.NoError(t, err)
	_, err = store.Status(t.Context(), identity, expiring.ID, expiring.ExpiresAt)
	assert.ErrorIs(t, err, rawsync.ErrNotFound)
}

func TestRawUploadStoreResetLeavesTruncationToNextLockedAppend(t *testing.T) {
	pg, dataDir := newRawUploadTestDatabase(t)
	store, err := NewRawUploadStore(pg, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	identity := rawUploadIdentity(t, pg, "tenant-a", "device-a")
	record := rawUploadRecord(t, identity, "upl_AQEBAQEBAQEBAQEBAQEBAQ", now)
	_, _, err = store.Create(t.Context(), record)
	require.NoError(t, err)
	_, err = store.Append(
		t.Context(), identity, record.ID, 0, []byte("hello world"), now,
	)
	require.NoError(t, err)

	reset, err := store.Reset(t.Context(), identity, record.ID, 0, now)
	require.NoError(t, err)
	assert.Zero(t, reset.Offset)
	stagePath := filepath.Join(dataDir, rawUploadSpoolDirectory, record.ID+".part")
	info, err := os.Stat(stagePath)
	require.NoError(t, err)
	assert.Equal(t, record.Object.Length, info.Size(),
		"reset must not truncate after releasing the row lock")
	appended, err := store.Append(
		t.Context(), identity, record.ID, 0, []byte("replacement"), now,
	)
	require.NoError(t, err)
	assert.Equal(t, record.Object.Length, appended.Offset)
	_, reader, err := store.Open(t.Context(), identity, record.ID, now)
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, []byte("replacement"), data)
}

func TestRawUploadStoreResetFencesVerifiedGeneration(t *testing.T) {
	pg, dataDir := newRawUploadTestDatabase(t)
	store, err := NewRawUploadStore(pg, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	identity := rawUploadIdentity(t, pg, "tenant-a", "device-a")
	record := rawUploadRecord(t, identity, "upl_AQEBAQEBAQEBAQEBAQEBAQ", now)
	_, _, err = store.Create(t.Context(), record)
	require.NoError(t, err)
	_, err = store.Append(
		t.Context(), identity, record.ID, 0, []byte("hello earth"), now,
	)
	require.NoError(t, err)

	verified, reader, err := store.Open(t.Context(), identity, record.ID, now)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	reset, err := store.Reset(
		t.Context(), identity, record.ID, verified.Generation, now,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(1), reset.Generation)
	_, err = store.Append(
		t.Context(), identity, record.ID, 0, []byte("hello world"), now,
	)
	require.NoError(t, err)

	_, err = store.Reset(
		t.Context(), identity, record.ID, verified.Generation, now,
	)
	assert.ErrorIs(t, err, rawsync.ErrConflict)
	status, err := store.Status(t.Context(), identity, record.ID, now)
	require.NoError(t, err)
	assert.Equal(t, record.Object.Length, status.Offset)
	assert.Equal(t, int64(1), status.Generation)
	_, reader, err = store.Open(t.Context(), identity, record.ID, now)
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, []byte("hello world"), data)
}

func TestRawUploadStoreRetriesFailedStageDirectorySync(t *testing.T) {
	pg, dataDir := newRawUploadTestDatabase(t)
	store, err := NewRawUploadStore(pg, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	syncCalls := 0
	store.syncDirectory = func() error {
		syncCalls++
		if syncCalls == 1 {
			return errors.New("sync failed")
		}
		return nil
	}

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	identity := rawUploadIdentity(t, pg, "tenant-a", "device-a")
	record := rawUploadRecord(t, identity, "upl_AQEBAQEBAQEBAQEBAQEBAQ", now)
	_, _, err = store.Create(t.Context(), record)
	require.NoError(t, err)
	_, err = store.Append(t.Context(), identity, record.ID, 0, []byte("hello"), now)
	require.ErrorContains(t, err, "sync failed")
	stagePath := filepath.Join(dataDir, rawUploadSpoolDirectory, record.ID+".part")
	_, err = os.Stat(stagePath)
	assert.ErrorIs(t, err, os.ErrNotExist)
	status, err := store.Status(t.Context(), identity, record.ID, now)
	require.NoError(t, err)
	assert.Zero(t, status.Offset)

	_, err = store.Append(t.Context(), identity, record.ID, 0, []byte("hello"), now)
	require.NoError(t, err)
	_, err = store.Append(t.Context(), identity, record.ID, 5, []byte(" world"), now)
	require.NoError(t, err)

	assert.Equal(t, 3, syncCalls)
}

func TestRawUploadStoreStartupCleansExpiredCompletedAndOrphanedSpools(t *testing.T) {
	pg, dataDir := newRawUploadTestDatabase(t)
	store, err := NewRawUploadStore(pg, dataDir)
	require.NoError(t, err)
	now := time.Now().UTC()
	past := now.Add(-2 * time.Hour)
	identity := rawUploadIdentity(t, pg, "tenant-a", "device-a")
	records := []rawsync.UploadSession{
		rawUploadRecord(t, identity, "upl_AQEBAQEBAQEBAQEBAQEBAQ", past),
		rawUploadRecord(t, identity, "upl_AgICAgICAgICAgICAgICAg", past),
		rawUploadRecord(t, identity, "upl_AwMDAwMDAwMDAwMDAwMDAw", now),
	}
	bodies := [][]byte{[]byte("hello world"), []byte("hello earth"), []byte("hello mars!")}
	for index := range records {
		records[index].Object = rawCustodyObjectRef(t, bodies[index])
		record := records[index]
		_, _, err = store.Create(t.Context(), record)
		require.NoError(t, err)
		_, err = store.Append(
			t.Context(), identity, record.ID, 0, bodies[index][:5], record.CreatedAt,
		)
		require.NoError(t, err)
	}
	_, err = pg.ExecContext(t.Context(), `
		UPDATE raw_upload_sessions
		SET state = 'complete', offset_bytes = size_bytes, completed_at = $1,
			updated_at = $1
		WHERE upload_id = $2`, now, records[1].ID,
	)
	require.NoError(t, err)
	orphanID := "upl_BAQEBAQEBAQEBAQEBAQEBA"
	spoolDir := filepath.Join(dataDir, rawUploadSpoolDirectory)
	require.NoError(t, os.WriteFile(
		filepath.Join(spoolDir, orphanID+".part"), []byte("orphan"), 0o600,
	))
	require.NoError(t, store.Close())

	restarted, err := NewRawUploadStore(pg, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })

	var terminalRows int
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT count(*) FROM raw_upload_sessions
		WHERE upload_id IN ($1, $2)`,
		records[0].ID, records[1].ID,
	).Scan(&terminalRows))
	assert.Zero(t, terminalRows)
	for _, uploadID := range []string{records[0].ID, records[1].ID, orphanID} {
		assert.NoFileExists(t, filepath.Join(spoolDir, uploadID+".part"))
	}
	assert.FileExists(t, filepath.Join(spoolDir, records[2].ID+".part"))
}

func TestRawUploadStorePeriodicCleanupRunsOnTick(t *testing.T) {
	pg, dataDir := newRawUploadTestDatabase(t)
	store, err := NewRawUploadStore(pg, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	identity := rawUploadIdentity(t, pg, "tenant-a", "device-a")
	record := rawUploadRecord(t, identity, "upl_AQEBAQEBAQEBAQEBAQEBAQ", now)
	_, _, err = store.Create(t.Context(), record)
	require.NoError(t, err)
	_, err = store.Append(t.Context(), identity, record.ID, 0, []byte("hello"), now)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.runCleanupLoop(ctx, ticks)
	}()
	ticks <- record.ExpiresAt
	stagePath := filepath.Join(dataDir, rawUploadSpoolDirectory, record.ID+".part")
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(stagePath)
		return errors.Is(statErr, os.ErrNotExist)
	}, time.Second, 10*time.Millisecond)
	cancel()
	<-done
}

func TestRawUploadStoreCleanupBoundsEachPass(t *testing.T) {
	pg, dataDir := newRawUploadTestDatabase(t)
	store, err := NewRawUploadStore(pg, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	now := time.Now().UTC()
	identity := rawUploadIdentity(t, pg, "tenant-a", "device-a")
	for index := range rawUploadCleanupBatch + 1 {
		_, err = pg.ExecContext(t.Context(), `
			INSERT INTO raw_upload_sessions (
				upload_id, tenant_id, device_id, provider, sha256, size_bytes,
				offset_bytes, state, created_at, updated_at, expires_at
			) VALUES ($1, $2, $3, $4, $5, 1, 0, 'open', $6, $6, $7)`,
			fmt.Sprintf("bounded-cleanup-%03d", index),
			identity.TenantID, identity.DeviceID, parser.AgentCodex,
			fmt.Sprintf("%064x", index+1), now.Add(-2*time.Hour), now.Add(-time.Hour),
		)
		require.NoError(t, err)
	}

	require.NoError(t, store.cleanupExpiredAndOrphaned(t.Context(), now))

	var open int
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT count(*) FILTER (WHERE state = 'open')
		FROM raw_upload_sessions`).Scan(&open))
	assert.Equal(t, 1, open)
}

func newRawUploadTestDatabase(t *testing.T) (*sql.DB, string) {
	t.Helper()
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))
	return pg, t.TempDir()
}

func rawUploadIdentity(
	t *testing.T,
	pg *sql.DB,
	tenantID string,
	deviceID string,
) rawsync.AuthIdentity {
	t.Helper()
	digest := sha256.Sum256([]byte(tenantID + "/" + deviceID))
	_, err := pg.ExecContext(t.Context(), `
		INSERT INTO raw_devices (
			device_id, tenant_id, display_name, credential_sha256, created_at
		) VALUES ($1, $2, $1, $3, $4)`,
		deviceID, tenantID, digest[:],
		time.Date(2026, time.August, 19, 11, 0, 0, 0, time.UTC),
	)
	require.NoError(t, err)
	identity, err := rawsync.NewAuthIdentity(tenantID, deviceID)
	require.NoError(t, err)
	return identity
}

func rawUploadRecord(
	t *testing.T,
	identity rawsync.AuthIdentity,
	uploadID string,
	now time.Time,
) rawsync.UploadSession {
	t.Helper()
	object := rawCustodyObjectRef(t, []byte("hello world"))
	return rawsync.UploadSession{
		ID: uploadID, Identity: identity, Provider: parser.AgentCodex,
		Object: object, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
}
