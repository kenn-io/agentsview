package sync_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestClaudeBlankThinkingUsageUpgradeRestoresUsage(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	projectDir := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(projectDir, 0o700))
	content, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "claude", "blank_thinking_usage.jsonl"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "blank.jsonl"), content, 0o600))
	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.OpenIsolated(ctx, archivePath)
	require.NoError(t, err)
	cfg := sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local"}
	engine := sync.NewEngine(ctx, database, cfg)
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	before := fetchMessages(t, database, "blank")
	require.Len(t, before, 3)
	_, err = database.PinMessage(ctx, "blank", before[2].ID, new("saved answer"))
	require.NoError(t, err)
	engine.Close()
	require.NoError(t, database.Close())

	// Model version 149, which dropped the blank record and its usage,
	// without changing the source fingerprint.
	previousVersion := 149
	raw, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM messages WHERE session_id = 'blank' AND ordinal = 1;
		UPDATE messages SET ordinal = 1 WHERE session_id = 'blank' AND ordinal = 2;
		UPDATE pinned_messages SET ordinal = 1 WHERE session_id = 'blank';
		UPDATE sessions SET message_count = 2 WHERE id = 'blank';
		UPDATE sessions SET data_version = %[1]d; PRAGMA user_version = %[1]d`,
		previousVersion))
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	reopened, err := db.OpenIsolated(ctx, archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	assert.False(t, reopened.NeedsResync(), "the archive upgrades in place")
	upgraded := sync.NewEngine(ctx, reopened, cfg)
	defer upgraded.Close()
	stats := upgraded.SyncAll(ctx, nil)
	require.Zero(t, stats.Failed)
	assert.Positive(t, stats.Synced)

	after := fetchMessages(t, reopened, "blank")
	require.Len(t, after, 3)
	assert.Equal(t, "a1", after[1].SourceUUID)
	assert.Equal(t, "msg_blank", after[1].ClaudeMessageID)
	assert.True(t, after[1].IsSystem)
	assert.Equal(t, 41, after[1].ContextTokens)
	assert.Equal(t, 7, after[1].OutputTokens)
	pins, err := reopened.ListPinnedMessages(ctx, "blank", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, after[2].ID, pins[0].MessageID)
	assert.Equal(t, 2, pins[0].Ordinal)
}
