package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wesm/agentsview/internal/parser"
)

const configFileName = "config.json"

func skipIfNotUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(
			"skipping: Unix permissions not reliable on Windows",
		)
	}
	if os.Getuid() == 0 {
		t.Skip(
			"skipping: running as root bypasses permissions",
		)
	}
}

func writeConfig(t *testing.T, dir string, data any) {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, configFileName), b, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func setupTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	t.Setenv("AGENT_VIEWER_DATA_DIR", dir)
	return dir
}

func loadConfigFromFlags(t *testing.T, args ...string) (Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterServeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return Load(fs)
}

func TestLoadEnv_OverridesDataDir(t *testing.T) {
	custom := setupTestEnv(t)

	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.loadEnv()

	if cfg.DataDir != custom {
		t.Errorf(
			"DataDir = %q, want %q", cfg.DataDir, custom,
		)
	}
}

func TestLoad_AppliesExplicitFlags(t *testing.T) {
	cfg, err := loadConfigFromFlags(t, "-host", "0.0.0.0", "-port", "9090")
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Host != "0.0.0.0" {
		t.Errorf("Host = %q, want %q", cfg.Host, "0.0.0.0")
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want %d", cfg.Port, 9090)
	}
}

func TestLoad_DefaultsWithoutFlags(t *testing.T) {
	cfg, err := loadConfigFromFlags(t)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Host != "127.0.0.1" {
		t.Errorf(
			"Host = %q, want default %q",
			cfg.Host, "127.0.0.1",
		)
	}
	if cfg.Port != 8080 {
		t.Errorf(
			"Port = %d, want default %d", cfg.Port, 8080,
		)
	}
	if len(cfg.PublicOrigins) != 0 {
		t.Errorf("PublicOrigins = %v, want none", cfg.PublicOrigins)
	}
}

func TestLoad_NilFlagSet(t *testing.T) {
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want %q", cfg.Host, "127.0.0.1")
	}
}

func TestLoad_PublicOriginFlagOverridesConfigFile(t *testing.T) {
	tmp := setupTestEnv(t)
	writeConfig(t, tmp, map[string]any{
		"public_origins": []string{"https://old.example.test"},
	})

	cfg, err := loadConfigFromFlags(
		t,
		"-public-origin", "https://viewer.example.test/",
		"-public-origin", "http://viewer.example.test:8004",
	)
	if err != nil {
		t.Fatal(err)
	}

	got := strings.Join(cfg.PublicOrigins, ",")
	want := "https://viewer.example.test,http://viewer.example.test:8004"
	if got != want {
		t.Fatalf("PublicOrigins = %q, want %q", got, want)
	}
}

func TestLoad_PublicOriginsFromConfigFile(t *testing.T) {
	tmp := setupTestEnv(t)
	writeConfig(t, tmp, map[string]any{
		"public_origins": []string{
			"https://Viewer.Example.Test:443/",
			"http://viewer.example.test:8004",
		},
	})

	cfg, err := LoadMinimal()
	if err != nil {
		t.Fatal(err)
	}

	got := strings.Join(cfg.PublicOrigins, ",")
	want := "https://viewer.example.test,http://viewer.example.test:8004"
	if got != want {
		t.Fatalf("PublicOrigins = %q, want %q", got, want)
	}
}

func TestLoad_PublicOriginsRejectInvalid(t *testing.T) {
	tmp := setupTestEnv(t)
	writeConfig(t, tmp, map[string]any{
		"public_origins": []string{"ftp://viewer.example.test"},
	})

	_, err := LoadMinimal()
	if err == nil {
		t.Fatal("expected invalid public origin error")
	}
	if !strings.Contains(err.Error(), "invalid public origins") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_PublicURLMergedIntoOrigins(t *testing.T) {
	tmp := setupTestEnv(t)
	writeConfig(t, tmp, map[string]any{
		"public_url": "https://viewer.example.test/",
	})

	cfg, err := LoadMinimal()
	if err != nil {
		t.Fatal(err)
	}

	if cfg.PublicURL != "https://viewer.example.test" {
		t.Fatalf("PublicURL = %q, want %q", cfg.PublicURL, "https://viewer.example.test")
	}
	if got := strings.Join(cfg.PublicOrigins, ","); got != "https://viewer.example.test" {
		t.Fatalf("PublicOrigins = %q, want %q", got, "https://viewer.example.test")
	}
}

func TestLoad_ProxyConfigFromFile(t *testing.T) {
	tmp := setupTestEnv(t)
	writeConfig(t, tmp, map[string]any{
		"public_url": "https://viewer.example.test",
		"proxy": map[string]any{
			"mode":            "caddy",
			"bind_host":       "10.0.60.2",
			"public_port":     9443,
			"tls_cert":        "/tmp/viewer.crt",
			"tls_key":         "/tmp/viewer.key",
			"allowed_subnets": []string{"10.1.2.3/16", "192.168.1.0/24"},
		},
	})

	cfg, err := LoadMinimal()
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Proxy.Mode != "caddy" {
		t.Fatalf("Proxy.Mode = %q, want %q", cfg.Proxy.Mode, "caddy")
	}
	if cfg.Proxy.Bin != "caddy" {
		t.Fatalf("Proxy.Bin = %q, want %q", cfg.Proxy.Bin, "caddy")
	}
	if cfg.Proxy.BindHost != "10.0.60.2" {
		t.Fatalf("BindHost = %q, want %q", cfg.Proxy.BindHost, "10.0.60.2")
	}
	if cfg.Proxy.PublicPort != 9443 {
		t.Fatalf("PublicPort = %d, want %d", cfg.Proxy.PublicPort, 9443)
	}
	if cfg.PublicURL != "https://viewer.example.test:9443" {
		t.Fatalf("PublicURL = %q, want %q", cfg.PublicURL, "https://viewer.example.test:9443")
	}
	if got := strings.Join(cfg.Proxy.AllowedSubnets, ","); got != "10.1.0.0/16,192.168.1.0/24" {
		t.Fatalf("AllowedSubnets = %q, want %q", got, "10.1.0.0/16,192.168.1.0/24")
	}
}

func TestLoad_ProxyFlags(t *testing.T) {
	cfg, err := loadConfigFromFlags(
		t,
		"-public-url", "https://viewer.example.test",
		"-proxy", "caddy",
		"-proxy-bind-host", "0.0.0.0",
		"-public-port", "9443",
		"-tls-cert", "/tmp/viewer.crt",
		"-tls-key", "/tmp/viewer.key",
		"-allowed-subnet", "10.0/16",
		"-allowed-subnet", "192.168.0.0/24",
	)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.PublicURL != "https://viewer.example.test:9443" {
		t.Fatalf("PublicURL = %q, want %q", cfg.PublicURL, "https://viewer.example.test:9443")
	}
	if cfg.Proxy.Mode != "caddy" {
		t.Fatalf("Proxy.Mode = %q, want %q", cfg.Proxy.Mode, "caddy")
	}
	if cfg.Proxy.BindHost != "0.0.0.0" {
		t.Fatalf("BindHost = %q, want %q", cfg.Proxy.BindHost, "0.0.0.0")
	}
	if cfg.Proxy.PublicPort != 9443 {
		t.Fatalf("PublicPort = %d, want %d", cfg.Proxy.PublicPort, 9443)
	}
	if got := strings.Join(cfg.Proxy.AllowedSubnets, ","); got != "10.0.0.0/16,192.168.0.0/24" {
		t.Fatalf("AllowedSubnets = %q, want %q", got, "10.0.0.0/16,192.168.0.0/24")
	}
}

func TestLoad_ManagedCaddyDefaultsPublicPortAndBindHost(t *testing.T) {
	cfg, err := loadConfigFromFlags(
		t,
		"-public-url", "https://viewer.example.test",
		"-proxy", "caddy",
	)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.PublicURL != "https://viewer.example.test:8443" {
		t.Fatalf("PublicURL = %q, want %q", cfg.PublicURL, "https://viewer.example.test:8443")
	}
	if cfg.Proxy.BindHost != "127.0.0.1" {
		t.Fatalf("BindHost = %q, want %q", cfg.Proxy.BindHost, "127.0.0.1")
	}
	if cfg.Proxy.PublicPort != 0 {
		t.Fatalf("PublicPort = %d, want %d", cfg.Proxy.PublicPort, 0)
	}
}

func TestLoad_ManagedCaddyRejectsConflictingPublicPort(t *testing.T) {
	_, err := loadConfigFromFlags(
		t,
		"-public-url", "https://viewer.example.test:9443",
		"-proxy", "caddy",
		"-public-port", "8443",
	)
	if err == nil {
		t.Fatal("expected public port conflict error")
	}
	if !strings.Contains(err.Error(), "conflicts with configured public port") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_ManagedCaddyRejectsPublicURLPath(t *testing.T) {
	_, err := loadConfigFromFlags(
		t,
		"-public-url", "https://viewer.example.test/path",
		"-proxy", "caddy",
	)
	if err == nil {
		t.Fatal("expected public URL path error")
	}
	if !strings.Contains(err.Error(), "must not include a path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_ManagedCaddyNormalizesExplicitDefaultPorts(t *testing.T) {
	cfg, err := loadConfigFromFlags(
		t,
		"-public-url", "https://viewer.example.test:443",
		"-proxy", "caddy",
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://viewer.example.test" {
		t.Fatalf("PublicURL = %q, want %q", cfg.PublicURL, "https://viewer.example.test")
	}

	cfg, err = loadConfigFromFlags(
		t,
		"-public-url", "http://viewer.example.test:80",
		"-proxy", "caddy",
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "http://viewer.example.test" {
		t.Fatalf("PublicURL = %q, want %q", cfg.PublicURL, "http://viewer.example.test")
	}
}

func TestLoad_AllowedSubnetsRejectInvalid(t *testing.T) {
	tmp := setupTestEnv(t)
	writeConfig(t, tmp, map[string]any{
		"proxy": map[string]any{
			"mode":            "caddy",
			"allowed_subnets": []string{"10.0.0.0/not-a-mask"},
		},
	})

	_, err := LoadMinimal()
	if err == nil {
		t.Fatal("expected invalid allowed subnets error")
	}
	if !strings.Contains(err.Error(), "invalid allowed subnets") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSaveGithubToken_RejectsCorruptConfig(t *testing.T) {
	tmp := setupTestEnv(t)
	cfg := Config{DataDir: tmp}

	// Write invalid JSON to config file
	path := filepath.Join(tmp, configFileName)
	if err := os.WriteFile(
		path, []byte("not json"), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	err := cfg.SaveGithubToken("tok")
	if err == nil {
		t.Fatal("expected error for corrupt config")
	}
}

func TestSaveGithubToken_ReturnsErrorOnReadFailure(t *testing.T) {
	skipIfNotUnix(t)

	tmp := setupTestEnv(t)
	cfg := Config{DataDir: tmp}

	// Create a config file that is not readable
	path := filepath.Join(tmp, configFileName)
	if err := os.WriteFile(
		path, []byte(`{"k":"v"}`), 0o000,
	); err != nil {
		t.Fatal(err)
	}

	err := cfg.SaveGithubToken("tok")
	if err == nil {
		t.Fatal("expected error for unreadable config file")
	}
	if !strings.Contains(err.Error(), "reading config file") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSaveGithubToken_PreservesExistingKeys(t *testing.T) {
	tmp := setupTestEnv(t)
	cfg := Config{DataDir: tmp}

	existing := map[string]any{"custom_key": "value"}
	writeConfig(t, tmp, existing)

	if err := cfg.SaveGithubToken("new-token"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(tmp, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(got, &result); err != nil {
		t.Fatal(err)
	}
	if result["custom_key"] != "value" {
		t.Errorf(
			"custom_key = %v, want %q",
			result["custom_key"], "value",
		)
	}
	if result["github_token"] != "new-token" {
		t.Errorf(
			"github_token = %v, want %q",
			result["github_token"], "new-token",
		)
	}
}

func TestLoadFile_ReadsDirArrays(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"claude_project_dirs": []string{"/path/one", "/path/two"},
		"codex_sessions_dirs": []string{"/codex/a"},
	})

	cfg, err := LoadMinimal()
	if err != nil {
		t.Fatal(err)
	}

	claudeDirs := cfg.ResolveDirs(parser.AgentClaude)
	if len(claudeDirs) != 2 {
		t.Fatalf(
			"claude dirs len = %d, want 2",
			len(claudeDirs),
		)
	}
	if claudeDirs[0] != "/path/one" ||
		claudeDirs[1] != "/path/two" {
		t.Errorf("claude dirs = %v", claudeDirs)
	}
	codexDirs := cfg.ResolveDirs(parser.AgentCodex)
	if len(codexDirs) != 1 || codexDirs[0] != "/codex/a" {
		t.Errorf("codex dirs = %v", codexDirs)
	}
}

func TestResolveDirs(t *testing.T) {
	tests := []struct {
		name           string
		config         map[string]any
		envValue       string
		expectDefault  bool
		wantDirs       []string
		wantUserConfig bool
	}{
		{
			"DefaultOnly",
			map[string]any{},
			"",
			true,
			nil,
			false,
		},
		{
			"ConfigOverrides",
			map[string]any{
				"claude_project_dirs": []string{"/a", "/b"},
			},
			"",
			false,
			[]string{"/a", "/b"},
			true,
		},
		{
			"EnvOverrides",
			map[string]any{
				"claude_project_dirs": []string{"/a"},
			},
			"/env/override",
			false,
			[]string{"/env/override"},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestEnv(t)
			writeConfig(t, dir, tt.config)
			if tt.envValue != "" {
				t.Setenv("CLAUDE_PROJECTS_DIR", tt.envValue)
			}

			cfg, err := LoadMinimal()
			if err != nil {
				t.Fatal(err)
			}

			dirs := cfg.ResolveDirs(parser.AgentClaude)

			want := tt.wantDirs
			if tt.expectDefault {
				// Default is the home-dir based path
				want = cfg.AgentDirs[parser.AgentClaude]
			}

			if len(dirs) != len(want) {
				t.Fatalf(
					"got %d dirs, want %d",
					len(dirs), len(want),
				)
			}
			for i, v := range dirs {
				if v != want[i] {
					t.Errorf(
						"dirs[%d] = %q, want %q",
						i, v, want[i],
					)
				}
			}

			got := cfg.IsUserConfigured(parser.AgentClaude)
			if got != tt.wantUserConfig {
				t.Errorf(
					"IsUserConfigured = %v, want %v",
					got, tt.wantUserConfig,
				)
			}
		})
	}
}

func TestResolveDataDir_DefaultAndEnvOverride(t *testing.T) {
	// Without env override, should return default
	dir, err := ResolveDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir == "" {
		t.Error("ResolveDataDir returned empty string")
	}

	// With env override, should return the override
	custom := t.TempDir()
	t.Setenv("AGENT_VIEWER_DATA_DIR", custom)
	dir, err = ResolveDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != custom {
		t.Errorf("ResolveDataDir = %q, want %q", dir, custom)
	}
}

func TestEnvOverridesConfigFile(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"codex_sessions_dirs": []string{"/from/config"},
	})
	t.Setenv("CODEX_SESSIONS_DIR", "/from/env")

	cfg, err := LoadMinimal()
	if err != nil {
		t.Fatal(err)
	}

	dirs := cfg.ResolveDirs(parser.AgentCodex)
	if len(dirs) != 1 || dirs[0] != "/from/env" {
		t.Errorf(
			"codex dirs = %v, want [/from/env]", dirs,
		)
	}
}

func TestLoadFile_MalformedDirValueLogsWarning(t *testing.T) {
	dir := setupTestEnv(t)

	// Write a config where claude_project_dirs is a string
	// instead of a string array.
	writeConfig(t, dir, map[string]any{
		"claude_project_dirs": "/not/an/array",
	})

	// Capture log output during Load.
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	cfg, err := LoadMinimal()
	if err != nil {
		t.Fatal(err)
	}

	// The malformed key should trigger a warning.
	logged := buf.String()
	if !strings.Contains(logged, "claude_project_dirs") {
		t.Errorf(
			"expected warning mentioning config key, got: %q",
			logged,
		)
	}
	if !strings.Contains(logged, "expected string array") {
		t.Errorf(
			"expected warning about type, got: %q",
			logged,
		)
	}

	// ResolveDirs should return the default (malformed value
	// was not applied).
	dirs := cfg.ResolveDirs(parser.AgentClaude)
	home, _ := os.UserHomeDir()
	defaultDir := filepath.Join(home, ".claude", "projects")
	if len(dirs) != 1 || dirs[0] != defaultDir {
		t.Errorf(
			"claude dirs = %v, want default [%s]",
			dirs, defaultDir,
		)
	}
}

func TestDefault_ResultContentBlockedCategories(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"Read", "Glob"}
	if len(cfg.ResultContentBlockedCategories) != len(want) {
		t.Fatalf(
			"ResultContentBlockedCategories len = %d, want %d",
			len(cfg.ResultContentBlockedCategories), len(want),
		)
	}
	for i, v := range cfg.ResultContentBlockedCategories {
		if v != want[i] {
			t.Errorf(
				"ResultContentBlockedCategories[%d] = %q, want %q",
				i, v, want[i],
			)
		}
	}
}

func TestLoadFile_ResultContentBlockedCategories(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		want   []string
	}{
		{
			"NoConfigFileUsesDefault",
			map[string]any{},
			[]string{"Read", "Glob"},
		},
		{
			"ConfigFileOverridesWithCustomArray",
			map[string]any{
				"result_content_blocked_categories": []string{"Bash"},
			},
			[]string{"Bash"},
		},
		{
			"ConfigFileWithMultipleCategories",
			map[string]any{
				"result_content_blocked_categories": []string{"Bash", "Write", "Edit"},
			},
			[]string{"Bash", "Write", "Edit"},
		},
		{
			"ConfigFileWithEmptyArrayClearsBlocklist",
			map[string]any{
				"result_content_blocked_categories": []string{},
			},
			[]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestEnv(t)
			writeConfig(t, dir, tt.config)

			cfg, err := LoadMinimal()
			if err != nil {
				t.Fatal(err)
			}

			if len(cfg.ResultContentBlockedCategories) != len(tt.want) {
				t.Fatalf(
					"ResultContentBlockedCategories len = %d, want %d",
					len(cfg.ResultContentBlockedCategories), len(tt.want),
				)
			}
			for i, v := range cfg.ResultContentBlockedCategories {
				if v != tt.want[i] {
					t.Errorf(
						"ResultContentBlockedCategories[%d] = %q, want %q",
						i, v, tt.want[i],
					)
				}
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestLoadFile_PGSyncConfig(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		envURL string
		want   PGSyncConfig
	}{
		{
			"NoConfig",
			map[string]any{},
			"",
			PGSyncConfig{},
		},
		{
			"FromConfigFile",
			map[string]any{
				"pg_sync": map[string]any{
					"enabled":      true,
					"postgres_url": "postgres://localhost/test",
					"machine_name": "laptop",
					"interval":     "30m",
				},
			},
			"",
			PGSyncConfig{
				Enabled:     boolPtr(true),
				PostgresURL: "postgres://localhost/test",
				MachineName: "laptop",
				Interval:    "30m",
			},
		},
		{
			"EnvOverridesConfig",
			map[string]any{
				"pg_sync": map[string]any{
					"postgres_url": "postgres://from-config",
				},
			},
			"postgres://from-env",
			PGSyncConfig{
				Enabled:     boolPtr(true),
				PostgresURL: "postgres://from-env",
			},
		},
		{
			"EnvURLMergesFileFields",
			map[string]any{
				"pg_sync": map[string]any{
					"postgres_url": "postgres://from-config",
					"interval":     "30m",
					"machine_name": "laptop",
				},
			},
			"postgres://from-env",
			PGSyncConfig{
				Enabled:     boolPtr(true),
				PostgresURL: "postgres://from-env",
				Interval:    "30m",
				MachineName: "laptop",
			},
		},
		{
			"URLWithoutEnabledDefaultsToEnabled",
			map[string]any{
				"pg_sync": map[string]any{
					"postgres_url": "postgres://localhost/test",
				},
			},
			"",
			PGSyncConfig{
				PostgresURL: "postgres://localhost/test",
			},
		},
		{
			"ExplicitFalseWithURL",
			map[string]any{
				"pg_sync": map[string]any{
					"enabled":      false,
					"postgres_url": "postgres://localhost/test",
				},
			},
			"",
			PGSyncConfig{
				Enabled:     boolPtr(false),
				PostgresURL: "postgres://localhost/test",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestEnv(t)
			writeConfig(t, dir, tt.config)
			if tt.envURL != "" {
				t.Setenv("AGENTSVIEW_PG_URL", tt.envURL)
			}

			cfg, err := LoadMinimal()
			if err != nil {
				t.Fatal(err)
			}

			gotEnabled := cfg.PGSync.IsEnabled()
			wantEnabled := tt.want.IsEnabled()
			if gotEnabled != wantEnabled {
				t.Errorf(
					"IsEnabled() = %v, want %v",
					gotEnabled, wantEnabled,
				)
			}
			if cfg.PGSync.PostgresURL != tt.want.PostgresURL {
				t.Errorf(
					"PostgresURL = %q, want %q",
					cfg.PGSync.PostgresURL,
					tt.want.PostgresURL,
				)
			}
			if cfg.PGSync.MachineName != tt.want.MachineName {
				t.Errorf(
					"MachineName = %q, want %q",
					cfg.PGSync.MachineName,
					tt.want.MachineName,
				)
			}
			if cfg.PGSync.Interval != tt.want.Interval {
				t.Errorf(
					"Interval = %q, want %q",
					cfg.PGSync.Interval,
					tt.want.Interval,
				)
			}
		})
	}
}

func TestResolvePGSync_Defaults(t *testing.T) {
	cfg := Config{
		PGSync: PGSyncConfig{
			Enabled:     boolPtr(true),
			PostgresURL: "postgres://localhost/test",
		},
	}
	resolved, err := cfg.ResolvePGSync()
	if err != nil {
		t.Fatalf("ResolvePGSync: %v", err)
	}

	if resolved.Interval != "1h" {
		t.Errorf("Interval = %q, want 1h", resolved.Interval)
	}
	if resolved.MachineName == "" {
		t.Error("MachineName should default to hostname")
	}
}

func TestResolvePGSync_ExpandsEnvVars(t *testing.T) {
	t.Setenv("PGPASS", "env-secret")
	t.Setenv("PGURL", "postgres://localhost/test")

	cfg := Config{
		PGSync: PGSyncConfig{
			PostgresURL: "${PGURL}?password=${PGPASS}",
		},
	}

	resolved, err := cfg.ResolvePGSync()
	if err != nil {
		t.Fatalf("ResolvePGSync: %v", err)
	}

	want := "postgres://localhost/test?password=env-secret"
	if resolved.PostgresURL != want {
		t.Fatalf("PostgresURL = %q, want %q", resolved.PostgresURL, want)
	}
}

func TestResolvePGSync_ExpandsLegacyBareEnvOnlyForWholeValue(t *testing.T) {
	t.Setenv("PGURL", "postgres://localhost/test")

	cfg := Config{
		PGSync: PGSyncConfig{
			PostgresURL: "$PGURL",
		},
	}

	resolved, err := cfg.ResolvePGSync()
	if err != nil {
		t.Fatalf("ResolvePGSync: %v", err)
	}

	want := "postgres://localhost/test"
	if resolved.PostgresURL != want {
		t.Fatalf("PostgresURL = %q, want %q", resolved.PostgresURL, want)
	}
}

func TestResolvePGSync_PreservesLiteralDollarSequencesInURL(t *testing.T) {
	t.Setenv("PGPASS", "env-secret")

	cfg := Config{
		PGSync: PGSyncConfig{
			PostgresURL: "postgres://user:pa$word@localhost/db?application_name=$client&password=${PGPASS}",
		},
	}

	resolved, err := cfg.ResolvePGSync()
	if err != nil {
		t.Fatalf("ResolvePGSync: %v", err)
	}

	want := "postgres://user:pa$word@localhost/db?application_name=$client&password=env-secret"
	if resolved.PostgresURL != want {
		t.Fatalf("PostgresURL = %q, want %q", resolved.PostgresURL, want)
	}
}

func TestResolvePGSync_ErrorsOnMissingEnvVar(t *testing.T) {
	cfg := Config{
		PGSync: PGSyncConfig{
			PostgresURL: "${NONEXISTENT_PG_VAR}",
		},
	}

	_, err := cfg.ResolvePGSync()
	if err == nil {
		t.Fatal("expected error for unset env var")
	}
	if !strings.Contains(err.Error(), "NONEXISTENT_PG_VAR") {
		t.Errorf("error = %v, want mention of NONEXISTENT_PG_VAR", err)
	}
}

func TestResolvePGSync_ErrorsOnMissingBareEnvVar(t *testing.T) {
	cfg := Config{
		PGSync: PGSyncConfig{
			PostgresURL: "$NONEXISTENT_PG_BARE_VAR",
		},
	}

	_, err := cfg.ResolvePGSync()
	if err == nil {
		t.Fatal("expected error for unset bare env var")
	}
	if !strings.Contains(err.Error(), "NONEXISTENT_PG_BARE_VAR") {
		t.Errorf("error = %v, want mention of NONEXISTENT_PG_BARE_VAR", err)
	}
}
