package config

import (
	"bytes"
	"flag"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/pflag"
	"github.com/wesm/agentsview/internal/parser"
)

const configFileName = "config.toml"

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
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(data); err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, configFileName), buf.Bytes(), 0o600); err != nil {
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

func loadConfigFromPFlags(t *testing.T, args ...string) (Config, error) {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterServePFlags(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return LoadPFlags(fs)
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

func TestLoadPFlags_AppliesExplicitFlags(t *testing.T) {
	cfg, err := loadConfigFromPFlags(t, "--host", "0.0.0.0", "--port", "9090")
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

	// Write invalid TOML to config file
	path := filepath.Join(tmp, configFileName)
	if err := os.WriteFile(
		path, []byte("[invalid toml = ="), 0o600,
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
		path, []byte("k = \"v\"\n"), 0o000,
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
	if _, err := toml.Decode(string(got), &result); err != nil {
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

func TestLoadFile_EventsCoalesceInterval(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		want   time.Duration
	}{
		{
			"NoConfigFileUsesDefault",
			map[string]any{},
			10 * time.Second,
		},
		{
			"ConfigFileOverrides",
			map[string]any{
				"events_coalesce_interval": "5s",
			},
			5 * time.Second,
		},
		{
			"ConfigFileExplicitZeroDisables",
			map[string]any{
				"events_coalesce_interval": "0s",
			},
			0,
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
			if cfg.EventsCoalesceInterval != tt.want {
				t.Errorf(
					"EventsCoalesceInterval = %v, want %v",
					cfg.EventsCoalesceInterval, tt.want,
				)
			}
		})
	}
}

func TestLoadFile_PGConfig(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		envURL string
		want   PGConfig
	}{
		{
			"NoConfig",
			map[string]any{},
			"",
			PGConfig{},
		},
		{
			"FromConfigFile",
			map[string]any{
				"pg": map[string]any{
					"url":          "postgres://localhost/test",
					"machine_name": "laptop",
				},
			},
			"",
			PGConfig{
				URL:         "postgres://localhost/test",
				MachineName: "laptop",
			},
		},
		{
			"EnvOverridesConfig",
			map[string]any{
				"pg": map[string]any{
					"url": "postgres://from-config",
				},
			},
			"postgres://from-env",
			PGConfig{
				URL: "postgres://from-env",
			},
		},
		{
			"EnvURLMergesFileFields",
			map[string]any{
				"pg": map[string]any{
					"url":          "postgres://from-config",
					"machine_name": "laptop",
				},
			},
			"postgres://from-env",
			PGConfig{
				URL:         "postgres://from-env",
				MachineName: "laptop",
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

			if cfg.PG.URL != tt.want.URL {
				t.Errorf(
					"URL = %q, want %q",
					cfg.PG.URL,
					tt.want.URL,
				)
			}
			if cfg.PG.MachineName != tt.want.MachineName {
				t.Errorf(
					"MachineName = %q, want %q",
					cfg.PG.MachineName,
					tt.want.MachineName,
				)
			}
		})
	}
}

func TestPGConfig_ProjectFilter(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte(`
[pg]
url = "postgres://localhost/test"
projects = ["alpha", "beta"]
`), 0o644)

	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	if err := cfg.loadFile(); err != nil {
		t.Fatalf("loadFile: %v", err)
	}

	if len(cfg.PG.Projects) != 2 {
		t.Fatalf("Projects = %v, want [alpha beta]", cfg.PG.Projects)
	}
	if cfg.PG.Projects[0] != "alpha" || cfg.PG.Projects[1] != "beta" {
		t.Errorf("Projects = %v, want [alpha beta]", cfg.PG.Projects)
	}
}

func TestPGConfig_ExcludeProjectFilter(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte(`
[pg]
url = "postgres://localhost/test"
exclude_projects = ["gamma"]
`), 0o644)

	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	if err := cfg.loadFile(); err != nil {
		t.Fatalf("loadFile: %v", err)
	}

	if len(cfg.PG.ExcludeProjects) != 1 {
		t.Fatalf("ExcludeProjects = %v, want [gamma]", cfg.PG.ExcludeProjects)
	}
	if cfg.PG.ExcludeProjects[0] != "gamma" {
		t.Errorf("ExcludeProjects = %v, want [gamma]", cfg.PG.ExcludeProjects)
	}
}

func TestResolvePG_Defaults(t *testing.T) {
	cfg := Config{
		PG: PGConfig{
			URL: "postgres://localhost/test",
		},
	}
	resolved, err := cfg.ResolvePG()
	if err != nil {
		t.Fatalf("ResolvePG: %v", err)
	}

	if resolved.Schema != "agentsview" {
		t.Errorf("Schema = %q, want agentsview", resolved.Schema)
	}
	if resolved.MachineName == "" {
		t.Error("MachineName should default to hostname")
	}
}

func TestResolvePG_ExpandsEnvVars(t *testing.T) {
	t.Setenv("PGPASS", "env-secret")
	t.Setenv("PGURL", "postgres://localhost/test")

	cfg := Config{
		PG: PGConfig{
			URL: "${PGURL}?password=${PGPASS}",
		},
	}

	resolved, err := cfg.ResolvePG()
	if err != nil {
		t.Fatalf("ResolvePG: %v", err)
	}

	want := "postgres://localhost/test?password=env-secret"
	if resolved.URL != want {
		t.Fatalf("URL = %q, want %q", resolved.URL, want)
	}
}

func TestResolvePG_ExpandsBareEnvOnlyForWholeValue(t *testing.T) {
	t.Setenv("PGURL", "postgres://localhost/test")

	cfg := Config{
		PG: PGConfig{
			URL: "$PGURL",
		},
	}

	resolved, err := cfg.ResolvePG()
	if err != nil {
		t.Fatalf("ResolvePG: %v", err)
	}

	want := "postgres://localhost/test"
	if resolved.URL != want {
		t.Fatalf("URL = %q, want %q", resolved.URL, want)
	}
}

func TestResolvePG_PreservesLiteralDollarSequencesInURL(t *testing.T) {
	t.Setenv("PGPASS", "env-secret")

	cfg := Config{
		PG: PGConfig{
			URL: "postgres://user:pa$word@localhost/db?application_name=$client&password=${PGPASS}",
		},
	}

	resolved, err := cfg.ResolvePG()
	if err != nil {
		t.Fatalf("ResolvePG: %v", err)
	}

	want := "postgres://user:pa$word@localhost/db?application_name=$client&password=env-secret"
	if resolved.URL != want {
		t.Fatalf("URL = %q, want %q", resolved.URL, want)
	}
}

func TestResolvePG_ErrorsOnMissingEnvVar(t *testing.T) {
	cfg := Config{
		PG: PGConfig{
			URL: "${NONEXISTENT_PG_VAR}",
		},
	}

	_, err := cfg.ResolvePG()
	if err == nil {
		t.Fatal("expected error for unset env var")
	}
	if !strings.Contains(err.Error(), "NONEXISTENT_PG_VAR") {
		t.Errorf("error = %v, want mention of NONEXISTENT_PG_VAR", err)
	}
}

func TestResolvePG_ErrorsOnMissingBareEnvVar(t *testing.T) {
	cfg := Config{
		PG: PGConfig{
			URL: "$NONEXISTENT_PG_BARE_VAR",
		},
	}

	_, err := cfg.ResolvePG()
	if err == nil {
		t.Fatal("expected error for unset bare env var")
	}
	if !strings.Contains(err.Error(), "NONEXISTENT_PG_BARE_VAR") {
		t.Errorf("error = %v, want mention of NONEXISTENT_PG_BARE_VAR", err)
	}
}

// ResolvePG must not reject configs with both filter lists —
// that's a push-specific concern validated in runPGPush after
// CLI flags are merged. status and serve use ResolvePG too and
// shouldn't fail on push-only filter conflicts.
func TestResolvePG_AllowsBothFilterLists(t *testing.T) {
	cfg := Config{
		PG: PGConfig{
			URL:             "postgres://localhost/test",
			Projects:        []string{"alpha"},
			ExcludeProjects: []string{"beta"},
		},
	}
	_, err := cfg.ResolvePG()
	if err != nil {
		t.Fatalf(
			"ResolvePG should not reject filter conflicts: %v",
			err,
		)
	}
}

func TestAutomatedPrefixesRoundTrip(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"automated": map[string]any{
			"prefixes": []string{
				"You are analyzing an essay",
				"You are grading quotes",
				"  ",                         // whitespace preserved here; normalization is db-side
				"You are analyzing an essay", // duplicate preserved here too
			},
		},
	})
	cfg, err := loadConfigFromPFlags(t)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	got := cfg.Automated.Prefixes
	want := []string{
		"You are analyzing an essay",
		"You are grading quotes",
		"  ",
		"You are analyzing an essay",
	}
	if !slices.Equal(got, want) {
		t.Errorf("prefixes = %q, want %q", got, want)
	}
}

func TestAutomatedPrefixesAbsentIsNil(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"public_url": "http://example.com",
	})
	cfg, err := loadConfigFromPFlags(t)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if cfg.Automated.Prefixes != nil {
		t.Errorf("expected nil, got %v", cfg.Automated.Prefixes)
	}
}

func TestLoadFile_CustomModelPricing(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want map[string]CustomModelRate
	}{
		{
			name: "basic rates",
			data: map[string]any{
				"custom_model_pricing": map[string]CustomModelRate{
					"acme-ultra-2.1": {Input: 2.0, Output: 8.0},
				},
			},
			want: map[string]CustomModelRate{
				"acme-ultra-2.1": {Input: 2.0, Output: 8.0},
			},
		},
		{
			name: "multiple models with cache rates",
			data: map[string]any{
				"custom_model_pricing": map[string]CustomModelRate{
					"acme-ultra-2.1": {Input: 2.0, Output: 8.0, CacheCreation: 2.5, CacheRead: 0.2},
					"acme-fast-2.1":  {Input: 0.8, Output: 4.0},
				},
			},
			want: map[string]CustomModelRate{
				"acme-ultra-2.1": {Input: 2.0, Output: 8.0, CacheCreation: 2.5, CacheRead: 0.2},
				"acme-fast-2.1":  {Input: 0.8, Output: 4.0},
			},
		},
		{
			name: "empty map omitted",
			data: map[string]any{},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestEnv(t)
			writeConfig(t, dir, tt.data)

			cfg, err := LoadMinimal()
			if err != nil {
				t.Fatalf("LoadMinimal: %v", err)
			}

			if len(tt.want) == 0 {
				if len(cfg.CustomModelPricing) != 0 {
					t.Fatalf("expected nil/empty, got %v", cfg.CustomModelPricing)
				}
				return
			}

			if len(cfg.CustomModelPricing) != len(tt.want) {
				t.Fatalf("got %d entries, want %d", len(cfg.CustomModelPricing), len(tt.want))
			}
			for model, wantRate := range tt.want {
				got, ok := cfg.CustomModelPricing[model]
				if !ok {
					t.Errorf("missing model %q", model)
					continue
				}
				if got != wantRate {
					t.Errorf("model %q = %+v, want %+v", model, got, wantRate)
				}
			}
		})
	}
}
