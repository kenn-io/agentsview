package config

import (
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
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	f.WriteConfigText(t, `
claude_homes = ["/homes/work/.claude"]
codex_homes = ["/homes/work/.codex", "/homes/other/.codex-alt"]
`)

	cfg := f.LoadMinimal(t)

	assert.Equal(t, []string{
		filepath.Join(home, ".claude", "projects"),
		filepath.Join("/homes/work/.claude", "projects"),
	}, cfg.ResolveDirs(parser.AgentClaude))
	assert.Equal(t, []string{
		filepath.Join(home, ".codex", "sessions"),
		filepath.Join(home, ".codex", "archived_sessions"),
		filepath.Join("/homes/work/.codex", "sessions"),
		filepath.Join("/homes/work/.codex", "archived_sessions"),
		filepath.Join("/homes/other/.codex-alt", "sessions"),
		filepath.Join("/homes/other/.codex-alt", "archived_sessions"),
	}, cfg.ResolveDirs(parser.AgentCodex))
	assert.True(t, cfg.IsUserConfigured(parser.AgentClaude))
	assert.True(t, cfg.IsUserConfigured(parser.AgentCodex))
	assert.Equal(t, cfg.LocalMachineName,
		cfg.SourceMachines[parser.AgentCodex][filepath.Join("/homes/work/.codex", "sessions")])
}

func TestLoadFileAgentHomesExpandTildeAndDeduplicate(t *testing.T) {
	f := newConfigFixture(t)
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	f.WriteConfigText(t, `
codex_sessions_dirs = ["/explicit/sessions"]
codex_homes = ["~/.codex", "/explicit", "/explicit/./"]

[[session_sources]]
agent = "codex"
dir = "/explicit/archived_sessions"
machine = "buildbox"
`)

	cfg := f.LoadMinimal(t)

	assert.Equal(t, []string{
		"/explicit/sessions",
		filepath.Join(home, ".codex", "sessions"),
		filepath.Join(home, ".codex", "archived_sessions"),
		filepath.Join("/explicit", "archived_sessions"),
	}, cfg.ResolveDirs(parser.AgentCodex))
	assert.Equal(t, "buildbox",
		cfg.SourceMachines[parser.AgentCodex][filepath.Join("/explicit", "archived_sessions")])
}

func TestLoadFileAgentHomesWithClearedDefaults(t *testing.T) {
	f := newConfigFixture(t)
	setTestHome(t, t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	f.WriteConfigText(t, `
claude_project_dirs = []
claude_homes = ["/homes/only"]
`)

	cfg := f.LoadMinimal(t)

	assert.Equal(t, []string{filepath.Join("/homes/only", "projects")},
		cfg.ResolveDirs(parser.AgentClaude))
}

func TestLoadFileAgentHomesRemainAdditiveToEnvDirs(t *testing.T) {
	f := newConfigFixture(t)
	setTestHome(t, t.TempDir())
	t.Setenv("CODEX_SESSIONS_DIR", "/env/codex")
	f.WriteConfigText(t, `
codex_homes = ["/homes/work/.codex"]
`)

	cfg := f.LoadMinimal(t)

	assert.Equal(t, []string{
		"/env/codex",
		filepath.Join("/homes/work/.codex", "sessions"),
		filepath.Join("/homes/work/.codex", "archived_sessions"),
	}, cfg.ResolveDirs(parser.AgentCodex))
}

func TestLoadFileAgentHomesValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name:    "empty home",
			config:  `claude_homes = [" "]`,
			wantErr: "agent homes: claude_homes: entry 1: home is required",
		},
		{
			name:    "s3 home",
			config:  `codex_homes = ["/ok", "s3://bucket/codex"]`,
			wantErr: "codex_homes: entry 2: home \"s3://bucket/codex\" is an S3 root",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newConfigFixture(t)
			setTestHome(t, t.TempDir())
			f.WriteConfigText(t, tt.config)

			_, err := LoadMinimal()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestResolveDirs_CodexHomeRootEnvVar(t *testing.T) {
	dir := setupTestEnv(t)
	setTestHome(t, t.TempDir())
	root := t.TempDir()
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
