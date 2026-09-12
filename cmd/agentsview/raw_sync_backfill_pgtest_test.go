//go:build pgtest

package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/rawtest"
	"go.kenn.io/agentsview/internal/server"
)

func TestRawSyncBackfillCommandCommitsOnceAndReusesHistoricalProof(t *testing.T) {
	serverCfg, admin := hostedRuntimeConfig(t)
	authStore, err := postgres.NewRawDeviceAuthStore(admin)
	require.NoError(t, err)
	auth, err := rawsync.NewDeviceAuthService(authStore, time.Minute)
	require.NoError(t, err)
	enrolled, err := auth.EnrollDevice(t.Context(), "tenant-runtime", "backfill test device")
	require.NoError(t, err)
	option, closeCustody, err := preparePGRawSyncServices(t.Context(), serverCfg.DataDir, admin)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closeCustody()) })
	rawServer := server.New(config.Config{
		Host: "127.0.0.1", Port: 0, RequireAuth: true,
		AuthToken: "synthetic-shared-token",
	}, nil, nil, option)
	var interruptManifestResponse atomic.Bool
	rawHandler := rawServer.Handler()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/raw-sync/manifests" &&
			interruptManifestResponse.CompareAndSwap(true, false) {
			recorded := httptest.NewRecorder()
			rawHandler.ServeHTTP(recorded, r)
			if recorded.Code >= 200 && recorded.Code < 300 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"code":"internal_error","error":"synthetic interrupted response"}`)
				return
			}
			for name, values := range recorded.Header() {
				w.Header()[name] = values
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
			return
		}
		rawHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)

	dataDir := t.TempDir()
	claudeRoot := filepath.Join(t.TempDir(), "claude")
	claudePath := rawtest.Claude(t, claudeRoot)
	codexRoot := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.MkdirAll(codexRoot, 0o700))
	unselectedRoot := filepath.Join(t.TempDir(), "unreadable-import")
	archivePath := filepath.Join(dataDir, "sessions.db")
	require.NoError(t, os.Mkdir(archivePath, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(archivePath, "sentinel"), []byte("archive untouched"), 0o600))
	configBody := rawSyncBackfillPGConfig(claudeRoot, codexRoot, unselectedRoot)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), configBody, 0o600))
	sourceBefore, err := os.ReadFile(claudePath)
	require.NoError(t, err)
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_RAW_SYNC_URL", httpServer.URL)
	t.Setenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID", enrolled.Identity.DeviceID)
	t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", enrolled.Credential)

	output, err := executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "migration-a",
		"--provider", "claude", "--provider", "codex", "--batch-size", "1", "--format", "json",
		"--allow-insecure-http",
	)
	require.NoError(t, err, output)
	assert.JSONEq(t, `{"run_id":"migration-a","discovery":"sealed","captured":1,"acknowledged":1,"pending":0,"watermark":1,"failures":{},"complete":true}`, output)
	var manifests int
	var minimumGeneration, maximumGeneration int64
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT count(*),min(generation),max(generation) FROM raw_manifests WHERE tenant_id=$1`, "tenant-runtime").Scan(&manifests, &minimumGeneration, &maximumGeneration))
	assert.Equal(t, 1, manifests)
	assert.Equal(t, int64(1), minimumGeneration)
	assert.Equal(t, int64(1), maximumGeneration)
	var manifestID string
	var canonicalManifest []byte
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT manifest_id,canonical_json FROM raw_manifests WHERE tenant_id=$1`, "tenant-runtime").Scan(&manifestID, &canonicalManifest))
	manifestDigest := sha256.Sum256(canonicalManifest)
	assert.Equal(t, hex.EncodeToString(manifestDigest[:]), manifestID)
	assert.Equal(t, configBody, mustReadRawSyncTestFile(t, filepath.Join(dataDir, "config.toml")))
	assert.Equal(t, sourceBefore, mustReadRawSyncTestFile(t, claudePath))
	assert.Equal(t, "archive untouched", string(mustReadRawSyncTestFile(t, filepath.Join(archivePath, "sentinel"))))

	checkpoint, err := rawcheckpoint.Open(t.Context(), rawSyncCheckpointPath(dataDir))
	require.NoError(t, err)
	roots, err := checkpoint.BackfillRoots(t.Context(), "migration-a", parser.AgentClaude)
	require.NoError(t, err)
	require.Len(t, roots, 1)
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{claudeRoot}})
	require.True(t, ok)
	discovered, err := parser.DiscoverRawCaptureSources(t.Context(), provider)
	require.NoError(t, err)
	require.Len(t, discovered.Sources, 1)
	member, found, err := checkpoint.BackfillSource(t.Context(), "migration-a", rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: roots[0].ID,
		SourceKey: discovered.Sources[0].Key,
	})
	require.NoError(t, err)
	require.True(t, found)
	assert.NotEmpty(t, member.Receipt)
	assert.Equal(t, int64(1), member.Generation)
	require.NoError(t, checkpoint.Close())

	rawtest.AppendClaude(t, claudePath)
	for _, providers := range [][]string{{"codex", "claude", "codex"}, {"claude", "codex"}} {
		args := []string{"raw-sync", "backfill", "--run-id", "migration-a", "--batch-size", "512", "--format", "json", "--allow-insecure-http"}
		for _, name := range providers {
			args = append(args, "--provider", name)
		}
		output, err = executeCommand(newRootCommand(), args...)
		require.NoError(t, err, output)
		assert.JSONEq(t, `{"run_id":"migration-a","discovery":"sealed","captured":1,"acknowledged":1,"pending":0,"watermark":1,"failures":{},"complete":true}`, output)
	}
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_manifests WHERE tenant_id=$1`, "tenant-runtime").Scan(&manifests))
	assert.Equal(t, 1, manifests)
	output, err = executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "migration-a",
		"--provider", "claude", "--format", "json", "--allow-insecure-http",
	)
	require.Error(t, err)
	assert.Empty(t, output)
	assert.Contains(t, err.Error(), "conflicts with durable state")

	otherHTTPServer := httptest.NewServer(rawHandler)
	t.Cleanup(otherHTTPServer.Close)
	output, err = executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "migration-a",
		"--provider", "claude", "--provider", "codex", "--format", "json",
		"--server", otherHTTPServer.URL, "--allow-insecure-http",
	)
	require.Error(t, err)
	assert.Empty(t, output)
	assert.Contains(t, err.Error(), "conflicts with durable state")

	output, err = executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "migration-a",
		"--provider", "claude", "--provider", "codex", "--format", "json",
		"--device-id", "different-device", "--allow-insecure-http",
	)
	require.Error(t, err)
	assert.Empty(t, output)
	assert.Contains(t, err.Error(), "device does not match")

	changedRoot := filepath.Join(t.TempDir(), "changed-claude")
	require.NoError(t, os.MkdirAll(changedRoot, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		rawSyncBackfillPGConfig(changedRoot, codexRoot, unselectedRoot), 0o600,
	))
	output, err = executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "migration-a",
		"--provider", "claude", "--provider", "codex", "--format", "json",
		"--allow-insecure-http",
	)
	require.Error(t, err)
	assert.Empty(t, output)
	assert.Contains(t, err.Error(), "conflicts with durable state")
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), configBody, 0o600))

	interruptManifestResponse.Store(true)
	output, err = executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "receipt-a",
		"--provider", "claude", "--format", "json", "--allow-insecure-http",
	)
	require.Error(t, err)
	assert.True(t, isSilentExitError(err))
	interrupted := decodeRawSyncBackfillProgress(t, output)
	assert.Equal(t, "receipt-a", interrupted.RunID)
	assert.Equal(t, int64(1), interrupted.Captured)
	assert.Zero(t, interrupted.Acknowledged)
	assert.Equal(t, int64(1), interrupted.Pending)
	assert.Equal(t, int64(1), interrupted.Failures["upload"])
	assert.False(t, interrupted.Complete)
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_manifests WHERE tenant_id=$1`, "tenant-runtime").Scan(&manifests))
	assert.Equal(t, 2, manifests, "custody committed before its response was interrupted")
	checkpoint, err = rawcheckpoint.Open(t.Context(), rawSyncCheckpointPath(dataDir))
	require.NoError(t, err)
	member, found, err = checkpoint.BackfillSource(t.Context(), "receipt-a", rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: roots[0].ID,
		SourceKey: discovered.Sources[0].Key,
	})
	require.NoError(t, err)
	require.True(t, found)
	assert.Empty(t, member.Receipt, "an interrupted response cannot acknowledge local custody")
	require.NoError(t, checkpoint.Close())
	makeRawSyncBackfillRetryDue(t, rawSyncCheckpointPath(dataDir))

	output, err = executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "receipt-a",
		"--provider", "claude", "--format", "json", "--allow-insecure-http",
	)
	require.NoError(t, err, output)
	assert.JSONEq(t, `{"run_id":"receipt-a","discovery":"sealed","captured":1,"acknowledged":1,"pending":0,"watermark":1,"failures":{},"complete":true}`, output)
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_manifests WHERE tenant_id=$1`, "tenant-runtime").Scan(&manifests))
	assert.Equal(t, 2, manifests, "receipt retry must replay the committed generation")
	checkpoint, err = rawcheckpoint.Open(t.Context(), rawSyncCheckpointPath(dataDir))
	require.NoError(t, err)
	member, found, err = checkpoint.BackfillSource(t.Context(), "receipt-a", rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: roots[0].ID,
		SourceKey: discovered.Sources[0].Key,
	})
	require.NoError(t, err)
	require.True(t, found)
	assert.NotEmpty(t, member.Receipt)
	assert.Equal(t, int64(2), member.Generation)
	require.NoError(t, checkpoint.Close())

	rawtest.AppendClaude(t, claudePath)
	revoked, err := auth.RevokeDevice(t.Context(), enrolled.Identity)
	require.NoError(t, err)
	require.True(t, revoked)
	output, err = executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "revoked-a",
		"--provider", "claude", "--format", "json", "--allow-insecure-http",
	)
	require.Error(t, err)
	assert.True(t, isSilentExitError(err))
	assert.JSONEq(t, `{"run_id":"revoked-a","discovery":"sealed","captured":1,"acknowledged":0,"pending":1,"watermark":1,"failures":{"upload":1},"complete":false}`, output)
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_manifests WHERE tenant_id=$1`, "tenant-runtime").Scan(&manifests))
	assert.Equal(t, 2, manifests, "revocation must not acknowledge or commit another generation")

	require.NoError(t, os.RemoveAll(claudeRoot))
	output, err = executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "migration-a",
		"--provider", "claude", "--provider", "codex", "--format", "json", "--allow-insecure-http",
	)
	require.NoError(t, err, output)
	assert.JSONEq(t, `{"run_id":"migration-a","discovery":"sealed","captured":1,"acknowledged":1,"pending":0,"watermark":1,"failures":{},"complete":true}`, output)
}

func TestRawSyncBackfillCommandRefusesChecksumFailure(t *testing.T) {
	serverCfg, admin := hostedRuntimeConfig(t)
	authStore, err := postgres.NewRawDeviceAuthStore(admin)
	require.NoError(t, err)
	auth, err := rawsync.NewDeviceAuthService(authStore, time.Minute)
	require.NoError(t, err)
	enrolled, err := auth.EnrollDevice(t.Context(), "tenant-runtime", "checksum test device")
	require.NoError(t, err)
	option, closeCustody, err := preparePGRawSyncServices(t.Context(), serverCfg.DataDir, admin)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closeCustody()) })
	rawServer := server.New(config.Config{
		Host: "127.0.0.1", Port: 0, RequireAuth: true,
		AuthToken: "synthetic-shared-token",
	}, nil, nil, option)
	rawHandler := rawServer.Handler()
	var rejected atomic.Int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && rejected.CompareAndSwap(0, 1) {
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil || len(body) == 0 {
				http.Error(w, "could not corrupt synthetic upload", http.StatusInternalServerError)
				return
			}
			body[0] ^= 0xff
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			rawHandler.ServeHTTP(w, r)
			return
		}
		rawHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)

	dataDir := t.TempDir()
	claudeRoot := filepath.Join(t.TempDir(), "claude")
	claudePath := rawtest.Claude(t, claudeRoot)
	configBody := []byte(fmt.Sprintf("[agents.claude]\ndirs = [%q]\n", claudeRoot))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), configBody, 0o600))
	sourceBefore := mustReadRawSyncTestFile(t, claudePath)
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_RAW_SYNC_URL", httpServer.URL)
	t.Setenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID", enrolled.Identity.DeviceID)
	t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", enrolled.Credential)

	output, err := executeCommand(
		newRootCommand(), "raw-sync", "backfill", "--run-id", "checksum-a",
		"--provider", "claude", "--format", "json", "--allow-insecure-http",
	)

	require.Error(t, err)
	assert.True(t, isSilentExitError(err))
	progress := decodeRawSyncBackfillProgress(t, output)
	assert.Equal(t, "checksum-a", progress.RunID)
	assert.Equal(t, int64(1), progress.Captured)
	assert.Zero(t, progress.Acknowledged)
	assert.Equal(t, int64(1), progress.Pending)
	assert.Equal(t, int64(1), progress.Failures["upload"])
	assert.False(t, progress.Complete)
	assert.Equal(t, int64(1), rejected.Load(), "the authenticated upload reached checksum verification")
	var manifests int
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_manifests WHERE tenant_id=$1`, "tenant-runtime").Scan(&manifests))
	assert.Zero(t, manifests, "checksum refusal must not commit a manifest")

	checkpoint, err := rawcheckpoint.Open(t.Context(), rawSyncCheckpointPath(dataDir))
	require.NoError(t, err)
	roots, err := checkpoint.BackfillRoots(t.Context(), "checksum-a", parser.AgentClaude)
	require.NoError(t, err)
	require.Len(t, roots, 1)
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{claudeRoot}})
	require.True(t, ok)
	discovered, err := parser.DiscoverRawCaptureSources(t.Context(), provider)
	require.NoError(t, err)
	require.Len(t, discovered.Sources, 1)
	member, found, err := checkpoint.BackfillSource(t.Context(), "checksum-a", rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: roots[0].ID,
		SourceKey: discovered.Sources[0].Key,
	})
	require.NoError(t, err)
	require.True(t, found)
	assert.Empty(t, member.Receipt)
	assert.Zero(t, member.Generation)
	require.NoError(t, checkpoint.Close())
	assert.Equal(t, configBody, mustReadRawSyncTestFile(t, filepath.Join(dataDir, "config.toml")))
	assert.Equal(t, sourceBefore, mustReadRawSyncTestFile(t, claudePath))
}

func rawSyncBackfillPGConfig(claudeRoot, codexRoot, unselectedRoot string) []byte {
	return []byte(fmt.Sprintf(
		"[agents.claude]\ndirs = [%q]\n[agents.codex]\ndirs = [%q]\n[agents.cursor]\ndirs = [%q]\n",
		claudeRoot, codexRoot, unselectedRoot,
	))
}

func makeRawSyncBackfillRetryDue(t *testing.T, checkpointPath string) {
	t.Helper()
	database, err := sql.Open("sqlite3", checkpointPath)
	require.NoError(t, err)
	result, err := database.Exec(`UPDATE outbox_generations SET retry_at='' WHERE retry_at!=''`)
	require.NoError(t, err)
	changed, err := result.RowsAffected()
	require.NoError(t, err)
	assert.Equal(t, int64(1), changed)
	require.NoError(t, database.Close())
}

func mustReadRawSyncTestFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	return body
}
