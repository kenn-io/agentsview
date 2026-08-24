package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
)

const gooseSyncTestSchema = `
	CREATE TABLE schema_version (version INTEGER PRIMARY KEY);
	INSERT INTO schema_version (version) VALUES (15);
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		session_type TEXT NOT NULL DEFAULT 'user',
		working_dir TEXT NOT NULL,
		created_at TIMESTAMP,
		updated_at TIMESTAMP,
		provider_name TEXT,
		model_config_json TEXT,
		project_id TEXT,
		parent_session_id TEXT,
		accumulated_total_tokens INTEGER,
		accumulated_input_tokens INTEGER,
		accumulated_output_tokens INTEGER,
		accumulated_cache_read_tokens INTEGER,
		accumulated_cache_write_tokens INTEGER,
		accumulated_cost REAL
	);
	CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT,
		session_id TEXT NOT NULL,
		role TEXT NOT NULL,
		content_json TEXT NOT NULL,
		created_timestamp INTEGER NOT NULL,
		timestamp TIMESTAMP,
		tokens INTEGER,
		metadata_json TEXT
	);
	CREATE TABLE usage_ledger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		created_timestamp INTEGER NOT NULL,
		model TEXT,
		input_tokens INTEGER,
		output_tokens INTEGER,
		total_tokens INTEGER,
		cache_read_tokens INTEGER,
		cache_write_tokens INTEGER,
		cost REAL,
		cost_source TEXT,
		is_compaction INTEGER DEFAULT 0
	);
`

func TestSyncGooseTranscriptAndChangedDatabase(t *testing.T) {
	pathRoot, dbPath, sourceDB := writeSyncGooseDB(t)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGoose: {pathRoot},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)

	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})

	session, err := database.GetSession(context.Background(), "goose:session-001")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 2, session.MessageCount)
	assert.Equal(t, "acme_app", session.Project)
	assert.Equal(t, dbPath+"#session-001", database.GetSessionFilePath("goose:session-001"))

	messages, err := database.GetMessages(context.Background(), "goose:session-001", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "Inspect the auth flow.", messages[0].Content)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "Read", messages[1].ToolCalls[0].ToolName)

	usage, err := database.GetUsageEvents(context.Background(), "goose:session-001")
	require.NoError(t, err)
	require.Len(t, usage, 2)
	assert.Equal(t, "goose-request", usage[0].Source)
	assert.Equal(t, 100, usage[0].InputTokens)
	assert.Equal(t, 20, usage[0].OutputTokens)
	require.NotNil(t, usage[0].Cost)
	assert.Equal(t, money.Money{Microdollars: 12_500}, *usage[0].Cost)
	assert.Equal(t, "goose-request", usage[1].Source)
	assert.Equal(t, 25, usage[1].InputTokens)
	assert.Nil(t, usage[1].Cost)

	daily, err := database.GetDailyUsage(context.Background(), db.UsageFilter{
		From: "2023-11-14", To: "2023-11-14", Agent: "goose", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 125, daily.Totals.InputTokens)
	assert.Equal(t, 25, daily.Totals.OutputTokens)
	assert.Equal(t, 11, daily.Totals.CacheCreationTokens)
	assert.Equal(t, 24, daily.Totals.CacheReadTokens)

	reportQuery, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2023-11-14", Timezone: "UTC",
	}, time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	report, err := database.GetActivityReport(
		context.Background(), db.AnalyticsFilter{Timezone: "UTC"}, reportQuery,
	)
	require.NoError(t, err)
	assert.Equal(t, 25, report.Totals.OutputTokens)

	runSyncAndAssert(t, engine, SyncStats{})

	_, err = sourceDB.Exec(`UPDATE sessions SET name = 'Renamed review' WHERE id = 'session-001'`)
	require.NoError(t, err)
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})
	storedName, found, err := database.GetSessionName(
		context.Background(), "goose:session-001",
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "Renamed review", storedName)

	insertSyncGooseMessage(t, sourceDB, "assistant", `[{"type":"text","text":"Review complete."}]`, 1_700_000_002)
	require.NoError(t, engine.SyncPathsContext(context.Background(), []string{dbPath}))
	messages, err = database.GetMessages(context.Background(), "goose:session-001", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	assert.Equal(t, "Review complete.", messages[2].Content)

	_, err = sourceDB.Exec(`DELETE FROM messages WHERE id = 1`)
	require.NoError(t, err)
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})
	messages, err = database.GetMessages(context.Background(), "goose:session-001", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "I will inspect the file.", messages[0].Content)

	_, err = sourceDB.Exec(`
		DELETE FROM usage_ledger WHERE session_id = 'session-001';
		DELETE FROM messages WHERE session_id = 'session-001';
		DELETE FROM sessions WHERE id = 'session-001';
	`)
	require.NoError(t, err)
	require.NoError(t, engine.ReconcileProviderRoots(
		context.Background(), parser.AgentGoose, []string{pathRoot},
	))
	active, err := database.GetSession(context.Background(), "goose:session-001")
	require.NoError(t, err)
	assert.NotNil(t, active)
	archived, err := database.GetSessionFull(context.Background(), "goose:session-001")
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
}

func TestReconcileProviderRootsGooseSkipsUnchangedSession(t *testing.T) {
	pathRoot, _, _ := writeSyncGooseDB(t)
	engine := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGoose: {pathRoot},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})

	writtenSessions := 0
	engine.writeBatchOverride = func(
		batch []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		writtenSessions += len(batch)
		return len(batch), 0, 0, 0
	}

	require.NoError(t, engine.ReconcileProviderRoots(
		context.Background(), parser.AgentGoose, []string{pathRoot},
	))
	assert.Zero(t, writtenSessions,
		"unchanged Goose sessions must not be rewritten during reconciliation")
}

func TestSyncGoosePreservesHumanUserMessageCount(t *testing.T) {
	pathRoot, _, sourceDB := writeSyncGooseDB(t)
	insertSyncGooseMessage(t, sourceDB, "user", `[
		{"type":"toolResponse","id":"call-read","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"package auth"}]}}},
		{"type":"actionRequired","message":"Approve the proposed edit."}
	]`, 1_700_000_002)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGoose: {pathRoot},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)

	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})
	session, err := database.GetSession(context.Background(), "goose:session-001")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 1, session.UserMessageCount,
		"tool-response carriers must not count as human user messages")
}

func TestProcessFileGooseChangedDatabaseUsesOneVirtualSource(t *testing.T) {
	pathRoot, dbPath, sourceDB := writeSyncGooseDB(t)
	// A second, never-changed session proves the classification is bounded to
	// the changed rows instead of fanning out to every stored session.
	_, err := sourceDB.Exec(`
		INSERT INTO sessions (
			id, name, session_type, working_dir, created_at, updated_at,
			provider_name, model_config_json, project_id,
			accumulated_total_tokens, accumulated_input_tokens,
			accumulated_output_tokens, accumulated_cache_read_tokens,
			accumulated_cache_write_tokens
		) VALUES (
			'session-002', 'Untouched', 'user', '/work/acme-app',
			'2023-11-14 22:13:00', '2023-11-14 22:13:25',
			'anthropic', '{"model_name":"claude-sonnet-4-6"}', 'acme',
			0, 0, 0, 0, 0
		)
	`)
	require.NoError(t, err)
	engine := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGoose: {pathRoot},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 2, Synced: 2})

	insertSyncGooseMessage(t, sourceDB, "assistant", `[{"type":"text","text":"Changed."}]`, 1_700_000_002)
	files := requireClassifyProviderChangedPath(t, engine, dbPath)
	require.Len(t, files, 1,
		"only the session with new rows may be classified for parsing")
	assert.Equal(t, parser.AgentGoose, files[0].Agent)
	assert.Equal(t, dbPath+"#session-001", files[0].Path)
	assert.False(t, files[0].ForceParse,
		"goose relies on the fingerprint hash gate instead of forced parses")

	result := engine.processFile(context.Background(), files[0])
	require.NoError(t, result.err)
	require.Len(t, result.results, 1)
	require.Len(t, result.results[0].Messages, 3)
	assert.Equal(t, "Changed.", result.results[0].Messages[2].Content)
}

func TestSyncAllGooseMissingDBPreservesArchive(t *testing.T) {
	pathRoot, dbPath, sourceDB := writeSyncGooseDB(t)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGoose: {pathRoot},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})

	require.NoError(t, sourceDB.Close())
	require.NoError(t, os.Remove(dbPath))
	runSyncAndAssert(t, engine, SyncStats{})
	require.NoError(t, engine.ReconcileProviderRoots(
		context.Background(), parser.AgentGoose, []string{pathRoot},
	))

	session, err := database.GetSession(context.Background(), "goose:session-001")
	require.NoError(t, err)
	require.NotNil(t, session,
		"a vanished sessions.db must not tombstone archived Goose sessions")
	messages, err := database.GetMessages(context.Background(), "goose:session-001", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "Inspect the auth flow.", messages[0].Content)
}

func writeSyncGooseDB(t *testing.T) (string, string, *sql.DB) {
	t.Helper()
	pathRoot := t.TempDir()
	sessionsDir := filepath.Join(pathRoot, "data", "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	dbPath := filepath.Join(sessionsDir, parser.GooseDBName)
	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.Exec(gooseSyncTestSchema)
	require.NoError(t, err)
	_, err = database.Exec(`
		INSERT INTO sessions (
			id, name, session_type, working_dir, created_at, updated_at,
			provider_name, model_config_json, project_id,
			accumulated_total_tokens, accumulated_input_tokens,
			accumulated_output_tokens, accumulated_cache_read_tokens,
			accumulated_cache_write_tokens
		) VALUES (
			'session-001', 'Auth review', 'user', '/work/acme-app',
			'2023-11-14 22:13:00', '2023-11-14 22:13:25',
			'anthropic', '{"model_name":"claude-sonnet-4-6"}', 'acme',
			0, 0, 0, 0, 0
		)
	`)
	require.NoError(t, err)
	insertSyncGooseMessage(t, database, "user", `[{"type":"text","text":"Inspect the auth flow."}]`, 1_700_000_000)
	insertSyncGooseMessage(t, database, "assistant", `[
		{"type":"text","text":"I will inspect the file."},
		{"type":"toolRequest","id":"call-read","toolCall":{"status":"success","value":{"name":"Read","arguments":{"file_path":"auth.go"}}}}
	]`, 1_700_000_001)
	_, err = database.Exec(`
		INSERT INTO usage_ledger (
			session_id, created_timestamp, model, input_tokens,
			output_tokens, total_tokens, cache_read_tokens,
			cache_write_tokens, cost, cost_source, is_compaction
		) VALUES (
			'session-001', 1700000010, 'claude-sonnet-4-6',
			100, 20, 150, 20, 10, 0.0125, 'provider_reported', 0
		), (
			'session-001', 1700000011, 'claude-sonnet-4-6',
			25, 5, 35, 4, 1, NULL, '', 0
		)
	`)
	require.NoError(t, err)
	return pathRoot, dbPath, database
}

func insertSyncGooseMessage(
	t *testing.T, database *sql.DB, role, content string, created int64,
) {
	t.Helper()
	_, err := database.Exec(`
		INSERT INTO messages (
			message_id, session_id, role, content_json,
			created_timestamp, timestamp, metadata_json
		) VALUES (?, 'session-001', ?, ?, ?, '2023-11-14 22:13:20', '{}')
	`, role, role, content, created)
	require.NoError(t, err)
}
