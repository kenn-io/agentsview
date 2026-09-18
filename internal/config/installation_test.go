package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallationIdentityIsIndependentOfTelemetryAndDisplayName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := setupTestEnv(t)
	t.Setenv("AGENTSVIEW_TELEMETRY_ENABLED", "0")
	t.Setenv("TELEMETRY_ENABLED", "0")
	const id = "0123456789abcdef0123456789abcdef"
	require.NoError(os.WriteFile(filepath.Join(dir, "telemetry-install-id"), []byte(id+"\n"), 0o600))
	for _, label := range []string{"host-a.local", "host-b.example", "local"} {
		writeConfig(t, dir, map[string]any{
			"local_machine_name": label,
			"session_sources": []map[string]string{
				{"agent": "claude", "dir": filepath.Join(dir, "sessions")},
				{"agent": "codex", "dir": filepath.Join(dir, "remote"), "machine": "remote-host"},
			},
		})
		cfg, err := LoadMinimal()
		require.NoError(err)
		require.Len(cfg.SessionSources, 2)
		assert.Equal(id, cfg.SessionSources[0].Machine)
		assert.Equal("remote-host", cfg.SessionSources[1].Machine)
		assert.Equal(label, cfg.LocalMachineName)
		cfg.PG.URL = "postgres://localhost/test"
		pg, err := cfg.ResolvePG()
		require.NoError(err)
		assert.Equal(id, pg.MachineName)
		duck, err := cfg.ResolveDuckDB()
		require.NoError(err)
		assert.Equal(id, duck.MachineName)
	}
	writeConfig(t, dir, map[string]any{"cursor_secret": "existing-secret"})
	cfg, err := LoadMinimal()
	require.NoError(err)
	hostname, err := os.Hostname()
	require.NoError(err)
	assert.Equal(hostname, cfg.LocalMachineName, "clearing the override restores the current hostname")
}

func TestInstallationIdentityLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := setupTestEnv(t)
	t.Setenv("AGENTSVIEW_TELEMETRY_ENABLED", "0")
	first, err := LoadMinimal()
	require.NoError(err)
	require.Regexp(`^[0-9a-f]{32}$`, first.InstallationID)
	second, err := LoadMinimal()
	require.NoError(err)
	assert.Equal(first.InstallationID, second.InstallationID, "restart retains installation identity")

	copyDir := t.TempDir()
	require.NoError(os.CopyFS(copyDir, os.DirFS(dir)))
	t.Setenv("AGENTSVIEW_DATA_DIR", copyDir)
	copied, err := LoadMinimal()
	require.NoError(err)
	assert.Equal(first.InstallationID, copied.InstallationID, "copying the data directory copies identity")

	require.NoError(os.Remove(filepath.Join(dir, "telemetry-install-id")))
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	replacement, err := LoadMinimal()
	require.NoError(err)
	assert.NotEqual(first.InstallationID, replacement.InstallationID, "deleting the identity file starts a new installation")

	freshDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", freshDir)
	_, err = LoadReadOnly()
	require.NoError(err)
	entries, err := os.ReadDir(freshDir)
	require.NoError(err)
	assert.Empty(entries, "read-only diagnostics must not initialize installation state")
	fresh, err := LoadMinimal()
	require.NoError(err)
	assert.NotEqual(first.InstallationID, fresh.InstallationID, "a fresh directory starts a new installation")
}

func TestInstallationIdentityRejectsInvalidFileWithoutReplacingIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		content string
	}{
		{"empty", "telemetry-install-id", "\n"},
		{"invalid", "telemetry-install-id", "not-an-installation-id\n"},
		{"wrong length", "telemetry-install-id", "0123456789abcdef\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			dir := setupTestEnv(t)
			path := filepath.Join(dir, tc.file)
			require.NoError(os.WriteFile(path, []byte(tc.content), 0o600))
			for _, load := range []func() (Config, error){LoadMinimal, LoadReadOnly} {
				_, err := load()
				require.ErrorContains(err, tc.file)
				assert.Contains(err.Error(), "restore")
				assert.Contains(err.Error(), "new identity")
			}
			stored, err := os.ReadFile(path)
			require.NoError(err)
			assert.Equal(tc.content, string(stored))
		})
	}
}

func TestInstallationIdentityReadOnlyLeavesFileUnchanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := setupTestEnv(t)
	const id = "0123456789abcdef0123456789abcdef"
	require.NoError(os.WriteFile(filepath.Join(dir, "telemetry-install-id"), []byte(id), 0o600))
	before, err := os.ReadDir(dir)
	require.NoError(err)
	cfg, err := LoadReadOnly()
	require.NoError(err)
	assert.Equal(id, cfg.InstallationID)
	after, err := os.ReadDir(dir)
	require.NoError(err)
	assert.Equal(before, after, "diagnostics leave installation state untouched")
}

func TestInstallationIdentityPreservesReadOnlyConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := setupTestEnv(t)
	path := filepath.Join(dir, configFileName)
	const content = "# Keep this comment and formatting.\ncursor_secret = \"existing-secret\"\n"
	require.NoError(os.WriteFile(path, []byte(content), 0o400))
	cfg, err := LoadMinimal()
	require.NoError(err)
	assert.NotEmpty(cfg.InstallationID)
	assert.NotEmpty(cfg.LocalMachineName)
	assert.Equal("existing-secret", cfg.CursorSecret)
	stored, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(content, string(stored))
}

func TestInstallationIdentityReadsFileWithoutConfigLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	skipIfNotUnix(t)
	dir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(dir, "telemetry-install-id"), []byte("0123456789abcdef0123456789abcdef\n"), 0o400))
	require.NoError(os.Chmod(dir, 0o500))
	t.Cleanup(func() { require.NoError(os.Chmod(dir, 0o700)) })
	cfg := Config{DataDir: dir, LocalMachineName: "host-b.example"}
	require.NoError(cfg.ensureInstallationID())
	assert.Equal("0123456789abcdef0123456789abcdef", cfg.InstallationID)
	assert.Equal("host-b.example", cfg.LocalMachineName)
}

func TestInstallationIdentityConcurrentCreation(t *testing.T) {
	dir := t.TempDir()
	const workers = 8
	ids := make([]string, workers)
	runConcurrent(t, workers, func(i int) error {
		cfg := Config{DataDir: dir}
		if err := cfg.ensureInstallationID(); err != nil {
			return err
		}
		ids[i] = cfg.InstallationID
		return nil
	})
	requireAllSameNonEmpty(t, ids)
	cfg := Config{DataDir: dir}
	require.NoError(t, cfg.ensureInstallationID())
	assert.Equal(t, ids[0], cfg.InstallationID)
}
