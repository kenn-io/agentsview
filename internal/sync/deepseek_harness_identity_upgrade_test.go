package sync_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestDeepSeekHarnessMessageIDUpgradeReparsesUnchangedSource(t *testing.T) {
	const (
		sessionID       = "deepseek-harness:identity"
		previousVersion = 168
	)
	root := t.TempDir()
	turn := harnessSyncCompleteTurn("answer")
	turn[2] = harnessSyncEvent(2, "user/message", harnessSyncUserWithID("u-1", "start work"), "append")
	turn[4] = harnessSyncEvent(4, "assistant/message", harnessSyncAssistantWithID(
		"a-1", 1, 1, "answer", 2,
	), "append")
	path := harnessSyncWriteLog(t, root, "identity", map[string]any{"version": 4, "isSeeded": false}, turn)
	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	config := sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentDeepSeekHarness: {root}},
		Machine:   "local",
	}
	engine := sync.NewEngine(t.Context(), database, config)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, before, 2)
	_, err = database.PinMessage(t.Context(), sessionID, before[1].ID, nil)
	require.NoError(t, err)
	engine.Close()
	require.NoError(t, database.Close())

	// Version 168 stored no source ids for Harness messages.
	conn, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE messages SET source_uuid = '' WHERE session_id = ?", sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE sessions SET data_version = ? WHERE id = ?", previousVersion, sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "PRAGMA user_version = 168")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	assert.Equal(t, []parser.AgentType{parser.AgentDeepSeekHarness},
		db.ScopedDataVersionProviders(previousVersion, previousVersion+1),
		"the identity repair must reparse only DeepSeek Harness sources in place")
	reopened, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	stale, err := reopened.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{"", ""}, harnessSyncSourceIDs(stale))
	upgraded := sync.NewEngine(t.Context(), reopened, config)
	defer upgraded.Close()
	require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced,
		"the version bump must bypass freshness checks on unchanged sources")

	after, err := reopened.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, []string{"u-1", "a-1"}, harnessSyncSourceIDs(after))
	assert.Equal(t, path, reopened.GetSessionFilePath(t.Context(), sessionID))
	pins, err := reopened.ListPinnedMessages(t.Context(), sessionID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, after[1].ID, pins[0].MessageID)
	assert.Equal(t, 1, pins[0].Ordinal)
}
