package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestGptmeCostUpgradeReparsesUnchangedSource(t *testing.T) {
	const (
		sessionID       = "gptme:2026-10-20-provider-cost"
		previousVersion = 140
	)
	root := t.TempDir()
	path := filepath.Join(root, "2026-10-20-provider-cost", "conversation.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := []byte(`{"role":"user","content":"hi","timestamp":"2026-10-20T10:00:00.000000"}
{"role":"assistant","content":"hello","timestamp":"2026-10-20T10:00:01.000000","metadata":{"model":"openrouter/example/model","cost_source":"provider","cost":0.000123,"usage":{"input_tokens":100,"output_tokens":20,"cache_read_tokens":50}}}
`)
	require.NoError(t, os.WriteFile(path, content, 0o600))
	infoBefore, err := os.Stat(path)
	require.NoError(t, err)
	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	config := EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGptme: {root}},
		Machine:   "local",
	}
	engine := NewEngine(t.Context(), database, config)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	hashBefore, ok := database.GetSessionFileHash(t.Context(), sessionID)
	require.True(t, ok)
	require.NotEmpty(t, hashBefore)
	engine.Close()
	require.NoError(t, database.Close())

	// Recreate version 140's catalog-priced message, leaving its source unchanged.
	conn, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "DELETE FROM usage_events WHERE session_id = ?", sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE messages SET token_usage = ? WHERE session_id = ? AND role = 'assistant'`,
		`{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":50,"cache_creation_input_tokens":0}`, sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE sessions SET data_version = ? WHERE id = ?", previousVersion, sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "PRAGMA user_version = 140")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	assert.Equal(t, []parser.AgentType{parser.AgentGptme}, db.ScopedDataVersionProviders(previousVersion, previousVersion+1))
	reopened, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	upgraded := NewEngine(t.Context(), reopened, config)
	defer upgraded.Close()
	require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced,
		"the version bump must bypass freshness checks on unchanged sources")

	after, err := reopened.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, before.ID, after.ID)
	assert.Equal(t, before.FilePath, after.FilePath)
	hashAfter, ok := reopened.GetSessionFileHash(t.Context(), sessionID)
	require.True(t, ok)
	assert.Equal(t, hashBefore, hashAfter)
	events, err := reopened.GetUsageEvents(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "exact", events[0].CostStatus)
	require.NotNil(t, events[0].Cost)
	assert.Equal(t, int64(123), events[0].Cost.Microdollars)
	assert.Equal(t, 20, events[0].OutputTokens)
	messages, err := reopened.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Empty(t, messages[1].TokenUsage, "billing tokens belong only to the event")
	assert.Equal(t, 20, messages[1].OutputTokens)
	assert.Equal(t, 150, messages[1].ContextTokens)
	usage, err := reopened.GetSessionUsage(t.Context(), sessionID, true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.True(t, usage.HasCost)
	assert.Equal(t, int64(123), usage.Cost.Microdollars)
	assert.Equal(t, 20, usage.TotalOutputTokens)
	assert.Equal(t, 150, usage.PeakContextTokens)
	require.Len(t, usage.Breakdown, 1, "only the event contributes billable usage")
	assert.Equal(t, 100, usage.Breakdown[0].InputTokens)
	assert.Equal(t, 20, usage.Breakdown[0].OutputTokens)
	assert.Equal(t, 50, usage.Breakdown[0].CacheReadInputTokens)
	assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(t.Context(), sessionID))
	page, err := reopened.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 1)
	infoAfter, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, infoBefore.ModTime(), infoAfter.ModTime())
	unchanged, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, unchanged)
	assert.Equal(t, 0, upgraded.SyncAll(t.Context(), nil).Synced, "the repaired source is fresh")
}
