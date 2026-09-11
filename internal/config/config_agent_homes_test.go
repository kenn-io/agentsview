package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestAgentHomeDirs(t *testing.T) {
	claude, ok := parser.AgentByType(parser.AgentClaude)
	require.True(t, ok)
	codex, ok := parser.AgentByType(parser.AgentCodex)
	require.True(t, ok)

	assert.Equal(t, []string{filepath.Join("/homes/a", "projects")},
		AgentHomeDirs(claude, "/homes/a"))
	assert.Equal(t, []string{
		filepath.Join("/homes/b", "sessions"),
		filepath.Join("/homes/b", "archived_sessions"),
	}, AgentHomeDirs(codex, "/homes/b"))
	assert.Nil(t, AgentHomeDirs(parser.AgentDef{Type: "none"}, "/homes/c"))
}

func TestLoadFileAgentHomesAreAdditiveToDefaults(t *testing.T) {
	f := newConfigFixture(t)
	home := canonicalTempDir(t)
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	f.WriteConfigText(t, `
[agents.claude]
homes = ["/homes/work/.claude"]

[agents.codex]
homes = ["/homes/work/.codex", "/homes/other/.codex-alt"]
`)

	cfg := f.LoadMinimal(t)

	assert.Equal(t, []string{
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(absoluteTestPath(t, "/homes/work/.claude"), "projects"),
	}, cfg.ResolveDirs(parser.AgentClaude))
	assert.Equal(t, []string{
		filepath.Join(home, ".codex", "sessions"),
		filepath.Join(home, ".codex", "archived_sessions"),
		filepath.Join(absoluteTestPath(t, "/homes/work/.codex"), "sessions"),
		filepath.Join(absoluteTestPath(t, "/homes/work/.codex"), "archived_sessions"),
		filepath.Join(absoluteTestPath(t, "/homes/other/.codex-alt"), "sessions"),
		filepath.Join(absoluteTestPath(t, "/homes/other/.codex-alt"), "archived_sessions"),
	}, cfg.ResolveDirs(parser.AgentCodex))
	assert.True(t, cfg.IsUserConfigured(parser.AgentClaude))
	assert.True(t, cfg.IsUserConfigured(parser.AgentCodex))
	assert.Equal(t, cfg.InstallationID,
		cfg.SourceMachines[parser.AgentCodex][filepath.Join(absoluteTestPath(t, "/homes/work/.codex"), "sessions")])
}

func TestLoadFileAgentHomesExpandTildeAndDeduplicate(t *testing.T) {
	f := newConfigFixture(t)
	home := canonicalTempDir(t)
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	f.WriteConfigText(t, `
[[session_sources]]
agent = "codex"
dir = "/explicit/archived_sessions"
machine = "buildbox"

[agents.codex]
dirs = ["/explicit/sessions"]
homes = ["~/.codex", "/explicit", "/explicit/./"]
`)

	cfg := f.LoadMinimal(t)

	assert.Equal(t, []string{
		absoluteTestPath(t, "/explicit/sessions"),
		filepath.Join(home, ".codex", "sessions"),
		filepath.Join(home, ".codex", "archived_sessions"),
		filepath.Join(absoluteTestPath(t, "/explicit"), "archived_sessions"),
	}, cfg.ResolveDirs(parser.AgentCodex))
	assert.Equal(t, "buildbox",
		cfg.SourceMachines[parser.AgentCodex][filepath.Join(absoluteTestPath(t, "/explicit"), "archived_sessions")])
}

func TestLoadFileAgentHomesWithClearedDefaults(t *testing.T) {
	f := newConfigFixture(t)
	setTestHome(t, canonicalTempDir(t))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	f.WriteConfigText(t, `
[agents.claude]
dirs = []
homes = ["/homes/only"]
`)

	cfg := f.LoadMinimal(t)

	assert.Equal(t, []string{filepath.Join(absoluteTestPath(t, "/homes/only"), "projects")},
		cfg.ResolveDirs(parser.AgentClaude))
}

func TestLoadFileAgentHomesRemainAdditiveToEnvDirs(t *testing.T) {
	f := newConfigFixture(t)
	setTestHome(t, canonicalTempDir(t))
	t.Setenv("CODEX_SESSIONS_DIR", absoluteTestPath(t, "/env/codex"))
	f.WriteConfigText(t, `
[agents.codex]
homes = ["/homes/work/.codex"]
`)

	cfg := f.LoadMinimal(t)

	assert.Equal(t, []string{
		absoluteTestPath(t, "/env/codex"),
		filepath.Join(absoluteTestPath(t, "/homes/work/.codex"), "sessions"),
		filepath.Join(absoluteTestPath(t, "/homes/work/.codex"), "archived_sessions"),
	}, cfg.ResolveDirs(parser.AgentCodex))
}

func TestLoadFileAgentHomesValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "s3 home",
			config: `
[agents.codex]
homes = ["/ok", "s3://bucket/codex"]
`,
			wantErr: "agents.codex.homes: entry 2: home \"s3://bucket/codex\" is an S3 root",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newConfigFixture(t)
			setTestHome(t, canonicalTempDir(t))
			f.WriteConfigText(t, tt.config)

			_, err := LoadMinimal()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestResolveDirs_CodexHomeRootEnvVar(t *testing.T) {
	dir := setupTestEnv(t)
	setTestHome(t, canonicalTempDir(t))
	root := canonicalTempDir(t)
	t.Setenv("CODEX_HOME", root)
	writeConfig(t, dir, map[string]any{})

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	assert.Equal(t, []string{
		filepath.Join(root, "sessions"),
		filepath.Join(root, "archived_sessions"),
	}, cfg.ResolveDirs(parser.AgentCodex))
	assert.False(t, cfg.IsUserConfigured(parser.AgentCodex))
}

func TestLoadFileAgentHomesDeduplicateSymlinkedRoots(t *testing.T) {
	f := newConfigFixture(t)
	home := canonicalTempDir(t)
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	primary := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(filepath.Join(primary, "sessions"), 0o755))
	alt := filepath.Join(home, ".codex-alt")
	require.NoError(t, os.MkdirAll(alt, 0o755))
	require.NoError(t, os.Symlink(
		filepath.Join(primary, "sessions"), filepath.Join(alt, "sessions")))
	f.WriteConfigText(t, "[agents.codex]\nhomes = [\"~/.codex-alt\"]\n")

	cfg := f.LoadMinimal(t)

	assert.Equal(t, []string{
		filepath.Join(primary, "sessions"),
		filepath.Join(primary, "archived_sessions"),
		filepath.Join(alt, "archived_sessions"),
	}, cfg.ResolveDirs(parser.AgentCodex))
	// Shared sessions read both homes. Each separate archive keeps its own metadata.
	assert.Equal(t, map[string][]string{
		filepath.Join(primary, "sessions"):          {primary, alt},
		filepath.Join(primary, "archived_sessions"): {primary},
		filepath.Join(alt, "archived_sessions"):     {alt},
	}, cfg.ProviderMetadata[parser.AgentCodex])
}

func TestSaveSettingsPersistsAgentHomes(t *testing.T) {
	dir := setupTestEnv(t)
	cfg, err := Default()
	require.NoError(t, err)
	cfg.DataDir = dir

	require.NoError(t, cfg.SaveSettings(map[string]any{
		"agent_homes": map[parser.AgentType][]string{
			parser.AgentCodex:  {" ~/.codex-work ", "~/.codex-work", absoluteTestPath(t, "/srv/codex")},
			parser.AgentClaude: {"~/.claude-work"},
		},
	}))
	assert.Equal(t, []string{"~/.codex-work", absoluteTestPath(t, "/srv/codex")},
		cfg.ConfiguredAgentHomes(parser.AgentCodex))
	assert.Equal(t, []string{"~/.claude-work"},
		cfg.ConfiguredAgentHomes(parser.AgentClaude))

	reloaded, err := LoadMinimal()
	require.NoError(t, err)
	assert.Equal(t, []string{"~/.codex-work", absoluteTestPath(t, "/srv/codex")},
		reloaded.ConfiguredAgentHomes(parser.AgentCodex))
	assert.Contains(t, reloaded.ResolveDirs(parser.AgentCodex),
		filepath.Join(absoluteTestPath(t, "/srv/codex"), "sessions"))

	require.NoError(t, cfg.SaveSettings(map[string]any{
		"agent_homes": map[parser.AgentType][]string{parser.AgentCodex: {}},
	}))
	assert.Nil(t, cfg.ConfiguredAgentHomes(parser.AgentCodex))
	reloaded, err = LoadMinimal()
	require.NoError(t, err)
	assert.Nil(t, reloaded.ConfiguredAgentHomes(parser.AgentCodex))
	assert.Equal(t, []string{"~/.claude-work"},
		reloaded.ConfiguredAgentHomes(parser.AgentClaude))
}

func TestNormalizeAgentHomesRejectsUnsupportedInput(t *testing.T) {
	tests := []struct {
		name    string
		input   map[string][]string
		wantErr string
	}{
		{
			name:    "unknown agent",
			input:   map[string][]string{"nope": {"/x"}},
			wantErr: `unknown session provider "nope"`,
		},
		{
			name:    "agent without home support",
			input:   map[string][]string{"gemini": {"/x"}},
			wantErr: `"gemini" does not support alternate homes`,
		},
		{
			name:    "empty home",
			input:   map[string][]string{"codex": {"/x", " "}},
			wantErr: "agents.codex.homes: entry 2: home is required",
		},
		{
			name:    "s3 home",
			input:   map[string][]string{"claude": {"s3://bucket/claude"}},
			wantErr: "is an S3 root",
		},
		{
			name:    "aliased provider keys",
			input:   map[string][]string{"codex": {"/a"}, " CODEX ": {"/b"}},
			wantErr: `session provider "codex" is listed more than once`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeAgentHomes(tt.input)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestLoadFileAgentHomesDropRepeatedSpellings(t *testing.T) {
	f := newConfigFixture(t)
	setTestHome(t, canonicalTempDir(t))
	t.Setenv("CODEX_HOME", "")
	f.WriteConfigText(t, `
[agents.codex]
homes = ["/homes/a", " /homes/a ", "", "/homes/b"]
`)

	cfg := f.LoadMinimal(t)

	assert.Equal(t, []string{"/homes/a", "/homes/b"},
		cfg.ConfiguredAgentHomes(parser.AgentCodex))
}

func TestRuntimeRootsResolveBeforeDeduplication(t *testing.T) {
	for _, createTarget := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing target", true: "existing target"}[createTarget], func(t *testing.T) {
			base := canonicalTempDir(t)
			t.Chdir(base)
			require.NoError(t, os.Mkdir("profile", 0o755))
			for _, name := range []string{"sessions", "archived_sessions"} {
				require.NoError(t, os.Symlink(filepath.Join("..", "primary", name), filepath.Join("profile", name)))
				if createTarget {
					require.NoError(t, os.MkdirAll(filepath.Join("primary", name), 0o755))
				}
			}
			cfg := Config{
				InstallationID: "host-a",
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentCodex: {"profile/sessions", "primary/sessions"},
				},
				agentHomes:           map[parser.AgentType][]string{parser.AgentCodex: {"profile"}},
				agentDirSource:       make(map[parser.AgentType]dirSource),
				sessionSourceConfigs: []sessionSourceConfig{{Agent: "codex", Dir: "profile/sessions", Machine: new("host-b")}},
			}
			require.NoError(t, cfg.resolveSessionSources())
			sessions := filepath.Join(base, "primary", "sessions")
			archive := filepath.Join(base, "primary", "archived_sessions")
			assert.Equal(t, []string{sessions, archive}, cfg.ResolveDirs(parser.AgentCodex))
			assert.Equal(t, "host-b", cfg.SourceMachines[parser.AgentCodex][sessions])
			require.Len(t, cfg.SessionSources, 1)
			assert.Equal(t, sessions, cfg.SessionSources[0].Dir)
			assert.Contains(t, cfg.ProviderMetadata[parser.AgentCodex][sessions], filepath.Join(base, "profile"))
		})
	}
}

func TestCLILoadPreservesCodexHomeMetadata(t *testing.T) {
	for _, loader := range []struct {
		name string
		load func(*testing.T, ...string) (Config, error)
	}{
		{"flags", loadConfigFromFlags},
		{"pflags", loadConfigFromPFlags},
	} {
		t.Run(loader.name, func(t *testing.T) {
			f := newConfigFixture(t)
			f.WriteConfigText(t, "")
			base := canonicalTempDir(t)
			home := filepath.Join(base, "profile")
			root := filepath.Join(base, "primary", "sessions")
			require.NoError(t, os.MkdirAll(root, 0o755))
			require.NoError(t, os.MkdirAll(home, 0o755))
			alias := filepath.Join(home, "sessions")
			require.NoError(t, os.Symlink(root, alias))
			t.Setenv("CODEX_HOME", home)
			t.Setenv("CODEX_SESSIONS_DIR", "")

			cfg, err := loader.load(t)
			require.NoError(t, err)
			assert.Contains(t, cfg.ResolveDirs(parser.AgentCodex), root)
			assert.Contains(t, cfg.ProviderMetadata[parser.AgentCodex][root], home,
				"CLI loading must retain the configured home's title and history location")
		})
	}
}
