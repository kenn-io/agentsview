package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestPiHomesAddSessionRoots(t *testing.T) {
	for _, tt := range []struct {
		name     string
		env      map[string]string
		dirs     []string
		wantBase string
	}{
		{name: "defaults", wantBase: ".pi/agent/sessions"},
		{name: "cleared defaults", dirs: []string{}},
		{name: "explicit dirs", dirs: []string{"~/explicit"}, wantBase: "explicit"},
		{name: "native home", env: map[string]string{"PI_CODING_AGENT_DIR": "~/native"}, wantBase: "native/sessions"},
		{name: "native session dir", env: map[string]string{"PI_CODING_AGENT_SESSION_DIR": "~/native-sessions"}, wantBase: "native-sessions"},
		{name: "PI_DIR", env: map[string]string{"PI_DIR": "~/override"}, wantBase: "override"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			dir := setupTestEnv(t)
			home := canonicalTempDir(t)
			setTestHome(t, home)
			for _, name := range []string{"PI_DIR", "PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR"} {
				t.Setenv(name, tt.env[name])
			}
			entry := map[string]any{"homes": []string{"~/pi-work/agent", "~/pi-personal/agent", "~/pi-work/agent/./"}}
			if tt.dirs != nil {
				entry["dirs"] = tt.dirs
			}
			writeConfig(t, dir, map[string]any{"agents": map[string]any{"pi": entry}})
			cfg, err := LoadMinimal()
			require.NoError(err)
			wantRoots := []string{}
			if tt.wantBase != "" {
				wantRoots = append(wantRoots, filepath.Join(home, filepath.FromSlash(tt.wantBase)))
			}
			wantRoots = append(wantRoots,
				filepath.Join(home, "pi-work", "agent", "sessions"),
				filepath.Join(home, "pi-personal", "agent", "sessions"))
			assert.Equal(wantRoots, cfg.ResolveDirs(parser.AgentPi))
			assert.True(cfg.IsUserConfigured(parser.AgentPi))

			// Both configured homes contribute real sessions; duplicate home
			// spellings must not register or discover a second copy.
			workSession := filepath.Join(home, "pi-work", "agent", "sessions", "--project-a--", "work.jsonl")
			personalSession := filepath.Join(home, "pi-personal", "agent", "sessions", "--project-b--", "personal.jsonl")
			for _, path := range []string{workSession, personalSession} {
				require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(os.WriteFile(path, []byte(`{"type":"session","version":3,"cwd":"/project-a"}`+"\n"), 0o600))
			}
			provider, ok := parser.NewProvider(parser.AgentPi, parser.ProviderConfig{Roots: cfg.ResolveDirs(parser.AgentPi)})
			require.True(ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(err)
			require.Len(sources, 2)
			assert.ElementsMatch([]string{workSession, personalSession}, []string{sources[0].DisplayPath, sources[1].DisplayPath})
		})
	}
}

func TestAgentTableMigration(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup", true: "read only"}[readOnly], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			dir := setupTestEnv(t)
			home := canonicalTempDir(t)
			setTestHome(t, home)
			for _, key := range []string{"CLAUDE_PROJECTS_DIR", "CLAUDE_CONFIG_DIR", "CODEX_SESSIONS_DIR", "CODEX_HOME", "PI_DIR", "PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR", "GEMINI_DIR"} {
				t.Setenv(key, "")
			}
			path := filepath.Join(dir, "config.toml")
			original := `port = 9191
claude_project_dirs = []
claude_homes = ["~/claude-work"]
codex_sessions_dirs = ["~/codex-sessions"]
codex_homes = ["~/codex-work"]
pi_dirs = ["~/pi-sessions"]
gemini_dirs = ["~/gemini-a", "~/gemini-b"]
[agents.pi]
homes = ["~/pi-work/agent"]
[terminal]
mode = "auto"
`
			require.NoError(os.WriteFile(path, []byte(original), 0o600))
			if !readOnly && runtime.GOOS != "windows" {
				require.NoError(os.Chmod(path, 0o644))
			}
			load := LoadMinimal
			if readOnly {
				load = LoadReadOnly
			}
			cfg, err := load()
			require.NoError(err)
			assert.Equal([]string{filepath.Join(home, "claude-work", "projects")}, cfg.ResolveDirs(parser.AgentClaude))
			assert.Equal([]string{filepath.Join(home, "codex-sessions"), filepath.Join(home, "codex-work", "sessions"), filepath.Join(home, "codex-work", "archived_sessions")}, cfg.ResolveDirs(parser.AgentCodex))
			assert.Equal([]string{filepath.Join(home, "pi-sessions"), filepath.Join(home, "pi-work", "agent", "sessions")}, cfg.ResolveDirs(parser.AgentPi))
			assert.Equal([]string{filepath.Join(home, "gemini-a"), filepath.Join(home, "gemini-b")}, cfg.ResolveDirs(parser.AgentGemini))
			after, err := os.ReadFile(path)
			require.NoError(err)
			if readOnly {
				assert.Equal(original, string(after))
				return
			}
			if runtime.GOOS != "windows" {
				info, err := os.Stat(path)
				require.NoError(err)
				assert.Equal(os.FileMode(0o600), info.Mode().Perm())
			}
			var saved struct {
				Port     int                             `toml:"port"`
				Agents   map[string]AgentDirectoryConfig `toml:"agents"`
				Terminal TerminalConfig                  `toml:"terminal"`
			}
			_, err = toml.Decode(string(after), &saved)
			require.NoError(err)
			assert.Equal([]string{}, saved.Agents["claude"].Dirs)
			assert.Equal([]string{"~/claude-work"}, saved.Agents["claude"].Homes)
			assert.Equal([]string{"~/codex-work"}, saved.Agents["codex"].Homes)
			assert.Equal([]string{"~/pi-sessions"}, saved.Agents["pi"].Dirs)
			assert.Equal(9191, saved.Port)
			assert.Equal("auto", saved.Terminal.Mode)
			_, err = load()
			require.NoError(err)
			again, err := os.ReadFile(path)
			require.NoError(err)
			assert.Equal(string(after), string(again), "a second load must leave the migrated file unchanged")
		})
	}
}

func TestAgentTableMigrationConflictPreservesFile(t *testing.T) {
	require := require.New(t)

	dir := setupTestEnv(t)
	original := "codex_homes = [\"~/old\"]\n[agents.codex]\nhomes = [\"~/new\"]\n"
	path := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(path, []byte(original), 0o600))
	_, err := LoadMinimal()
	require.ErrorContains(err, "both codex_homes and agents.codex.homes")
	after, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(t, original, string(after))
}

func TestAgentTablesRejectInvalidProviders(t *testing.T) {
	for _, tt := range []struct {
		name string
		text string
		want string
	}{
		{name: "unknown provider", text: "[agents.unknown]\ndirs = []", want: `unknown session provider "unknown"`},
		{name: "import-only dirs", text: "[agents.chatgpt]\ndirs = [\"~/exports\"]", want: "agents.chatgpt.dirs: provider does not support configured directories"},
		{name: "unsupported homes", text: "[agents.gemini]\nhomes = [\"~/profile\"]", want: "agents.gemini.homes: provider does not support alternate homes"},
		{name: "agents is not a table", text: "agents = []", want: "agents: expected a TOML table"},
		{name: "provider is not a table", text: "[agents]\npi = []", want: "agents.pi: expected a TOML table"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestEnv(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tt.text), 0o600))
			_, err := LoadMinimal()
			require.ErrorContains(t, err, tt.want)
		})
	}
}
