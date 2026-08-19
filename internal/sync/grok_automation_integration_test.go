package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestGrokPromptContextAutomationSurvivesResyncAndAudit(t *testing.T) {
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
	engine := sync.NewEngine(database, sync.EngineConfig{
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
	require.True(t, after.IsAutomated)

	require.NoError(t, database.ForceBackfillIsAutomated())
	afterAudit, err := database.GetSession(t.Context(), "grok:sess-1")
	require.NoError(t, err)
	require.NotNil(t, afterAudit)
	assert.True(t, afterAudit.IsAutomated)
}
