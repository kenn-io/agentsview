package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadFile_ClickHouseConfig(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		envURL string
		want   ClickHouseConfig
	}{
		{
			"NoConfig",
			map[string]any{},
			"",
			ClickHouseConfig{},
		},
		{
			"FromConfigFile",
			map[string]any{
				"clickhouse": map[string]any{
					"url":          "clickhouse://localhost:9000/agentsview",
					"machine_name": "laptop",
				},
			},
			"",
			ClickHouseConfig{
				URL:         "clickhouse://localhost:9000/agentsview",
				MachineName: "laptop",
			},
		},
		{
			"EnvOverridesConfig",
			map[string]any{
				"clickhouse": map[string]any{
					"url": "clickhouse://from-config",
				},
			},
			"clickhouse://from-env",
			ClickHouseConfig{
				URL: "clickhouse://from-env",
			},
		},
		{
			"PushVectorsOptOut",
			map[string]any{
				"clickhouse": map[string]any{
					"url":          "clickhouse://from-config",
					"push_vectors": false,
				},
			},
			"",
			ClickHouseConfig{
				URL:         "clickhouse://from-config",
				PushVectors: new(false),
			},
		},
		{
			"EnvURLMergesFileFields",
			map[string]any{
				"clickhouse": map[string]any{
					"url":          "clickhouse://from-config",
					"machine_name": "laptop",
				},
			},
			"clickhouse://from-env",
			ClickHouseConfig{
				URL:         "clickhouse://from-env",
				MachineName: "laptop",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newConfigFixture(t)
			f.WriteTOML(t, tt.config)
			if tt.envURL != "" {
				t.Setenv("AGENTSVIEW_CLICKHOUSE_URL", tt.envURL)
			}

			cfg := f.LoadMinimal(t)

			resolved, err := cfg.ResolveClickHouse()
			require.NoError(t, err)

			assert.Equal(t, tt.want.URL, resolved.URL)
			if tt.want.MachineName == "" {
				assert.NotEmpty(t, resolved.MachineName)
			} else {
				assert.Equal(t, tt.want.MachineName, resolved.MachineName)
			}
			assert.Equal(t, "agentsview", resolved.Database)
			assert.Equal(t, tt.want.PushVectorsEnabled(), resolved.PushVectorsEnabled())
		})
	}
}

func TestResolveClickHouseTarget_NamedTargets(t *testing.T) {
	cfg := Config{
		DefaultClickHouse: "work",
		ClickHouseTargets: map[string]ClickHouseConfig{
			"work": {
				URL:         "clickhouse://work",
				MachineName: "workbox",
			},
			"archive": {
				URL:         "clickhouse://archive",
				MachineName: "archivebox",
			},
		},
		clickHouseEnvOverrides: clickHouseEnvOverrides{
			URL:         "clickhouse://env-default",
			MachineName: "envbox",
		},
	}

	defaultTarget, err := cfg.ResolveClickHouse()
	require.NoError(t, err)
	assert.Equal(t, "clickhouse://env-default", defaultTarget.URL)
	assert.Equal(t, "envbox", defaultTarget.MachineName)

	archiveTarget, err := cfg.ResolveClickHouseTarget("archive")
	require.NoError(t, err)
	assert.Equal(t, "clickhouse://archive", archiveTarget.URL)
	assert.Equal(t, "archivebox", archiveTarget.MachineName)
	assert.Equal(t, "agentsview", archiveTarget.Database)
}

func TestResolveClickHouse_UsesURLPathDatabase(t *testing.T) {
	cfg := Config{
		ClickHouse: ClickHouseConfig{
			URL: "clickhouse://localhost:9000/mirror_from_url",
		},
	}
	resolved, err := cfg.ResolveClickHouse()
	require.NoError(t, err)
	assert.Equal(t, "mirror_from_url", resolved.Database)

	cfg.ClickHouse.Database = "explicit_db"
	resolved, err = cfg.ResolveClickHouse()
	require.NoError(t, err)
	assert.Equal(t, "explicit_db", resolved.Database)
}

func TestResolveClickHouseTargets_DefaultFirst(t *testing.T) {
	cfg := Config{
		DefaultClickHouse: "work",
		ClickHouseTargets: map[string]ClickHouseConfig{
			"archive": {URL: "clickhouse://archive"},
			"work":    {URL: "clickhouse://work"},
		},
	}

	targets, err := cfg.ResolveClickHouseTargets()
	require.NoError(t, err)
	require.Len(t, targets, 2)
	assert.Equal(t, "work", targets[0].Name)
	assert.True(t, targets[0].IsDefault)
	assert.Equal(t, "archive", targets[1].Name)
	assert.False(t, targets[1].IsDefault)
}

func TestResolveClickHouseTargets_OneNamedTargetWithoutDefault(t *testing.T) {
	cfg := Config{
		ClickHouseTargets: map[string]ClickHouseConfig{
			"work": {URL: "clickhouse://work"},
		},
	}

	targets, err := cfg.ResolveClickHouseTargets()
	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, "work", targets[0].Name)
	assert.True(t, targets[0].IsDefault)
}

func TestResolveClickHouseTargets_MultipleNamedTargetsRequireDefault(t *testing.T) {
	cfg := Config{
		ClickHouseTargets: map[string]ClickHouseConfig{
			"work":    {URL: "clickhouse://work"},
			"archive": {URL: "clickhouse://archive"},
		},
	}

	_, err := cfg.ResolveClickHouseTargets()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default_clickhouse is required")
}

func TestLoadMinimal_DefersNamedClickHouseValidationForNonClickHouseCommands(t *testing.T) {
	dir := setupTestEnv(t)
	path := filepath.Join(dir, configFileName)
	data := []byte(`
default_clickhouse = "missing"

[clickhouse.work]
url = "clickhouse://work"
`)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	_, err = cfg.ResolveClickHouse()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `default_clickhouse "missing" does not match any named [clickhouse.NAME] target`)
}

func TestLoadFile_ClickHouseMixedLegacyAndNamedTargetsFails(t *testing.T) {
	dir := setupTestEnv(t)
	path := filepath.Join(dir, configFileName)
	data := []byte(`
[clickhouse]
url = "clickhouse://legacy"

[clickhouse.archive]
url = "clickhouse://archive"
`)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err := LoadMinimal()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot mix legacy [clickhouse] fields with named [clickhouse.NAME] targets")
}

func TestLoadFile_ClickHouseNamedTargetValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			name: "reserved all target",
			toml: `
[clickhouse.all]
url = "clickhouse://all"
`,
			wantErr: `named ClickHouse target "all" is reserved`,
		},
		{
			name: "reserved local target",
			toml: `
[clickhouse.local]
url = "clickhouse://local"
`,
			wantErr: `named ClickHouse target "local" is reserved`,
		},
		{
			name: "named target must be table",
			toml: `
[clickhouse]
archive = "clickhouse://archive"
`,
			wantErr: `[clickhouse].archive must be a named target table`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestEnv(t)
			path := filepath.Join(dir, configFileName)
			require.NoError(t, os.WriteFile(path, []byte(tt.toml), 0o600))

			_, err := LoadMinimal()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestResolveClickHouse_DatabaseDefault(t *testing.T) {
	cfg := Config{ClickHouse: ClickHouseConfig{URL: "clickhouse://localhost:9000"}}
	resolved, err := cfg.ResolveClickHouse()
	require.NoError(t, err)
	assert.Equal(t, "agentsview", resolved.Database)
}

func TestResolveClickHouse_EnvOverrideOnlyOnDefault(t *testing.T) {
	dir := setupTestEnv(t)
	path := filepath.Join(dir, configFileName)
	data := []byte(`
default_clickhouse = "work"

[clickhouse.work]
url = "clickhouse://work"
database = "workdb"

[clickhouse.archive]
url = "clickhouse://archive"
database = "archivedb"
`)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	t.Setenv("AGENTSVIEW_CLICKHOUSE_URL", "clickhouse://from-env")
	t.Setenv("AGENTSVIEW_CLICKHOUSE_DATABASE", "envdb")

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	work, err := cfg.ResolveClickHouseTarget("work")
	require.NoError(t, err)
	assert.Equal(t, "clickhouse://from-env", work.URL)
	assert.Equal(t, "envdb", work.Database)

	archive, err := cfg.ResolveClickHouseTarget("archive")
	require.NoError(t, err)
	assert.Equal(t, "clickhouse://archive", archive.URL)
	assert.Equal(t, "archivedb", archive.Database)
}
