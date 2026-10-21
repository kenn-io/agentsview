package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/archivesource"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawwatch"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestArchiveSourceDaemonRemoteSyncIncludesAttachedHistory(t *testing.T) {
	source := archiveSourceCommandFixture(t)
	remoteDB := dbtest.OpenTestDB(t)
	remote := httptest.NewServer(server.New(config.Config{
		Host: "127.0.0.1", DataDir: t.TempDir(), AuthToken: "remote-token",
	}, remoteDB, nil).Handler())
	t.Cleanup(remote.Close)
	for _, tc := range []struct {
		name         string
		includeLocal bool
		full         bool
		offline      bool
	}{
		{name: "incremental", includeLocal: true},
		{name: "full", includeLocal: true, full: true},
		{name: "partial archive failure", includeLocal: true, offline: true},
		{name: "remote only", offline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfigWithClaudeFixture(t)
			cfg.Host = "127.0.0.1"
			cfg.ArchiveSources = []config.ArchiveSource{source}
			if tc.offline {
				cfg.ArchiveSources = append(cfg.ArchiveSources, config.ArchiveSource{
					Name: "offline", Repository: filepath.Join(t.TempDir(), "missing"), Snapshot: source.Snapshot,
				})
			}
			cfg.RemoteHosts = []config.RemoteHost{{
				Host: "remote", Transport: config.RemoteTransportHTTP,
				URL: remote.URL, Token: "remote-token",
			}}
			database := dbtest.OpenTestDBAt(t, cfg.DBPath)
			engine := sync.NewEngine(t.Context(), database, localSyncEngineConfig(cfg))
			t.Cleanup(engine.Close)
			attached, err := archivesource.NewManager(t.Context(), cfg, database, engine, nil)
			require.NoError(t, err)
			t.Cleanup(attached.Close)
			srv := server.New(cfg, database, engine,
				server.WithArchiveSources(attached),
				server.WithLocalSyncRunner(withAttachedSources(attached,
					newForegroundSyncRunner(t.Context(), cfg, engine, database, nil))),
			)
			body, err := json.Marshal(remoteSyncRequest{
				Full: tc.full, IncludeLocal: tc.includeLocal, Hosts: cfg.RemoteHosts,
			})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			req := httptest.NewRequestWithContext(ctx, http.MethodPost,
				"/api/v1/sync/remotes", bytes.NewReader(body))
			req.Host = "127.0.0.1:0"
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "http://127.0.0.1:0")
			response := httptest.NewRecorder()
			srv.Handler().ServeHTTP(response, req)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var result struct {
				LocalStats *sync.SyncStats `json:"local_stats"`
				Failures   []any           `json:"failures"`
				Error      string          `json:"error"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
			require.Empty(t, result.Failures)
			messages, err := database.GetAllMessages(t.Context(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa~saved")
			require.NoError(t, err)
			if !tc.includeLocal {
				assert.Empty(t, result.Error)
				assert.Nil(t, result.LocalStats)
				assert.Empty(t, messages)
				return
			}
			if tc.offline {
				assert.Equal(t, "local sync failed", result.Error)
			} else {
				assert.Empty(t, result.Error)
			}
			require.NotNil(t, result.LocalStats)
			assert.Equal(t, 4, result.LocalStats.Synced)
			require.Len(t, messages, 1)
			assert.Equal(t, "archived message", messages[0].Content)
		})
	}
}

func TestArchiveSourceNoSyncDaemonManualSync(t *testing.T) {
	source := archiveSourceCommandFixture(t)
	remoteDB := dbtest.OpenTestDB(t)
	remote := httptest.NewServer(server.New(config.Config{
		Host: "127.0.0.1", DataDir: t.TempDir(), AuthToken: "remote-token",
	}, remoteDB, nil).Handler())
	t.Cleanup(remote.Close)
	for _, tc := range []struct {
		name       string
		remote     bool
		full       bool
		onlyRemote bool
	}{
		{name: "local"},
		{name: "local full", full: true},
		{name: "remote and local", remote: true},
		{name: "remote and local full", remote: true, full: true},
		{name: "remote only", remote: true, onlyRemote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfigWithClaudeFixture(t)
			cfg.Host = "127.0.0.1"
			cfg.AuthToken = "receiver-token"
			cfg.RequireAuth = true
			cfg.NoSync = true
			cfg.ArchiveSources = []config.ArchiveSource{source}
			cfg.RemoteHosts = []config.RemoteHost{{
				Host: "remote", Transport: config.RemoteTransportHTTP,
				URL: remote.URL, Token: "remote-token",
			}}
			database := dbtest.OpenTestDBAt(t, cfg.DBPath)
			const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa~saved"
			if tc.full {
				_, _, err := runLocalSyncResult(t.Context(), cfg, database, false)
				require.NoError(t, err)
				require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), `UPDATE messages SET content = 'stale parser output' WHERE session_id = ?`, id)
					return err
				}))
			}
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "retained", Agent: "claude", Machine: cfg.InstallationID,
				FilePath: new(filepath.Join(t.TempDir(), "missing.jsonl")),
			}))
			httpServer := httptest.NewUnstartedServer(nil)
			cfg.PublicURL = "http://" + httpServer.Listener.Addr().String()
			srv := server.New(cfg, database, nil, server.WithBaseContext(t.Context()))
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
			httpServer.Config.Handler = srv.Handler()
			httpServer.Start()
			t.Cleanup(httpServer.Close)
			tr := transport{URL: httpServer.URL}
			if !tc.full {
				// CLI startup reconciliation must not scan a --no-sync daemon.
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
					"/api/v1/sync?startup_only=true", nil)
				req.Host = "127.0.0.1:0"
				req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
				response := httptest.NewRecorder()
				srv.Handler().ServeHTTP(response, req)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var sessions int
				require.NoError(t, database.Reader().QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&sessions))
				assert.Equal(t, 1, sessions, "startup-only requests must leave local and attached sources unread")
			}
			if tc.remote {
				failures, err := runDaemonRemoteSync(t.Context(), tr, cfg.AuthToken, cfg.RemoteHosts, tc.full, !tc.onlyRemote, nil)
				require.NoError(t, err)
				assert.Empty(t, failures)
			} else {
				stats, err := runDaemonSync(t.Context(), tr, cfg.AuthToken, tc.full, nil)
				require.NoError(t, err)
				assert.Equal(t, 4, stats.Synced)
			}
			messages, err := database.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			if tc.onlyRemote {
				assert.Empty(t, messages, "remote-only sync must not drain attached archives")
				return
			}
			require.Len(t, messages, 1)
			assert.Equal(t, "archived message", messages[0].Content)
			retained, err := database.GetSessionFull(t.Context(), "retained")
			require.NoError(t, err)
			assert.NotNil(t, retained, "full sync must keep sessions whose source files are gone")
		})
	}
}

func TestArchiveSourceWorkerRebuildAfterStartupSchemaUpgrade(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, err := db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	engine := sync.NewEngine(t.Context(), database, workerEngineConfig(cfg))
	require.Equal(t, 3, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DROP TABLE archive_session_provenance; DROP TABLE archive_attachment_sources; DROP TABLE archive_source_bindings; DROP TABLE archive_attachments`)
		return err
	}))
	require.NoError(t, database.Close())
	// The daemon upgrades schema before handing a read-only archive to its worker.
	database, lock, err := openWriteDB(t.Context(), cfg)
	require.NoError(t, err)
	closeWriteDB(database, lock)
	var output bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, syncWorkerRequest{Mode: "resync-build"}, &output))
	assert.Equal(t, 3, decodeSingleResult(t, &output).Synced)
}

func archiveSourceCommandFixture(t *testing.T) config.ArchiveSource {
	t.Helper()
	inputs := t.TempDir()
	data := filepath.Join(inputs, "source")
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())
	root := filepath.Join(inputs, "claude")
	dbtest.WriteTestFile(t, filepath.Join(root, "project", "saved.jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "archived message", "saved").String()))
	capture := filepath.Join(inputs, "capture")
	settings := rawarchive.RecoverySettings{LocalMachineName: "retired-desktop"}
	_, err = rawarchive.Capture(t.Context(), rawarchive.CaptureOptions{DataDir: data, Destination: capture, Settings: settings, Roots: []rawarchive.RootSpec{{Provider: "claude", Path: root}}})
	require.NoError(t, err)
	seed := filepath.Join(inputs, "seed")
	_, err = rawarchive.Seed(t.Context(), filepath.Join(capture, "capture.json"), seed, nil)
	require.NoError(t, err)
	database, err = db.OpenIsolatedContext(t.Context(), filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	archive, err := rawarchive.Open(t.Context(), database, seed, nil)
	require.NoError(t, err)
	repo := filepath.Join(t.TempDir(), "repository")
	report, err := archive.Backup(t.Context(), repo, settings, "test")
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, database.Close())
	require.NoError(t, os.RemoveAll(inputs))
	return config.ArchiveSource{Name: "history", Repository: repo, Snapshot: report.SnapshotID}
}

func TestArchiveSourceDirectSyncAndWorkerRebuild(t *testing.T) {
	source := archiveSourceCommandFixture(t)
	cfg := testConfigWithClaudeFixture(t)
	cfg.ArchiveSources = []config.ArchiveSource{source}
	database, err := db.OpenIsolatedContext(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	_, err = database.EnsureInstallationIdentity(t.Context(), cfg.InstallationID)
	require.NoError(t, err)
	_, stats, err := runLocalSyncResult(t.Context(), cfg, database, false)
	require.NoError(t, err)
	assert.Equal(t, 4, stats.Synced, "the command result includes attached history")
	// The actual raw capture audit queues only the three local sources.
	providers, _, err := rawSyncProvidersAndRoots(t.Context(), cfg)
	require.NoError(t, err)
	checkpoint, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
	require.NoError(t, err)
	defer checkpoint.Close()
	require.NoError(t, checkpoint.EnsureDevice(t.Context(), "receiver-device"))
	capturer := rawcapture.New(checkpoint)
	auditor := rawwatch.NewAuditor(checkpoint, capturer, 128)
	defer auditor.Close()
	captured := 0
	for _, provider := range providers {
		result, err := auditor.AuditProviderFull(t.Context(), provider)
		require.NoError(t, err)
		require.Zero(t, result.Degraded)
		captured += result.Captured
	}
	assert.Equal(t, 3, captured)
	queued, err := checkpoint.ClientStatus(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, queued.PendingGenerations)

	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa~saved"
	before, err := database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.NoError(t, database.Close())
	var output bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, syncWorkerRequest{Mode: "resync-build"}, &output))
	result := decodeSingleResult(t, &output)
	assert.Equal(t, 4, result.Synced, "both local and attached sources are rebuilt")
	replacement, err := db.OpenReadOnly(t.Context(), cfg.DBPath+"-resync")
	require.NoError(t, err)
	defer replacement.Close()
	messages, err := replacement.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "archived message", messages[0].Content)
	identity, err := replacement.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, identity)
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", identity.Machine)
}

func TestArchiveSourceSessionRefreshReportsRequiredCommand(t *testing.T) {
	source := archiveSourceCommandFixture(t)
	cfg := testConfigWithClaudeFixture(t)
	cfg.ArchiveSources = []config.ArchiveSource{source}
	database, err := db.OpenIsolatedContext(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	defer database.Close()
	_, _, err = runLocalSyncResult(t.Context(), cfg, database, false)
	require.NoError(t, err)
	engine := sync.NewEngine(t.Context(), database, localSyncEngineConfig(cfg))
	defer engine.Close()
	backend := service.NewDirectBackend(database, engine)
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa~saved"
	before, err := backend.Get(t.Context(), id)
	require.NoError(t, err)
	for _, subagents := range []bool{false, true} {
		_, err := backend.Sync(t.Context(), service.SyncInput{ID: id, Subagents: subagents})
		require.ErrorContains(t, err, "sync --full")
	}
	after, err := backend.Get(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}
