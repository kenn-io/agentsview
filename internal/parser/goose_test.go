package parser

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

const gooseTestSchema = `
	CREATE TABLE schema_version (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	INSERT INTO schema_version (version) VALUES (15);
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		user_set_name BOOLEAN DEFAULT FALSE,
		session_type TEXT NOT NULL DEFAULT 'user',
		working_dir TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		extension_data TEXT DEFAULT '{}',
		total_tokens INTEGER,
		input_tokens INTEGER,
		output_tokens INTEGER,
		cache_read_tokens INTEGER,
		cache_write_tokens INTEGER,
		accumulated_total_tokens INTEGER,
		accumulated_input_tokens INTEGER,
		accumulated_output_tokens INTEGER,
		accumulated_cache_read_tokens INTEGER,
		accumulated_cache_write_tokens INTEGER,
		accumulated_cost REAL,
		schedule_id TEXT,
		recipe_json TEXT,
		user_recipe_values_json TEXT,
		provider_name TEXT,
		model_config_json TEXT,
		goose_mode TEXT NOT NULL DEFAULT 'auto',
		archived_at TIMESTAMP,
		project_id TEXT,
		parent_session_id TEXT
	);
	CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT,
		session_id TEXT NOT NULL REFERENCES sessions(id),
		role TEXT NOT NULL,
		content_json TEXT NOT NULL,
		created_timestamp INTEGER NOT NULL,
		timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		tokens INTEGER,
		metadata_json TEXT
	);
	CREATE INDEX idx_messages_session ON messages(session_id);
	CREATE TABLE usage_ledger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
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
	CREATE INDEX idx_usage_ledger_session ON usage_ledger(session_id);
`

type gooseTestFixture struct {
	pathRoot   string
	sessionDir string
	dbPath     string
	database   *sql.DB
}

func TestGooseDefaultDirs(t *testing.T) {
	def, ok := AgentByType(AgentGoose)
	require.True(t, ok)
	assert.Equal(t, []string{
		".local/share/goose/sessions",
		"AppData/Roaming/Block/goose/data/sessions",
	}, def.DefaultDirs)
}

func newGooseTestFixture(t *testing.T) *gooseTestFixture {
	t.Helper()
	pathRoot := t.TempDir()
	sessionDir := filepath.Join(pathRoot, "data", "sessions")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	dbPath := filepath.Join(sessionDir, GooseDBName)
	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.Exec(gooseTestSchema)
	require.NoError(t, err)
	return &gooseTestFixture{
		pathRoot: pathRoot, sessionDir: sessionDir,
		dbPath: dbPath, database: database,
	}
}

func (fixture *gooseTestFixture) insertSession(
	t *testing.T, id, name, sessionType, parentID string,
) {
	t.Helper()
	_, err := fixture.database.Exec(`
		INSERT INTO sessions (
			id, name, description, session_type, working_dir,
			created_at, updated_at, provider_name, model_config_json,
			project_id, parent_session_id,
			accumulated_total_tokens, accumulated_input_tokens,
			accumulated_output_tokens, accumulated_cache_read_tokens,
			accumulated_cache_write_tokens
		) VALUES (?, ?, '', ?, '/work/acme-app',
			'2023-11-14 22:13:00', '2023-11-14 22:13:25',
			'anthropic', '{"model_name":"claude-sonnet-4-6"}',
			'acme', ?, 0, 0, 0, 0, 0)
	`, id, name, sessionType, nullableGooseTestString(parentID))
	require.NoError(t, err)
}

func (fixture *gooseTestFixture) insertMessage(
	t *testing.T, sessionID, role, content string, created int64,
) {
	t.Helper()
	_, err := fixture.database.Exec(`
		INSERT INTO messages (
			message_id, session_id, role, content_json,
			created_timestamp, metadata_json
		) VALUES (?, ?, ?, ?, ?, '{"userVisible":true,"agentVisible":true}')
	`, fmt.Sprintf("message-%s-%d", role, created), sessionID, role, content, created)
	require.NoError(t, err)
}

func (fixture *gooseTestFixture) insertUsage(
	t *testing.T, sessionID, model string,
	created, input, output, cacheRead, cacheWrite int64,
	cost float64, costSource string, compaction bool,
) {
	t.Helper()
	_, err := fixture.database.Exec(`
		INSERT INTO usage_ledger (
			session_id, created_timestamp, model,
			input_tokens, output_tokens, total_tokens,
			cache_read_tokens, cache_write_tokens,
			cost, cost_source, is_compaction
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, sessionID, created, model, input, output,
		input+output+cacheRead+cacheWrite, cacheRead, cacheWrite,
		cost, costSource, compaction)
	require.NoError(t, err)
}

func TestGooseProviderParsesTranscriptToolsRelationshipsAndUsage(t *testing.T) {
	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "child", "Auth review", "sub_agent", "parent")
	fixture.insertMessage(t, "child", "user", `[
		{"type":"text","text":"Inspect the authentication flow.","annotations":[]}
	]`, 1_700_000_000)
	fixture.insertMessage(t, "child", "assistant", `[
		{"type":"thinking","thinking":"I should inspect auth.go first.","signature":"opaque"},
		{"type":"text","text":"I will inspect the file."},
		{"type":"toolRequest","id":"call-read","toolCall":{"status":"success","value":{"name":"Read","arguments":{"file_path":"auth.go"}}}}
	]`, 1_700_000_001)
	fixture.insertMessage(t, "child", "user", `[
		{"type":"toolResponse","id":"call-read","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"package auth"}]}}},
		{"type":"actionRequired","message":"Approve the proposed edit."}
	]`, 1_700_000_001)
	fixture.insertMessage(t, "child", "assistant", `[
		{"type":"redactedThinking","data":"do-not-display"},
		{"type":"image","data":"do-not-index"},
		{"type":"systemNotification","message":"Finished review."},
		{"type":"futureContent","secret":"ignored"}
	]`, 1_700_000_002)
	_, err := fixture.database.Exec(`
		INSERT INTO messages (
			message_id, session_id, role, content_json,
			created_timestamp, metadata_json
		) VALUES (
			'hidden', 'child', 'assistant',
			'[{"type":"text","text":"internal context"}]',
			1700000003, '{"userVisible":false,"agentVisible":true}'
		)
	`)
	require.NoError(t, err)
	fixture.insertUsage(
		t, "child", "claude-sonnet-4-6",
		1_700_000_010, 100, 20, 30, 10,
		0.0125, "provider_reported", false,
	)
	fixture.insertUsage(
		t, "child", "claude-sonnet-4-6",
		1_700_000_011, 40, 5, 6, 2,
		0.004, "estimated", true,
	)

	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{fixture.pathRoot}, Machine: "devbox",
	})
	require.True(t, ok)
	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, fixture.sessionDir, plan.Roots[0].Path)
	assert.Contains(t, plan.Roots[0].IncludeGlobs, GooseDBName)
	assert.Contains(t, plan.Roots[0].IncludeGlobs, GooseDBName+"-*")

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, fixture.dbPath+"#child", sources[0].DisplayPath)
	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	assert.NotZero(t, fingerprint.MTimeNS)
	assert.Len(t, fingerprint.Hash, 64)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0], Fingerprint: fingerprint, Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	session := result.Session
	assert.Equal(t, "goose:child", session.ID)
	assert.Equal(t, AgentGoose, session.Agent)
	assert.Equal(t, "acme_app", session.Project)
	assert.Equal(t, "Auth review", session.SessionName)
	assert.Equal(t, "Inspect the authentication flow.", session.FirstMessage)
	assert.Equal(t, "goose:parent", session.ParentSessionID)
	assert.Equal(t, RelSubagent, session.RelationshipType)
	assert.Equal(t, "goose-sqlite-v15", session.SourceVersion)
	assert.Equal(t, 4, session.MessageCount)
	assert.Equal(t, 1, session.UserMessageCount)
	assert.Equal(t, fixture.dbPath+"#child", session.File.Path)
	assert.Equal(t, fingerprint.Hash, session.File.Hash)
	assert.Equal(t, 25, session.TotalOutputTokens)
	assert.Equal(t, 140, session.PeakContextTokens)

	require.Len(t, result.Messages, 4)
	assistant := result.Messages[1]
	assert.Equal(t, RoleAssistant, assistant.Role)
	assert.Equal(t, "claude-sonnet-4-6", assistant.Model)
	assert.True(t, assistant.HasThinking)
	assert.Equal(t, "I should inspect auth.go first.", assistant.ThinkingText)
	assert.Contains(t, assistant.Content, "[Thinking]")
	assert.NotContains(t, assistant.Content, "opaque")
	require.Len(t, assistant.ToolCalls, 1)
	assert.Equal(t, "call-read", assistant.ToolCalls[0].ToolUseID)
	assert.Equal(t, "Read", assistant.ToolCalls[0].Category)
	assert.JSONEq(t, `{"file_path":"auth.go"}`, assistant.ToolCalls[0].InputJSON)
	require.Len(t, result.Messages[2].ToolResults, 1)
	assert.Equal(t, "package auth", DecodeContent(result.Messages[2].ToolResults[0].ContentRaw))
	assert.Contains(t, result.Messages[2].Content, "Approve the proposed edit.")
	assert.True(t, result.Messages[3].HasThinking)
	assert.Equal(t, "[Image]\nFinished review.", result.Messages[3].Content)
	assert.NotContains(t, result.Messages[3].Content, "do-not-display")
	assert.NotContains(t, result.Messages[3].Content, "ignored")
	assert.NotContains(t, result.Messages[3].Content, "internal context")

	require.Len(t, result.UsageEvents, 2)
	firstUsage := result.UsageEvents[0]
	assert.Nil(t, firstUsage.MessageOrdinal)
	assert.Equal(t, "goose-request", firstUsage.Source)
	assert.Equal(t, 100, firstUsage.InputTokens)
	assert.Equal(t, 20, firstUsage.OutputTokens)
	assert.Equal(t, 30, firstUsage.CacheReadInputTokens)
	assert.Equal(t, 10, firstUsage.CacheCreationInputTokens)
	require.NotNil(t, firstUsage.Cost)
	assert.Equal(t, money.Money{Microdollars: 12_500}, *firstUsage.Cost)
	assert.Equal(t, "exact", firstUsage.CostStatus)
	assert.Equal(t, "goose-provider-reported", firstUsage.CostSource)
	assert.Contains(t, firstUsage.DedupKey, "ledger_id=1")
	assert.Equal(t, "estimated", result.UsageEvents[1].CostStatus)
	assert.Equal(t, "goose-estimated", result.UsageEvents[1].CostSource)
	assert.Contains(t, result.UsageEvents[1].DedupKey, "is_compaction=true")
}

func TestGooseUsesAccumulatedUsageWhenLedgerIsUnavailable(t *testing.T) {
	fixture := newGooseTestFixture(t)
	_, err := fixture.database.Exec(`DROP TABLE usage_ledger`)
	require.NoError(t, err)
	fixture.insertSession(t, "legacy", "Legacy", "user", "")
	_, err = fixture.database.Exec(`
		UPDATE sessions SET
			accumulated_input_tokens = 90,
			accumulated_output_tokens = 10,
			accumulated_total_tokens = 110,
			accumulated_cache_read_tokens = 8,
			accumulated_cache_write_tokens = 2,
			accumulated_cost = 0.02
		WHERE id = 'legacy'
	`)
	require.NoError(t, err)

	result, err := parseGooseSession(t.Context(), fixture.dbPath, "legacy", "devbox")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.UsageEvents, 1)
	event := result.UsageEvents[0]
	assert.Equal(t, "session", event.Source)
	assert.Equal(t, 90, event.InputTokens)
	assert.Equal(t, 10, event.OutputTokens)
	assert.Equal(t, 8, event.CacheReadInputTokens)
	assert.Equal(t, 2, event.CacheCreationInputTokens)
	require.NotNil(t, event.Cost)
	assert.Equal(t, money.Money{Microdollars: 20_000}, *event.Cost)
	assert.Equal(t, "goose-accumulated", event.CostSource)
}

func TestGooseChangedPathWorkStaysProportionalToNewRows(t *testing.T) {
	for _, sessionCount := range []int{2, 200} {
		t.Run(strconv.Itoa(sessionCount), func(t *testing.T) {
			fixture := newGooseTestFixture(t)
			for i := range sessionCount {
				id := fmt.Sprintf("session-%03d", i)
				fixture.insertSession(t, id, id, "user", "")
				fixture.insertMessage(t, id, "user", `[{"type":"text","text":"seed"}]`, 1_700_000_000)
			}
			provider, ok := NewProvider(AgentGoose, ProviderConfig{
				Roots: []string{fixture.pathRoot}, Machine: "devbox",
			})
			require.True(t, ok)
			_, err := provider.Discover(context.Background())
			require.NoError(t, err)

			fixture.insertMessage(t, "session-000", "assistant", `[{"type":"text","text":"changed"}]`, 1_700_000_001)
			sources, err := provider.SourcesForChangedPath(
				context.Background(), ChangedPathRequest{
					Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
				},
			)
			require.NoError(t, err)
			require.Len(t, sources, 1)
			assert.Equal(t, fixture.dbPath+"#session-000", sources[0].DisplayPath)
		})
	}
}

func TestGooseTailDeletionWatcherWorkStaysBounded(t *testing.T) {
	tests := []struct {
		name   string
		delete func(*testing.T, *gooseTestFixture)
	}{
		{
			name: "session",
			delete: func(t *testing.T, fixture *gooseTestFixture) {
				t.Helper()
				_, err := fixture.database.Exec(`DELETE FROM sessions WHERE id = 'session-tail'`)
				require.NoError(t, err)
			},
		},
		{
			name: "message",
			delete: func(t *testing.T, fixture *gooseTestFixture) {
				t.Helper()
				_, err := fixture.database.Exec(`DELETE FROM messages WHERE session_id = 'session-tail'`)
				require.NoError(t, err)
			},
		},
		{
			name: "usage",
			delete: func(t *testing.T, fixture *gooseTestFixture) {
				t.Helper()
				_, err := fixture.database.Exec(`DELETE FROM usage_ledger WHERE session_id = 'session-tail'`)
				require.NoError(t, err)
			},
		},
	}

	for _, sessionCount := range []int{2, 200} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%d/%s", sessionCount, test.name), func(t *testing.T) {
				fixture := newGooseTestFixture(t)
				for i := 0; i < sessionCount-1; i++ {
					id := fmt.Sprintf("session-%03d", i)
					fixture.insertSession(t, id, id, "user", "")
					fixture.insertMessage(t, id, "user", `[{"type":"text","text":"seed"}]`, 1_700_000_000)
					fixture.insertUsage(t, id, "model", 1_700_000_000, 1, 1, 0, 0, 0, "", false)
				}
				fixture.insertSession(t, "session-tail", "tail", "user", "")
				fixture.insertMessage(t, "session-tail", "user", `[{"type":"text","text":"tail"}]`, 1_700_000_001)
				fixture.insertUsage(t, "session-tail", "model", 1_700_000_001, 1, 1, 0, 0, 0, "", false)

				provider, ok := NewProvider(AgentGoose, ProviderConfig{
					Roots: []string{fixture.pathRoot}, Machine: "devbox",
				})
				require.True(t, ok)
				_, err := provider.Discover(context.Background())
				require.NoError(t, err)

				test.delete(t, fixture)
				sources, err := provider.SourcesForChangedPath(
					context.Background(), ChangedPathRequest{
						Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
					},
				)
				require.NoError(t, err)
				assert.Empty(t, sources,
					"tail deletion must wait for reconciliation instead of enumerating the archive")
			})
		}
	}
}

func TestGooseTailDeleteKeepsInsertsFromOtherTablesInSameWindow(t *testing.T) {
	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session-keep", "keep", "user", "")
	fixture.insertMessage(t, "session-keep", "user", `[{"type":"text","text":"seed"}]`, 1_700_000_000)
	fixture.insertSession(t, "session-tail", "tail", "user", "")

	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{fixture.pathRoot}, Machine: "devbox",
	})
	require.True(t, ok)
	_, err := provider.Discover(context.Background())
	require.NoError(t, err)

	// One debounce window: the newest session row disappears (sessions cursor
	// invalidates) while another session gains a message.
	_, err = fixture.database.Exec(`DELETE FROM sessions WHERE id = 'session-tail'`)
	require.NoError(t, err)
	fixture.insertMessage(t, "session-keep", "assistant", `[{"type":"text","text":"new"}]`, 1_700_000_001)

	sources, err := provider.SourcesForChangedPath(
		context.Background(), ChangedPathRequest{
			Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
		},
	)
	require.NoError(t, err)
	require.Len(t, sources, 1,
		"inserts on tables with intact cursors must survive another table's tail delete")
	assert.Equal(t, fixture.dbPath+"#session-keep", sources[0].DisplayPath)
}

func TestGooseColdWatcherEventCommitsCursorAfterFullEnumeration(t *testing.T) {
	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session-a", "a", "user", "")
	fixture.insertSession(t, "session-b", "b", "user", "")
	fixture.insertMessage(t, "session-a", "user", `[{"type":"text","text":"seed"}]`, 1_700_000_000)

	// No Discover first: the tracker is cold, so the first watcher event must
	// fall back to full enumeration.
	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{fixture.pathRoot}, Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.SourcesForChangedPath(
		context.Background(), ChangedPathRequest{
			Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
		},
	)
	require.NoError(t, err)
	require.Len(t, sources, 2)

	// The successful cold pass published its watermark, so the next event is
	// bounded to the newly inserted rows.
	fixture.insertMessage(t, "session-b", "assistant", `[{"type":"text","text":"new"}]`, 1_700_000_001)
	sources, err = provider.SourcesForChangedPath(
		context.Background(), ChangedPathRequest{
			Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
		},
	)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, fixture.dbPath+"#session-b", sources[0].DisplayPath)
}

func TestGooseDiscoveryWatermarkDoesNotRetreatWatcherCursor(t *testing.T) {
	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session", "Race", "user", "")
	fixture.insertMessage(t, "session", "user", `[{"type":"text","text":"seed"}]`, 1_700_000_000)
	_, err := fixture.database.Exec("PRAGMA journal_mode=WAL")
	require.NoError(t, err)

	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{fixture.pathRoot}, Machine: "devbox",
	})
	require.True(t, ok)
	_, err = provider.Discover(context.Background())
	require.NoError(t, err)

	// A watcher event lands mid-discovery: the row it processes must not be
	// re-listed after the older discovery watermark is stored.
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(t, ok)
	err = discoverer.DiscoverEach(context.Background(), func(SourceRef) error {
		fixture.insertMessage(t, "session", "assistant", `[{"type":"text","text":"mid"}]`, 1_700_000_001)
		sources, err := provider.SourcesForChangedPath(
			context.Background(), ChangedPathRequest{
				Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
			},
		)
		require.NoError(t, err)
		require.Len(t, sources, 1)
		return nil
	})
	require.NoError(t, err)

	sources, err := provider.SourcesForChangedPath(
		context.Background(), ChangedPathRequest{
			Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, sources,
		"rows already delivered to the watcher must not be re-listed after discovery stores its watermark")
}

func TestGooseDiscoveryLeavesConcurrentRowsForWatcherProcessing(t *testing.T) {
	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session", "Race", "user", "")
	fixture.insertMessage(t, "session", "user", `[
		{"type":"text","text":"Initial prompt"}
	]`, 1_700_000_000)
	_, err := fixture.database.Exec("PRAGMA journal_mode=WAL")
	require.NoError(t, err)

	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{fixture.pathRoot}, Machine: "devbox",
	})
	require.True(t, ok)
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(t, ok)
	err = discoverer.DiscoverEach(context.Background(), func(SourceRef) error {
		fixture.insertMessage(t, "session", "assistant", `[
			{"type":"text","text":"Committed during discovery"}
		]`, 1_700_000_001)
		return nil
	})
	require.NoError(t, err)

	sources, err := provider.SourcesForChangedPath(
		context.Background(), ChangedPathRequest{
			Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
		},
	)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, fixture.dbPath+"#session", sources[0].DisplayPath)
}

func TestGooseDiscoveryRejectsUnsupportedMessagesSchema(t *testing.T) {
	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session", "Old", "user", "")
	_, err := fixture.database.Exec(`ALTER TABLE messages DROP COLUMN metadata_json`)
	require.NoError(t, err)

	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{fixture.pathRoot}, Machine: "devbox",
	})
	require.True(t, ok)
	_, err = provider.Discover(context.Background())
	require.ErrorContains(t, err, "unsupported goose messages schema")
	assert.ErrorContains(t, err, "metadata_json")
}

func TestGooseParsesSessionsSchemaWithoutOptionalColumns(t *testing.T) {
	pathRoot := t.TempDir()
	sessionDir := filepath.Join(pathRoot, "data", "sessions")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	dbPath := filepath.Join(sessionDir, GooseDBName)
	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			working_dir TEXT NOT NULL,
			created_at TIMESTAMP,
			updated_at TIMESTAMP
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
		INSERT INTO sessions (id, working_dir, created_at, updated_at)
		VALUES ('bare', '/work/acme-app', '2023-11-14 22:13:00', '2023-11-14 22:13:25');
		INSERT INTO messages (session_id, role, content_json, created_timestamp)
		VALUES ('bare', 'user', '[{"type":"text","text":"Old schema prompt."}]', 1700000000);
	`)
	require.NoError(t, err)

	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{pathRoot}, Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	assert.Len(t, fingerprint.Hash, 64)

	result, err := parseGooseSession(t.Context(), dbPath, "bare", "devbox")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "goose:bare", result.Session.ID)
	assert.Equal(t, "Old schema prompt.", result.Session.FirstMessage)
	assert.Equal(t, "goose-sqlite-v0", result.Session.SourceVersion)
	assert.Empty(t, result.Session.ParentSessionID)
	assert.Empty(t, result.UsageEvents)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, "Old schema prompt.", result.Messages[0].Content)
}

func TestGooseUnknownToolResponseStatusIsNotAHumanMessage(t *testing.T) {
	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session", "Pending", "user", "")
	fixture.insertMessage(t, "session", "user", `[{"type":"text","text":"Run the tool."}]`, 1_700_000_000)
	fixture.insertMessage(t, "session", "user", `[
		{"type":"toolResponse","id":"call-1","toolResult":{"status":"pending"}}
	]`, 1_700_000_001)

	result, err := parseGooseSession(t.Context(), fixture.dbPath, "session", "devbox")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1, result.Session.UserMessageCount,
		"a tool-response carrier with an unknown status must not count as a human message")
	require.Len(t, result.Messages, 2)
	require.Len(t, result.Messages[1].ToolResults, 1)
	assert.Equal(t, "call-1", result.Messages[1].ToolResults[0].ToolUseID)
}

func TestGooseLedgerModelFallsBackToSessionModel(t *testing.T) {
	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session", "Carried", "user", "")
	fixture.insertMessage(t, "session", "user", `[{"type":"text","text":"hi"}]`, 1_700_000_000)
	// Upstream inserts carried_forward rows without a model.
	_, err := fixture.database.Exec(`
		INSERT INTO usage_ledger (
			session_id, created_timestamp, model, input_tokens, output_tokens,
			total_tokens, cache_read_tokens, cache_write_tokens,
			cost, cost_source, is_compaction
		) VALUES ('session', 1700000010, NULL, 50, 5, 55, 0, 0, 0.01, 'carried_forward', 0)
	`)
	require.NoError(t, err)

	result, err := parseGooseSession(t.Context(), fixture.dbPath, "session", "devbox")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.UsageEvents, 1)
	event := result.UsageEvents[0]
	assert.Equal(t, "claude-sonnet-4-6", event.Model,
		"model-less ledger rows must inherit the session model so aggregates keep them")
	assert.Equal(t, "unknown", event.CostStatus)
	assert.Equal(t, "goose-carried-forward", event.CostSource)
	assert.Equal(t, 50, event.InputTokens)
}

func nullableGooseTestString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func TestGooseTimestampAcceptsSecondsMillisecondsAndSQLiteText(t *testing.T) {
	assert.Equal(t, time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC), gooseUnixTimestamp(1_700_000_000))
	assert.Equal(t, time.Date(2023, 11, 14, 22, 13, 20, 123_000_000, time.UTC), gooseUnixTimestamp(1_700_000_000_123))
	assert.Equal(t, time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC), gooseParseTime("2023-11-14 22:13:20"))
}

func TestGooseObservedSessionsDatabase(t *testing.T) {
	sourceDB := os.Getenv("GOOSE_SOURCE_DB")
	if sourceDB == "" {
		t.Skip("set GOOSE_SOURCE_DB to an isolated Goose sessions.db copy")
	}
	raw, err := os.ReadFile(sourceDB)
	require.NoError(t, err)
	dbPath := filepath.Join(t.TempDir(), GooseDBName)
	require.NoError(t, os.WriteFile(dbPath, raw, 0o600))
	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{filepath.Dir(dbPath)}, Machine: "observed-fixture",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, sources)
	for _, source := range sources {
		fingerprint, err := provider.Fingerprint(context.Background(), source)
		require.NoError(t, err)
		outcome, err := provider.Parse(context.Background(), ParseRequest{
			Source: source, Fingerprint: fingerprint, Machine: "observed-fixture",
		})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		result := outcome.Results[0].Result
		assert.Equal(t, AgentGoose, result.Session.Agent)
		assert.Equal(t, len(result.Messages), result.Session.MessageCount)
		assert.NotEmpty(t, result.Session.SourceSessionID)
	}
}
