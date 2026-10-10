package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawarchive"
)

func TestArchiveModeRecoversInterruptedCompaction(t *testing.T) {
	if os.Getenv("AGENTSVIEW_COMPACT_SYNC_HELPER") == "1" {
		doSync(SyncConfig{Host: "host-a.example", Full: true})
		return
	}
	for _, entry := range []string{"serve", "archive", "watch", "backfill", "sync"} {
		for _, primary := range []string{"missing", "unreadable"} {
			t.Run(entry+"/"+primary, func(t *testing.T) {
				cfg := testConfigWithClaudeFixture(t)
				t.Setenv("AGENTSVIEW_DATA_DIR", cfg.DataDir)
				database, err := db.OpenIsolatedContext(t.Context(), cfg.DBPath)
				require.NoError(t, err)
				require.NoError(t, database.EnableArchiveOnly(t.Context()))
				require.NoError(t, database.Close())
				inspection, err := sql.Open("sqlite3", cfg.DBPath)
				require.NoError(t, err)
				var version int
				require.NoError(t, inspection.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version))
				rows, err := inspection.QueryContext(t.Context(), `SELECT type, name, COALESCE(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
				require.NoError(t, err)
				defer rows.Close()
				schema := sha256.New()
				for rows.Next() {
					var kind, name, statement string
					require.NoError(t, rows.Scan(&kind, &name, &statement))
					fmt.Fprintf(schema, "%s\x00%s\x00%s\n", kind, name, statement)
				}
				require.NoError(t, rows.Err())
				counts := map[string]int64{}
				for _, table := range []string{"sessions", "messages", "tool_calls", "tool_result_events", "recall_entries", "recall_evidence"} {
					var count int64
					require.NoError(t, inspection.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count))
					counts[table] = count
				}
				require.NoError(t, inspection.Close())
				backup, err := os.ReadFile(cfg.DBPath)
				require.NoError(t, err)
				opDir := filepath.Join(cfg.DataDir, "compact-staging", "agentsview-compact-interrupted")
				require.NoError(t, os.MkdirAll(opDir, 0o700))
				backupPath := filepath.Join(opDir, "sessions.original.db")
				require.NoError(t, os.WriteFile(backupPath, backup, 0o600))
				digest := sha256.Sum256(backup)
				manifest, err := json.Marshal(map[string]any{
					"version": 1, "phase": "prepared", "database_path": cfg.DBPath,
					"original_backup_path": backupPath, "compacted_path": filepath.Join(opDir, "sessions.compacted.db"),
					"installing_path": cfg.DBPath + ".installing", "original_sha256": hex.EncodeToString(digest[:]),
					"original_bytes": len(backup), "compacted_sha256": hex.EncodeToString(digest[:]),
					"expected_compacted_bytes": len(backup), "expected_user_version": version,
					"expected_schema_hash": hex.EncodeToString(schema.Sum(nil)), "expected_counts": counts,
				})
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, "compact-recovery.json"), manifest, 0o600))
				if primary == "missing" {
					require.NoError(t, os.Remove(cfg.DBPath))
				} else {
					require.NoError(t, os.WriteFile(cfg.DBPath, []byte("interrupted install"), 0o600))
				}
				switch entry {
				case "serve":
					enabled, err := archiveModeAfterRecovery(t.Context(), cfg)
					require.NoError(t, err)
					assert.True(t, enabled)
				case "archive":
					called := false
					cmd := &cobra.Command{}
					cmd.SetContext(t.Context())
					err = withRawArchive(cmd, func(*rawarchive.Archive) error { called = true; return nil })
					require.NoError(t, err)
					assert.True(t, called)
				case "sync":
					var requests atomic.Int32
					remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						requests.Add(1)
						http.Error(w, "unexpected transfer", http.StatusForbidden)
					}))
					defer remote.Close()
					require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, "config.toml"), []byte(fmt.Sprintf("[[remote_hosts]]\nhost = \"host-a.example\"\nurl = %q\ntoken = \"test-token\"\n", remote.URL)), 0o600))
					child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestArchiveModeRecoversInterruptedCompaction$")
					child.Env = append(os.Environ(), "AGENTSVIEW_COMPACT_SYNC_HELPER=1", "AGENTSVIEW_NO_DAEMON=1")
					out, err := child.CombinedOutput()
					var exitErr *exec.ExitError
					require.ErrorAs(t, err, &exitErr, string(out))
					assert.Equal(t, 1, exitErr.ExitCode())
					assert.Contains(t, string(out), db.ErrArchiveOnly.Error())
					assert.Zero(t, requests.Load(), "rejection must precede remote transfer")
				default:
					err = runRawSyncWithoutHostWork(t, cfg, entry)
					require.ErrorIs(t, err, db.ErrArchiveOnly)
				}
				assert.NoFileExists(t, filepath.Join(cfg.DataDir, "compact-recovery.json"))
				enabled, err := db.ArchiveOnlyAt(t.Context(), cfg.DBPath)
				require.NoError(t, err)
				assert.True(t, enabled)
			})
		}
	}
}
