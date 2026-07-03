package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestBrowserURLUsesPublicURL(t *testing.T) {
	cfg := config.Config{
		Host:      "127.0.0.1",
		Port:      8080,
		PublicURL: "https://viewer.example.test",
	}
	assert.Equal(t, "https://viewer.example.test", browserURL(cfg))
}

func TestBrowserURLWithPlatformUsesWSLEth0ForBindAll(t *testing.T) {
	cfg := config.Config{Host: "0.0.0.0", Port: 8080}

	got := browserURLWithPlatform(
		cfg,
		func() bool { return true },
		func(name string) (string, bool) {
			assert.Equal(t, "eth0", name)
			return "172.20.10.5", true
		},
	)

	assert.Equal(t, "http://172.20.10.5:8080", got)
}

func TestBrowserURLWithPlatformKeepsLoopbackOutsideWSL(t *testing.T) {
	cfg := config.Config{Host: "0.0.0.0", Port: 8080}

	got := browserURLWithPlatform(
		cfg,
		func() bool { return false },
		func(string) (string, bool) {
			t.Fatal("interface lookup should not run outside WSL")
			return "", false
		},
	)

	assert.Equal(t, "http://127.0.0.1:8080", got)
}

func TestBrowserURLWithPlatformKeepsLoopbackWhenWSLEth0Missing(t *testing.T) {
	cfg := config.Config{Host: "0.0.0.0", Port: 8080}

	got := browserURLWithPlatform(
		cfg,
		func() bool { return true },
		func(string) (string, bool) { return "", false },
	)

	assert.Equal(t, "http://127.0.0.1:8080", got)
}

func TestValidateServeConfigManagedCaddyAllowsHTTPS(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "viewer.crt")
	keyPath := filepath.Join(dir, "viewer.key")
	require.NoError(t, os.WriteFile(certPath, []byte("cert"), 0o600))
	require.NoError(t, os.WriteFile(keyPath, []byte("key"), 0o600))

	cfg := config.Config{
		Host:      "127.0.0.1",
		Port:      8080,
		PublicURL: "https://viewer.example.test",
		Proxy: config.ProxyConfig{
			Mode:           "caddy",
			Bin:            os.Args[0],
			TLSCert:        certPath,
			TLSKey:         keyPath,
			AllowedSubnets: []string{"10.0.0.0/16"},
		},
	}
	assert.NoError(t, validateServeConfig(cfg))
}

func TestValidateServeConfigManagedCaddyRejectsNonLoopbackHost(t *testing.T) {
	cfg := config.Config{
		Host:        "0.0.0.0",
		Port:        8080,
		PublicURL:   "http://viewer.example.test:8004",
		RequireAuth: true,
		Proxy: config.ProxyConfig{
			Mode: "caddy",
			Bin:  os.Args[0],
		},
	}
	err := validateServeConfig(cfg)
	require.Error(t, err, "expected error for non-loopback backend host")
	assert.Contains(t, err.Error(), "loopback backend host")
}

func TestValidateServeConfigNonLoopbackHostGuardrail(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Config
		wantErr string
	}{
		{
			name: "config host without auth is rejected",
			cfg: config.Config{
				Host: "0.0.0.0", Port: 8080,
			},
			wantErr: "require_auth",
		},
		{
			name: "config host with require_auth is allowed",
			cfg: config.Config{
				Host: "0.0.0.0", Port: 8080,
				RequireAuth: true,
			},
		},
		{
			name: "explicit --host flag stays allowed without auth",
			cfg: config.Config{
				Host: "0.0.0.0", Port: 8080,
				HostExplicit: true,
			},
		},
		{
			name: "loopback host needs no auth",
			cfg: config.Config{
				Host: "127.0.0.1", Port: 8080,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateServeConfig(tt.cfg)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateServeConfigManagedCaddyRequiresAllowlistForNonLoopbackBind(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "viewer.crt")
	keyPath := filepath.Join(dir, "viewer.key")
	require.NoError(t, os.WriteFile(certPath, []byte("cert"), 0o600))
	require.NoError(t, os.WriteFile(keyPath, []byte("key"), 0o600))

	cfg := config.Config{
		Host:      "127.0.0.1",
		Port:      8080,
		PublicURL: "https://viewer.example.test:8443",
		Proxy: config.ProxyConfig{
			Mode:     "caddy",
			Bin:      os.Args[0],
			BindHost: "0.0.0.0",
			TLSCert:  certPath,
			TLSKey:   keyPath,
		},
	}
	err := validateServeConfig(cfg)
	require.Error(t, err, "expected non-loopback bind allowlist error")
	assert.Contains(t, err.Error(), "allowed_subnet")
}

func TestValidateServeConfigManagedCaddyRejectsHTTPWithTLS(t *testing.T) {
	cfg := config.Config{
		Host:      "127.0.0.1",
		Port:      8080,
		PublicURL: "http://viewer.example.test:8004",
		Proxy: config.ProxyConfig{
			Mode:    "caddy",
			Bin:     os.Args[0],
			TLSCert: "/tmp/viewer.crt",
			TLSKey:  "/tmp/viewer.key",
		},
	}
	err := validateServeConfig(cfg)
	require.Error(t, err, "expected HTTP-with-TLS error")
	assert.Contains(t, err.Error(), "HTTP mode")
}

func TestBuildManagedCaddyfileIncludesAllowlistAndTLS(t *testing.T) {
	got := buildManagedCaddyfile(
		"https://viewer.example.test:8443",
		"0.0.0.0",
		"127.0.0.1:8080",
		"/tmp/viewer.crt",
		"/tmp/viewer.key",
		[]string{"10.0.0.0/16", "192.168.1.0/24"},
	)

	for _, want := range []string{
		"admin off",
		"auto_https off",
		"https://viewer.example.test:8443 {",
		"bind 0.0.0.0",
		"@blocked not remote_ip 10.0.0.0/16 192.168.1.0/24",
		"respond @blocked \"Forbidden\" 403",
		"tls \"/tmp/viewer.crt\" \"/tmp/viewer.key\"",
		"reverse_proxy 127.0.0.1:8080",
	} {
		assert.Contains(t, got, want,
			"generated caddyfile missing %q", want)
	}
}

func TestManagedCaddyConfigPathNamespacesMode(t *testing.T) {
	dataDir := t.TempDir()

	gotServe := managedCaddyConfigPath(dataDir, "serve")
	gotPG := managedCaddyConfigPath(dataDir, "pg-serve")

	assert.NotEqual(t, gotServe, gotPG,
		"managed caddy paths must differ by mode")
	assert.True(t, strings.HasSuffix(
		gotServe,
		filepath.Join("managed-caddy", "serve", "Caddyfile"),
	), "serve path = %q", gotServe)
	assert.True(t, strings.HasSuffix(
		gotPG,
		filepath.Join("managed-caddy", "pg-serve", "Caddyfile"),
	), "pg path = %q", gotPG)
}

func TestPrepareManagedCaddyConfigForPGServeUsesNamespacedPathAndBackend(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Config{
		DataDir:   dataDir,
		PublicURL: "https://viewer.example.test",
		Proxy: config.ProxyConfig{
			BindHost:       "0.0.0.0",
			TLSCert:        "/tmp/viewer.crt",
			TLSKey:         "/tmp/viewer.key",
			AllowedSubnets: []string{"10.0.0.0/16"},
		},
	}

	path, content, err := prepareManagedCaddyConfig(
		cfg,
		"pg-serve",
		"127.0.0.1:18080",
	)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(
		path,
		filepath.Join("managed-caddy", "pg-serve", "Caddyfile"),
	), "path = %q", path)
	assert.Contains(t, content, "reverse_proxy 127.0.0.1:18080")
}

func TestRewriteConfiguredPublicURLPort_RewritesMatchingExplicitPort(t *testing.T) {
	updatedURL, updatedOrigins, changed, err := rewriteConfiguredPublicURLPort(
		"http://viewer.example.test:8004",
		[]string{"http://viewer.example.test:8004"},
		8004,
		8005,
	)
	require.NoError(t, err)
	assert.True(t, changed, "expected public URL rewrite")
	assert.Equal(t, "http://viewer.example.test:8005", updatedURL)
	assert.Equal(t, "http://viewer.example.test:8005",
		strings.Join(updatedOrigins, ","))
}

func TestRewriteConfiguredPublicURLPort_PreservesExternalProxyPort(t *testing.T) {
	updatedURL, updatedOrigins, changed, err := rewriteConfiguredPublicURLPort(
		"https://viewer.example.test",
		[]string{"https://viewer.example.test"},
		8080,
		8081,
	)
	require.NoError(t, err)
	assert.False(t, changed, "expected public URL to remain unchanged")
	assert.Equal(t, "https://viewer.example.test", updatedURL)
	assert.Equal(t, "https://viewer.example.test",
		strings.Join(updatedOrigins, ","))
}

func TestReadinessProbeHost(t *testing.T) {
	tests := map[string]string{
		"":          "127.0.0.1",
		"0.0.0.0":   "127.0.0.1",
		"::":        "::1",
		"127.0.0.1": "127.0.0.1",
		"10.0.60.2": "10.0.60.2",
	}
	for input, want := range tests {
		assert.Equal(t, want, readinessProbeHost(input),
			"readinessProbeHost(%q)", input)
	}
}

func TestWaitForLocalPortReturnsEarlyOnErrorChannel(t *testing.T) {
	errCh := make(chan error, 1)
	errCh <- errors.New("backend failed")
	err := waitForLocalPort(
		context.Background(),
		"127.0.0.1",
		65535,
		5*time.Second,
		errCh,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backend failed")
}

func TestWaitForLocalPortHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitForLocalPort(
		ctx,
		"127.0.0.1",
		65535,
		5*time.Second,
		nil,
	)
	require.ErrorIs(t, err, context.Canceled)
}

func TestWaitForLocalPortPrefersContextCancellationOverError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	errCh := make(chan error, 1)
	errCh <- errors.New("caddy exited")
	err := waitForLocalPort(
		ctx,
		"127.0.0.1",
		65535,
		5*time.Second,
		errCh,
	)
	require.ErrorIs(t, err, context.Canceled)
}

type countingCaddyGuard struct{ closed int }

func (g *countingCaddyGuard) Close() error {
	g.closed++
	return nil
}

func TestManagedCaddyStopClosesGuard(t *testing.T) {
	guard := &countingCaddyGuard{}
	called := 0
	m := &managedCaddy{
		cancel: func() { called++ },
		guard:  guard,
	}
	m.Stop()
	assert.Equal(t, 1, called, "Stop must cancel the run context")
	assert.Equal(t, 1, guard.closed, "Stop must close the lifetime guard")
}

func TestManagedCaddyStopNilSafe(t *testing.T) {
	var m *managedCaddy
	assert.NotPanics(t, func() { m.Stop() })
	assert.NotPanics(t, func() { (&managedCaddy{}).Stop() })
}

func TestNewCaddyGuardNoopOnPosix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX no-op guard; Windows builds a job-object guard")
	}
	guard, err := newCaddyGuard(nil)
	require.NoError(t, err)
	require.NotNil(t, guard)
	require.NoError(t, guard.Close())
}
