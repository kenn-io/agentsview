package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestHostedRawMissingArchiveRefusesPendingCompactRecovery(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{DataDir: dir, DBPath: filepath.Join(dir, "sessions.db"), RawSync: config.RawSyncConfig{Hosted: true}}
	database, err := db.OpenIsolated(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "retained", Agent: "claude", Project: "fixture", Machine: "device"}))
	require.NoError(t, database.Close())
	original, err := os.ReadFile(cfg.DBPath)
	require.NoError(t, err)
	digest := sha256.Sum256(original)
	hash := hex.EncodeToString(digest[:])
	stage := filepath.Join(dir, "compact-staging", "agentsview-compact-fixture")
	require.NoError(t, os.MkdirAll(stage, 0o700))
	manifest, err := json.Marshal(map[string]any{
		"version": 1, "phase": "prepared", "database_path": cfg.DBPath,
		"original_backup_path": filepath.Join(stage, "sessions.original.db"),
		"compacted_path":       filepath.Join(stage, "sessions.compacted.db"),
		"installing_path":      cfg.DBPath + ".installing",
		"original_sha256":      hash, "original_bytes": len(original),
		"compacted_sha256": hash, "expected_compacted_bytes": len(original),
		// Recovery of a hash-verified .failed original does not need a staged
		// candidate's schema proof. These fields keep the manifest complete.
		"expected_schema_hash": "verified-original", "expected_counts": map[string]int{"sessions": 1},
	})
	require.NoError(t, err)
	manifestPath := filepath.Join(dir, "compact-recovery.json")
	require.NoError(t, os.WriteFile(manifestPath, manifest, 0o600))
	require.NoError(t, os.Rename(cfg.DBPath, cfg.DBPath+".failed"))

	database, lock, err := openServeDBWith(t.Context(), cfg, nil)
	if database != nil {
		require.NoError(t, database.Close())
	}
	require.Nil(t, lock)
	require.ErrorContains(t, err, "interrupted archive compaction")
	assert.NoFileExists(t, cfg.DBPath)
	retained, err := os.ReadFile(cfg.DBPath + ".failed")
	require.NoError(t, err)
	assert.Equal(t, original, retained)

	// The ordinary owner path can still recover the complete retained archive.
	cfg.RawSync.Hosted = false
	database, lock, err = openServeDBWith(t.Context(), cfg, openDB)
	require.NoError(t, err)
	defer closeWriteDB(database, lock)
	session, err := database.GetSession(t.Context(), "retained")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.NoFileExists(t, manifestPath)
	assert.NoFileExists(t, cfg.DBPath+".failed")
}

func TestDaemonArchiveQueryRetainsNoSyncPolicy(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		name := "explicit_no_sync"
		if hosted {
			name = "hosted_forced_no_sync"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var syncRequests, statusRequests int
			ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/sessions/codex:stored":
					writeJSONResponse(w, `{"id":"codex:stored","agent":"codex","project":"fixture"}`)
				case "/api/v1/sessions/codex:stored/usage":
					writeJSONResponse(w, `{"session_id":"codex:stored","agent":"codex","total_output_tokens":42,"has_token_data":true}`)
				case "/api/v1/sessions/codex:missing":
					http.NotFound(w, r)
				case "/api/v1/sessions/sync":
					syncRequests++
					w.WriteHeader(http.StatusForbidden)
				case "/api/v1/sync/status":
					statusRequests++
					writeJSONResponse(w, `{"startup_pending":true,"archive_stale":true}`)
				default:
					assert.Failf(t, "unexpected request", "%s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			registerTestRuntime(t, dir, ts.URL, false)
			cfg := config.Config{DataDir: dir, DBPath: filepath.Join(dir, "sessions.db"), RawSync: config.RawSyncConfig{Hosted: hosted}}
			backend, cleanup, err := resolveArchiveQueryBackendWithConfig(t.Context(), cfg, archiveQueryPolicy{NoSync: !hosted})
			require.NoError(t, err)
			t.Cleanup(cleanup)
			stderr := captureStderr(t, func() {
				out, code, err := backend.SessionUsage(t.Context(), sessionUsageQuery{SessionID: "codex:stored"})
				require.NoError(t, err)
				require.NotNil(t, out)
				assert.Equal(t, tokenUseExitOK, code)
				assert.Equal(t, 42, out.TotalOutputTokens)
				assert.False(t, out.RefreshIncomplete)
				out, code, err = backend.SessionUsage(t.Context(), sessionUsageQuery{SessionID: "codex:missing"})
				require.NoError(t, err)
				assert.Nil(t, out)
				assert.Equal(t, tokenUseExitNotFound, code)
			})
			assert.Empty(t, stderr)
			assert.Zero(t, syncRequests)
			assert.Zero(t, statusRequests)
		})
	}
}
