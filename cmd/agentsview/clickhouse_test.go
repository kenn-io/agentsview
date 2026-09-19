package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestResolveClickHouseTargetSelections_LegacyNamedLookup(t *testing.T) {
	appCfg := config.Config{
		ClickHouse: config.ClickHouseConfig{
			URL:         "clickhouse://legacy",
			MachineName: "legacybox",
		},
	}
	_, err := resolveClickHouseTargetSelections(appCfg, "archive", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single legacy [clickhouse] block")
}

func TestResolveClickHouseTargetSelections_DefaultAndAll(t *testing.T) {
	appCfg := config.Config{
		DefaultClickHouse: "work",
		ClickHouseTargets: map[string]config.ClickHouseConfig{
			"work":    {URL: "clickhouse://work", MachineName: "workbox"},
			"archive": {URL: "clickhouse://archive", MachineName: "archivebox"},
		},
	}

	defaultTarget, err := resolveClickHouseTargetSelections(appCfg, "", false)
	require.NoError(t, err)
	require.Len(t, defaultTarget, 1)
	assert.Equal(t, "work", defaultTarget[0].Name)
	assert.True(t, defaultTarget[0].IsDefault)

	allTargets, err := resolveClickHouseTargetSelections(appCfg, "", true)
	require.NoError(t, err)
	require.Len(t, allTargets, 2)
	assert.Equal(t, "work", allTargets[0].Name)
	assert.Equal(t, "archive", allTargets[1].Name)
}

func TestResolveClickHouseTargetSelections_RejectsTargetWithAll(t *testing.T) {
	appCfg := config.Config{
		DefaultClickHouse: "work",
		ClickHouseTargets: map[string]config.ClickHouseConfig{
			"work": {URL: "clickhouse://work"},
		},
	}
	_, err := resolveClickHouseTargetSelections(appCfg, "work", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be combined with --all")
}

func TestResolveClickHouseTargetSelections_UnknownName(t *testing.T) {
	appCfg := config.Config{
		DefaultClickHouse: "work",
		ClickHouseTargets: map[string]config.ClickHouseConfig{
			"work": {URL: "clickhouse://work"},
		},
	}
	_, err := resolveClickHouseTargetSelections(appCfg, "missing", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `clickhouse target "missing" is not configured`)
}

func TestResolveClickHousePushProjects_FlagExclusivity(t *testing.T) {
	chCfg := config.ClickHouseConfig{Projects: []string{"from-config"}}
	_, _, err := resolveClickHousePushProjects(chCfg, ClickHousePushConfig{
		ProjectsFlag:    "alpha",
		ExcludeProjects: "beta",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--projects and --exclude-projects are mutually exclusive")

	_, _, err = resolveClickHousePushProjects(chCfg, ClickHousePushConfig{
		AllProjects:  true,
		ProjectsFlag: "alpha",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all-projects cannot be combined")

	projects, exclude, err := resolveClickHousePushProjects(chCfg, ClickHousePushConfig{
		ProjectsFlag: "alpha, beta",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "beta"}, projects)
	assert.Empty(t, exclude)
}

func TestNewClickHousePushCommandRejectsAllWatch(t *testing.T) {
	cmd := newClickHousePushCommand()
	cmd.SetArgs([]string{"--all", "--watch"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all cannot be combined with --watch")
}

func TestBuildServiceSpec_ClickHouseLogAndURL(t *testing.T) {
	t.Setenv("AGENTSVIEW_CLICKHOUSE_URL", "")
	dataDir := t.TempDir()
	spec, err := buildServiceSpec(config.Config{
		DataDir: dataDir,
		ClickHouse: config.ClickHouseConfig{
			URL: "clickhouse://localhost:9000/agentsview",
		},
	}, clickHouseServiceKind)
	require.NoError(t, err)
	assert.Equal(t, dataDir, spec.DataDir)
	assert.Equal(t, filepath.Join(dataDir, "clickhouse-watch.log"), spec.LogPath)
	assert.Equal(t, clickHouseServiceKind.Label, spec.Kind.Label)
}
