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
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessionDir := filepath.Join(root, "cwd-key", "sess-1")
	require.NoError(os.MkdirAll(sessionDir, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(sessionDir, "summary.json"),
		[]byte(`{
			"summary":"Inspect a function",
			"firstPrompt":"Explain this function",
			"createdAt":"2026-07-08T10:00:00Z"
		}`),
		0o644,
	))
	require.NoError(os.WriteFile(
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
	require.Equal(1, stats.Synced)
	before, err := database.GetSession(t.Context(), "grok:sess-1")
	require.NoError(err)
	require.NotNil(before)
	require.False(before.IsAutomated)

	require.NoError(os.WriteFile(
		filepath.Join(sessionDir, "prompt_context.json"),
		[]byte(`{"is_non_interactive":true}`),
		0o644,
	))
	stats = engine.SyncAll(t.Context(), nil)
	require.Equal(1, stats.Synced)

	after, err := database.GetSession(t.Context(), "grok:sess-1")
	require.NoError(err)
	require.NotNil(after)
	assert.Equal("non-interactive", after.SessionKind)
	require.True(after.IsAutomated)

	require.NoError(database.ForceBackfillIsAutomated())
	afterAudit, err := database.GetSession(t.Context(), "grok:sess-1")
	require.NoError(err)
	require.NotNil(afterAudit)
	assert.True(afterAudit.IsAutomated)
}
