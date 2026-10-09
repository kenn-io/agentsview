package sync_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestGrokNativeSubagentsSurviveParentLoss(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.CopyFS(root, os.DirFS(filepath.Join("..", "parser", "testdata", "grok-build", "subagents"))))
	parentDir := filepath.Join(root, "%2Fworkspace%2Fproject", "019f6000-0000-7000-8000-000000000010")
	children := []struct{ cwd, id, kind string }{
		{"%2Fworkspace%2Fproject", "019f6000-0000-7000-8000-000000000011", "subagent"},
		{"%2Fworkspace%2Fproject", "019f6000-0000-7000-8000-000000000012", "subagent_resume"},
		{"%2Fworkspace%2Fworktree-child", "019f6000-0000-7000-8000-000000000015", "subagent_fork"},
	}
	for _, child := range children {
		dir := filepath.Join(root, child.cwd, child.id)
		if child.kind == "subagent_fork" {
			path := filepath.Join(dir, "summary.json")
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(string(body), `"subagent"`, `"subagent_fork"`)), 0o600))
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "chat_history.jsonl"), []byte(`{"type":"user","content":"You are a code reviewer. Review this change."}`+"\n"), 0o600))
	}
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGrok: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	assertChildren := func() {
		t.Helper()
		for _, child := range children {
			stored, err := database.GetSession(t.Context(), "grok:"+child.id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, "subagent", stored.RelationshipType, child.kind)
			assert.Empty(t, stored.SessionKind, child.kind)
			assert.True(t, stored.IsAutomated, child.kind)
		}
	}
	require.Equal(t, 6, engine.SyncAll(t.Context(), nil).Synced)
	assertChildren()
	// All removal targets are fixture directories beneath this test's temporary root.
	require.NoError(t, os.RemoveAll(filepath.Join(parentDir, "subagents")))
	require.GreaterOrEqual(t, engine.SyncAll(t.Context(), nil).Synced, 3)
	assertChildren()
	require.NoError(t, database.ForceBackfillIsAutomated(t.Context()))
	assertChildren()
	require.NoError(t, database.DeleteSession(t.Context(), "grok:019f6000-0000-7000-8000-000000000010"))
	require.NoError(t, os.RemoveAll(parentDir))
	engine.SyncAll(t.Context(), nil)
	require.NoError(t, database.ForceBackfillIsAutomated(t.Context()))
	assertChildren()
	require.False(t, engine.ResyncAll(t.Context(), nil).Aborted)
	require.NoError(t, database.ForceBackfillIsAutomated(t.Context()))
	assertChildren()
}

func TestGrokPromptContextSubagentMatchesParseDiff(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "cwd-key", "sess-1")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "summary.json"),
		[]byte(`{
			"summary":"Inspect a function",
			"firstPrompt":"Explain this function",
			"createdAt":"2026-07-08T10:00:00Z"
		}`),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "prompt_context.json"),
		[]byte(`{"is_non_interactive":false}`),
		0o644,
	))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGrok: {root}},
		Machine:   "local",
	})

	stats := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)
	before, err := database.GetSession(t.Context(), "grok:sess-1")
	require.NoError(t, err)
	require.NotNil(t, before)
	require.False(t, before.IsAutomated)

	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "prompt_context.json"),
		[]byte(`{"is_non_interactive":true}`),
		0o644,
	))
	stats = engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)

	after, err := database.GetSession(t.Context(), "grok:sess-1")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, "non-interactive", after.SessionKind)
	assert.False(t, after.IsAutomated)
	assert.Equal(t, "subagent", after.RelationshipType)

	diff := sync.NewDiffEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGrok: {root}}, Machine: "local",
	})
	t.Cleanup(diff.Close)
	report, err := diff.ParseDiff(t.Context(), sync.ParseDiffOptions{Agents: []parser.AgentType{parser.AgentGrok}})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Totals.Identical)
	assert.False(t, report.HasFailures())
}
