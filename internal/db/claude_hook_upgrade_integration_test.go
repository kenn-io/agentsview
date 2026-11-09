package db_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestClaudeHookOutputUpgradePreservesArchive(t *testing.T) {
	ctx := t.Context()
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "project", "c0000000-0000-4000-8000-000000000101.jsonl")
	dbtest.WriteTestFile(t, path, []byte(`{"type":"user","uuid":"u1","sessionId":"c0000000-0000-4000-8000-000000000101","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"request"}}
{"type":"attachment","uuid":"h1","parentUuid":"u1","timestamp":"2024-01-01T00:00:01Z","attachment":{"type":"hook_success","content":"hook output"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2024-01-01T00:00:02Z","message":{"id":"answer-1","role":"assistant","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn"}}
`))
	cfg := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}
	engine := sync.NewEngine(ctx, database, cfg)
	first := engine.SyncAll(ctx, nil)
	engine.Close()
	require.Zero(t, first.Failed)
	var id string
	require.NoError(t, database.Reader().QueryRow(ctx, "SELECT id FROM sessions WHERE file_path = ?", path).Scan(&id))
	parsed, err := database.GetAllMessages(ctx, id)
	require.NoError(t, err)
	require.Len(t, parsed, 3)
	legacy := []db.Message{parsed[0], parsed[2]}
	legacy[1].Ordinal = 1
	require.NoError(t, database.ReplaceSessionMessages(ctx, id, legacy))
	legacy, err = database.GetAllMessages(ctx, id)
	require.NoError(t, err)
	require.Len(t, legacy, 2)
	_, err = database.PinMessage(ctx, id, legacy[1].ID, nil)
	require.NoError(t, err)
	// Version 166 keeps the peer-message rows but still lacks hook rows.
	const previous = 166
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE sessions SET data_version = ?, message_count = 2, user_message_count = 1 WHERE id = ?", previous, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", previous))
		return err
	}))
	// The upgrade from 166 covers exactly the providers whose stored output
	// the hook rows change.
	assert.ElementsMatch(t,
		[]parser.AgentType{
			parser.AgentClaude,
			parser.AgentCowork,
			parser.AgentQoder,
			parser.AgentIcodemate,
		},
		db.ScopedDataVersionProviders(previous, previous+1),
	)

	archivePath := database.Path()
	require.NoError(t, database.Close())
	// A scoped upgrade must not ask for a full rebuild: the archive only
	// needs the affected sources reparsed by an ordinary sync pass.
	needsResync, fullRebuild, err := db.ArchiveResyncState(ctx, archivePath)
	require.NoError(t, err)
	assert.True(t, needsResync)
	assert.False(t, fullRebuild)
	reopened, err := db.OpenIsolated(ctx, archivePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	// The writable reopen promotes the scoped upgrade instead of marking the
	// archive stale for a rebuild.
	assert.False(t, reopened.NeedsResync())
	upgraded := sync.NewEngine(ctx, reopened, cfg)
	t.Cleanup(upgraded.Close)
	stats := upgraded.SyncAll(ctx, nil)
	require.Zero(t, stats.Failed)
	after, err := reopened.GetAllMessages(ctx, id)
	require.NoError(t, err)
	require.Len(t, after, 3)
	assert.Equal(t, []string{"u1", "h1", "a1"}, []string{after[0].SourceUUID, after[1].SourceUUID, after[2].SourceUUID})
	assert.Equal(t, []int{0, 1, 2}, []int{after[0].Ordinal, after[1].Ordinal, after[2].Ordinal})
	assert.True(t, after[1].IsSystem)
	assert.Equal(t, "hook_success", after[1].SourceSubtype)
	assert.Equal(t, "Content:\nhook output", after[1].Content)
	pins, err := reopened.ListPinnedMessages(ctx, id, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, after[2].ID, pins[0].MessageID)
	assert.Equal(t, 2, pins[0].Ordinal)
}
