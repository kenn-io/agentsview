package config

import (
	"bytes"
	"flag"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

type configFixture struct {
	Dir string
}

func newConfigFixture(t *testing.T) configFixture {
	t.Helper()
	return configFixture{Dir: setupTestEnv(t)}
}

func (f configFixture) Path(name string) string {
	return filepath.Join(f.Dir, name)
}

func (f configFixture) WriteTOML(t *testing.T, data any) {
	t.Helper()
	writeConfig(t, f.Dir, data)
}

func (f configFixture) WriteConfigText(t *testing.T, text string) {
	t.Helper()
	require.NoError(t, os.WriteFile(f.Path(configFileName), []byte(text), 0o600),
		"write config")
}

func (f configFixture) WriteLegacyJSON(t *testing.T, text string) {
	t.Helper()
	require.NoError(t, os.WriteFile(f.Path("config.json"), []byte(text), 0o600),
		"write legacy config")
}

func (f configFixture) LoadMinimal(t *testing.T) Config {
	t.Helper()
	cfg, err := LoadMinimal()
	require.NoError(t, err)
	return cfg
}

func (f configFixture) LoadMinimalErr(t *testing.T) error {
	t.Helper()
	_, err := LoadMinimal()
	return err
}

func (f configFixture) LoadFile(t *testing.T) Config {
	t.Helper()
	cfg, err := Default()
	require.NoError(t, err)
	cfg.DataDir = f.Dir
	require.NoError(t, cfg.loadFile(), "loadFile")
	return cfg
}

func (f configFixture) ReadTOMLMap(t *testing.T) map[string]any {
	t.Helper()
	got, err := os.ReadFile(f.Path(configFileName))
	require.NoError(t, err)
	var result map[string]any
	_, err = toml.Decode(string(got), &result)
	require.NoError(t, err)
	return result
}

func loadMinimalWithConfig(t *testing.T, data any) Config {
	t.Helper()
	f := newConfigFixture(t)
	f.WriteTOML(t, data)
	return f.LoadMinimal(t)
}

func loadMinimalErrWithConfig(t *testing.T, data any) error {
	t.Helper()
	f := newConfigFixture(t)
	f.WriteTOML(t, data)
	return f.LoadMinimalErr(t)
}

func loadPFlagsWithConfig(
	t *testing.T,
	data any,
	args ...string,
) Config {
	t.Helper()
	f := newConfigFixture(t)
	f.WriteTOML(t, data)
	cfg, err := loadConfigFromPFlags(t, args...)
	require.NoError(t, err, "loading config")
	return cfg
}

func runConcurrent(t *testing.T, workers int, fn func(i int) error) {
	t.Helper()
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func() {
			defer wg.Done()
			<-start
			if err := fn(i); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func requireAllSameNonEmpty[T comparable](t *testing.T, values []T) {
	t.Helper()
	require.NotEmpty(t, values)
	for _, value := range values {
		require.NotZero(t, value)
		assert.Equal(t, values[0], value)
	}
}

func requireErrorContains(t *testing.T, err error, substrs ...string) {
	t.Helper()
	require.Error(t, err)
	for _, substr := range substrs {
		assert.Contains(t, err.Error(), substr)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func assertLogContains(t *testing.T, buf *bytes.Buffer, substrs ...string) {
	t.Helper()
	logged := buf.String()
	for _, substr := range substrs {
		assert.Contains(t, logged, substr)
	}
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
	f := newConfigFixture(t)
	f.WriteConfigText(t, `[agent.claude]
binary = "/opt/agents/claude"

[agent.gemini]
binary = "/usr/local/bin/gemini"
sandbox = "sandbox-exec"
allow_unsafe = true
`)

	cfg := f.LoadMinimal(t)

	assert.Equal(t, "/opt/agents/claude", cfg.Agent["claude"].Binary)
	assert.Equal(t, "/usr/local/bin/gemini", cfg.Agent["gemini"].Binary)
	assert.Equal(t, "sandbox-exec", cfg.Agent["gemini"].Sandbox)
	assert.True(t, cfg.Agent["gemini"].AllowUnsafe)
}

func TestLoadReadOnlyReadsLegacyJSONWithoutMigrating(t *testing.T) {
	f := newConfigFixture(t)
	jsonPath := f.Path("config.json")
	f.WriteLegacyJSON(t, `{
		"codex_sessions_dirs": ["/legacy/codex"],
		"result_content_blocked_categories": ["Read", "Search"]
	}`)

	cfg, err := LoadReadOnly()
	require.NoError(t, err)

	assert.Equal(t, []string{"/legacy/codex"},
		cfg.ResolveDirs(parser.AgentCodex))
	assert.Equal(t, []string{"Read", "Search"},
		cfg.ResultContentBlockedCategories)
	assert.FileExists(t, jsonPath)
	assert.NoFileExists(t, f.Path(configFileName))
	assert.NoFileExists(t, jsonPath+".bak")
}

func TestDefault_IncludesCodexArchivedSessionsDir(t *testing.T) {
	cfg, err := Default()
	require.NoError(t, err)

	dirs := cfg.ResolveDirs(parser.AgentCodex)
	require.Len(t, dirs, 2)
	assert.True(t, strings.HasSuffix(dirs[0], filepath.Join(".codex", "sessions")), "dirs[0] = %q", dirs[0])
	assert.True(t, strings.HasSuffix(dirs[1], filepath.Join(".codex", "archived_sessions")), "dirs[1] = %q", dirs[1])
}

func TestDefault_SkipsAiderUntilConfigured(t *testing.T) {
	t.Setenv("AIDER_DIR", "")
	cfg, err := Default()
	require.NoError(t, err)

	// Aider has no safe default root: a passive viewer must not enumerate
	// $HOME (macOS privacy prompts), so it stays unresolved until the user
	// opts in via AIDER_DIR or aider_dirs.
	assert.Empty(t, cfg.ResolveDirs(parser.AgentAider))
	assert.False(t, cfg.IsUserConfigured(parser.AgentAider))
}

func TestDefault_IncludesDevinLocalShareRoots(t *testing.T) {
	cfg, err := Default()
	require.NoError(t, err)

	dirs := cfg.ResolveDirs(parser.AgentDevin)
	require.Len(t, dirs, 2)
	assert.True(t, strings.HasSuffix(dirs[0], filepath.Join("Library", "Application Support", "devin")), "dirs[0] = %q", dirs[0])
	assert.True(t, strings.HasSuffix(dirs[1], filepath.Join(".local", "share", "devin")), "dirs[1] = %q", dirs[1])
	assert.False(t, cfg.IsUserConfigured(parser.AgentDevin))
}

func TestLoadEnv_OverridesDataDir(t *testing.T) {
	custom := setupTestEnv(t)

	cfg, err := Default()
	require.NoError(t, err)
	cfg.loadEnv()

	assert.Equal(t, custom, cfg.DataDir)
}

func TestLoadEnv_UsesPrefixedCursorAdminVarsWithLegacyFallback(t *testing.T) {
	setupTestEnv(t)
	t.Setenv("AGENTSVIEW_CURSOR_ADMIN_API_KEY", "prefixed-key")
	t.Setenv("CURSOR_ADMIN_API_KEY", "legacy-key")
	t.Setenv("CURSOR_ADMIN_EMAIL", "legacy@example.com")
	t.Setenv("AGENTSVIEW_CURSOR_ADMIN_USER_ID", "prefixed-user")

	cfg, err := Default()
	require.NoError(t, err)
	cfg.loadEnv()

	assert.Equal(t, "prefixed-key", cfg.CursorAdminAPIKey)
	assert.Equal(t, "legacy@example.com", cfg.CursorAdminEmail)
	assert.Equal(t, "prefixed-user", cfg.CursorAdminUserID)
}

func TestLoadMinimal_PreservesCursorAdminEnvOverFile(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"cursor_admin_api_key": "file-key",
		"cursor_admin_email":   "file@example.com",
		"cursor_admin_user_id": "file-user",
	})
	t.Setenv("AGENTSVIEW_CURSOR_ADMIN_API_KEY", "env-key")
	t.Setenv("AGENTSVIEW_CURSOR_ADMIN_EMAIL", "env@example.com")
	t.Setenv("AGENTSVIEW_CURSOR_ADMIN_USER_ID", "env-user")

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	assert.Equal(t, "env-key", cfg.CursorAdminAPIKey)
	assert.Equal(t, "env@example.com", cfg.CursorAdminEmail)
	assert.Equal(t, "env-user", cfg.CursorAdminUserID)
}

func TestLoadMinimal_PreservesAuthTokenEnvOverFile(t *testing.T) {
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"auth_token": "file-token",
	})
	t.Setenv("AGENTSVIEW_AUTH_TOKEN", "env-token")

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	assert.Equal(t, "env-token", cfg.AuthToken)
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
	setupTestEnv(t)
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

func TestLoad_HostFromConfigFile(t *testing.T) {
	cfg := loadMinimalWithConfig(t, map[string]any{
		"host": "0.0.0.0",
	})

	assert.Equal(t, "0.0.0.0", cfg.Host)
	assert.False(t, cfg.HostExplicit,
		"config-file host must not count as an explicit flag")
}

func TestLoad_HostFlagOverridesConfigFile(t *testing.T) {
	tmp := setupTestEnv(t)
	writeConfig(t, tmp, map[string]any{
		"host": "0.0.0.0",
	})

	cfg, err := loadConfigFromFlags(t, "-host", "192.168.1.5")
	require.NoError(t, err)

	assert.Equal(t, "192.168.1.5", cfg.Host)
	assert.True(t, cfg.HostExplicit)
}

func TestLoad_PortFromConfigFile(t *testing.T) {
	cfg := loadMinimalWithConfig(t, map[string]any{
		"port": 7357,
	})

	assert.Equal(t, 7357, cfg.Port)
}

func TestLoad_PortFlagOverridesConfigFile(t *testing.T) {
	tmp := setupTestEnv(t)
	writeConfig(t, tmp, map[string]any{
		"port": 7357,
	})

	cfg, err := loadConfigFromFlags(t, "-port", "9090")
	require.NoError(t, err)

	assert.Equal(t, 9090, cfg.Port)
}

func TestLoad_PublicOriginsFromConfigFile(t *testing.T) {
	cfg := loadMinimalWithConfig(t, map[string]any{
		"public_origins": []string{
			"https://Viewer.Example.Test:443/",
			"http://viewer.example.test:8004",
		},
	})

	got := strings.Join(cfg.PublicOrigins, ",")
	assert.Equal(t, "https://viewer.example.test,http://viewer.example.test:8004", got)
}

func TestLoad_PublicOriginsRejectInvalid(t *testing.T) {
	err := loadMinimalErrWithConfig(t, map[string]any{
		"public_origins": []string{"ftp://viewer.example.test"},
	})

	requireErrorContains(t, err, "invalid public origins")
}

func TestLoad_PublicURLMergedIntoOrigins(t *testing.T) {
	cfg := loadMinimalWithConfig(t, map[string]any{
		"public_url": "https://viewer.example.test/",
	})

	assert.Equal(t, "https://viewer.example.test", cfg.PublicURL)
	assert.Equal(t, "https://viewer.example.test", strings.Join(cfg.PublicOrigins, ","))
}

func TestLoad_ProxyConfigFromFile(t *testing.T) {
	cfg := loadMinimalWithConfig(t, map[string]any{
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
	requireErrorContains(t, err, "conflicts with configured public port")
}

func TestLoad_ManagedCaddyRejectsPublicURLPath(t *testing.T) {
	_, err := loadConfigFromFlags(
		t,
		"-public-url", "https://viewer.example.test/path",
		"-proxy", "caddy",
	)
	requireErrorContains(t, err, "must not include a path")
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
	err := loadMinimalErrWithConfig(t, map[string]any{
		"proxy": map[string]any{
			"mode":            "caddy",
			"allowed_subnets": []string{"10.0.0.0/not-a-mask"},
		},
	})

	requireErrorContains(t, err, "invalid allowed subnets")
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

	f := newConfigFixture(t)
	cfg := Config{DataDir: f.Dir}

	// Create a config file that is not readable
	path := f.Path(configFileName)
	require.NoError(t, os.WriteFile(path, []byte("k = \"v\"\n"), 0o000))

	err := cfg.SaveGithubToken("tok")
	requireErrorContains(t, err, "reading config file")
}

func TestSaveGithubToken_PreservesExistingKeys(t *testing.T) {
	f := newConfigFixture(t)
	cfg := Config{DataDir: f.Dir}

	existing := map[string]any{"custom_key": "value"}
	f.WriteTOML(t, existing)

	require.NoError(t, cfg.SaveGithubToken("new-token"))

	result := f.ReadTOMLMap(t)
	assert.Equal(t, "value", result["custom_key"])
	assert.Equal(t, "new-token", result["github_token"])
}

func TestEnsureAuthTokenConcurrentCallersSharePersistedToken(t *testing.T) {
	f := newConfigFixture(t)
	const workers = 8
	tokens := make([]string, workers)

	runConcurrent(t, workers, func(i int) error {
		cfg := Config{DataDir: f.Dir}
		if err := cfg.EnsureAuthToken(); err != nil {
			return err
		}
		tokens[i] = cfg.AuthToken
		return nil
	})

	requireAllSameNonEmpty(t, tokens)
	result := f.ReadTOMLMap(t)
	assert.Equal(t, tokens[0], result["auth_token"])
}

func TestEnsureCursorSecretConcurrentCallersSharePersistedSecret(t *testing.T) {
	f := newConfigFixture(t)
	const workers = 8
	secrets := make([]string, workers)

	runConcurrent(t, workers, func(i int) error {
		cfg := Config{DataDir: f.Dir}
		if err := cfg.ensureCursorSecret(); err != nil {
			return err
		}
		secrets[i] = cfg.CursorSecret
		return nil
	})

	requireAllSameNonEmpty(t, secrets)
	result := f.ReadTOMLMap(t)
	assert.Equal(t, secrets[0], result["cursor_secret"])
}

func TestMigrateJSONToTOMLConcurrentCallersMigrateOnce(t *testing.T) {
	f := newConfigFixture(t)
	jsonPath := f.Path("config.json")
	f.WriteLegacyJSON(t, `{
		"github_token": "legacy-token",
		"require_auth": true
	}`)

	const workers = 4
	runConcurrent(t, workers, func(int) error {
		cfg := Config{DataDir: f.Dir}
		return cfg.migrateJSONToTOML()
	})

	assert.NoFileExists(t, jsonPath)
	assert.FileExists(t, jsonPath+".bak")
	result := f.ReadTOMLMap(t)
	assert.Equal(t, "legacy-token", result["github_token"])
	assert.Equal(t, true, result["require_auth"])
}

func TestLoadFile_ReadsDirArrays(t *testing.T) {
	cfg := loadMinimalWithConfig(t, map[string]any{
		"claude_project_dirs": []string{"/path/one", "/path/two"},
		"codex_sessions_dirs": []string{"/codex/a"},
		"aider_dirs":          []string{"/code"},
	})

	claudeDirs := cfg.ResolveDirs(parser.AgentClaude)
	require.Len(t, claudeDirs, 2)
	assert.Equal(t, "/path/one", claudeDirs[0])
	assert.Equal(t, "/path/two", claudeDirs[1])
	codexDirs := cfg.ResolveDirs(parser.AgentCodex)
	require.Len(t, codexDirs, 1)
	assert.Equal(t, "/codex/a", codexDirs[0])
	assert.Equal(t, []string{"/code"}, cfg.ResolveDirs(parser.AgentAider))
	assert.True(t, cfg.IsUserConfigured(parser.AgentAider))
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
			t.Setenv("CLAUDE_CONFIG_DIR", "")
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

func TestResolveDirs_ClaudeConfigDirRootEnvVar(t *testing.T) {
	t.Run("root env re-roots implicit default", func(t *testing.T) {
		dir := setupTestEnv(t)
		root := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", root)
		writeConfig(t, dir, map[string]any{})

		cfg, err := LoadMinimal()
		require.NoError(t, err)

		assert.Equal(t, []string{filepath.Join(root, "projects")},
			cfg.ResolveDirs(parser.AgentClaude))
		assert.False(t, cfg.IsUserConfigured(parser.AgentClaude))
	})

	t.Run("projects env beats root env", func(t *testing.T) {
		dir := setupTestEnv(t)
		root := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", root)
		t.Setenv("CLAUDE_PROJECTS_DIR", "/env/override")
		writeConfig(t, dir, map[string]any{})

		cfg, err := LoadMinimal()
		require.NoError(t, err)

		assert.Equal(t, []string{"/env/override"},
			cfg.ResolveDirs(parser.AgentClaude))
		assert.True(t, cfg.IsUserConfigured(parser.AgentClaude))
	})

	t.Run("config file beats root env", func(t *testing.T) {
		dir := setupTestEnv(t)
		root := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", root)
		writeConfig(t, dir, map[string]any{
			"claude_project_dirs": []string{"/from/config"},
		})

		cfg, err := LoadMinimal()
		require.NoError(t, err)

		assert.Equal(t, []string{"/from/config"},
			cfg.ResolveDirs(parser.AgentClaude))
		assert.True(t, cfg.IsUserConfigured(parser.AgentClaude))
	})
}

func TestResolveDirs_DevinPrecedenceAndMergeRules(t *testing.T) {
	t.Run("config overrides defaults", func(t *testing.T) {
		cfg := loadMinimalWithConfig(t, map[string]any{
			"devin_dirs": []string{"/from/config/devin"},
		})

		assert.Equal(t, []string{"/from/config/devin"},
			cfg.ResolveDirs(parser.AgentDevin))
		assert.True(t, cfg.IsUserConfigured(parser.AgentDevin))
	})

	t.Run("env overrides config", func(t *testing.T) {
		f := newConfigFixture(t)
		f.WriteTOML(t, map[string]any{
			"devin_dirs": []string{"/from/config/devin"},
		})
		t.Setenv("DEVIN_DIR", "/from/env/devin")

		cfg := f.LoadMinimal(t)

		assert.Equal(t, []string{"/from/env/devin"},
			cfg.ResolveDirs(parser.AgentDevin))
		assert.True(t, cfg.IsUserConfigured(parser.AgentDevin))
	})

	t.Run("config file still applies when env unset", func(t *testing.T) {
		f := newConfigFixture(t)
		f.WriteTOML(t, map[string]any{
			"devin_dirs": []string{"/from/config/devin", "/second/config/devin"},
		})
		t.Setenv("DEVIN_DIR", "")

		cfg := f.LoadMinimal(t)

		assert.Equal(t, []string{"/from/config/devin", "/second/config/devin"},
			cfg.ResolveDirs(parser.AgentDevin))
		assert.True(t, cfg.IsUserConfigured(parser.AgentDevin))
	})
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
	f := newConfigFixture(t)
	f.WriteTOML(t, map[string]any{
		"codex_sessions_dirs": []string{"/from/config"},
	})
	t.Setenv("CODEX_SESSIONS_DIR", "/from/env")

	cfg := f.LoadMinimal(t)

	dirs := cfg.ResolveDirs(parser.AgentCodex)
	assert.Equal(t, []string{"/from/env"}, dirs)
}

func TestLoadFile_MalformedDirValueLogsWarning(t *testing.T) {
	f := newConfigFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	// Write a config where claude_project_dirs is a string
	// instead of a string array.
	f.WriteTOML(t, map[string]any{
		"claude_project_dirs": "/not/an/array",
	})

	// Capture log output during Load.
	buf := captureLog(t)

	cfg := f.LoadMinimal(t)

	// The malformed key should trigger a warning.
	assertLogContains(t, buf, "claude_project_dirs", "expected string array")

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
			cfg := loadMinimalWithConfig(t, tt.config)

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
			cfg := loadMinimalWithConfig(t, tt.config)
			assert.Equal(t, tt.want, cfg.EventsCoalesceInterval)
		})
	}
}

func TestLoadFile_DaemonIdleTimeout(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want time.Duration
	}{
		{name: "absent uses default", data: map[string]any{}, want: 20 * time.Minute},
		{name: "configured", data: map[string]any{"daemon_idle_timeout": "6h"}, want: 6 * time.Hour},
		{name: "explicit zero disables", data: map[string]any{"daemon_idle_timeout": "0s"}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadMinimalWithConfig(t, tt.data)
			assert.Equal(t, tt.want, cfg.DaemonIdleTimeout)
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
			f := newConfigFixture(t)
			f.WriteTOML(t, tt.config)
			if tt.envURL != "" {
				t.Setenv("AGENTSVIEW_PG_URL", tt.envURL)
			}

			cfg := f.LoadMinimal(t)

			resolved, err := cfg.ResolvePG()
			require.NoError(t, err)

			assert.Equal(t, tt.want.URL, resolved.URL)
			if tt.want.MachineName == "" {
				assert.NotEmpty(t, resolved.MachineName)
			} else {
				assert.Equal(t, tt.want.MachineName, resolved.MachineName)
			}
		})
	}
}

func TestResolvePGTarget_NamedTargets(t *testing.T) {
	cfg := Config{
		DefaultPG: "work",
		PGTargets: map[string]PGConfig{
			"work": {
				URL:         "postgres://work",
				MachineName: "workbox",
			},
			"archive": {
				URL:         "postgres://archive",
				MachineName: "archivebox",
			},
		},
		pgEnvOverrides: pgEnvOverrides{
			URL:         "postgres://env-default",
			MachineName: "envbox",
		},
	}

	defaultTarget, err := cfg.ResolvePG()
	require.NoError(t, err)
	assert.Equal(t, "postgres://env-default", defaultTarget.URL)
	assert.Equal(t, "envbox", defaultTarget.MachineName)

	archiveTarget, err := cfg.ResolvePGTarget("archive")
	require.NoError(t, err)
	assert.Equal(t, "postgres://archive", archiveTarget.URL)
	assert.Equal(t, "archivebox", archiveTarget.MachineName)
}

func TestResolvePGTargets_DefaultFirst(t *testing.T) {
	cfg := Config{
		DefaultPG: "work",
		PGTargets: map[string]PGConfig{
			"archive": {URL: "postgres://archive"},
			"work":    {URL: "postgres://work"},
		},
	}

	targets, err := cfg.ResolvePGTargets()
	require.NoError(t, err)
	require.Len(t, targets, 2)
	assert.Equal(t, "work", targets[0].Name)
	assert.True(t, targets[0].IsDefault)
	assert.Equal(t, "archive", targets[1].Name)
	assert.False(t, targets[1].IsDefault)
}

func TestResolvePGTargets_OneNamedTargetWithoutDefault(t *testing.T) {
	cfg := Config{
		PGTargets: map[string]PGConfig{
			"work": {URL: "postgres://work"},
		},
	}

	targets, err := cfg.ResolvePGTargets()
	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, "work", targets[0].Name)
	assert.True(t, targets[0].IsDefault)
}

func TestResolvePGTargets_MultipleNamedTargetsRequireDefault(t *testing.T) {
	cfg := Config{
		PGTargets: map[string]PGConfig{
			"work":    {URL: "postgres://work"},
			"archive": {URL: "postgres://archive"},
		},
	}

	_, err := cfg.ResolvePGTargets()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default_pg is required")
}

func TestLoadMinimal_DefersNamedPGValidationForNonPGCommands(t *testing.T) {
	dir := setupTestEnv(t)
	path := filepath.Join(dir, configFileName)
	data := []byte(`
default_pg = "missing"

[pg.work]
url = "postgres://work"
`)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	_, err = cfg.ResolvePG()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `default_pg "missing" does not match any named [pg.NAME] target`)
}

func TestLoadFile_PGMixedLegacyAndNamedTargetsFails(t *testing.T) {
	dir := setupTestEnv(t)
	path := filepath.Join(dir, configFileName)
	data := []byte(`
[pg]
url = "postgres://legacy"

[pg.archive]
url = "postgres://archive"
`)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err := LoadMinimal()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot mix legacy [pg] fields with named [pg.NAME] targets")
}

func TestLoadFile_PGNamedTargetValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			name: "reserved all target",
			toml: `
[pg.all]
url = "postgres://all"
`,
			wantErr: `named PG target "all" is reserved`,
		},
		{
			name: "reserved local target",
			toml: `
[pg.local]
url = "postgres://local"
`,
			wantErr: `named PG target "local" is reserved`,
		},
		{
			name: "duplicate normalized target names",
			toml: `
[pg.Work]
url = "postgres://work"

[pg.work]
url = "postgres://work2"
`,
			wantErr: `normalize to the same name "work"`,
		},
		{
			name: "named target must be table",
			toml: `
[pg]
archive = "postgres://archive"
`,
			wantErr: `[pg].archive must be a named target table`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestEnv(t)
			path := filepath.Join(dir, configFileName)
			require.NoError(t, os.WriteFile(path, []byte(tt.toml), 0o600))

			_, err := LoadMinimal()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestPGConfig_ProjectFilter(t *testing.T) {
	f := newConfigFixture(t)
	f.WriteConfigText(t, `
[pg]
url = "postgres://localhost/test"
projects = ["alpha", "beta"]
`)

	cfg := f.LoadFile(t)

	require.Len(t, cfg.PG.Projects, 2)
	assert.Equal(t, "alpha", cfg.PG.Projects[0])
	assert.Equal(t, "beta", cfg.PG.Projects[1])
}

func TestPGConfig_ExcludeProjectFilter(t *testing.T) {
	f := newConfigFixture(t)
	f.WriteConfigText(t, `
[pg]
url = "postgres://localhost/test"
exclude_projects = ["gamma"]
`)

	cfg := f.LoadFile(t)

	require.Len(t, cfg.PG.ExcludeProjects, 1)
	assert.Equal(t, "gamma", cfg.PG.ExcludeProjects[0])
}

func TestEnsureAuthTokenAdoptsPersistedToken(t *testing.T) {
	f := newConfigFixture(t)
	f.WriteConfigText(t, `
auth_token = "persisted-token"
require_auth = true
`)

	cfg, err := Default()
	require.NoError(t, err)
	cfg.DataDir = f.Dir
	cfg.RequireAuth = true

	require.NoError(t, cfg.EnsureAuthToken())
	assert.Equal(t, "persisted-token", cfg.AuthToken)

	data, err := os.ReadFile(f.Path(configFileName))
	require.NoError(t, err)
	assert.Contains(t, string(data), `auth_token = "persisted-token"`)
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
	requireErrorContains(t, err, "NONEXISTENT_PG_VAR")
}

func TestResolvePG_ErrorsOnMissingBareEnvVar(t *testing.T) {
	cfg := Config{
		PG: PGConfig{
			URL: "$NONEXISTENT_PG_BARE_VAR",
		},
	}

	_, err := cfg.ResolvePG()
	requireErrorContains(t, err, "NONEXISTENT_PG_BARE_VAR")
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
	f := newConfigFixture(t)
	f.WriteTOML(t, map[string]any{
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

	cfg := f.LoadMinimal(t)

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
	requireErrorContains(t, err, "MISSING_DUCKDB_URL")
}

func TestAutomatedConfigRoundTrip(t *testing.T) {
	cfg := loadPFlagsWithConfig(t, map[string]any{
		"automated": map[string]any{
			"prefixes": []string{
				"You are analyzing an essay",
				"You are grading quotes",
				"  ",                         // whitespace preserved here; normalization is db-side
				"You are analyzing an essay", // duplicate preserved here too
			},
			"substrings": []string{
				"invoked by roborev to perform this review",
				"  embedded marker  ",
				"invoked by roborev to perform this review",
			},
			"exact_matches": []string{
				"Warmup",
				"  Reply with exactly OK.  ",
				"Warmup",
			},
		},
	})
	wantPrefixes := []string{
		"You are analyzing an essay",
		"You are grading quotes",
		"  ",
		"You are analyzing an essay",
	}
	wantSubstrings := []string{
		"invoked by roborev to perform this review",
		"  embedded marker  ",
		"invoked by roborev to perform this review",
	}
	wantExactMatches := []string{
		"Warmup",
		"  Reply with exactly OK.  ",
		"Warmup",
	}
	assert.Equal(t, wantPrefixes, cfg.Automated.Prefixes)
	assert.Equal(t, wantSubstrings, cfg.Automated.Substrings)
	assert.Equal(t, wantExactMatches, cfg.Automated.ExactMatches)
}

func TestAutomatedConfigAbsentIsNil(t *testing.T) {
	cfg := loadPFlagsWithConfig(t, map[string]any{
		"public_url": "http://example.com",
	})
	assert.Nil(t, cfg.Automated.Prefixes)
	assert.Nil(t, cfg.Automated.Substrings)
	assert.Nil(t, cfg.Automated.ExactMatches)
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
			cfg := loadMinimalWithConfig(t, tt.data)

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
	f := newConfigFixture(t)
	f.WriteConfigText(t, `[[remote_hosts]]
host = "devbox1"
user = "jesse"
port = 22
interval = "5m"

[[remote_hosts]]
host = "  laptop2  "
`)

	cfg := f.LoadMinimal(t)

	require.Len(t, cfg.RemoteHosts, 2)
	assert.Equal(t, RemoteHost{Host: "devbox1", User: "jesse", Port: 22, Interval: 5 * time.Minute}, cfg.RemoteHosts[0])
	assert.Equal(t, 5*time.Minute, cfg.RemoteHosts[0].Interval)
	assert.Equal(t, time.Duration(0), cfg.RemoteHosts[1].Interval)
	// host is trimmed at load so validation and SSH see the same value
	assert.Equal(t, RemoteHost{Host: "laptop2"}, cfg.RemoteHosts[1])
}

func TestLoadFile_RemoteHostsHTTP(t *testing.T) {
	f := newConfigFixture(t)
	f.WriteConfigText(t, `[[remote_hosts]]
host = "devbox1"
transport = "http"
url = "http://devbox1.tailnet.ts.net:8080"
token = "remote-token"
interval = "5m"
`)

	cfg := f.LoadMinimal(t)

	require.Len(t, cfg.RemoteHosts, 1)
	assert.Equal(t, RemoteHost{
		Host:      "devbox1",
		Transport: RemoteTransportHTTP,
		URL:       "http://devbox1.tailnet.ts.net:8080",
		Token:     "remote-token",
		Interval:  5 * time.Minute,
	}, cfg.RemoteHosts[0])
}

func TestLoadFile_InvalidRemoteHostsDoNotBlockConfigLoad(t *testing.T) {
	f := newConfigFixture(t)
	f.WriteConfigText(t, `[[remote_hosts]]
host = "devbox1"
transport = "http"
`)

	cfg := f.LoadMinimal(t)

	require.Len(t, cfg.RemoteHosts, 1)
	assert.Equal(t, RemoteTransportHTTP, cfg.RemoteHosts[0].Transport)
	assert.Equal(t, "devbox1", cfg.RemoteHosts[0].Host)
}

func TestLoadFile_RemoteHostsAbsentIsNil(t *testing.T) {
	cfg := loadMinimalWithConfig(t,
		map[string]any{"public_url": "http://example.com"})

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
		{"option shaped host", []RemoteHost{{Host: "-oProxyCommand=sh"}}, []string{"host must not begin with '-'"}},
		{"option shaped user", []RemoteHost{{Host: "box", User: "-lroot"}}, []string{"user must not begin with '-'"}},
		{"none configured", nil, nil},
		{"negative interval", []RemoteHost{{Host: "a", Interval: -1}}, []string{"invalid interval"}},
		{"zero interval ok", []RemoteHost{{Host: "a", Interval: 0}}, nil},
		{"http valid", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP, URL: "https://a.example.test", Token: "token"}}, nil},
		{"http requires url", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP}}, []string{"url is required"}},
		{"http requires token", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP, URL: "https://a.example.test"}}, []string{"token is required"}},
		{"http rejects user", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP, URL: "https://a.example.test", User: "alice"}}, []string{"user is only valid for ssh"}},
		{"http rejects port", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP, URL: "https://a.example.test", Port: 443}}, []string{"port is only valid for ssh"}},
		{"http rejects bad scheme", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP, URL: "ftp://a.example.test"}}, []string{"url must use http or https"}},
		{"http rejects empty hostname with port", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP, URL: "http://:8080"}}, []string{"url must include a host"}},
		{"http rejects query", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP, URL: "https://a.example.test?token=x"}}, []string{"url must not include query"}},
		{"http rejects empty query delimiter", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP, URL: "https://a.example.test?", Token: "token"}}, []string{"url must not include query"}},
		{"http rejects fragment", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP, URL: "https://a.example.test/#remote"}}, []string{"url must not include fragment"}},
		{"http rejects empty fragment delimiter", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP, URL: "https://a.example.test#", Token: "token"}}, []string{"url must not include fragment"}},
		{"http rejects userinfo", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP, URL: "https://alice@a.example.test"}}, []string{"url must not include userinfo"}},
		{"ssh rejects url", []RemoteHost{{Host: "a", Transport: RemoteTransportSSH, URL: "https://a.example.test"}}, []string{"url is only valid for http"}},
		{"ssh rejects token", []RemoteHost{{Host: "a", Transport: RemoteTransportSSH, Token: "token"}}, []string{"token is only valid for http"}},
		{"unknown transport", []RemoteHost{{Host: "a", Transport: RemoteTransport("sftp")}}, []string{"invalid transport"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Config{RemoteHosts: tt.hosts}.ValidateRemoteHosts()
			if len(tt.wantErr) == 0 {
				require.NoError(t, err)
				return
			}
			requireErrorContains(t, err, tt.wantErr...)
		})
	}
}

func TestLoadFile_SyncIncludeCwdPrefixes(t *testing.T) {
	f := newConfigFixture(t)
	f.WriteConfigText(t, `sync_include_cwd_prefixes = ["/home/me/work", "/home/me/oss"]
`)

	cfg := f.LoadMinimal(t)

	assert.Equal(t,
		[]string{"/home/me/work", "/home/me/oss"},
		cfg.SyncIncludeCwdPrefixes,
	)
}

func TestLoadFile_SyncIncludeCwdPrefixesDefaultsEmpty(t *testing.T) {
	f := newConfigFixture(t)
	f.WriteConfigText(t, `host = "127.0.0.1"
`)

	cfg := f.LoadMinimal(t)

	assert.Empty(t, cfg.SyncIncludeCwdPrefixes)
}

func TestIsDefaultAgentsviewDBPath(t *testing.T) {
	t.Parallel()

	// A plain file inside a real ~/.agentsview directory.
	defaultDir := filepath.Join(t.TempDir(), ".agentsview")
	require.NoError(t, os.MkdirAll(defaultDir, 0o700))
	defaultDB := filepath.Join(defaultDir, "sessions.db")
	require.NoError(t, os.WriteFile(defaultDB, []byte("db"), 0o600))

	// A plain file outside ~/.agentsview.
	labDir := filepath.Join(t.TempDir(), "recall-lab-data")
	require.NoError(t, os.MkdirAll(labDir, 0o700))
	labDB := filepath.Join(labDir, "sessions.db")
	require.NoError(t, os.WriteFile(labDB, []byte("db"), 0o600))

	// A symlink whose target already exists inside ~/.agentsview.
	liveLink := filepath.Join(labDir, "live-link.db")
	require.NoError(t, os.Symlink(defaultDB, liveLink))

	// An absolute symlink whose target does not exist yet. Opening SQLite
	// through it would create the production archive, so it must be guarded
	// even though EvalSymlinks cannot resolve the dangling target.
	danglingLink := filepath.Join(labDir, "dangling.db")
	require.NoError(t, os.Symlink(
		filepath.Join(defaultDir, "not-created-yet.db"), danglingLink,
	))

	// A relative dangling symlink resolves against the link's own directory.
	siblingRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(siblingRoot, ".agentsview"), 0o700,
	))
	relLinkDir := filepath.Join(siblingRoot, "lab")
	require.NoError(t, os.MkdirAll(relLinkDir, 0o700))
	relLink := filepath.Join(relLinkDir, "sessions.db")
	require.NoError(t, os.Symlink(
		filepath.Join("..", ".agentsview", "missing.db"), relLink,
	))

	// A symlink pointing at a harmless location is not the default archive.
	safeLink := filepath.Join(labDir, "safe-link.db")
	require.NoError(t, os.Symlink(labDB, safeLink))

	tests := []struct {
		name   string
		dbPath string
		want   bool
	}{
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"plain file in default dir", defaultDB, true},
		{"plain file outside default dir", labDB, false},
		{"symlink to existing default db", liveLink, true},
		{"absolute dangling symlink into default dir", danglingLink, true},
		{"relative dangling symlink into default dir", relLink, true},
		{"symlink to non-default db", safeLink, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsDefaultAgentsviewDBPath(tc.dbPath))
		})
	}
}

func TestPGConfigPushVectorsEnabled(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	tests := []struct {
		name string
		cfg  PGConfig
		want bool
	}{
		{"unset defaults to true", PGConfig{}, true},
		{"explicit false", PGConfig{PushVectors: boolPtr(false)}, false},
		{"explicit true", PGConfig{PushVectors: boolPtr(true)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.cfg.PushVectorsEnabled())
		})
	}
}

func TestPGConfig_LoadsFromTOML(t *testing.T) {
	f := newConfigFixture(t)
	f.WriteTOML(t, map[string]any{
		"pg": map[string]any{
			"url":          "postgres://localhost/test",
			"push_vectors": false,
		},
	})

	cfg := f.LoadMinimal(t)

	assert.False(t, cfg.PG.PushVectorsEnabled())
}

func TestPGConfig_PushVectorsDefaultsTrue(t *testing.T) {
	f := newConfigFixture(t)
	f.WriteTOML(t, map[string]any{
		"pg": map[string]any{
			"url": "postgres://localhost/test",
		},
	})

	cfg := f.LoadMinimal(t)

	assert.True(t, cfg.PG.PushVectorsEnabled())
}
