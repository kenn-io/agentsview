package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/config"
)

func TestBrowserURLUsesPublicURL(t *testing.T) {
	cfg := config.Config{
		Host:      "127.0.0.1",
		Port:      8080,
		PublicURL: "https://viewer.example.test",
	}
	if got := browserURL(cfg); got != "https://viewer.example.test" {
		t.Fatalf("browserURL = %q, want %q", got, "https://viewer.example.test")
	}
}

func TestValidateServeConfigManagedCaddyAllowsHTTPS(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "viewer.crt")
	keyPath := filepath.Join(dir, "viewer.key")
	if err := os.WriteFile(certPath, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

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
	if err := validateServeConfig(cfg); err != nil {
		t.Fatalf("validateServeConfig returned error: %v", err)
	}
}

func TestValidateServeConfigManagedCaddyRejectsNonLoopbackHost(t *testing.T) {
	cfg := config.Config{
		Host:      "0.0.0.0",
		Port:      8080,
		PublicURL: "http://viewer.example.test:8004",
		Proxy: config.ProxyConfig{
			Mode: "caddy",
			Bin:  os.Args[0],
		},
	}
	err := validateServeConfig(cfg)
	if err == nil {
		t.Fatal("expected error for non-loopback backend host")
	}
	if !strings.Contains(err.Error(), "loopback backend host") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateServeConfigManagedCaddyRequiresAllowlistForNonLoopbackBind(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "viewer.crt")
	keyPath := filepath.Join(dir, "viewer.key")
	if err := os.WriteFile(certPath, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

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
	if err == nil {
		t.Fatal("expected non-loopback bind allowlist error")
	}
	if !strings.Contains(err.Error(), "allowed_subnet") {
		t.Fatalf("unexpected error: %v", err)
	}
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
	if err == nil {
		t.Fatal("expected HTTP-with-TLS error")
	}
	if !strings.Contains(err.Error(), "HTTP mode") {
		t.Fatalf("unexpected error: %v", err)
	}
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
		if !strings.Contains(got, want) {
			t.Fatalf("generated caddyfile missing %q:\n%s", want, got)
		}
	}
}

func TestRewriteConfiguredPublicURLPort_RewritesMatchingExplicitPort(t *testing.T) {
	updatedURL, updatedOrigins, changed, err := rewriteConfiguredPublicURLPort(
		"http://viewer.example.test:8004",
		[]string{"http://viewer.example.test:8004"},
		8004,
		8005,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected public URL rewrite")
	}
	if updatedURL != "http://viewer.example.test:8005" {
		t.Fatalf("updatedURL = %q, want %q", updatedURL, "http://viewer.example.test:8005")
	}
	if got := strings.Join(updatedOrigins, ","); got != "http://viewer.example.test:8005" {
		t.Fatalf("updatedOrigins = %q, want %q", got, "http://viewer.example.test:8005")
	}
}

func TestRewriteConfiguredPublicURLPort_PreservesExternalProxyPort(t *testing.T) {
	updatedURL, updatedOrigins, changed, err := rewriteConfiguredPublicURLPort(
		"https://viewer.example.test",
		[]string{"https://viewer.example.test"},
		8080,
		8081,
	)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected public URL to remain unchanged")
	}
	if updatedURL != "https://viewer.example.test" {
		t.Fatalf("updatedURL = %q, want %q", updatedURL, "https://viewer.example.test")
	}
	if got := strings.Join(updatedOrigins, ","); got != "https://viewer.example.test" {
		t.Fatalf("updatedOrigins = %q, want %q", got, "https://viewer.example.test")
	}
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
		if got := readinessProbeHost(input); got != want {
			t.Fatalf("readinessProbeHost(%q) = %q, want %q", input, got, want)
		}
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
	if err == nil || !strings.Contains(err.Error(), "backend failed") {
		t.Fatalf("expected backend failure, got %v", err)
	}
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}
