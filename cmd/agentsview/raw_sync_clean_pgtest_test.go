//go:build pgtest

package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/postgres"
)

const rawSyncCleanUploadsSchema = "agentsview_raw_sync_clean_test"

func TestRawSyncCleanUploadsCommand(t *testing.T) {
	pg, dataDir, pgURL := newRawSyncCleanUploadsPG(t)
	configureRawSyncCleanUploads(t, dataDir, pgURL)

	now := time.Now().UTC().Truncate(time.Microsecond)
	dueID := "clean-due-upload"
	futureID := "clean-future-upload"
	terminalID := "clean-terminal-upload"
	orphanID := "clean-orphan-upload"
	insertRawSyncCleanUpload(
		t, pg, dueID, 1, "open", 0,
		now.Add(-2*time.Hour), now.Add(-time.Hour), nil,
	)
	insertRawSyncCleanUpload(
		t, pg, futureID, 2, "open", 6,
		now, now.Add(time.Hour), nil,
	)
	terminalCompletedAt := now.Add(-30 * time.Minute)
	insertRawSyncCleanUpload(
		t, pg, terminalID, 3, "complete", 8,
		now.Add(-2*time.Hour), now.Add(-time.Hour), &terminalCompletedAt,
	)

	spoolDir := filepath.Join(dataDir, "raw-upload-spool")
	require.NoError(t, os.MkdirAll(spoolDir, 0o700))
	writeRawSyncCleanUploadsFile(t, filepath.Join(spoolDir, dueID+".part"), "due")
	writeRawSyncCleanUploadsFile(t, filepath.Join(spoolDir, futureID+".part"), "future")
	writeRawSyncCleanUploadsFile(t, filepath.Join(spoolDir, terminalID+".part"), "terminal")
	writeRawSyncCleanUploadsFile(t, filepath.Join(spoolDir, orphanID+".part"), "orphan")
	writeRawSyncCleanUploadsFile(t, filepath.Join(spoolDir, "keep.txt"), "keep")

	checkpoint := filepath.Join(dataDir, "raw-sync", "checkpoint.sentinel")
	accepted := filepath.Join(dataDir, "accepted-storage", "object.sentinel")
	writeRawSyncCleanUploadsFile(t, checkpoint, "checkpoint")
	writeRawSyncCleanUploadsFile(t, accepted, "accepted")

	output, err := executeCommand(newRootCommand(), "raw-sync", "clean-uploads")

	require.NoError(t, err)
	assert.Equal(t, rawSyncCleanUploadsCompletion+"\n", output)
	assertRawSyncCleanUploadAbsent(t, pg, dueID)
	assertRawSyncCleanUploadAbsent(t, pg, terminalID)

	var state string
	var expiresAt time.Time
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT state, expires_at
		FROM raw_upload_sessions
		WHERE upload_id = $1`, futureID).Scan(&state, &expiresAt))
	assert.Equal(t, "open", state)
	assert.WithinDuration(t, now.Add(time.Hour), expiresAt, time.Second)

	assert.NoFileExists(t, filepath.Join(spoolDir, dueID+".part"))
	assert.NoFileExists(t, filepath.Join(spoolDir, terminalID+".part"))
	assert.NoFileExists(t, filepath.Join(spoolDir, orphanID+".part"))
	assertFileContents(t, filepath.Join(spoolDir, futureID+".part"), "future")
	assertFileContents(t, filepath.Join(spoolDir, "keep.txt"), "keep")
	assertFileContents(t, checkpoint, "checkpoint")
	assertFileContents(t, accepted, "accepted")
}

func TestRawSyncCleanUploadsFailures(t *testing.T) {
	_, dataDir, pgURL := newRawSyncCleanUploadsPG(t)
	configureRawSyncCleanUploads(t, dataDir, pgURL)

	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "raw-upload-spool"), []byte("occupied"), 0o600,
	))

	output, err := executeCommand(newRootCommand(), "raw-sync", "clean-uploads")

	require.Error(t, err)
	assert.Empty(t, output)
	assert.Contains(t, err.Error(), "raw upload spool")
	assert.NotContains(t, err.Error(), rawSyncCleanUploadsCompletion)
}

func TestRawSyncCleanUploadsRequiresWritableSchema(t *testing.T) {
	pg, dataDir, pgURL := newRawSyncCleanUploadsPG(t)
	const role = "agentsview_raw_cleanup_runtime"
	const password = "agentsview_raw_cleanup_runtime_password"
	_, err := pg.ExecContext(t.Context(), "DROP ROLE IF EXISTS "+role)
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(),
		"CREATE ROLE "+role+" LOGIN PASSWORD '"+password+"'",
	)
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(),
		"GRANT USAGE ON SCHEMA "+rawSyncCleanUploadsSchema+" TO "+role,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pg.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role) })
	parsedURL, err := url.Parse(pgURL)
	require.NoError(t, err)
	parsedURL.User = url.UserPassword(role, password)
	configureRawSyncCleanUploads(t, dataDir, parsedURL.String())
	var logs bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLog) })

	output, err := executeCommand(newRootCommand(), "raw-sync", "clean-uploads")

	require.Error(t, err)
	assert.Empty(t, output)
	assert.Contains(t, err.Error(), "raw sync write privileges missing")
	assert.NotContains(t, logs.String(), "pg serve")
	_, statErr := os.Stat(filepath.Join(dataDir, "raw-upload-spool"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRawSyncCleanUploadsRequiresProvisionedSchema(t *testing.T) {
	pg, dataDir, pgURL := newRawSyncCleanUploadsPG(t)
	configureRawSyncCleanUploads(t, dataDir, pgURL)
	_, err := pg.ExecContext(t.Context(),
		"DROP TABLE raw_upload_sessions",
	)
	require.NoError(t, err)

	output, err := executeCommand(newRootCommand(), "raw-sync", "clean-uploads")

	require.Error(t, err)
	assert.Empty(t, output)
	assert.Contains(t, err.Error(), "raw sync schema is not provisioned")
	_, statErr := os.Stat(filepath.Join(dataDir, "raw-upload-spool"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func newRawSyncCleanUploadsPG(t *testing.T) (*sql.DB, string, string) {
	t.Helper()
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("TEST_PG_URL not set; skipping PG tests")
	}

	admin, err := sql.Open("pgx", pgURL)
	require.NoError(t, err)
	require.NoError(t, admin.PingContext(t.Context()))
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	_, err = admin.ExecContext(t.Context(),
		"DROP SCHEMA IF EXISTS "+rawSyncCleanUploadsSchema+" CASCADE",
	)
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(),
		"CREATE SCHEMA "+rawSyncCleanUploadsSchema,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(
			context.Background(),
			"DROP SCHEMA IF EXISTS "+rawSyncCleanUploadsSchema+" CASCADE",
		)
	})

	pg, err := postgres.Open(pgURL, rawSyncCleanUploadsSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, postgres.EnsureSchema(
		t.Context(), pg, rawSyncCleanUploadsSchema,
	))
	return pg, t.TempDir(), pgURL
}

func configureRawSyncCleanUploads(t *testing.T, dataDir, pgURL string) {
	t.Helper()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_PG_URL", "")
	t.Setenv("AGENTSVIEW_PG_SCHEMA", "")
	config := fmt.Sprintf(
		"[pg]\nurl = %q\nschema = %q\nallow_insecure = true\n",
		pgURL, rawSyncCleanUploadsSchema,
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"), []byte(config), 0o600,
	))
}

func insertRawSyncCleanUpload(
	t *testing.T,
	pg *sql.DB,
	uploadID string,
	shaValue int,
	state string,
	offset int64,
	createdAt time.Time,
	expiresAt time.Time,
	completedAt *time.Time,
) {
	t.Helper()
	const tenantID = "clean-test-tenant"
	const deviceID = "clean-test-device"
	_, err := pg.ExecContext(t.Context(), `
		INSERT INTO raw_devices (
			device_id, tenant_id, display_name, credential_sha256, created_at
		) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (device_id) DO NOTHING`,
		deviceID, tenantID, "cleanup test device", make([]byte, 32), createdAt,
	)
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(), `
		INSERT INTO raw_upload_sessions (
			upload_id, tenant_id, device_id, provider, sha256, size_bytes,
			offset_bytes, state, created_at, updated_at, expires_at, completed_at
		) VALUES ($1, $2, $3, 'codex', $4, 8, $5, $6, $7, $7, $8, $9)`,
		uploadID, tenantID, deviceID,
		fmt.Sprintf("%064x", shaValue), offset, state, createdAt, expiresAt,
		completedAt,
	)
	require.NoError(t, err)
}

func assertRawSyncCleanUploadAbsent(t *testing.T, pg *sql.DB, uploadID string) {
	t.Helper()
	var state string
	err := pg.QueryRowContext(t.Context(), `
		SELECT state FROM raw_upload_sessions WHERE upload_id = $1`, uploadID,
	).Scan(&state)
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func writeRawSyncCleanUploadsFile(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, string(contents))
}
