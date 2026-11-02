package main

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func runAgentsCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"source", "list"}, args...))
	err := root.Execute()
	return out.String(), err
}

type agentsJSONResult struct {
	SchemaVersion int `json:"schema_version"`
	Agents        []struct {
		Type        string            `json:"type"`
		DisplayName string            `json:"display_name"`
		ImportOnly  bool              `json:"import_only"`
		DefaultDirs []string          `json:"default_dirs"`
		EnvVar      string            `json:"env_var"`
		Content     map[string]string `json:"content"`
	} `json:"agents"`
}

func TestAgentsCommandJSONListsEveryRegisteredAgent(t *testing.T) {
	out, err := runAgentsCommand(t, "--json")
	require.NoError(t, err)

	var got agentsJSONResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, 1, got.SchemaVersion)
	require.Len(t, got.Agents, len(parser.Registry))

	for i, def := range parser.Registry {
		agent := got.Agents[i]
		assert.Equal(t, string(def.Type), agent.Type)
		assert.Equal(t, def.DisplayName, agent.DisplayName)
		assert.Len(t, agent.Content, 17, "%s content capabilities", agent.Type)
		for name, support := range agent.Content {
			assert.Contains(t, []string{"supported", "unsupported", "not_applicable"},
				support, "%s %s", agent.Type, name)
		}
	}
}

func TestAgentsCommandJSONReportsAgentDetails(t *testing.T) {
	out, err := runAgentsCommand(t, "--format", "json", "codex", "claude", "claude-ai")
	require.NoError(t, err)

	var got agentsJSONResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Agents, 3)

	codex, claude, claudeAI := got.Agents[0], got.Agents[1], got.Agents[2]
	assert.Equal(t, "codex", codex.Type)
	assert.Equal(t, "supported", codex.Content["subagents"])
	assert.Equal(t, "supported", codex.Content["per_message_token_usage"])

	assert.Equal(t, "claude", claude.Type)
	assert.Equal(t, []string{".claude/projects"}, claude.DefaultDirs)
	assert.Equal(t, "CLAUDE_PROJECTS_DIR", claude.EnvVar)
	assert.Equal(t, "supported", claude.Content["tool_calls"])
	assert.False(t, claude.ImportOnly)

	assert.True(t, claudeAI.ImportOnly)
	assert.Equal(t, "supported", claudeAI.Content["first_message"])
	assert.Equal(t, "supported", claudeAI.Content["thinking"])
	assert.Equal(t, "unsupported", claudeAI.Content["tool_calls"])
	assert.Equal(t, "not_applicable", claudeAI.Content["cwd"])
}

func TestAgentsCommandHumanOutput(t *testing.T) {
	out, err := runAgentsCommand(t, "claude", "claude-ai")
	require.NoError(t, err)

	assert.Contains(t, out, "claude (Claude Code)\n")
	assert.Contains(t, out, "  sessions: ~/.claude/projects\n")
	assert.Contains(t, out, "  override: CLAUDE_PROJECTS_DIR\n")
	assert.Regexp(t, `(?m)^  reads: first_message, .*tool_calls`, out)
	assert.Contains(t, out, "sessions: imported with `agentsview source import`")
}

func TestAgentsCommandRejectsUnknownAgent(t *testing.T) {
	_, err := runAgentsCommand(t, "claude", "not-an-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown agent "not-an-agent"`)
}
