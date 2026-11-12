package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/skills"
)

func TestAgentSetupReplacesExistingMCP(t *testing.T) {
	for _, tc := range []struct{ agent, config, content string }{
		{"claude", ".claude.json", `{"mcpServers":{"agentsview":{"command":"agentsview","args":["mcp"]},"other":{"command":"other"}}}`},
		{"codex", ".codex/config.toml", "[mcp_servers.agentsview]\ncommand = 'agentsview'\nargs = ['mcp']\n[mcp_servers.other]\ncommand = 'other'\n"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			home := t.TempDir()
			setDoctorMemoryTestEnvironment(t, home)
			fakeAgent(t, tc.agent, "")
			path := filepath.Join(home, tc.config)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0600))
			out, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", tc.agent)
			require.NoError(t, err, out)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			var entries map[string]struct {
				Command string   `json:"command" toml:"command"`
				Args    []string `json:"args" toml:"args"`
			}
			if tc.agent == "claude" {
				var config struct {
					Servers map[string]struct {
						Command string   `json:"command" toml:"command"`
						Args    []string `json:"args" toml:"args"`
					} `json:"mcpServers"`
				}
				require.NoError(t, json.Unmarshal(got, &config))
				entries = config.Servers
			} else {
				var config struct {
					Servers map[string]struct {
						Command string   `json:"command" toml:"command"`
						Args    []string `json:"args" toml:"args"`
					} `toml:"mcp_servers"`
				}
				require.NoError(t, toml.Unmarshal(got, &config))
				entries = config.Servers
			}
			require.Equal(t, "agentsview", entries["agentsview"].Command)
			require.Equal(t, []string{"mcp", "--profile", "memory"}, entries["agentsview"].Args)
			require.Equal(t, "other", entries["other"].Command)
		})
	}
}

func TestAgentSetupDoesNotRewriteCurrentSkills(t *testing.T) {
	home := t.TempDir()
	setDoctorMemoryTestEnvironment(t, home)
	fakeAgent(t, "claude", "")
	config := filepath.Join(home, ".claude.json")
	original := `{ "mcpServers": { "agentsview": {"type":"stdio", "command":"agentsview", "args":["mcp","--profile","memory"]} } }`
	require.NoError(t, os.WriteFile(config, []byte(original), 0600))
	out, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	require.NoError(t, err, out)
	path := filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName)
	old := time.Unix(1234567890, 0)
	require.NoError(t, os.Chtimes(path, old, old))
	out, err = executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	require.NoError(t, err, out)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, old, info.ModTime())
	got, err := os.ReadFile(config)
	require.NoError(t, err)
	require.Equal(t, original, string(got))
}

func TestAgentSetupOfflineMCPInstallsMissingSkill(t *testing.T) {
	home := t.TempDir()
	setDoctorMemoryTestEnvironment(t, home)
	fakeAgent(t, "claude", "Scope: User config\\nStatus: Failed to connect\\n")
	out, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	require.Error(t, err, out)
	_, err = os.Stat(filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName))
	require.NoError(t, err)
}

func TestAgentSetupPreservesRemoteAndProtectsEdits(t *testing.T) {
	home := t.TempDir()
	setDoctorMemoryTestEnvironment(t, home)
	fakeAgent(t, "claude", "")
	out, err := executeCommand(newRootCommand(), "skills", "install", "--harness", "claude", "--server", "https://hosted.example")
	require.NoError(t, err, out)
	path := filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName)
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	out, err = executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	require.NoError(t, err, out)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, got)
	edited := append(got, []byte("\ncustom edit\n")...)
	require.NoError(t, os.WriteFile(path, edited, 0600))
	out, err = executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	require.Error(t, err, out)
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, edited, got)
	out, err = executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude", "--force")
	require.NoError(t, err, out)
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, got)
}

func TestAgentSetupUpdatesStaleSkill(t *testing.T) {
	home := t.TempDir()
	setDoctorMemoryTestEnvironment(t, home)
	fakeAgent(t, "claude", "")
	out, err := executeCommand(newRootCommand(), "skills", "install", "--harness", "claude")
	require.NoError(t, err, out)
	path := filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName)
	require.NoError(t, os.WriteFile(path, []byte(staleClaudeContent()), 0600))
	out, err = executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	require.NoError(t, err, out)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, freshClaudeSkill(t).Content, string(got))
}

func TestSkillsInstallMCPPointer(t *testing.T) {
	for _, tc := range []struct {
		name, config  string
		project, hint bool
	}{
		{"missing", "", true, true},
		{"outdated", `{"mcpServers":{"agentsview":{"command":"agentsview","args":["mcp"]}}}`, false, true},
		{"malformed", "invalid", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setDoctorMemoryTestEnvironment(t, home)
			if tc.config != "" {
				require.NoError(t, os.WriteFile(filepath.Join(home, ".claude.json"), []byte(tc.config), 0600))
			}
			args := []string{"skills", "install", "--harness", "claude"}
			if tc.project {
				t.Chdir(t.TempDir())
				args = append(args, "--project")
			}
			out, err := executeCommand(newRootCommand(), args...)
			require.NoError(t, err, out)
			if tc.hint {
				require.Contains(t, out, "agentsview agent setup")
			} else {
				require.NotContains(t, out, "agentsview agent setup")
			}
		})
	}
}
