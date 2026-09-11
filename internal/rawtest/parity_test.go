package rawtest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestCaptureOracleLiteralContent(t *testing.T) {
	root := t.TempDir()
	Claude(t, root)
	archive, engine := Oracle(t, parser.AgentClaude, root, "", "")
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	session, err := archive.GetSessionFull(t.Context(), ClaudeID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, ClaudeID, session.SourceSessionID)
	assert.True(t, session.HasToolCalls)
	assert.True(t, session.HasContextData)
	assert.Equal(t, 1, session.SecretLeakCount)
	messages, err := archive.GetAllMessages(t.Context(), ClaudeID)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	require.Len(t, messages[1].ToolCalls, 1)
	call := messages[1].ToolCalls[0]
	assert.Equal(t, "Bash", call.ToolName)
	assert.Equal(t, "Error: synthetic build failure\n", call.ResultContent)
	assert.Empty(t, call.ResultEvents, "Claude persists its summary without Codex lifecycle events")
	assert.Equal(t, "Check the build first.", messages[1].ThinkingText)
	assert.Equal(t, 7, messages[1].OutputTokens)
	assert.Equal(t, 116, messages[1].ContextTokens)
	assert.True(t, messages[2].HasOutputTokens)
	assert.True(t, messages[2].HasContextTokens)
	assert.Zero(t, messages[2].OutputTokens)
	assert.False(t, messages[0].HasOutputTokens)
	assert.False(t, messages[0].HasContextTokens)
	findings, err := archive.ListSecretFindings(t.Context(), db.SecretFindingFilter{})
	require.NoError(t, err)
	require.Len(t, findings.Findings, 1)
	finding := findings.Findings[0]
	assert.Equal(t, "github-pat", finding.RuleName)
	assert.Equal(t, "message", finding.LocationKind)
	assert.Equal(t, 0, finding.MessageOrdinal)
	assert.Nil(t, finding.CallIndex)
	assert.Nil(t, finding.EventIndex)
	assert.Equal(t, 46, finding.MatchStart)
	assert.Equal(t, 86, finding.MatchEnd)
	assert.Equal(t, 0, finding.MatchIndex)
}

func TestCaptureForkPartialAndCompanionRecovery(t *testing.T) {
	root := t.TempDir()
	CodexFork(t, root)
	provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := parser.DiscoverRawCaptureSources(t.Context(), provider)
	require.NoError(t, err)
	require.Len(t, sources.Sources, 1)
	out, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources.Sources[0], ForceParse: true})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	assert.Equal(t, parser.DataVersionNeedsRetry, out.Results[0].DataVersion)
	assert.Len(t, out.Results[0].Result.Messages, 2)
	CodexParent(t, root)
	// A fresh provider avoids allowing an earlier unresolved-parent cache entry
	// to stand in for a newly accepted immutable generation.
	provider, ok = parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), parser.FindSourceRequest{RawSessionID: CodexChildID})
	require.NoError(t, err)
	require.True(t, found)
	out, err = provider.Parse(t.Context(), parser.ParseRequest{Source: source, ForceParse: true})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	assert.Equal(t, parser.DataVersionCurrent, out.Results[0].DataVersion)
	plan, supported, err := parser.ResolveRawCapturePlan(t.Context(), provider, source)
	require.NoError(t, err)
	require.True(t, supported)
	require.Len(t, plan.Entries, 2)
	for _, entry := range plan.Entries {
		_, err := os.Stat(filepath.Clean(entry.LocalPath))
		require.NoError(t, err)
	}
}

func TestCaptureOracleRelationshipsAndToolEvents(t *testing.T) {
	t.Run("claude parent", func(t *testing.T) {
		root := t.TempDir()
		Claude(t, root)
		ClaudeChild(t, root)
		archive, engine := Oracle(t, parser.AgentClaude, root, "", "")
		require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
		child, err := archive.GetSessionFull(t.Context(), ClaudeChildID)
		require.NoError(t, err)
		require.NotNil(t, child)
		require.NotNil(t, child.ParentSessionID)
		assert.Equal(t, ClaudeID, *child.ParentSessionID)
		assert.Equal(t, "subagent", child.RelationshipType)
	})
	t.Run("codex tool events", func(t *testing.T) {
		root := t.TempDir()
		CodexTools(t, root)
		archive, engine := Oracle(t, parser.AgentCodex, root, "", "")
		require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
		messages, err := archive.GetAllMessages(t.Context(), "codex:"+CodexToolsID)
		require.NoError(t, err)
		var events []db.ToolResultEvent
		for _, message := range messages {
			for _, call := range message.ToolCalls {
				events = append(events, call.ResultEvents...)
			}
		}
		require.Len(t, events, 1)
		assert.Equal(t, "Process exited with code 1\nError: synthetic failure", events[0].Content)
	})
}

func TestCaptureOracleBillableWithoutMessages(t *testing.T) {
	root := t.TempDir()
	ZCode(t, root)
	archive, engine := Oracle(t, parser.AgentZCode, root, "", "")
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	messages, err := archive.GetAllMessages(t.Context(), ZCodeBillableID)
	require.NoError(t, err)
	assert.Empty(t, messages)
	usage, err := archive.GetSessionUsage(t.Context(), ZCodeBillableID, true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 17, usage.TotalOutputTokens)
	assert.True(t, usage.HasCost)
}
