package config

import (
	"bytes"
	"flag"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
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
	require.NoError(t, toml.NewEncoder(&buf).Encode(data), "marshal config")
	require.NoError(t, os.WriteFile(filepath.Join(dir, configFileName), buf.Bytes(), 0o600), "write config")
}

func setupTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	return dir
}

func loadConfigFromFlags(t *testing.T, args ...string) (Config, error) {
	t.Helper()
	if os.Getenv("AGENTSVIEW_DATA_DIR") == "" {
		t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterServeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return Load(fs)
}

func loadConfigFromPFlags(t *testing.T, args ...string) (Config, error) {
	t.Helper()
	if os.Getenv("AGENTSVIEW_DATA_DIR") == "" {
		t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	}
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterServePFlags(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return LoadPFlags(fs)
}

func TestLoadMinimal_LoadsAgentBinaryConfig(t *testing.T) {
	dir := setupTestEnv(t)
	path := filepath.Join(dir, configFileName)
	data := []byte(`[agent.claude]
binary = "/opt/agents/claude"

[agent.gemini]
binary = "/usr/local/bin/gemini"
`)
	require.NoError(t, os.WriteFile(path, data, 0o600), "write config")

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	assert.Equal(t, "/opt/agents/claude", cfg.Agent["claude"].Binary)
	assert.Equal(t, "/usr/local/bin/gemini", cfg.Agent["gemini"].Binary)
}

func TestDefault_IncludesCodexArchivedSessionsDir(t *testing.T) {
	cfg, err := Default()
	require.NoError(t, err)

	dirs := cfg.ResolveDirs(parser.AgentCodex)
	require.Len(t, dirs, 2)
	assert.True(t, strings.HasSuffix(dirs[0], filepath.Join(".codex", "sessions")), "dirs[0] = %q", dirs[0])
	assert.True(t, strings.HasSuffix(dirs[1], filepath.Join(".codex", "archived_sessions")), "dirs[1] = %q", dirs[1])
}

func TestLoadEnv_OverridesDataDir(t *testing.T) {
	custom := setupTestEnv(t)

	cfg, err := Default()
	require.NoError(t, err)
	cfg.loadEnv()

	assert.Equal(t, custom, cfg.DataDir)
}

func TestLoad_AppliesExplicitFlags(t *testing.T) {
	cfg, err := loadConfigFromFlags(t, "-host", "0.0.0.0", "-port", "9090")
	require.NoError(t, err)

	assert.Equal(t, "0.0.0.0", cfg.Host)
	assert.Equal(t, 9090, cfg.Port)
}

func TestLoad_DefaultsWithoutFlags(t *testing.T) {
	cfg, err := loadConfigFromFlags(t)
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1", cfg.Host)
	assert.Equal(t, 8080, cfg.Port)
	assert.Empty(t, cfg.PublicOrigins)
}

func TestLoadPFlags_AppliesExplicitFlags(t *testing.T) {
	cfg, err := loadConfigFromPFlags(t, "--host", "0.0.0.0", "--port", "9090")
	require.NoError(t, err)

	assert.Equal(t, "0.0.0.0", cfg.Host)
	assert.Equal(t, 9090, cfg.Port)
}

func TestLoad_NilFlagSet(t *testing.T) {
	cfg, err := Load(nil)
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1", cfg.Host)
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
	require.NoError(t, err)

	got := strings.Join(cfg.PublicOrigins, ",")
	assert.Equal(t, "https://viewer.example.test,http://viewer.example.test:8004", got)
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
	require.NoError(t, err)

	got := strings.Join(cfg.PublicOrigins, ",")
	assert.Equal(t, "https://viewer.example.test,http://viewer.example.test:8004", got)
}

func TestLoad_PublicOriginsRejectInvalid(t *testing.T) {
	tmp := setupTestEnv(t)
	writeConfig(t, tmp, map[string]any{
		"public_origins": []string{"ftp://viewer.example.test"},
	})

	_, err := LoadMinimal()
	require.Error(t, err, "expected invalid public origin error")
	assert.Contains(t, err.Error(), "invalid public origins")
}

func TestLoad_PublicURLMergedIntoOrigins(t *testing.T) {
	tmp := setupTestEnv(t)
	writeConfig(t, tmp, map[string]any{
		"public_url": "https://viewer.example.test/",
	})

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	assert.Equal(t, "https://viewer.example.test", cfg.PublicURL)
	assert.Equal(t, "https://viewer.example.test", strings.Join(cfg.PublicOrigins, ","))
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
	require.NoError(t, err)

	assert.Equal(t, "caddy", cfg.Proxy.Mode)
	assert.Equal(t, "caddy", cfg.Proxy.Bin)
	assert.Equal(t, "10.0.60.2", cfg.Proxy.BindHost)
	assert.Equal(t, 9443, cfg.Proxy.PublicPort)
	assert.Equal(t, "https://viewer.example.test:9443", cfg.PublicURL)
	assert.Equal(t, "10.1.0.0/16,192.168.1.0/24", strings.Join(cfg.Proxy.AllowedSubnets, ","))
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
	require.NoError(t, err)

	assert.Equal(t, "https://viewer.example.test:9443", cfg.PublicURL)
	assert.Equal(t, "caddy", cfg.Proxy.Mode)
	assert.Equal(t, "0.0.0.0", cfg.Proxy.BindHost)
	assert.Equal(t, 9443, cfg.Proxy.PublicPort)
	assert.Equal(t, "10.0.0.0/16,192.168.0.0/24", strings.Join(cfg.Proxy.AllowedSubnets, ","))
}

func TestLoad_ManagedCaddyDefaultsPublicPortAndBindHost(t *testing.T) {
	cfg, err := loadConfigFromFlags(
		t,
		"-public-url", "https://viewer.example.test",
		"-proxy", "caddy",
	)
	require.NoError(t, err)

	assert.Equal(t, "https://viewer.example.test:8443", cfg.PublicURL)
	assert.Equal(t, "127.0.0.1", cfg.Proxy.BindHost)
	assert.Equal(t, 0, cfg.Proxy.PublicPort)
}

func TestLoad_ManagedCaddyRejectsConflictingPublicPort(t *testing.T) {
	_, err := loadConfigFromFlags(
		t,
		"-public-url", "https://viewer.example.test:9443",
		"-proxy", "caddy",
		"-public-port", "8443",
	)
	require.Error(t, err, "expected public port conflict error")
	assert.Contains(t, err.Error(), "conflicts with configured public port")
}

func TestLoad_ManagedCaddyRejectsPublicURLPath(t *testing.T) {
	_, err := loadConfigFromFlags(
		t,
		"-public-url", "https://viewer.example.test/path",
		"-proxy", "caddy",
	)
	require.Error(t, err, "expected public URL path error")
	assert.Contains(t, err.Error(), "must not include a path")
}

func TestLoad_ManagedCaddyNormalizesExplicitDefaultPorts(t *testing.T) {
	cfg, err := loadConfigFromFlags(
		t,
		"-public-url", "https://viewer.example.test:443",
		"-proxy", "caddy",
	)
	require.NoError(t, err)
	assert.Equal(t, "https://viewer.example.test", cfg.PublicURL)

	cfg, err = loadConfigFromFlags(
		t,
		"-public-url", "http://viewer.example.test:80",
		"-proxy", "caddy",
	)
	require.NoError(t, err)
	assert.Equal(t, "http://viewer.example.test", cfg.PublicURL)
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
	require.Error(t, err, "expected invalid allowed subnets error")
	assert.Contains(t, err.Error(), "invalid allowed subnets")
}

func TestSaveGithubToken_RejectsCorruptConfig(t *testing.T) {
	tmp := setupTestEnv(t)
	cfg := Config{DataDir: tmp}

	// Write invalid TOML to config file
	path := filepath.Join(tmp, configFileName)
	require.NoError(t, os.WriteFile(path, []byte("[invalid toml = ="), 0o600))

	err := cfg.SaveGithubToken("tok")
	require.Error(t, err, "expected error for corrupt config")
}

func TestSaveGithubToken_ReturnsErrorOnReadFailure(t *testing.T) {
	skipIfNotUnix(t)

	tmp := setupTestEnv(t)
	cfg := Config{DataDir: tmp}

	// Create a config file that is not readable
	path := filepath.Join(tmp, configFileName)
	require.NoError(t, os.WriteFile(path, []byte("k = \"v\"\n"), 0o000))

	err := cfg.SaveGithubToken("tok")
	require.Error(t, err, "expected error for unreadable config file")
	assert.Contains(t, err.Error(), "reading config file")
}

func TestSaveGithubToken_PreservesExistingKeys(t *testing.T) {
	tmp := setupTestEnv(t)
	cfg := Config{DataDir: tmp}

	existing := map[string]any{"custom_key": "value"}
	writeConfig(t, tmp, existing)

	require.NoError(t, cfg.SaveGithubToken("new-token"))

	got, err := os.ReadFile(filepath.Join(tmp, configFileName))
	require.NoError(t, err)
	var result map[string]any
	_, err = toml.Decode(string(got), &result)
	require.NoError(t, err)
	assert.Equal(t, "value", result["custom_key"])
	assert.Equal(t, "new-token", result["github_token"])
}

func TestLoadFile_ReadsDirArrays(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"claude_project_dirs": []string{"/path/one", "/path/two"},
		"codex_sessions_dirs": []string{"/codex/a"},
	})

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	claudeDirs := cfg.ResolveDirs(parser.AgentClaude)
	require.Len(t, claudeDirs, 2)
	assert.Equal(t, "/path/one", claudeDirs[0])
	assert.Equal(t, "/path/two", claudeDirs[1])
	codexDirs := cfg.ResolveDirs(parser.AgentCodex)
	require.Len(t, codexDirs, 1)
	assert.Equal(t, "/codex/a", codexDirs[0])
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
			require.NoError(t, err)

			dirs := cfg.ResolveDirs(parser.AgentClaude)

			want := tt.wantDirs
			if tt.expectDefault {
				// Default is the home-dir based path
				want = cfg.AgentDirs[parser.AgentClaude]
			}

			assert.Equal(t, want, dirs)
			assert.Equal(t, tt.wantUserConfig, cfg.IsUserConfigured(parser.AgentClaude))
		})
	}
}

func TestResolveDataDir_DefaultAndEnvOverride(t *testing.T) {
	// Without env override, should return default
	dir, err := ResolveDataDir()
	require.NoError(t, err)
	assert.NotEmpty(t, dir, "ResolveDataDir returned empty string")

	// With env override, should return the override
	custom := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", custom)
	dir, err = ResolveDataDir()
	require.NoError(t, err)
	assert.Equal(t, custom, dir)
}

// TestDataDir_LegacyEnvFallback verifies that the legacy AGENT_VIEWER_DATA_DIR
// env var still takes effect when the canonical AGENTSVIEW_DATA_DIR is unset,
// and that the canonical name wins when both are set.
func TestDataDir_LegacyEnvFallback(t *testing.T) {
	t.Run("legacy used when canonical unset", func(t *testing.T) {
		legacy := t.TempDir()
		t.Setenv("AGENT_VIEWER_DATA_DIR", legacy)
		dir, err := ResolveDataDir()
		require.NoError(t, err)
		assert.Equal(t, legacy, dir)
	})

	t.Run("canonical wins over legacy", func(t *testing.T) {
		legacy := t.TempDir()
		canonical := t.TempDir()
		t.Setenv("AGENT_VIEWER_DATA_DIR", legacy)
		t.Setenv("AGENTSVIEW_DATA_DIR", canonical)
		dir, err := ResolveDataDir()
		require.NoError(t, err)
		assert.Equal(t, canonical, dir, "canonical should win")
	})
}

func TestEnvOverridesConfigFile(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"codex_sessions_dirs": []string{"/from/config"},
	})
	t.Setenv("CODEX_SESSIONS_DIR", "/from/env")

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	dirs := cfg.ResolveDirs(parser.AgentCodex)
	assert.Equal(t, []string{"/from/env"}, dirs)
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
	require.NoError(t, err)

	// The malformed key should trigger a warning.
	logged := buf.String()
	assert.Contains(t, logged, "claude_project_dirs")
	assert.Contains(t, logged, "expected string array")

	// ResolveDirs should return the default (malformed value
	// was not applied).
	dirs := cfg.ResolveDirs(parser.AgentClaude)
	home, _ := os.UserHomeDir()
	defaultDir := filepath.Join(home, ".claude", "projects")
	assert.Equal(t, []string{defaultDir}, dirs)
}

func TestDefault_ResultContentBlockedCategories(t *testing.T) {
	cfg, err := Default()
	require.NoError(t, err)

	assert.Equal(t, []string{"Read", "Glob"}, cfg.ResultContentBlockedCategories)
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
			require.NoError(t, err)

			assert.Equal(t, tt.want, cfg.ResultContentBlockedCategories)
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
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.EventsCoalesceInterval)
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
			require.NoError(t, err)

			assert.Equal(t, tt.want.URL, cfg.PG.URL)
			assert.Equal(t, tt.want.MachineName, cfg.PG.MachineName)
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
	require.NoError(t, err)
	cfg.DataDir = dir
	require.NoError(t, cfg.loadFile(), "loadFile")

	require.Len(t, cfg.PG.Projects, 2)
	assert.Equal(t, "alpha", cfg.PG.Projects[0])
	assert.Equal(t, "beta", cfg.PG.Projects[1])
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
	require.NoError(t, err)
	cfg.DataDir = dir
	require.NoError(t, cfg.loadFile(), "loadFile")

	require.Len(t, cfg.PG.ExcludeProjects, 1)
	assert.Equal(t, "gamma", cfg.PG.ExcludeProjects[0])
}

func TestResolvePG_Defaults(t *testing.T) {
	cfg := Config{
		PG: PGConfig{
			URL: "postgres://localhost/test",
		},
	}
	resolved, err := cfg.ResolvePG()
	require.NoError(t, err, "ResolvePG")

	assert.Equal(t, "agentsview", resolved.Schema)
	assert.NotEmpty(t, resolved.MachineName, "MachineName should default to hostname")
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
	require.NoError(t, err, "ResolvePG")

	assert.Equal(t, "postgres://localhost/test?password=env-secret", resolved.URL)
}

func TestResolvePG_ExpandsBareEnvOnlyForWholeValue(t *testing.T) {
	t.Setenv("PGURL", "postgres://localhost/test")

	cfg := Config{
		PG: PGConfig{
			URL: "$PGURL",
		},
	}

	resolved, err := cfg.ResolvePG()
	require.NoError(t, err, "ResolvePG")

	assert.Equal(t, "postgres://localhost/test", resolved.URL)
}

func TestResolvePG_PreservesLiteralDollarSequencesInURL(t *testing.T) {
	t.Setenv("PGPASS", "env-secret")

	cfg := Config{
		PG: PGConfig{
			URL: "postgres://user:pa$word@localhost/db?application_name=$client&password=${PGPASS}",
		},
	}

	resolved, err := cfg.ResolvePG()
	require.NoError(t, err, "ResolvePG")

	assert.Equal(t, "postgres://user:pa$word@localhost/db?application_name=$client&password=env-secret", resolved.URL)
}

func TestResolvePG_ErrorsOnMissingEnvVar(t *testing.T) {
	cfg := Config{
		PG: PGConfig{
			URL: "${NONEXISTENT_PG_VAR}",
		},
	}

	_, err := cfg.ResolvePG()
	require.Error(t, err, "expected error for unset env var")
	assert.Contains(t, err.Error(), "NONEXISTENT_PG_VAR")
}

func TestResolvePG_ErrorsOnMissingBareEnvVar(t *testing.T) {
	cfg := Config{
		PG: PGConfig{
			URL: "$NONEXISTENT_PG_BARE_VAR",
		},
	}

	_, err := cfg.ResolvePG()
	require.Error(t, err, "expected error for unset bare env var")
	assert.Contains(t, err.Error(), "NONEXISTENT_PG_BARE_VAR")
}

// TestIsEnvDependentURL locks the helper to the same expansion semantics
// as expandBracedEnv: any ${VAR}, or a whole-string bare $VAR, is
// env-dependent; an embedded bare $VAR or literal dollar sequence is not.
func TestIsEnvDependentURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"braced var", "${PGURL}", true},
		{"braced var embedded", "postgres://h/db?password=${PGPASS}", true},
		{"whole-string bare var", "$PGURL", true},
		{"whole-string bare var with surrounding space", "  $PGURL  ", true},
		{"embedded bare var not expanded", "postgres://$USER@host/db", false},
		{"literal dollar sequence", "postgres://user:pa$word@host/db", false},
		{"plain literal", "postgres://user:pass@localhost/db?sslmode=disable", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, IsEnvDependentURL(c.in))
		})
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
	require.NoError(t, err, "ResolvePG should not reject filter conflicts")
}

func TestDuckDBConfig_LoadsFileAndEnv(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"duckdb": map[string]any{
			"path":             "/from/config/sessions.duckdb",
			"url":              "quack:config-host",
			"token":            "config-token",
			"machine_name":     "config-machine",
			"allow_insecure":   true,
			"projects":         []string{"alpha", "beta"},
			"exclude_projects": []string{"gamma"},
		},
	})
	t.Setenv("AGENTSVIEW_DUCKDB_PATH", "/from/env/sessions.duckdb")
	t.Setenv("AGENTSVIEW_DUCKDB_URL", "quack:env-host")
	t.Setenv("AGENTSVIEW_DUCKDB_TOKEN", "env-token")
	t.Setenv("AGENTSVIEW_DUCKDB_MACHINE", "env-machine")

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	assert.Equal(t, "/from/env/sessions.duckdb", cfg.DuckDB.Path)
	assert.Equal(t, "quack:env-host", cfg.DuckDB.URL)
	assert.Equal(t, "env-token", cfg.DuckDB.Token)
	assert.Equal(t, "env-machine", cfg.DuckDB.MachineName)
	assert.True(t, cfg.DuckDB.AllowInsecure)
	assert.Equal(t, []string{"alpha", "beta"}, cfg.DuckDB.Projects)
	assert.Equal(t, []string{"gamma"}, cfg.DuckDB.ExcludeProjects)
}

func TestResolveDuckDB_Defaults(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DataDir: dir}

	resolved, err := cfg.ResolveDuckDB()
	require.NoError(t, err, "ResolveDuckDB")

	assert.Equal(t, filepath.Join(dir, "sessions.duckdb"), resolved.Path)
	assert.NotEmpty(t, resolved.MachineName, "MachineName should default to hostname")
}

func TestResolveDuckDB_ExpandsEnvVars(t *testing.T) {
	t.Setenv("DUCKDB_URL", "quack:localhost")
	t.Setenv("DUCKDB_TOKEN", "secret-token")
	t.Setenv("DUCKDB_PATH", filepath.Join(t.TempDir(), "remote.duckdb"))

	cfg := Config{
		DuckDB: DuckDBConfig{
			Path:  "$DUCKDB_PATH",
			URL:   "${DUCKDB_URL}",
			Token: "${DUCKDB_TOKEN}",
		},
	}

	resolved, err := cfg.ResolveDuckDB()
	require.NoError(t, err, "ResolveDuckDB")

	assert.Equal(t, os.Getenv("DUCKDB_PATH"), resolved.Path)
	assert.Equal(t, "quack:localhost", resolved.URL)
	assert.Equal(t, "secret-token", resolved.Token)
}

func TestResolveDuckDB_ErrorsOnMissingEnvVar(t *testing.T) {
	cfg := Config{
		DuckDB: DuckDBConfig{
			URL: "${MISSING_DUCKDB_URL}",
		},
	}

	_, err := cfg.ResolveDuckDB()
	require.Error(t, err, "expected error for unset env var")
	assert.Contains(t, err.Error(), "MISSING_DUCKDB_URL")
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
	require.NoError(t, err, "loading config")
	want := []string{
		"You are analyzing an essay",
		"You are grading quotes",
		"  ",
		"You are analyzing an essay",
	}
	assert.Equal(t, want, cfg.Automated.Prefixes)
}

func TestAutomatedPrefixesAbsentIsNil(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"public_url": "http://example.com",
	})
	cfg, err := loadConfigFromPFlags(t)
	require.NoError(t, err, "loading config")
	assert.Nil(t, cfg.Automated.Prefixes)
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
			require.NoError(t, err, "LoadMinimal")

			if len(tt.want) == 0 {
				assert.Empty(t, cfg.CustomModelPricing)
				return
			}

			require.Len(t, cfg.CustomModelPricing, len(tt.want))
			for model, wantRate := range tt.want {
				got, ok := cfg.CustomModelPricing[model]
				if !ok {
					t.Errorf("missing model %q", model)
					continue
				}
				assert.Equal(t, wantRate, got, "model %q", model)
			}
		})
	}
}

func TestLoadFile_RemoteHosts(t *testing.T) {
	dir := setupTestEnv(t)
	path := filepath.Join(dir, configFileName)
	data := []byte(`[[remote_hosts]]
host = "devbox1"
user = "jesse"
port = 22

[[remote_hosts]]
host = "  laptop2  "
`)
	require.NoError(t, os.WriteFile(path, data, 0o600), "write config")

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	require.Len(t, cfg.RemoteHosts, 2)
	assert.Equal(t, RemoteHost{Host: "devbox1", User: "jesse", Port: 22}, cfg.RemoteHosts[0])
	// host is trimmed at load so validation and SSH see the same value
	assert.Equal(t, RemoteHost{Host: "laptop2"}, cfg.RemoteHosts[1])
}

func TestLoadFile_RemoteHostsAbsentIsNil(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{"public_url": "http://example.com"})

	cfg, err := LoadMinimal()
	require.NoError(t, err)
	assert.Nil(t, cfg.RemoteHosts)
}

func TestValidateRemoteHosts(t *testing.T) {
	tests := []struct {
		name    string
		hosts   []RemoteHost
		wantErr []string // substrings expected in error; empty => no error
	}{
		{"valid", []RemoteHost{{Host: "a"}, {Host: "b", Port: 22}, {Host: "c", Port: 0}}, nil},
		{"empty host", []RemoteHost{{Host: ""}}, []string{"host is required"}},
		{"negative port", []RemoteHost{{Host: "a", Port: -1}}, []string{"invalid port"}},
		{"port too large", []RemoteHost{{Host: "a", Port: 70000}}, []string{"invalid port"}},
		{"aggregates both", []RemoteHost{{Host: ""}, {Host: "b", Port: 99999}}, []string{"host is required", "invalid port"}},
		{"duplicate host", []RemoteHost{{Host: "a"}, {Host: "a"}}, []string{"duplicate host"}},
		{"duplicate host different user or port", []RemoteHost{{Host: "box", User: "alice"}, {Host: "box", User: "bob", Port: 2222}}, []string{"duplicate host"}},
		{"none configured", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Config{RemoteHosts: tt.hosts}.ValidateRemoteHosts()
			if len(tt.wantErr) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range tt.wantErr {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}
