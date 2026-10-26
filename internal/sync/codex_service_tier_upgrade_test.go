package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexServiceTierUpgradeReparsesUnchangedSource(t *testing.T) {
	const sessionID = "codex:019f0000-0000-7000-8000-000000000001"
	root := t.TempDir()
	path := filepath.Join(root, "rollout-2026-10-19T10-00-00-019f0000-0000-7000-8000-000000000001.jsonl")
	content := []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("019f0000-0000-7000-8000-000000000001", "/workspace/project-a", "codex_cli_rs", "2026-10-19T10:00:00Z"),
		testjsonl.CodexTurnContextJSON("gpt-6.1-sol", "2026-10-19T10:00:00Z"),
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"priority"}}}`,
		testjsonl.CodexMsgJSON("user", "hello", "2026-10-19T10:00:01Z"),
		testjsonl.CodexMsgJSON("assistant", "answer", "2026-10-19T10:00:02Z"),
		testjsonl.CodexTokenCountJSON("2026-10-19T10:00:03Z", 20360, 808, 6912),
	))
	require.NoError(t, os.WriteFile(path, content, 0o600))
	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:     "gpt-6.1-sol",
		InputPerMTok:     money.MustParseDollars("2"),
		CacheReadPerMTok: money.MustParseDollars("0.10"),
		OutputPerMTok:    money.MustParseDollars("10"),
	}}))
	config := EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:   "local",
	}
	engine := NewEngine(t.Context(), database, config)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, before)
	engine.Close()
	require.NoError(t, database.Close())

	// Model an archive written before service-tier billing, without touching its source.
	conn, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE messages SET provider_id = '' WHERE session_id = ? AND role = 'assistant'", sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE sessions SET data_version = ? WHERE id = ?", 146, sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "PRAGMA user_version = 146")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	reopened, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	upgraded := NewEngine(t.Context(), reopened, config)
	defer upgraded.Close()
	require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced,
		"the version bump must bypass freshness checks on unchanged sources")

	usage, err := reopened.GetSessionUsage(t.Context(), sessionID, true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, money.MustParseDollars("0.071334"), usage.Cost)
	after, err := reopened.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, before.ID, after.ID)
	assert.Equal(t, before.FilePath, after.FilePath)
	unchanged, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, unchanged)
}
