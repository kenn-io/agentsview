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

// Archives written before version 169 stored Harness rows without source IDs.
// The reparse must add the IDs to the same rows: pins and notes stay, even on
// system rows, and exported messages keep their IDs.
func TestDeepSeekHarnessMessageIDUpgradeReparsesUnchangedSource(t *testing.T) {
	const (
		sessionID       = "deepseek-harness:identity"
		previousVersion = 168
	)
	root := t.TempDir()
	path := harnessSyncWriteLog(t, root, "identity", map[string]any{"version": 4, "isSeeded": false}, []any{
		harnessSyncEvent(0, "system/message", map[string]any{
			"message": map[string]any{
				"id": "s-1", "role": "system", "source": map[string]any{"kind": "system-prompt"},
				"content": []any{map[string]any{"type": "text", "text": "be brief"}},
			},
		}, "append"),
		harnessSyncEvent(1, "turn/start", map[string]any{"turn": 1}, nil),
		harnessSyncEvent(2, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
		harnessSyncEvent(3, "user/message", harnessSyncUserWithID("u-1", "start work"), "append"),
		harnessSyncEvent(4, "request/header", harnessSyncRequest(), nil),
		harnessSyncEvent(5, "assistant/message", harnessSyncAssistantWithID(
			"a-1", 1, 1, "answer", 2,
		), "append"),
		harnessSyncEvent(6, "step/end", map[string]any{"turn": 1, "step": 1}, nil),
		harnessSyncEvent(7, "turn/end", map[string]any{
			"turn": 1, "reason": map[string]any{"kind": "completed"},
		}, nil),
	})
	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	config := sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentDeepSeekHarness: {root}},
		Machine:   "local",
	}
	engine := sync.NewEngine(t.Context(), database, config)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	_, err = database.EnsureConversationExportInitialized(t.Context())
	require.NoError(t, err)
	before, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, before, 3)
	require.True(t, before[0].IsSystem)
	note := "keep the prompt"
	_, err = database.PinMessage(t.Context(), sessionID, before[0].ID, &note)
	require.NoError(t, err)
	_, err = database.PinMessage(t.Context(), sessionID, before[2].ID, nil)
	require.NoError(t, err)
	pinsBefore, err := database.ListPinnedMessages(t.Context(), sessionID, "")
	require.NoError(t, err)
	require.Len(t, pinsBefore, 2)
	engine.Close()
	require.NoError(t, database.Close())

	// Version 168 stored no source IDs for Harness messages, so their export
	// rows had no identity either.
	conn, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE messages SET source_uuid = '' WHERE session_id = ?", sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE conversation_messages SET source_id = '', gap = 'identity_unavailable' WHERE session_id = ?", sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE sessions SET data_version = ? WHERE id = ?", previousVersion, sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "PRAGMA user_version = 168")
	require.NoError(t, err)
	exportedBefore := harnessSyncExportRows(t, conn, sessionID)
	require.Len(t, exportedBefore, 2)
	require.NoError(t, conn.Close())

	assert.Equal(t, []parser.AgentType{parser.AgentDeepSeekHarness},
		db.ScopedDataVersionProviders(previousVersion, previousVersion+1),
		"the identity repair must reparse only DeepSeek Harness sources in place")
	reopened, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	stale, err := reopened.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{"", "", ""}, harnessSyncSourceIDs(stale))
	upgraded := sync.NewEngine(t.Context(), reopened, config)
	defer upgraded.Close()
	require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced,
		"the version bump must bypass freshness checks on unchanged sources")

	after, err := reopened.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, []string{"s-1", "u-1", "a-1"}, harnessSyncSourceIDs(after))
	for index := range after {
		assert.Equal(t, before[index].ID, after[index].ID, "row %d keeps its ID", index)
	}
	assert.Equal(t, path, reopened.GetSessionFilePath(t.Context(), sessionID))
	pins, err := reopened.ListPinnedMessages(t.Context(), sessionID, "")
	require.NoError(t, err)
	require.Len(t, pins, 2)
	pinned := map[int64]*string{}
	for _, pin := range pins {
		pinned[pin.MessageID] = pin.Note
	}
	require.Contains(t, pinned, after[0].ID, "the system row keeps its pin")
	require.NotNil(t, pinned[after[0].ID])
	assert.Equal(t, note, *pinned[after[0].ID])
	assert.Contains(t, pinned, after[2].ID)

	exportConn, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, exportConn.Close()) }()
	exportedAfter := harnessSyncExportRows(t, exportConn, sessionID)
	require.Len(t, exportedAfter, 2, "no exported message is removed or reissued")
	for index, row := range exportedAfter {
		assert.Equal(t, exportedBefore[index].messageID, row.messageID)
		assert.Empty(t, row.gap)
	}
	assert.Equal(t, []string{"u-1", "a-1"},
		[]string{exportedAfter[0].sourceID, exportedAfter[1].sourceID})
}

type harnessSyncExportRow struct {
	messageID, sourceID, gap string
}

// harnessSyncExportRows returns every export row of the session, including
// removed ones, so a reissued message shows up as an extra row.
func harnessSyncExportRows(t *testing.T, conn *sql.DB, sessionID string) []harnessSyncExportRow {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(),
		"SELECT message_id, source_id, gap FROM conversation_messages WHERE session_id = ? ORDER BY ordinal, removed",
		sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var result []harnessSyncExportRow
	for rows.Next() {
		var row harnessSyncExportRow
		require.NoError(t, rows.Scan(&row.messageID, &row.sourceID, &row.gap))
		result = append(result, row)
	}
	require.NoError(t, rows.Err())
	return result
}
