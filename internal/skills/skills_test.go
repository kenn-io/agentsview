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

func TestRenderPackage_HarnessArtifacts(t *testing.T) {
	claude, err := RenderPackage(HarnessClaude, "dev", Remote{})
	require.NoError(t, err)
	assert.Equal(t, []string{
		".claude/skills/agentsview-finding-history/SKILL.md",
		".claude/agents/agentsview-search-conversations.md",
	}, renderedPaths(claude))

	agents, err := RenderPackage(HarnessAgents, "dev", Remote{})
	require.NoError(t, err)
	assert.Equal(t, []string{
		".agents/skills/agentsview-finding-history/SKILL.md",
	}, renderedPaths(agents))
}

func TestRenderPluginPackageReusesClaudeArtifacts(t *testing.T) {
	standalone, err := RenderPackage(HarnessClaude, "0.1.0", Remote{})
	require.NoError(t, err)
	require.Len(t, standalone, 2)

	plugin, err := RenderPluginPackage("0.1.0")
	require.NoError(t, err)
	require.Len(t, plugin, 2)
	assert.Equal(t, []string{
		"skills/agentsview-finding-history/SKILL.md",
		"agents/agentsview-search-conversations.md",
	}, renderedPaths(plugin))
	// The plugin's skill is byte-identical to the standalone one, so
	// lifecycle diagnostics can recognize a duplicate standalone install.
	// The agent differs only in its allowlisted tool names: the plugin
	// names its own tools, the standalone render names the documented
	// generic spellings.
	assert.Equal(t, standalone[0].Content, plugin[0].Content)
	assert.NotEqual(t, standalone[1].Content, plugin[1].Content)
	assert.Contains(t, plugin[1].Content, "mcp__plugin_agentsview-memory_agentsview__")
	assert.Contains(t, standalone[1].Content, "tools: mcp__agentsview__search_content")
}

func TestRenderPackage_ProtectsSearchAgentEdits(t *testing.T) {
	pkg, err := RenderPluginPackage("dev")
	require.NoError(t, err)
	require.Len(t, pkg, 2)
	agent := pkg[1]

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
	assert.Contains(t, skill, "top 2-5")
	assert.Contains(t, skill, "next_from")
	assert.Contains(t, skill, "subordinate")
	assert.Contains(t, skill, "user accepted")
	assert.Contains(t, skill, "agent in effect is the file this skill generated")
	assert.Contains(t, skill, "without that header")
	assert.Contains(t, skill, "not registered in this session")
	assert.NotContains(t, skill, ".claude/agents")
	assert.Contains(t, skill, "semantic search failed")
	assert.Contains(t, skill, "Do not fall back on authentication or wrong-target errors")
	assert.Contains(t, skill, "Copyright (c) 2025 Jesse Vincent")
	assert.NotContains(t, skill, "mcp__plugin_episodic-memory")
	assert.NotContains(t, skill, "50-100x")
}

// TestRenderClaudeSkillDelegationGuard pins the Claude-specific delegation
// guard: delegation is allowed only to the plugin's namespaced agent, whose
// identity a repository cannot override. A project-level or user-level
// same-named file — however it is marked — must be refused, because its
// body cannot be verified from model instructions alone.
func TestRenderClaudeSkillDelegationGuard(t *testing.T) {
	claude, err := RenderPluginPackage("dev")
	require.NoError(t, err)
	require.Len(t, claude, 2)
	skill := claude[0].Content

	assert.Contains(t, skill, "agentsview-memory:agentsview-search-conversations",
		"the guard must name the plugin-owned, unshadowable agent")
	assert.Contains(t, skill, ".claude/agents/` directory")
	assert.Contains(t, skill, "not the plugin's agent: do not delegate to it")
	assert.Equal(t, 1, strings.Count(skill, "hash:"),
		"only the file's own generated-by header mentions a hash: body-to-hash comparison cannot be verified from model instructions")
}

func TestRenderClaudeSearchAgentContract(t *testing.T) {
	rendered, err := RenderPluginPackage("dev")
	require.NoError(t, err)
	require.Len(t, rendered, 2)
	agent := rendered[1].Content

	assert.Contains(t, agent, "name: agentsview-search-conversations")
	assert.Contains(t, agent, "model: haiku")
	// The subagent reads untrusted archived transcripts, so its frontmatter
	// allowlists exactly the focused AgentsView read-only MCP tools as
	// served by the AgentsView memory plugin. Claude Code prefixes
	// plugin-provided servers with mcp__plugin_<plugin>_<server>__, an
	// identity a project-level .mcp.json cannot shadow with a same-named
	// server, so untrusted transcript content can neither prompt writes
	// through inherited tools nor redirect searches to an impostor server.
	// Without the plugin the agent has no tools and fails closed: it
	// reports it could not search and the skill's step 1 falls back to
	// following the probes directly.
	assert.Contains(t, agent,
		"tools: mcp__plugin_agentsview-memory_agentsview__search_content, "+
			"mcp__plugin_agentsview-memory_agentsview__get_messages")
	assert.NotContains(t, agent, "disallowedTools:")
	assert.NotContains(t, agent, "mcp__agentsview__",
		"generic server-name spellings are shadowable and must not be allowlisted")

	// The standalone render names the documented generic spellings instead:
	// usable with the documented MCP registrations, failing closed on any
	// other server key.
	standalone, err := RenderPackage(HarnessClaude, "dev", Remote{})
	require.NoError(t, err)
	require.Len(t, standalone, 2)
	assert.Contains(t, standalone[1].Content,
		"tools: mcp__agentsview__search_content, mcp__agentsview__get_messages, "+
			"mcp__agentsview-memory__search_content, "+
			"mcp__agentsview-memory__get_messages")
	assert.NotContains(t, standalone[1].Content, "mcp__plugin_")

	assert.Contains(t, agent, "Use only the AgentsView MCP tools")
	assert.Contains(t, agent, "incomplete evidence")
	assert.Contains(t, agent, "### Summary")
	assert.Contains(t, agent, "### Sources")
	assert.Contains(t, agent, "### For Follow-Up")
	assert.Contains(t, agent, "1,000 words")
	assert.Contains(t, agent, "Read in detail")
	assert.Contains(t, agent, "Summary only")
	assert.Contains(t, agent, "Skimmed")
	assert.Contains(t, agent, "ordinal range")
	assert.NotContains(t, agent, "% match")
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
