package sync_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestSyncPathsClaudeEarlierContinuationReplacesMessages(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)
	t.Cleanup(env.engine.Close)
	later := testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-08-05T03:42:00Z", "B0").
		AddClaudeAssistant("2026-08-05T03:43:00Z", "B1").String()
	laterPath := env.writeSession(t, env.claudeDir,
		filepath.Join("project", "later-parent", "subagents", "agent-reviewer.jsonl"), later)
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{laterPath}))
	assertMessageContent(t, env.db, "agent-reviewer", "B0", "B1")

	// A restored earlier transcript is larger, so discovery selects its path.
	// Its messages precede the already archived ordinals and require replacement.
	earlier := testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-08-05T03:40:00Z", "A0").
		AddClaudeAssistant("2026-08-05T03:41:00Z", "A1").String() +
		"{\"type\":\"ai-title\",\"aiTitle\":\"Earlier review\"}\n"
	require.Greater(t, len(earlier), len(later))
	earlierPath := env.writeSession(t, env.claudeDir,
		filepath.Join("project", "earlier-parent", "subagents", "agent-reviewer.jsonl"), earlier)
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{earlierPath}))
	assert.Equal(t, earlierPath, env.db.GetSessionFilePath(t.Context(), "agent-reviewer"))
	assertMessageContent(t, env.db, "agent-reviewer", "A0", "A1", "B0", "B1")
}
