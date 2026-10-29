package db_test

import (
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

// The upgrade that drops Codex cloud items with no text reparses Codex
// cloud sources in place: the session keeps its identity and source, and
// the empty row an older parser stored is gone.
func TestEmptyCodexCloudItemsUpgradeReparsesInPlace(t *testing.T) {
	scope := db.ScopedDataVersionProviders(151, 152)
	assert.Equal(t, []parser.AgentType{parser.AgentCodexCloud}, scope)
	version := db.CurrentDataVersion()
	t.Cleanup(db.InstallDataVersionScopesForTest(map[int][]parser.AgentType{version: scope}))
	ctx := t.Context()

	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	record := func(kind mirror.Kind, data string) mirror.Record {
		return mirror.Record{Kind: kind, FetchedAt: fetched, Data: jsontext.Value(data)}
	}
	root := t.TempDir()
	source := filepath.Join(root, "account-key", "thread-image.jsonl.zstd")
	_, err := mirror.Append(source, 0, []mirror.Record{
		record(mirror.KindThread,
			`{"id":"thread-image","createdAt":1759320000,"updatedAt":1759320060,"model":"gpt-6","cwd":"/workspace/project-a"}`),
		record(mirror.KindItem,
			`{"turnId":"turn-1","startedAtMs":1759320000000,"item":{"type":"userMessage","id":"u1","content":[{"type":"text","text":"Draw a bird"}]}}`),
		record(mirror.KindItem,
			`{"turnId":"turn-1","startedAtMs":1759320010000,"item":{"type":"imageGeneration","id":"g1","status":"completed"}}`),
		record(mirror.KindItem,
			`{"turnId":"turn-1","startedAtMs":1759320030000,"item":{"type":"agentMessage","id":"a1","text":"","phase":"final_answer"}}`),
	})
	require.NoError(t, err)

	database := dbtest.OpenTestDB(t)
	require.Equal(t, 1, syncCodexCloud(t, database, root).Synced)

	// Reproduce an archive from the version before the upgrade, whose
	// parser stored the empty final answer as a message.
	const sessionID = "codex-cloud:thread-image"
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, fmt.Sprintf(
			`INSERT INTO messages (session_id, ordinal, role, content, timestamp)
			 VALUES ('%s', 99, 'assistant', '', '2025-10-01T12:00:30Z');
			 UPDATE sessions SET data_version=%d; PRAGMA user_version=%d`,
			sessionID, version-1, version-1))
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	database, err = db.OpenIsolated(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	needsSync, fullRebuild, err := db.ArchiveResyncState(ctx, path)
	require.NoError(t, err)
	assert.True(t, needsSync)
	assert.False(t, fullRebuild, "the upgrade must not rebuild the archive")

	assert.Equal(t, 1, syncCodexCloud(t, database, root).Synced)
	messages, err := database.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	var contents []string
	for _, msg := range messages {
		contents = append(contents, msg.Role+": "+msg.Content)
	}
	assert.Equal(t, []string{"user: Draw a bird", "assistant: Generate image"}, contents)
	session, err := database.GetSessionFull(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.FilePath)
	assert.Equal(t, source, *session.FilePath)
	assert.Equal(t, version, session.DataVersion)
	var sessions int
	require.NoError(t, database.Reader().QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&sessions))
	assert.Equal(t, 1, sessions, "the reparse must keep the existing session identity")
}
