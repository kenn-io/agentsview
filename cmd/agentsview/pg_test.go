package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
)

func loadPGServeConfigForTest(t *testing.T, args ...string) (config.Config, string, error) {
	t.Helper()
	cmd := newPGServeCommand()
	if err := cmd.Flags().Parse(args); err != nil {
		return config.Config{}, "", err
	}
	return loadPGServeConfig(cmd)
}

func TestLoadPGServeConfigDoesNotInheritServeProxySettings(t *testing.T) {
	dataDir := testDataDir(t)

	err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(`
public_url = "https://viewer.example.test"
public_origins = ["https://app.example.test"]

[proxy]
mode = "caddy"
bind_host = "0.0.0.0"
public_port = 8443
tls_cert = "/tmp/viewer.crt"
tls_key = "/tmp/viewer.key"
allowed_subnets = ["10.0.0.0/16"]

[pg]
url = "postgres://user:pass@db.example.test:5432/agentsview?sslmode=require"
`), 0o600)
	require.NoError(t, err)

	cfg, _, err := loadPGServeConfigForTest(t)
	require.NoError(t, err, "loadPGServeConfigForTest")
	require.NotEmpty(t, cfg.PG.URL, "expected PG URL")
	assert.Empty(t, cfg.PublicURL, "PublicURL should be empty")
	assert.Empty(t, cfg.PublicOrigins, "PublicOrigins should be empty")
	assert.Empty(t, cfg.Proxy.Mode, "Proxy.Mode should be empty")
	assert.Equal(t, "127.0.0.1", cfg.Host)
	assert.Equal(t, 8080, cfg.Port)
}

func TestLoadPGServeConfigIgnoresInvalidPersistedServeSettings(t *testing.T) {
	dataDir := testDataDir(t)

	err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(`
public_url = "not a url"

[proxy]
mode = "bogus"

[pg]
url = "postgres://user:pass@db.example.test:5432/agentsview?sslmode=require"
`), 0o600)
	require.NoError(t, err)

	cfg, _, err := loadPGServeConfigForTest(t)
	require.NoError(t, err, "loadPGServeConfigForTest")
	require.NotEmpty(t, cfg.PG.URL, "expected PG URL")
	assert.Empty(t, cfg.PublicURL, "PublicURL should be empty")
	assert.Empty(t, cfg.Proxy.Mode, "Proxy.Mode should be empty")
}

func TestPGServeConfigAcceptsManagedCaddyFlags(t *testing.T) {
	testDataDir(t)

	cfg, basePath, err := loadPGServeConfigForTest(t,
		"--host", "127.0.0.1",
		"--port", "8081",
		"--public-url", "https://viewer.example.test",
		"--public-origin", "https://app.example.test/",
		"--proxy", "caddy",
		"--caddy-bin", "/usr/local/bin/caddy",
		"--proxy-bind-host", "0.0.0.0",
		"--public-port", "8443",
		"--tls-cert", "/tmp/viewer.crt",
		"--tls-key", "/tmp/viewer.key",
		"--allowed-subnet", "10.0.0.0/16",
	)
	require.NoError(t, err, "loadPGServeConfigForTest")
	assert.Equal(t, "caddy", cfg.Proxy.Mode)
	assert.Equal(t, "https://viewer.example.test:8443", cfg.PublicURL)
	assert.Equal(t,
		"https://app.example.test,https://viewer.example.test:8443",
		strings.Join(cfg.PublicOrigins, ","))
	assert.Equal(t, "/usr/local/bin/caddy", cfg.Proxy.Bin)
	assert.Equal(t, "0.0.0.0", cfg.Proxy.BindHost)
	assert.Equal(t, 8443, cfg.Proxy.PublicPort)
	assert.Equal(t, "/tmp/viewer.crt", cfg.Proxy.TLSCert)
	assert.Equal(t, "/tmp/viewer.key", cfg.Proxy.TLSKey)
	assert.Equal(t, "10.0.0.0/16",
		strings.Join(cfg.Proxy.AllowedSubnets, ","))
	assert.Empty(t, basePath, "basePath should be empty")
}

func TestRunPGServeRejectsInvalidManagedCaddyConfigBeforePGSetup(t *testing.T) {
	dataDir := t.TempDir()

	cmd := exec.Command(os.Args[0], "-test.run=TestRunPGServeHelperProcess", "--",
		"--host", "0.0.0.0",
		"--public-url", "https://viewer.example.test",
		"--proxy", "caddy",
		"--caddy-bin", os.Args[0],
	)
	cmd.Env = append(
		os.Environ(),
		"AGENTSVIEW_RUN_PG_SERVE_HELPER=1",
		"AGENTSVIEW_DATA_DIR="+dataDir,
	)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "runPGServe unexpectedly succeeded")
	assert.Contains(t, string(out), "loopback backend host")
}

func TestRunPGServeNonLoopbackWithoutProxyFallsThroughToPGConfig(t *testing.T) {
	dataDir := t.TempDir()

	cmd := exec.Command(os.Args[0], "-test.run=TestRunPGServeHelperProcess", "--",
		"--host", "0.0.0.0",
		"--port", "8081",
	)
	cmd.Env = append(
		os.Environ(),
		"AGENTSVIEW_RUN_PG_SERVE_HELPER=1",
		"AGENTSVIEW_DATA_DIR="+dataDir,
	)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "runPGServe unexpectedly succeeded")
	output := string(out)
	assert.NotContains(t, output, "invalid serve config",
		"unexpected serve validation failure")
	assert.Contains(t, output, "pg serve: url not configured")
}

func TestRunPGServeHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_RUN_PG_SERVE_HELPER") != "1" {
		return
	}

	args := os.Args
	sep := -1
	for i, arg := range args {
		if arg == "--" {
			sep = i
			break
		}
	}
	require.NotEqual(t, -1, sep, "missing argument separator")

	cmd := newPGServeCommand()
	require.NoError(t, cmd.Flags().Parse(args[sep+1:]))
	cfg, basePath, err := loadPGServeConfig(cmd)
	require.NoError(t, err)
	runPGServe(cfg, basePath)
}

func TestWritePGPushSummaryIncludesSkippedConflicts(t *testing.T) {
	var out bytes.Buffer

	writePGPushSummary(&out, postgres.PushResult{
		SessionsPushed:   3,
		MessagesPushed:   9,
		SkippedConflicts: 2,
		Duration:         1500 * time.Millisecond,
	})

	got := out.String()
	assert.Contains(t, got,
		"Pushed 3 sessions, 9 messages, skipped 2 ownership conflict(s) in 1.5s")
	assert.Contains(t, got,
		"Warning: skipped 2 session(s) owned by another PostgreSQL push marker")
}
