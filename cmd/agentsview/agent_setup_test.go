package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/skills"
)

func TestAgentSetupReplacesExistingMCP(t *testing.T) {
	type serverConfig struct {
		Command string   `json:"command" toml:"command"`
		Args    []string `json:"args" toml:"args"`
	}
	for _, tc := range []struct {
		agent, config, content string
	}{
		{
			agent:   "claude",
			config:  ".claude.json",
			content: `{"mcpServers":{"agentsview":{"command":"custom-command","args":["serve"]}}}`,
		},
		{
			agent:   "codex",
			config:  ".codex/config.toml",
			content: "[mcp_servers.agentsview]\ncommand = 'custom-command'\nargs = ['serve']\n",
		},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			home := t.TempDir()
			setDoctorMemoryTestEnvironment(t, home)
			fakeAgent(t, tc.agent, "")
			path := filepath.Join(home, tc.config)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))

			out, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", tc.agent)
			require.Error(t, err, out)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(got))

			out, err = executeCommand(
				newRootCommand(), "agent", "setup", "--agent", tc.agent, "--force")
			require.NoError(t, err, out)
			got, err = os.ReadFile(path)
			require.NoError(t, err)
			var config struct {
				Servers map[string]serverConfig `json:"mcpServers" toml:"mcp_servers"`
			}
			if tc.agent == "claude" {
				require.NoError(t, json.Unmarshal(got, &config))
			} else {
				require.NoError(t, toml.Unmarshal(got, &config))
			}
			require.Equal(t, "agentsview", config.Servers["agentsview"].Command)
			require.Equal(t, []string{"mcp", "--profile", "memory"}, config.Servers["agentsview"].Args)
		})
	}
}

func TestAgentSetupFormatJSON(t *testing.T) {
	setDoctorMemoryTestEnvironment(t, t.TempDir())
	fakeAgent(t, "claude", "")
	out, _ := executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude", "--check", "--format", "json")
	var report struct {
		Rows []json.RawMessage `json:"rows"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report), out)
	require.NotEmpty(t, report.Rows)
}

func TestAgentSetupPreservesCodexOptions(t *testing.T) {
	home := t.TempDir()
	setDoctorMemoryTestEnvironment(t, home)
	fakeAgent(t, "codex", "")
	path := filepath.Join(home, ".codex", "config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	config := "[mcp_servers.agentsview]\ncommand = 'agentsview'\nargs = ['mcp', '--profile', 'memory']\nenabled_tools = ['get_memory_status']\n"
	require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
	out, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", "codex")
	require.Error(t, err, out)
	require.Contains(t, out, "--force")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, config, string(got))
}

func TestAgentSetupUpdatesOrProtectsSkillChanges(t *testing.T) {
	for _, tc := range []struct {
		name            string
		stale           bool
		modified        bool
		unreadableCodex bool
	}{
		{name: "stale", stale: true},
		{name: "modified", modified: true},
		{name: "unreadable codex artifact", unreadableCodex: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setDoctorMemoryTestEnvironment(t, home)
			fakeAgent(t, "claude", "")
			out, err := executeCommand(
				newRootCommand(), "skills", "install", "--harness", "claude")
			require.NoError(t, err, out)
			path := filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName)
			if tc.stale {
				require.NoError(t, os.WriteFile(path, []byte(staleClaudeContent()), 0o600))
			}
			if tc.modified {
				got, err := os.ReadFile(path)
				require.NoError(t, err)
				got = append(got, []byte("\ncustom edit\n")...)
				require.NoError(t, os.WriteFile(path, got, 0o600))
			}
			if tc.unreadableCodex {
				codexSkill := filepath.Join(skills.TargetDir(skills.HarnessAgents, home), skillFileName)
				require.NoError(t, os.MkdirAll(filepath.Dir(codexSkill), 0o700))
				require.NoError(t, os.Mkdir(codexSkill, 0o700))
			}

			out, err = executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
			if tc.modified {
				require.Error(t, err, out)
				got, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				require.Contains(t, string(got), "custom edit")
				out, err = executeCommand(
					newRootCommand(), "agent", "setup", "--agent", "claude", "--force")
			}
			require.NoError(t, err, out)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, freshClaudeSkill(t).Content, string(got))
		})
	}
}

func TestAgentSetupUsesSkillRemoteEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, server, tokenFile, want string
	}{
		{
			name:   "hosted server",
			server: "https://hosted.example",
			want:   "--server https://hosted.example",
		},
		{
			name:      "token file without server",
			tokenFile: "token",
			want:      "AGENTSVIEW_SKILLS_SERVER_TOKEN_FILE requires AGENTSVIEW_SKILLS_SERVER",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setDoctorMemoryTestEnvironment(t, home)
			t.Setenv("AGENTSVIEW_SKILLS_SERVER", tc.server)
			t.Setenv("AGENTSVIEW_SKILLS_SERVER_TOKEN_FILE", tc.tokenFile)
			fakeAgent(t, "claude", "")

			out, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
			if tc.tokenFile != "" {
				require.Error(t, err, out)
				require.Contains(t, err.Error(), tc.want)
				return
			}
			require.NoError(t, err, out)
			path := filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Contains(t, string(got), tc.want)
		})
	}
}

func TestSkillsInstallMCPPointer(t *testing.T) {
	for _, tc := range []struct {
		name, config          string
		project, client, hint bool
	}{
		{"missing client entry", "", true, true, true},
		{"malformed config", "invalid", false, true, false},
		{"undetected client", `{"mcpServers":{"agentsview":{"command":"agentsview","args":["mcp"]}}}`, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setDoctorMemoryTestEnvironment(t, home)
			if tc.client {
				fakeAgent(t, "claude", "")
			}
			if tc.config != "" {
				require.NoError(t, os.WriteFile(filepath.Join(home, ".claude.json"), []byte(tc.config), 0o600))
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
