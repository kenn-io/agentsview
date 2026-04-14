package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesm/agentsview/internal/config"
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
	dataDir := t.TempDir()
	t.Setenv("AGENT_VIEWER_DATA_DIR", dataDir)

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
	if err != nil {
		t.Fatal(err)
	}

	cfg, _, err := loadPGServeConfigForTest(t)
	if err != nil {
		t.Fatalf("loadPGServeConfigForTest: %v", err)
	}
	if cfg.PG.URL == "" {
		t.Fatal("expected PG URL")
	}
	if cfg.PublicURL != "" {
		t.Fatalf("PublicURL = %q, want empty", cfg.PublicURL)
	}
	if len(cfg.PublicOrigins) != 0 {
		t.Fatalf("PublicOrigins = %v, want empty", cfg.PublicOrigins)
	}
	if cfg.Proxy.Mode != "" {
		t.Fatalf("Proxy.Mode = %q, want empty", cfg.Proxy.Mode)
	}
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("Host = %q, want %q", cfg.Host, "127.0.0.1")
	}
	if cfg.Port != 8080 {
		t.Fatalf("Port = %d, want %d", cfg.Port, 8080)
	}
}

func TestLoadPGServeConfigIgnoresInvalidPersistedServeSettings(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_VIEWER_DATA_DIR", dataDir)

	err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(`
public_url = "not a url"

[proxy]
mode = "bogus"

[pg]
url = "postgres://user:pass@db.example.test:5432/agentsview?sslmode=require"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, _, err := loadPGServeConfigForTest(t)
	if err != nil {
		t.Fatalf("loadPGServeConfigForTest: %v", err)
	}
	if cfg.PG.URL == "" {
		t.Fatal("expected PG URL")
	}
	if cfg.PublicURL != "" {
		t.Fatalf("PublicURL = %q, want empty", cfg.PublicURL)
	}
	if cfg.Proxy.Mode != "" {
		t.Fatalf("Proxy.Mode = %q, want empty", cfg.Proxy.Mode)
	}
}

func TestPGServeConfigAcceptsManagedCaddyFlags(t *testing.T) {
	t.Setenv("AGENT_VIEWER_DATA_DIR", t.TempDir())

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
	if err != nil {
		t.Fatalf("loadPGServeConfigForTest: %v", err)
	}
	if cfg.Proxy.Mode != "caddy" {
		t.Fatalf(
			"Proxy.Mode = %q, want %q",
			cfg.Proxy.Mode,
			"caddy",
		)
	}
	if cfg.PublicURL != "https://viewer.example.test:8443" {
		t.Fatalf("PublicURL = %q", cfg.PublicURL)
	}
	if got := strings.Join(cfg.PublicOrigins, ","); got != "https://app.example.test,https://viewer.example.test:8443" {
		t.Fatalf(
			"PublicOrigins = %q, want %q",
			got,
			"https://app.example.test,https://viewer.example.test:8443",
		)
	}
	if cfg.Proxy.Bin != "/usr/local/bin/caddy" {
		t.Fatalf(
			"Proxy.Bin = %q, want %q",
			cfg.Proxy.Bin,
			"/usr/local/bin/caddy",
		)
	}
	if cfg.Proxy.BindHost != "0.0.0.0" {
		t.Fatalf(
			"Proxy.BindHost = %q, want %q",
			cfg.Proxy.BindHost,
			"0.0.0.0",
		)
	}
	if cfg.Proxy.PublicPort != 8443 {
		t.Fatalf("Proxy.PublicPort = %d, want %d", cfg.Proxy.PublicPort, 8443)
	}
	if cfg.Proxy.TLSCert != "/tmp/viewer.crt" {
		t.Fatalf(
			"Proxy.TLSCert = %q, want %q",
			cfg.Proxy.TLSCert,
			"/tmp/viewer.crt",
		)
	}
	if cfg.Proxy.TLSKey != "/tmp/viewer.key" {
		t.Fatalf(
			"Proxy.TLSKey = %q, want %q",
			cfg.Proxy.TLSKey,
			"/tmp/viewer.key",
		)
	}
	if got := strings.Join(cfg.Proxy.AllowedSubnets, ","); got != "10.0.0.0/16" {
		t.Fatalf(
			"Proxy.AllowedSubnets = %q, want %q",
			got,
			"10.0.0.0/16",
		)
	}
	if basePath != "" {
		t.Fatalf("basePath = %q, want empty", basePath)
	}
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
		"AGENT_VIEWER_DATA_DIR="+dataDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("runPGServe unexpectedly succeeded")
	}
	if !strings.Contains(string(out), "loopback backend host") {
		t.Fatalf("output = %s", out)
	}
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
		"AGENT_VIEWER_DATA_DIR="+dataDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("runPGServe unexpectedly succeeded")
	}
	output := string(out)
	if strings.Contains(output, "invalid serve config") {
		t.Fatalf("unexpected serve validation failure: %s", output)
	}
	if !strings.Contains(output, "pg serve: url not configured") {
		t.Fatalf("output = %s", output)
	}
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
	if sep == -1 {
		t.Fatal("missing argument separator")
	}

	cmd := newPGServeCommand()
	if err := cmd.Flags().Parse(args[sep+1:]); err != nil {
		t.Fatal(err)
	}
	cfg, basePath, err := loadPGServeConfig(cmd)
	if err != nil {
		t.Fatal(err)
	}
	runPGServe(cfg, basePath)
}
