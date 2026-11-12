package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/skills"
)

func TestAgentSetupProtectsExistingMCP(t *testing.T) {
	for _, tc := range []struct {
		agent, config, content string
		harness                skills.Harness
	}{
		{"claude", ".claude.json", `{"mcpServers":{"agentsview":{"type":"http","url":"https://old.example/mcp","headers":{"Authorization":"secret"}}}}`, skills.HarnessClaude},
		{"codex", ".codex/config.toml", "[mcp_servers.agentsview]\ncommand = 'remote'\nargs = ['--token', 'secret']\n", skills.HarnessAgents},
		{"claude", ".claude.json", `{"mcpServers":{"agentsview":{"type":"stdio","command":"agentsview","args":["mcp","--profile","memory"],"env":{"TOKEN":"secret"}}}}`, skills.HarnessClaude},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			home := t.TempDir()
			setTestHome(t, home)
			t.Setenv("PATH", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("CODEX_HOME", "")
			path := filepath.Join(home, tc.config)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0600))
			out, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", tc.agent)
			require.Error(t, err)
			require.Contains(t, out, "--force")
			require.NotContains(t, out, "secret")
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(got))
			_, err = os.Stat(filepath.Join(skills.TargetDir(tc.harness, home), skillFileName))
			require.ErrorIs(t, err, os.ErrNotExist)
			_, _ = executeCommand(newRootCommand(), "agent", "setup", "--agent", tc.agent, "--force", "--server", "https://new.example", "--server-token-file", "/token")
			got, err = os.ReadFile(path)
			require.NoError(t, err)
			require.Contains(t, string(got), "memory")
			require.Contains(t, string(got), "https://new.example")
			require.NotContains(t, string(got), "secret")
			body, err := os.ReadFile(filepath.Join(skills.TargetDir(tc.harness, home), skillFileName))
			require.NoError(t, err)
			require.Contains(t, string(body), "https://new.example")
		})
	}
}

func TestAgentSetupPreservesCanonicalMCP(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf 'Scope: User config\\nStatus: Connected\\n'\n"), 0755))
	path := filepath.Join(home, ".claude.json")
	original := `{ "mcpServers": { "agentsview": {"type":"stdio", "command":"agentsview", "args":["mcp","--profile","memory"]} } }`
	require.NoError(t, os.WriteFile(path, []byte(original), 0600))
	out, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	require.NoError(t, err, out)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, string(got))
	_, err = os.Stat(filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName))
	require.NoError(t, err)
	out, err = executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	require.NoError(t, err, out)
	require.False(t, strings.Contains(out, "failed"))
}

func TestAgentSetupRetriesAfterMCPWriteFailure(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf 'Scope: User config\\nStatus: Connected\\n'\n"), 0755))
	path := filepath.Join(home, ".claude.json")
	if err := os.Symlink(filepath.Join(home, "missing", "config"), path); err != nil {
		t.Skip(err)
	}
	out, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	require.Error(t, err)
	require.Contains(t, out, "Unable to install MCP")
	_, err = os.Stat(filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName))
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	out, err = executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	require.NoError(t, err, out)
	_, err = os.Stat(path)
	require.NoError(t, err)
}

func TestAgentSetupUsesHostedTarget(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("AGENTSVIEW_MEMORY_SERVER", "https://hosted.example")
	t.Setenv("AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE", "/hosted-token")
	_, _ = executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	config, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	require.NoError(t, err)
	require.Contains(t, string(config), "https://hosted.example")
	body, err := os.ReadFile(filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName))
	require.NoError(t, err)
	require.Contains(t, string(body), "https://hosted.example")
}

func TestAgentSetupBothClientsAndDoctor(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf 'Scope: User config\\nStatus: Connected\\n'\n"), 0755))
	codex := "#!/bin/sh\nread line\nprintf '{\"id\":1,\"result\":{}}\\n'\nread line\nread line\nprintf '{\"id\":2,\"result\":{\"data\":[{\"name\":\"agentsview\",\"tools\":{\"search_content\":{}}}]}}\\n'\nwhile read line; do :; done\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "codex"), []byte(codex), 0755))
	out, err := executeCommand(newRootCommand(), "agent", "setup")
	require.NoError(t, err, out)
	client := inspectDoctorAgentSetup(t.Context(), doctorMemoryClientStatus{}, doctorMemoryTargetSelection{Kind: "local"})
	require.Len(t, client.Rows, 4)
	for _, row := range client.Rows {
		require.EqualValues(t, "ok", row.State, "%s %s", row.Agent, row.Piece)
	}
	require.NoError(t, os.Remove(filepath.Join(home, ".codex", "config.toml")))
	client = inspectDoctorAgentSetup(t.Context(), doctorMemoryClientStatus{}, doctorMemoryTargetSelection{Kind: "local"})
	require.EqualValues(t, "missing", client.Rows[3].State)
	require.Equal(t, "agentsview agent setup", client.Rows[3].NextStep)
}

func TestAgentSetupConflictRedactsJSON(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", t.TempDir())
	original := `{"mcpServers":{"agentsview":{"type":"http","url":"https://private.example/mcp","headers":{"Authorization":"private-token"}}}}`
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude.json"), []byte(original), 0600))
	out, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude", "--json")
	require.Error(t, err)
	require.NotContains(t, out, "private.example")
	require.NotContains(t, out, "private-token")
	require.NotContains(t, out, home)
}

func TestSkillsInstallIgnoresMalformedMCP(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude.json"), []byte("invalid"), 0600))
	out, err := executeCommand(newRootCommand(), "skills", "install", "--harness", "claude")
	require.NoError(t, err, out)
	_, err = os.Stat(filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName))
	require.NoError(t, err)
}

func TestAgentSetupDoesNotRewriteCurrentSkills(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf 'Scope: User config\\nStatus: Connected\\n'\n"), 0755))
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
}

func TestAgentSetupOfflineMCPInstallsMissingSkill(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	script := "#!/bin/sh\nprintf 'Scope: User config\\nStatus: Failed to connect\\nIssue: dial tcp private.example:123: connection refused private-token\\n'\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0755))
	original := `{ "mcpServers": { "agentsview": {"type":"stdio", "command":"agentsview", "args":["mcp","--profile","memory"]} } }`
	path := filepath.Join(home, ".claude.json")
	require.NoError(t, os.WriteFile(path, []byte(original), 0600))
	out, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName))
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, string(got))
	require.Contains(t, strings.ToLower(out), "connection refused")
	require.NotContains(t, out, "private.example")
	require.NotContains(t, out, "private-token")
}

func TestAgentSetupRejectsOrphanMemoryToken(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("AGENTSVIEW_MEMORY_SERVER", "")
	t.Setenv("AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE", "/private-token")
	_, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude")
	require.ErrorContains(t, err, "requires")
	_, err = os.Stat(filepath.Join(skills.TargetDir(skills.HarnessClaude, home), skillFileName))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestSkillsProjectInstallPointsToMCPSetup(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", t.TempDir())
	t.Chdir(t.TempDir())
	out, err := executeCommand(newRootCommand(), "skills", "install", "--harness", "claude", "--project")
	require.NoError(t, err, out)
	require.Contains(t, out, "agentsview agent setup")
}

func TestAgentSetupReportsConnectionRefused(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	script := "#!/bin/sh\nprintf 'Scope: User config\\nStatus: Failed to connect\\nIssue: dial tcp private.example:123: connection refused private-token\\n'\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0755))
	original := `{ "mcpServers": { "agentsview": {"type":"stdio", "command":"agentsview", "args":["mcp","--profile","memory"]} } }`
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude.json"), []byte(original), 0600))
	out, err := executeCommand(newRootCommand(), "agent", "setup", "--agent", "claude", "--check", "--json")
	require.Error(t, err)
	require.Contains(t, strings.ToLower(out), "connection refused")
	require.NotContains(t, out, "private.example")
	require.NotContains(t, out, "private-token")
}
