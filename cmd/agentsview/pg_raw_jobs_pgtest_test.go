//go:build pgtest

package main

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestPGRawJobsListAndRetry(t *testing.T) {
	cfg, admin := hostedRuntimeConfig(t)
	runtime, err := postgres.OpenHosted(cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, cfg.PG.AllowInsecure)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close()) })
	auth, err := postgres.NewTenantRawDeviceAuthStore(runtime, cfg.PG.RawTenant)
	require.NoError(t, err)
	service, err := rawsync.NewDeviceAuthService(auth, time.Minute)
	require.NoError(t, err)
	device, err := service.EnrollDevice(t.Context(), cfg.PG.RawTenant, "synthetic device")
	require.NoError(t, err)
	jobs, err := postgres.NewHostedRawIngestStore(runtime, cfg.PG.RawTenant, "parser-test")
	require.NoError(t, err)
	manifest, err := rawsync.ValidateAndCanonicalize(device.Identity, rawsync.Manifest{SchemaVersion: rawsync.ManifestSchemaVersion, Provider: parser.AgentClaude, ConfiguredRootID: "root", SourceKey: "sessions/deleted.jsonl", CaptureID: "capture", CapturedAt: time.Now().UTC(), Kind: rawsync.ManifestTombstone}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	_, err = jobs.CommitManifest(t.Context(), manifest, "parser-test")
	require.NoError(t, err)
	leases, err := jobs.ClaimRawParseJobs(t.Context(), "worker", 1, time.Minute, 5)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, jobs.FailRawParseJob(t.Context(), leases[0], "parse", "parse:internal"))
	t.Setenv("AGENTSVIEW_DATA_DIR", cfg.DataDir)
	writeTestConfig(t, cfg.DataDir, fmt.Sprintf("default_pg = \"hosted\"\n[pg.hosted]\nurl = %q\nschema = %q\nraw_tenant = %q\n", cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant))
	for _, args := range [][]string{{"list"}, {"retry", "--error-class", "parse"}} {
		cmd := newPGRawJobsCommand()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetArgs(args)
		require.NoError(t, cmd.Execute())
		if args[0] == "list" {
			require.Contains(t, output.String(), "failed")
			require.Contains(t, output.String(), "parse")
		} else {
			require.Contains(t, output.String(), "Replayed 1 failed jobs")
		}
	}
	var state string
	var attempt int
	var epoch int64
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT state,attempt_count,lease_epoch FROM raw_ingest_jobs WHERE id=$1`, leases[0].ID).Scan(&state, &attempt, &epoch))
	require.Equal(t, "ready", state)
	require.Zero(t, attempt)
	require.Equal(t, leases[0].LeaseEpoch+1, epoch)
}
