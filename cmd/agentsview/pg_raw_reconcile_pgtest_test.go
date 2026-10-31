//go:build pgtest

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawsync"
)

// The SQL-only command must report custody findings without acquiring the raw
// repository. A dry run must leave the selected tuple intact.
func TestPGRawReconcileDryRunAndRepair(t *testing.T) {
	cfg, admin := hostedRuntimeConfig(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", cfg.DataDir)
	writeTestConfig(t, cfg.DataDir, fmt.Sprintf("default_pg = \"hosted\"\n[pg.hosted]\nurl = %q\nschema = %q\nraw_tenant = %q\n", cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant))
	runtime, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close()) })
	auth, err := postgres.NewTenantRawDeviceAuthStore(runtime, cfg.PG.RawTenant)
	require.NoError(t, err)
	service, err := rawsync.NewDeviceAuthService(auth, time.Minute)
	require.NoError(t, err)
	device, err := service.EnrollDevice(t.Context(), cfg.PG.RawTenant, "synthetic")
	require.NoError(t, err)
	jobs, err := postgres.NewHostedRawIngestStore(runtime, cfg.PG.RawTenant, "durable-parser")
	require.NoError(t, err)
	manifest, err := rawsync.ValidateAndCanonicalize(device.Identity, rawsync.Manifest{SchemaVersion: 1, Provider: parser.AgentClaude, ConfiguredRootID: "root", SourceKey: "departed.jsonl", CaptureID: "capture", CapturedAt: time.Now().UTC(), Kind: rawsync.ManifestTombstone}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	_, err = jobs.CommitManifest(t.Context(), manifest, "durable-parser")
	require.NoError(t, err)
	_, err = runtime.ExecContext(t.Context(), `DELETE FROM raw_projection_generations;DELETE FROM raw_source_projections;UPDATE raw_ingest_jobs SET attempt_count=3,lease_epoch=19`)
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `INSERT INTO raw_custody_findings(tenant_id,manifest_id,kind,key,size) VALUES($1,$2,'object','synthetic',1)`, cfg.PG.RawTenant, manifest.ManifestID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, pgRawSyncDataDirectory), []byte("occupied"), 0600))
	for _, args := range [][]string{{}, {"--repair"}} {
		command := newPGRawReconcileCommand()
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetArgs(args)
		require.NoError(t, command.ExecuteContext(t.Context()))
		require.Contains(t, out.String(), fmt.Sprintf("scanned=1 found=1 repaired=%d unrecoverable=0", len(args)))
		require.Contains(t, out.String(), "historical_custody_findings=1")
		require.Contains(t, out.String(), "Historical custody findings require investigation; restored bytes may already be available.")
		var selected int
		require.NoError(t, runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_source_projections`).Scan(&selected))
		require.Equal(t, len(args), selected)
	}
	var version string
	var epoch int64
	var attempt int
	require.NoError(t, runtime.QueryRowContext(t.Context(), `SELECT processing_version,lease_epoch,attempt_count FROM raw_ingest_jobs`).Scan(&version, &epoch, &attempt))
	require.Equal(t, "durable-parser", version)
	require.Equal(t, int64(20), epoch)
	require.Zero(t, attempt)
}
