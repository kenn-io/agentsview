package skills

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func renderedPaths(pkg []Rendered) []string {
	paths := make([]string, 0, len(pkg))
	for _, artifact := range pkg {
		paths = append(paths, filepath.ToSlash(artifact.RelativePath))
	}
	return paths
}

func artifactByBase(t *testing.T, pkg []Rendered, base string) Rendered {
	t.Helper()
	for _, artifact := range pkg {
		if filepath.Base(artifact.RelativePath) == base {
			return artifact
		}
	}
	require.FailNow(t, "no artifact named "+base+" in "+strings.Join(renderedPaths(pkg), ", "))
	return Rendered{}
}

func TestRenderPackage_HarnessArtifacts(t *testing.T) {
	claude, err := RenderPackage(HarnessClaude, "dev", Remote{})
	require.NoError(t, err)
	assert.Equal(t, []string{
		".claude/skills/agentsview-finding-history/SKILL.md",
		".claude/skills/agentsview-finding-history/LICENSE",
		".claude/agents/agentsview-search-conversations.md",
	}, renderedPaths(claude))

	agents, err := RenderPackage(HarnessAgents, "dev", Remote{})
	require.NoError(t, err)
	assert.Equal(t, []string{
		".agents/skills/agentsview-finding-history/SKILL.md",
		".agents/skills/agentsview-finding-history/LICENSE",
	}, renderedPaths(agents))
}

func TestRenderPluginPackageReusesClaudeArtifacts(t *testing.T) {
	standalone, err := RenderPackage(HarnessClaude, "0.1.0", Remote{})
	require.NoError(t, err)

	plugin, err := RenderPluginPackage("0.1.0")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"skills/agentsview-finding-history/SKILL.md",
		"skills/agentsview-finding-history/LICENSE",
		"agents/agentsview-search-conversations.md",
	}, renderedPaths(plugin))
	for _, artifact := range plugin {
		assert.Equal(t, artifactByBase(t, standalone,
			filepath.Base(artifact.RelativePath)).Content, artifact.Content)
	}
}

func TestRenderPackage_ProtectsLicenseEdits(t *testing.T) {
	pkg, err := RenderPackage(HarnessAgents, "dev", Remote{})
	require.NoError(t, err)
	license := artifactByBase(t, pkg, licenseFileName)

	assert.Equal(t, StateCurrent, Classify([]byte(license.Content), license))
	assert.Equal(t, StateModified,
		Classify([]byte(license.Content+"\nlocal edit\n"), license))
	assert.Equal(t, StateForeign, Classify([]byte("MIT License\n"), license))
}

func TestRenderPackage_ProtectsSearchAgentEdits(t *testing.T) {
	pkg, err := RenderPackage(HarnessClaude, "dev", Remote{})
	require.NoError(t, err)
	agent := artifactByBase(t, pkg, "agentsview-search-conversations.md")

	assert.Equal(t, StateCurrent, Classify([]byte(agent.Content), agent))
	assert.Equal(t, StateModified,
		Classify([]byte(agent.Content+"\nlocal edit\n"), agent))
}

func TestRenderRecallWorkflowContract(t *testing.T) {
	rendered, err := RenderPackage(HarnessAgents, "dev", Remote{})
	require.NoError(t, err)
	require.NotEmpty(t, rendered)
	skill := rendered[0].Content

	assert.Contains(t, skill, "Search before guessing")
	assert.Contains(t, skill, "inspect the current code first")
	assert.Contains(t, skill, "already answered in this conversation")
	assert.Contains(t, skill, "search_content")
	assert.Contains(t, skill, "get_messages")
	assert.Contains(t, skill, "limit: 10")
	assert.Contains(t, skill, "include_active: true")
	assert.Contains(t, skill, "include_one_shot: true")
	assert.Contains(t, skill, "include_automated: true")
	assert.Contains(t, skill, "current_session_id")
	assert.Contains(t, skill, "Do not invent a project filter")
	assert.Contains(t, skill, "omit `scope`")
	assert.Contains(t, skill, "top 2-5")
	assert.Contains(t, skill, "next_from")
	assert.Contains(t, skill, "subordinate")
	assert.Contains(t, skill, "user accepted")
	assert.Contains(t, skill, "verify the agent in effect")
	assert.Contains(t, skill, "without that header")
	assert.Contains(t, skill, "not registered in this session")
	assert.NotContains(t, skill, ".claude/agents")
	assert.Contains(t, skill, "semantic search failed")
	assert.Contains(t, skill, "Do not fall back on authentication or wrong-target errors")
	assert.NotContains(t, skill, "Copyright (c) 2025 Jesse Vincent")
	assert.NotContains(t, skill, "mcp__plugin_episodic-memory")
	assert.NotContains(t, skill, "50-100x")

	license := artifactByBase(t, rendered, licenseFileName).Content
	assert.Contains(t, license, "Copyright (c) 2025 Jesse Vincent")
	assert.Contains(t, license, "Permission is hereby granted")
	assert.Contains(t, license, "7e06519357777badd7a115d2014a7ef845904310")
	assert.False(t, strings.HasPrefix(license, "---\n"))
	assert.Equal(t, StateCurrent,
		Classify([]byte(license), artifactByBase(t, rendered, licenseFileName)))
}

// TestRenderClaudeSkillDelegationGuard pins the Claude-specific delegation
// guard: only the Claude harness renders the project agent directory into the
// guard text.
func TestRenderClaudeSkillDelegationGuard(t *testing.T) {
	claude, err := RenderPackage(HarnessClaude, "dev", Remote{})
	require.NoError(t, err)
	require.NotEmpty(t, claude)
	skill := claude[0].Content

	assert.Contains(t, skill, "the project's")
	assert.Contains(t, skill, ".claude/agents/` directory")
	assert.Contains(t, skill, "without that header")
	assert.Contains(t, skill, "registered as `agentsview`")
}

func TestRenderClaudeSearchAgentContract(t *testing.T) {
	rendered, err := RenderPackage(HarnessClaude, "dev", Remote{})
	require.NoError(t, err)
	agent := artifactByBase(t, rendered, "agentsview-search-conversations.md").Content

	assert.Contains(t, agent, "name: agentsview-search-conversations")
	assert.Contains(t, agent, "model: haiku")
	// The subagent reads untrusted archived transcripts. Claude Code treats
	// `tools` as the complete set, and an allowlist cannot wildcard the MCP
	// server segment, so the agent names the documented `agentsview` server
	// and only its read-only search and message tools. A deny list would
	// leave every other registered MCP server callable.
	assert.Contains(t, agent,
		"tools: mcp__agentsview__search_content, mcp__agentsview__get_messages")
	assert.NotContains(t, agent, "disallowedTools:")
	assert.NotContains(t, agent, "Ignore tools from every other")
	assert.Contains(t, agent, "`mcp__agentsview__search_content`")
	assert.Contains(t, agent, "`mcp__agentsview__get_messages`")
	assert.Contains(t, agent, "incomplete evidence")
	assert.Contains(t, agent, "### Summary")
	assert.Contains(t, agent, "### Sources")
	assert.Contains(t, agent, "### For Follow-Up")
	assert.Contains(t, agent, "1,000 words")
	assert.Contains(t, agent, "Read in detail")
	assert.Contains(t, agent, "Summary only")
	assert.Contains(t, agent, "Skimmed")
	assert.Contains(t, agent, "ordinal range")
	assert.Contains(t, agent, "include_active: true")
	assert.Contains(t, agent, "include_one_shot: true")
	assert.Contains(t, agent, "include_automated: true")
	assert.Contains(t, agent, "current_session_id")
	assert.Contains(t, agent, "Do not invent a project filter")
	assert.Contains(t, agent, "omit `scope`")
	assert.NotContains(t, agent, "% match")
	assert.NotContains(t, agent, "Copyright (c) 2025 Jesse Vincent")
}

func TestRemoteArgs(t *testing.T) {
	assert.Empty(t, Remote{}.Args())
	assert.Equal(t, " --server https://example.invalid",
		Remote{Server: "https://example.invalid"}.Args())
	assert.Equal(t, " --server https://example.invalid --server-token-file token",
		Remote{Server: "https://example.invalid", TokenFile: "token"}.Args())
}

func TestRenderBakesServerArgsAndRemoteLine(t *testing.T) {
	remote := Remote{Server: "https://example.invalid", TokenFile: "token"}
	rendered, err := Render(HarnessClaude, "dev", remote)
	require.NoError(t, err)

	assert.Contains(t, rendered.Content, "--server https://example.invalid")
	assert.Contains(t, rendered.Content, "--server-token-file token")
	assert.Contains(t, rendered.Content, "--exclude-session <this-session-id>")
	assert.NotContains(t, rendered.Content, "--fts --in")
	assert.Equal(t, remote, ParseRemote(rendered.Content))
	assert.Equal(t, StateCurrent, Classify([]byte(rendered.Content), rendered))
}

func TestRenderWithoutRemoteOmitsServerFlags(t *testing.T) {
	rendered, err := Render(HarnessClaude, "dev", Remote{})
	require.NoError(t, err)
	assert.NotContains(t, rendered.Content, "--limit 8 --server")
	assert.NotContains(t, rendered.Content, "--json --server")
	assert.True(t, ParseRemote(rendered.Content).Empty())
	assert.Contains(t, rendered.Content, "silently searches local SQLite")
}

func TestParseRemoteIgnoresMalformedLine(t *testing.T) {
	assert.True(t, ParseRemote(skillFileWithRemoteLine("not-json")).Empty())
}

// skillFileWithRemoteLine builds the first four lines of an installed skill
// file with an arbitrary install-remote payload, so ParseRemote can be
// exercised on files it did not render itself.
func skillFileWithRemoteLine(payload string) string {
	return "---\n# generated-by: agentsview dev hash:" + strings.Repeat("a", 64) +
		" — do not edit; re-run `agentsview skills install`\n" +
		installRemotePrefix + payload + "\nname: x\n"
}

func TestRemoteArgsQuotesUnsafeValues(t *testing.T) {
	tests := []struct {
		name   string
		remote Remote
		want   string
	}{
		{
			name:   "safe values stay bare",
			remote: Remote{Server: "https://example.invalid", TokenFile: "~/.tok"},
			want:   " --server https://example.invalid --server-token-file ~/.tok",
		},
		{
			name:   "space is quoted",
			remote: Remote{Server: "https://example.invalid", TokenFile: "/My Tokens/tok"},
			want:   " --server https://example.invalid --server-token-file '/My Tokens/tok'",
		},
		{
			name:   "shell metacharacters are quoted",
			remote: Remote{Server: "https://example.invalid;rm -rf /"},
			want:   ` --server 'https://example.invalid;rm -rf /'`,
		},
		{
			name:   "embedded single quote is escaped",
			remote: Remote{Server: `a'b`},
			want:   ` --server 'a'\''b'`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.remote.Args())
		})
	}
}

func TestRemoteValidateRejectsControlCharacters(t *testing.T) {
	require.NoError(t, Remote{Server: "https://example.invalid"}.Validate())
	require.Error(t, Remote{Server: "https://example.invalid\nname: evil"}.Validate())
	require.Error(t, Remote{Server: "ok", TokenFile: "tok\ttab"}.Validate())

	_, err := Render(HarnessClaude, "dev", Remote{Server: "a\nb"})
	assert.Error(t, err, "Render must refuse a remote that would break the file")
}

// TestParseRemoteDropsControlCharacters covers a hand-edited file whose JSON
// is well formed but decodes to a value that would not survive re-rendering.
func TestParseRemoteDropsControlCharacters(t *testing.T) {
	body := skillFileWithRemoteLine(`{"server":"https://example.invalid\nname: evil"}`)
	assert.True(t, ParseRemote(body).Empty())
}
