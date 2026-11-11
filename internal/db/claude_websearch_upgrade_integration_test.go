package db_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// Archives parsed before version 171 stored each record of a split response
// with only its own web searches. The scoped upgrade reparses Claude-format
// sources so every record carries the response's total.
func TestClaudeSplitResponseWebSearchUpgradeRestoresTotal(t *testing.T) {
	ctx := t.Context()
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "project", "c0000000-0000-4000-8000-000000000169.jsonl")
	dbtest.WriteTestFile(t, path, []byte(`{"type":"user","uuid":"u1","sessionId":"c0000000-0000-4000-8000-000000000169","timestamp":"2026-07-30T10:00:00Z","message":{"role":"user","content":"find something"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-07-30T10:00:01Z","message":{"id":"msg_1","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"tool_use","id":"toolu_s1","name":"WebSearch","input":{"query":"a"}}],"usage":{"input_tokens":10,"output_tokens":5,"server_tool_use":{"web_search_requests":0}}}}
{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"2026-07-30T10:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_s1","content":"hits"}]},"toolUseResult":{"query":"a","searchCount":2}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","timestamp":"2026-07-30T10:00:05Z","message":{"id":"msg_1","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"tool_use","id":"toolu_s2","name":"WebSearch","input":{"query":"b"}}],"usage":{"input_tokens":10,"output_tokens":5,"server_tool_use":{"web_search_requests":0}}}}
{"type":"user","uuid":"u3","parentUuid":"a2","timestamp":"2026-07-30T10:00:08Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_s2","content":"hits"}]},"toolUseResult":{"query":"b","searchCount":3}}
`))
	cfg := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}
	engine := sync.NewEngine(ctx, database, cfg)
	first := engine.SyncAll(ctx, nil)
	engine.Close()
	require.Zero(t, first.Failed)
	var id string
	require.NoError(t, database.Reader().QueryRow(ctx, "SELECT id FROM sessions WHERE file_path = ?", path).Scan(&id))

	searches := func(d *db.DB) map[string]int64 {
		t.Helper()
		messages, err := d.GetAllMessages(ctx, id)
		require.NoError(t, err)
		out := map[string]int64{}
		for _, m := range messages {
			if m.Role == "assistant" {
				out[m.SourceUUID] = gjson.GetBytes(m.TokenUsage, "server_tool_use.web_search_requests").Int()
			}
		}
		return out
	}
	require.Equal(t, map[string]int64{"a1": 5, "a2": 5}, searches(database))

	// Model version 170, which stored each record's own searches, without
	// changing the source fingerprint.
	const previous = 170
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		for uuid, count := range map[string]int{"a1": 2, "a2": 3} {
			if _, err := tx.ExecContext(ctx, `UPDATE messages
				SET token_usage = json_set(token_usage, '$.server_tool_use.web_search_requests', ?)
				WHERE session_id = ? AND source_uuid = ?`, count, id, uuid); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE sessions SET data_version = ? WHERE id = ?", previous, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", previous))
		return err
	}))
	require.Equal(t, map[string]int64{"a1": 2, "a2": 3}, searches(database))
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
	needsResync, fullRebuild, err := db.ArchiveResyncState(ctx, archivePath)
	require.NoError(t, err)
	assert.True(t, needsResync)
	assert.False(t, fullRebuild)
	reopened, err := db.OpenIsolated(ctx, archivePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	assert.False(t, reopened.NeedsResync())
	upgraded := sync.NewEngine(ctx, reopened, cfg)
	t.Cleanup(upgraded.Close)
	require.Zero(t, upgraded.SyncAll(ctx, nil).Failed)

	assert.Equal(t, map[string]int64{"a1": 5, "a2": 5}, searches(reopened))
	var sessionID string
	require.NoError(t, reopened.Reader().QueryRow(ctx, "SELECT id FROM sessions WHERE file_path = ?", path).Scan(&sessionID))
	assert.Equal(t, id, sessionID, "the reparse keeps the session identity")
}
