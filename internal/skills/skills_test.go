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

	plugin, err := RenderPluginPackage("0.1.0")
	require.NoError(t, err)
	require.Len(t, plugin, 2)
	assert.Equal(t, []string{
		"skills/agentsview-finding-history/SKILL.md",
		"agents/agentsview-search-conversations.md",
	}, renderedPaths(plugin))
	assert.Equal(t, standalone[0].Content, plugin[0].Content)
	assert.Equal(t, standalone[1].Content, plugin[1].Content)
}

func TestRenderPackage_ProtectsSearchAgentEdits(t *testing.T) {
	pkg, err := RenderPackage(HarnessClaude, "dev", Remote{})
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
	assert.Contains(t, collapseSpaces(skill), "If you cannot verify the agent")
	assert.Contains(t, skill, "not registered in this session")
	assert.NotContains(t, skill, ".claude/agents")
	assert.Contains(t, skill, "semantic search failed")
	assert.Contains(t, skill, "Do not fall back on authentication or wrong-target errors")
	assert.Contains(t, skill, "Copyright (c) 2025 Jesse Vincent")
	assert.NotContains(t, skill, "mcp__plugin_episodic-memory")
	assert.NotContains(t, skill, "50-100x")
}

// TestRenderClaudeSkillDelegationGuard pins the Claude-specific delegation
// guard: only the Claude harness renders the project agent directory into the
// guard text.
func TestRenderClaudeSkillDelegationGuard(t *testing.T) {
	claude, err := RenderPackage(HarnessClaude, "dev", Remote{})
	require.NoError(t, err)
	require.NotEmpty(t, claude)
	skill := claude[0].Content

	flat := collapseSpaces(skill)
	assert.Contains(t, flat, "the project's `.claude/agents/` directory")
	assert.Contains(t, flat, "byte-for-byte")
	assert.Contains(t, flat, "do not delegate to it")
}

// collapseSpaces replaces every run of whitespace with one space so
// assertions on rendered template prose do not depend on line wrapping.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// The delegation guard must pin the search agent's full content hash: a
// project-level override that copies the generated-by header would otherwise
// redirect the skill to attacker-controlled instructions. The rendered skill
// embeds the exact digest of the agent artifact, and the Agents harness,
// which installs no agent, embeds none.
func TestRenderClaudeDelegationGuardPinsAgentHash(t *testing.T) {
	skill, err := Render(HarnessClaude, "dev", Remote{})
	require.NoError(t, err)
	packageArtifacts, err := RenderPackage(HarnessClaude, "dev", Remote{})
	require.NoError(t, err)
	require.Len(t, packageArtifacts, 2)
	agent := packageArtifacts[1]

	flat := collapseSpaces(skill.Content)
	assert.Contains(t, flat, agent.Hash,
		"the guard must embed the rendered agent's content hash")
	assert.Contains(t, flat,
		"remove its second line (the `# generated-by:` comment)")
	assert.Contains(t, flat, "the project's `.claude/agents/` directory")

	agentsSkill, err := Render(HarnessAgents, "dev", Remote{})
	require.NoError(t, err)
	assert.NotContains(t, agentsSkill.Content, agent.Hash,
		"the Agents harness installs no agent to verify")
}

func TestRenderClaudeSearchAgentContract(t *testing.T) {
	rendered, err := RenderPackage(HarnessClaude, "dev", Remote{})
	require.NoError(t, err)
	require.Len(t, rendered, 2)
	agent := rendered[1].Content

	assert.Contains(t, agent, "name: agentsview-search-conversations")
	assert.Contains(t, agent, "model: haiku")
	// The subagent reads untrusted archived transcripts, so its frontmatter
	// allowlists exactly the two read-only AgentsView MCP tools it uses.
	// The native package and the documented registration both name the MCP
	// server `agentsview`, so the allowlist can pin full tool names; tools
	// from every other registered MCP server stay unreachable, which a
	// deny list alone could never guarantee. The built-in deny list is kept
	// so runtimes that ignore `tools` still fail closed on capability-
	// bearing built-ins. An install whose server uses a different name
	// makes the agent report failure, and the parent falls back to the
	// skill's direct workflow.
	assert.Contains(t, agent,
		"tools: mcp__agentsview__search_content, mcp__agentsview__get_messages")
	assert.Contains(t, agent,
		"disallowedTools: Bash, Edit, Write, NotebookEdit, Read, Grep, Glob, "+
			"WebFetch, WebSearch, Task, Agent, SlashCommand, Skill, TodoWrite, "+
			"BashOutput, KillShell, AskUserQuestion, ExitPlanMode, EnterPlanMode, "+
			"MCPSearch")
	assert.Contains(t, agent, "allowlists exactly")
	assert.Contains(t, agent, "canonical name `agentsview`")
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
