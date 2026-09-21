package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/storage"
)

func TestClickHouseTargets_LegacyNamedLookup(t *testing.T) {
	appCfg := config.Config{
		ClickHouse: config.ClickHouseConfig{
			URL:         "clickhouse://legacy",
			MachineName: "legacybox",
		},
	}
	_, err := storage.SelectTargets(clickhouse.Backend{}, appCfg, "archive", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single legacy [clickhouse] block")
}

func TestClickHouseTargets_DefaultAndAll(t *testing.T) {
	appCfg := config.Config{
		DefaultClickHouse: "work",
		ClickHouseTargets: map[string]config.ClickHouseConfig{
			"work":    {URL: "clickhouse://work", MachineName: "workbox"},
			"archive": {URL: "clickhouse://archive", MachineName: "archivebox"},
		},
	}

	defaultTarget, err := storage.SelectTargets(clickhouse.Backend{}, appCfg, "", false)
	require.NoError(t, err)
	require.Len(t, defaultTarget, 1)
	assert.Equal(t, "work", defaultTarget[0].Name)
	assert.True(t, defaultTarget[0].IsDefault)

	allTargets, err := storage.SelectTargets(clickhouse.Backend{}, appCfg, "", true)
	require.NoError(t, err)
	require.Len(t, allTargets, 2)
	assert.Equal(t, "work", allTargets[0].Name)
	assert.Equal(t, "archive", allTargets[1].Name)

	resolved, err := clickhouse.Backend{}.ResolveTarget(appCfg, allTargets[1])
	require.NoError(t, err)
	assert.Equal(t, "clickhouse://archive", resolved.Target.URL)
	assert.Equal(t, "archivebox", resolved.Target.MachineName)
	assert.True(t, resolved.Target.PushVectors, "ClickHouse pushes vectors by default")
}

func TestClickHouseTargets_RejectsTargetWithAll(t *testing.T) {
	appCfg := config.Config{
		DefaultClickHouse: "work",
		ClickHouseTargets: map[string]config.ClickHouseConfig{
			"work": {URL: "clickhouse://work"},
		},
	}
	_, err := storage.SelectTargets(clickhouse.Backend{}, appCfg, "work", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be combined with --all")
}

func TestClickHouseTargets_UnknownName(t *testing.T) {
	appCfg := config.Config{
		DefaultClickHouse: "work",
		ClickHouseTargets: map[string]config.ClickHouseConfig{
			"work": {URL: "clickhouse://work"},
		},
	}
	_, err := storage.SelectTargets(clickhouse.Backend{}, appCfg, "missing", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `clickhouse target "missing" is not configured`)
}

func TestNewClickHousePushCommandRejectsAllWatch(t *testing.T) {
	cmd := newReplicaPushCommand(clickhouse.Backend{})
	cmd.SetArgs([]string{"--all", "--watch"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all cannot be combined with --watch")
	assert.Contains(t, err.Error(), "clickhouse push --watch:")
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

// TestReplicaBackendsHaveDaemonPushOperations fails when a registered
// replica has no generated daemon operation, which means the OpenAPI
// document and clients were not regenerated after adding the backend.
func TestReplicaBackendsHaveDaemonPushOperations(t *testing.T) {
	for _, backend := range replicaBackends {
		_, err := replicaPushOperation(backend.Name())
		assert.NoError(t, err, backend.Name())
	}
}

// TestReplicaBackendsHaveServiceKinds pins that every registered replica can
// be installed as a background push service.
func TestReplicaBackendsHaveCommands(t *testing.T) {
	root := newRootCommand()
	for _, backend := range replicaBackends {
		cmd, _, err := root.Find([]string{backend.Name(), "push"})
		require.NoError(t, err, backend.Name())
		assert.Equal(t, "push [target]", cmd.Use)
		_, err = replicaBackendNamed(backend.Name())
		assert.NoError(t, err)
	}
}
