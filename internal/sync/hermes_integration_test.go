package sync_test

import (
	"database/sql"
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

func TestSyncPathsHermesStateDBEventRefreshesArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := t.TempDir()
	stateDB := writeHermesSyncStateDB(t, root)
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {filepath.Join(root, "sessions")},
		},
		Machine: "local",
	})

	engine.SyncPaths([]string{stateDB})

	assertSessionState(t, database, "hermes:child", func(sess *db.Session) {
		assert.Equal(t, string(parser.AgentHermes), sess.Agent)
		assert.Equal(t, "hermes-discord", sess.Project)
		require.NotNil(t, sess.DisplayName)
		assert.Equal(t, "Child Session", *sess.DisplayName)
	})
}

func TestSyncPathsHermesArchiveTranscriptEventRefreshesArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := t.TempDir()
	stateDB := writeHermesSyncStateDB(t, root)
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {filepath.Join(root, "sessions")},
		},
		Machine: "local",
	})
	engine.SyncPaths([]string{stateDB})
	assertSessionState(t, database, "hermes:child", nil)

	transcriptPath := filepath.Join(root, "sessions", "extra.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
	require.NoError(t, os.WriteFile(
		transcriptPath,
		[]byte(
			`{"role":"session_meta","platform":"cli","timestamp":"2026-05-14T10:00:00.000000"}`+"\n"+
				`{"role":"user","content":"new transcript","timestamp":"2026-05-14T10:01:00.000000"}`+"\n"+
				`{"role":"assistant","content":"Done.","timestamp":"2026-05-14T10:02:00.000000"}`+"\n",
		),
		0o644,
	))

	engine.SyncPaths([]string{transcriptPath})

	assertSessionState(t, database, "hermes:extra", func(sess *db.Session) {
		require.NotNil(t, sess.FirstMessage)
		assert.Equal(t, "new transcript", *sess.FirstMessage)
	})
}

func writeHermesSyncStateDB(t *testing.T, root string) string {
	t.Helper()
	stateDB := filepath.Join(root, "state.db")
	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, err = conn.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			user_id TEXT,
			model TEXT,
			model_config TEXT,
			system_prompt TEXT,
			parent_session_id TEXT,
			started_at REAL NOT NULL,
			ended_at REAL,
			end_reason TEXT,
			message_count INTEGER DEFAULT 0,
			tool_call_count INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			billing_provider TEXT,
			billing_base_url TEXT,
			billing_mode TEXT,
			estimated_cost_usd REAL,
			actual_cost_usd REAL,
			cost_status TEXT,
			cost_source TEXT,
			pricing_version TEXT,
			title TEXT,
			api_call_count INTEGER DEFAULT 0
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT,
			tool_call_id TEXT,
			tool_calls TEXT,
			tool_name TEXT,
			timestamp REAL NOT NULL,
			token_count INTEGER,
			finish_reason TEXT,
			reasoning TEXT,
			reasoning_content TEXT,
			reasoning_details TEXT,
			codex_reasoning_items TEXT,
			codex_message_items TEXT
		);
		INSERT INTO sessions (
			id, source, model, parent_session_id, started_at, ended_at,
			message_count, input_tokens, output_tokens, cache_read_tokens,
			cache_write_tokens, reasoning_tokens, estimated_cost_usd,
			cost_status, cost_source, title, api_call_count
		) VALUES (
			'child', 'discord', 'gpt-5.4', 'parent',
			1778767200.0, 1778767800.0, 1, 300, 70, 20, 5, 9,
			0.123, 'estimated', 'hermes', 'Child Session', 4
		);
		INSERT INTO messages (
			session_id, role, content, timestamp
		) VALUES (
			'child', 'user', 'state db only has one message', 1778767210.0
		);
	`)
	require.NoError(t, err)
	return stateDB
}
