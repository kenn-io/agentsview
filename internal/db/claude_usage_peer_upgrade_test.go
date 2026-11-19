package db_test

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestClaudeUsagePeerUpgradePreservesConversation(t *testing.T) {
	ctx := t.Context()
	version := db.CurrentDataVersion()
	t.Cleanup(db.InstallDataVersionScopesForTest(map[int][]parser.AgentType{
		version: db.ScopedDataVersionProviders(165, 166),
	}))
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	const id = "c0000000-0000-4000-8000-000000000167"
	source := filepath.Join(root, "project", id+".jsonl")
	body, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "claude", "usage_peer_conversation.jsonl"))
	require.NoError(t, err)
	dbtest.WriteTestFile(t, source, body)
	info, err := os.Stat(source)
	require.NoError(t, err)

	// Version 165 stored the non-meta peer as a human turn. Seed that archive
	// directly so the parser under test cannot hide an excluded conversation.
	var messages []db.Message
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		var entry struct {
			Type, UUID, ParentUUID, Timestamp string
			Message                           struct{ Content string }
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &entry))
		messages = append(messages, db.Message{
			SessionID: id, Ordinal: len(messages), Role: entry.Type,
			SourceUUID: entry.UUID, SourceParentUUID: entry.ParentUUID,
			Content: entry.Message.Content, ContentLength: len(entry.Message.Content), Timestamp: entry.Timestamp,
		})
	}
	require.NoError(t, scanner.Err())
	require.Len(t, messages, 3)
	require.NoError(t, database.UpsertSession(ctx, db.Session{
		ID: id, Project: "project", Machine: "local", Agent: "claude",
		FirstMessage: &messages[1].Content, MessageCount: 3, UserMessageCount: 2,
		FilePath: &source, FileSize: dbtest.Ptr(info.Size()), FileMtime: dbtest.Ptr(info.ModTime().UnixNano()),
		ClaudeLinearParse: dbtest.Ptr(true),
	}))
	dbtest.SeedMessages(t, database, messages...)
	displayName, note := "Saved peer review", "Keep this peer request"
	require.NoError(t, database.RenameSession(ctx, id, &displayName))
	stored, err := database.GetAllMessages(ctx, id)
	require.NoError(t, err)
	require.Len(t, stored, 3)
	_, err = database.PinMessage(ctx, id, stored[1].ID, &note)
	require.NoError(t, err)
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE sessions SET data_version=%d; PRAGMA user_version=%d", version-1, version-1))
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	database, err = db.OpenIsolated(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.False(t, database.NeedsResync())
	needsSync, fullRebuild, err := db.ArchiveResyncState(ctx, path)
	require.NoError(t, err)
	assert.True(t, needsSync)
	assert.False(t, fullRebuild)
	engine := sync.NewEngine(ctx, database, sync.EngineConfig{
		Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
	})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(ctx, nil)
	require.Zero(t, stats.Failed)
	after, err := database.GetSessionFull(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, after, "scoped reparsing must not delete a peer conversation")
	assert.Equal(t, 1, stats.Synced)
	assert.Equal(t, &source, after.FilePath)
	assert.Equal(t, &displayName, after.DisplayName)
	assert.Nil(t, after.DeletedAt)
	assert.Equal(t, 1, after.UserMessageCount)
	assert.Equal(t, version, after.DataVersion)
	stored, err = database.GetAllMessages(ctx, id)
	require.NoError(t, err)
	require.Len(t, stored, len(messages))
	for i, old := range messages {
		assert.Equal(t, old.SourceUUID, stored[i].SourceUUID)
		assert.Equal(t, old.Content, stored[i].Content)
	}
	assert.True(t, stored[1].IsSystem)
	assert.Equal(t, "peer_message", stored[1].SourceSubtype)
	pins, err := database.ListPinnedMessages(ctx, "", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, id, pins[0].SessionID)
	assert.Equal(t, &messages[1].Content, pins[0].Content)
	assert.Equal(t, &note, pins[0].Note)
	afterInfo, err := os.Stat(source)
	require.NoError(t, err)
	assert.Equal(t, info.ModTime(), afterInfo.ModTime())
	afterBody, err := os.ReadFile(source)
	require.NoError(t, err)
	assert.Equal(t, body, afterBody)
}
