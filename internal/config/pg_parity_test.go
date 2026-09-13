package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPGParityValidationAndDefaults(t *testing.T) {
	valid := PGConfig{
		RawTenant: "tenant-a", Schema: "hosted", ParityEnabled: true,
		ParityBaselines: map[string]PGParityBaseline{
			"baseline": {Target: "archive", Identity: "00000000-0000-4000-8000-000000000001"},
		},
	}
	require.NoError(t, valid.ValidateParity(true))
	poll, attempt, snapshot := valid.ParityWorkerBounds()
	assert.Equal(t, 2, poll)
	assert.Equal(t, 300, attempt)
	assert.Equal(t, 300, snapshot)

	for _, tc := range []struct {
		name string
		pg   PGConfig
		auth bool
		want string
	}{
		{name: "disabled", pg: PGConfig{}},
		{name: "authentication", pg: valid, want: "authentication"},
		{name: "tenant", pg: func() PGConfig { p := valid; p.RawTenant = ""; return p }(), auth: true, want: "raw_tenant"},
		{name: "profile target", pg: func() PGConfig {
			p := valid
			p.ParityBaselines = map[string]PGParityBaseline{"baseline": {Identity: "00000000-0000-4000-8000-000000000001"}}
			return p
		}(), auth: true, want: "target"},
		{name: "identity", pg: func() PGConfig {
			p := valid
			p.ParityBaselines = map[string]PGParityBaseline{"baseline": {Target: "archive", Identity: "NOT-A-UUID"}}
			return p
		}(), auth: true, want: "identity"},
		{name: "poll", pg: func() PGConfig { p := valid; p.ParityPollSeconds = 61; return p }(), auth: true, want: "bounds"},
		{name: "attempt", pg: func() PGConfig { p := valid; p.ParityAttemptSeconds = 1801; return p }(), auth: true, want: "bounds"},
		{name: "snapshot", pg: func() PGConfig { p := valid; p.ParitySnapshotSeconds = -1; return p }(), auth: true, want: "bounds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.pg.ValidateParity(tc.auth)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestResolveParityBaselineUsesNamedTargetAndSameTenant(t *testing.T) {
	runtime := PGConfig{
		RawTenant: "tenant-a", Schema: "runtime_schema", ParityEnabled: true,
		ParityBaselines: map[string]PGParityBaseline{
			"before": {Target: "archive", Identity: "00000000-0000-4000-8000-000000000001"},
		},
	}
	t.Setenv("PARITY_READER", "reader")
	cfg := Config{RequireAuth: true, DefaultPG: "archive", pgEnvOverrides: pgEnvOverrides{URL: "postgres://redirect.invalid/db", Schema: "redirect_schema"}, PGTargets: map[string]PGConfig{
		"runtime": runtime,
		"archive": {URL: "postgres://${PARITY_READER}@example.invalid/db?sslmode=require", Schema: "archive_schema", RawTenant: "tenant-a"},
	}}

	resolved, profile, err := cfg.ResolveParityBaseline(runtime, "before")
	require.NoError(t, err)
	assert.Equal(t, "archive", resolved.Name)
	assert.True(t, resolved.IsDefault)
	assert.Equal(t, "postgres://reader@example.invalid/db?sslmode=require", resolved.Config.URL)
	assert.Equal(t, "archive_schema", resolved.Config.Schema)
	assert.Equal(t, runtime.ParityBaselines["before"], profile)

	_, _, err = cfg.ResolveParityBaseline(runtime, "missing")
	require.ErrorContains(t, err, "missing")
	wrong := cfg
	wrong.PGTargets = map[string]PGConfig{
		"runtime": runtime,
		"archive": {URL: "postgres://reader@example.invalid/db?sslmode=require", Schema: "archive_schema", RawTenant: "tenant-b"},
	}
	_, _, err = wrong.ResolveParityBaseline(runtime, "before")
	require.ErrorContains(t, err, "tenant")
	wrong.PGTargets["archive"] = PGConfig{URL: "postgres://reader@example.invalid/db?sslmode=require", Schema: "9invalid", RawTenant: "tenant-a"}
	_, _, err = wrong.ResolveParityBaseline(runtime, "before")
	require.ErrorContains(t, err, "schema")
	missingName := "AGENTSVIEW_PARITY_DEFINITELY_MISSING_9B3E8A"
	previous, existed := os.LookupEnv(missingName)
	require.NoError(t, os.Unsetenv(missingName))
	t.Cleanup(func() {
		if existed {
			require.NoError(t, os.Setenv(missingName, previous))
		} else {
			require.NoError(t, os.Unsetenv(missingName))
		}
	})
	wrong.PGTargets["archive"] = PGConfig{URL: "postgres://${" + missingName + "}@example.invalid/db?sslmode=require", Schema: "archive_schema", RawTenant: "tenant-a"}
	_, _, err = wrong.ResolveParityBaseline(runtime, "before")
	require.ErrorContains(t, err, "expanding url")
}

func TestPGParityTOMLFieldsLoadOnNamedTarget(t *testing.T) {
	var cfg Config
	require.NoError(t, cfg.applyConfigTOML(`
default_pg = "runtime"
[pg.runtime]
url = "postgres://runtime.example/db"
schema = "runtime_schema"
raw_tenant = "tenant-a"
parity_enabled = true
parity_poll_seconds = 3
parity_attempt_seconds = 40
parity_snapshot_seconds = 50
[pg.runtime.parity_baselines.before]
target = "archive"
identity = "00000000-0000-4000-8000-000000000001"
[pg.archive]
url = "postgres://archive.example/db"
schema = "archive_schema"
raw_tenant = "tenant-a"
`))
	runtime := cfg.PGTargets["runtime"]
	assert.True(t, runtime.ParityEnabled)
	assert.Equal(t, PGParityBaseline{Target: "archive", Identity: "00000000-0000-4000-8000-000000000001"}, runtime.ParityBaselines["before"])
	assert.Equal(t, 3, runtime.ParityPollSeconds)
	assert.Equal(t, 40, runtime.ParityAttemptSeconds)
	assert.Equal(t, 50, runtime.ParitySnapshotSeconds)
}
