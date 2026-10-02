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

func TestCodexCacheWriteUpgradeReparsesUnchangedSource(t *testing.T) {
	const id = "019f0000-0000-7000-8000-00000000c0d1"
	const sessionID = "codex:" + id
	// The data version before Codex cache writes were split out of input.
	const previousVersion = 122
	root := t.TempDir()
	day := filepath.Join(root, "2026", "09", "15")
	require.NoError(t, os.MkdirAll(day, 0o755))
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(id, "/workspace/project", "user", "2026-09-15T10:00:00Z"),
		testjsonl.CodexTurnContextJSON("gpt-5.6-luna", "2026-09-15T10:00:01Z"),
		testjsonl.CodexMsgJSON("user", "hello", "2026-09-15T10:00:01Z"),
		testjsonl.CodexMsgJSON("assistant", "hi there", "2026-09-15T10:00:05Z"),
		testjsonl.CodexTokenCountWithCacheWriteJSON("2026-09-15T10:00:05Z", 100000, 10000, 40000, 60000),
	)
	require.NoError(t, os.WriteFile(filepath.Join(day, "rollout-2026-09-15T10-00-00-"+id+".jsonl"), []byte(content), 0o600))
	database := openTestDB(t)
	require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "gpt-5.6-luna",
		InputPerMTok:         money.MustParseDollars("0.20"),
		OutputPerMTok:        money.MustParseDollars("1.20"),
		CacheCreationPerMTok: money.MustParseDollars("0.25"),
		CacheReadPerMTok:     money.MustParseDollars("0.02"),
	}}))
	cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local"}
	engine := NewEngine(t.Context(), database, cfg)
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()

	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "retained", Agent: "gemini", Project: "sample", Machine: "local", MessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{
		SessionID: "retained", Role: "user", Content: "Only stored in the archive",
	}}))
	path := database.Path()
	require.NoError(t, database.Close())

	// Keep source fingerprints but restore the old parser's token JSON, which
	// left cache writes inside ordinary input.
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	res, err := raw.ExecContext(t.Context(), `UPDATE messages SET token_usage=? WHERE session_id=? AND token_usage<>''`,
		`{"cache_read_input_tokens":40000,"input_tokens":60000,"output_tokens":10000}`, sessionID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, err = raw.ExecContext(t.Context(), `UPDATE sessions SET data_version=? WHERE id=?`, previousVersion, sessionID)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), "PRAGMA user_version = 122")
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	reopened, err := db.OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	filter := db.UsageFilter{From: "2026-09-15", To: "2026-09-15", Agent: "codex"}
	warmed, err := reopened.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.0248"), warmed.Totals.TotalCost)
	require.True(t, reopened.NeedsResync(), "the parser migration must reach unchanged sources")

	upgraded := NewEngine(t.Context(), reopened, cfg)
	t.Cleanup(upgraded.Close)
	stats, err := upgraded.SyncThenRun(t.Context(), false, nil, func(full bool) error {
		assert.True(t, full, "startup must select a full resync without an explicit request")
		return nil
	})
	require.NoError(t, err)
	require.False(t, stats.Aborted)
	require.Zero(t, stats.Failed)
	assert.False(t, reopened.NeedsResync())

	messages, err := reopened.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.JSONEq(t, `{"cache_creation_input_tokens":60000,"cache_read_input_tokens":40000,"input_tokens":0,"output_tokens":10000}`,
		string(messages[1].TokenUsage))
	assert.Equal(t, 100000, messages[1].ContextTokens)

	usage, err := reopened.GetSessionUsage(t.Context(), sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, money.MustParseDollars("0.0278"), usage.Cost)
	daily, err := reopened.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.0278"), daily.Totals.TotalCost)

	retained, err := reopened.GetAllMessages(t.Context(), "retained")
	require.NoError(t, err)
	require.Len(t, retained, 1)
	assert.Equal(t, "Only stored in the archive", retained[0].Content)
}
