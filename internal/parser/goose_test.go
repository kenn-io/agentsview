package parser

import (
	"database/sql"
	"errors"
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
	_, err = database.ExecContext(t.Context(), gooseTestSchema)
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
	_, err := fixture.database.ExecContext(t.Context(), `
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
	_, err := fixture.database.ExecContext(t.Context(), `
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
	_, err := fixture.database.ExecContext(t.Context(), `
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
	assert := assert.New(t)
	require := require.New(t)

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
	_, err := fixture.database.ExecContext(t.Context(), `
		INSERT INTO messages (
			message_id, session_id, role, content_json,
			created_timestamp, metadata_json
		) VALUES (
			'hidden', 'child', 'assistant',
			'[{"type":"text","text":"internal context"}]',
			1700000003, '{"userVisible":false,"agentVisible":true}'
		)
	`)
	require.NoError(err)
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
	require.True(ok)
	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(fixture.sessionDir, plan.Roots[0].Path)
	assert.Contains(plan.Roots[0].IncludeGlobs, GooseDBName)
	assert.Contains(plan.Roots[0].IncludeGlobs, GooseDBName+"-*")

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(fixture.dbPath+"#child", sources[0].DisplayPath)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	assert.NotZero(fingerprint.MTimeNS)
	assert.Len(fingerprint.Hash, 64)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Fingerprint: fingerprint, Machine: "devbox",
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0].Result
	session := result.Session
	assert.Equal("goose:child", session.ID)
	assert.Equal(AgentGoose, session.Agent)
	assert.Equal("acme_app", session.Project)
	assert.Equal("Auth review", session.SessionName)
	assert.Equal("Inspect the authentication flow.", session.FirstMessage)
	assert.Equal("goose:parent", session.ParentSessionID)
	assert.Equal(RelSubagent, session.RelationshipType)
	assert.Equal("goose-sqlite-v15", session.SourceVersion)
	assert.Equal(4, session.MessageCount)
	assert.Equal(1, session.UserMessageCount)
	assert.Equal(fixture.dbPath+"#child", session.File.Path)
	assert.Equal(fingerprint.Hash, session.File.Hash)
	assert.Equal(25, session.TotalOutputTokens)
	assert.Equal(140, session.PeakContextTokens)

	require.Len(result.Messages, 4)
	assistant := result.Messages[1]
	assert.Equal(RoleAssistant, assistant.Role)
	assert.Equal("claude-sonnet-4-6", assistant.Model)
	assert.True(assistant.HasThinking)
	assert.Equal("I should inspect auth.go first.", assistant.ThinkingText)
	assert.Contains(assistant.Content, "[Thinking]")
	assert.NotContains(assistant.Content, "opaque")
	require.Len(assistant.ToolCalls, 1)
	assert.Equal("call-read", assistant.ToolCalls[0].ToolUseID)
	assert.Equal("Read", assistant.ToolCalls[0].Category)
	assert.JSONEq(`{"file_path":"auth.go"}`, assistant.ToolCalls[0].InputJSON)
	require.Len(result.Messages[2].ToolResults, 1)
	assert.Equal("package auth", DecodeContent(result.Messages[2].ToolResults[0].ContentRaw))
	assert.Contains(result.Messages[2].Content, "Approve the proposed edit.")
	assert.True(result.Messages[3].HasThinking)
	assert.Equal("[Image]\nFinished review.", result.Messages[3].Content)
	assert.NotContains(result.Messages[3].Content, "do-not-display")
	assert.NotContains(result.Messages[3].Content, "ignored")
	assert.NotContains(result.Messages[3].Content, "internal context")

	require.Len(result.UsageEvents, 2)
	firstUsage := result.UsageEvents[0]
	assert.Nil(firstUsage.MessageOrdinal)
	assert.Equal("goose-request", firstUsage.Source)
	assert.Equal(100, firstUsage.InputTokens)
	assert.Equal(20, firstUsage.OutputTokens)
	assert.Equal(30, firstUsage.CacheReadInputTokens)
	assert.Equal(10, firstUsage.CacheCreationInputTokens)
	require.NotNil(firstUsage.Cost)
	assert.Equal(money.Money{Microdollars: 12_500}, *firstUsage.Cost)
	assert.Equal("exact", firstUsage.CostStatus)
	assert.Equal("goose-provider-reported", firstUsage.CostSource)
	assert.Contains(firstUsage.DedupKey, "ledger_id=1")
	assert.Equal("estimated", result.UsageEvents[1].CostStatus)
	assert.Equal("goose-estimated", result.UsageEvents[1].CostSource)
	assert.Contains(result.UsageEvents[1].DedupKey, "is_compaction=true")
}

func TestGooseUsesAccumulatedUsageWhenLedgerIsUnavailable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newGooseTestFixture(t)
	_, err := fixture.database.ExecContext(t.Context(), `DROP TABLE usage_ledger`)
	require.NoError(err)
	fixture.insertSession(t, "legacy", "Legacy", "user", "")
	_, err = fixture.database.ExecContext(t.Context(), `
		UPDATE sessions SET
			accumulated_input_tokens = 90,
			accumulated_output_tokens = 10,
			accumulated_total_tokens = 110,
			accumulated_cache_read_tokens = 8,
			accumulated_cache_write_tokens = 2,
			accumulated_cost = 0.02
		WHERE id = 'legacy'
	`)
	require.NoError(err)

	result, err := parseGooseSession(t.Context(), fixture.dbPath, "legacy", "devbox", false)
	require.NoError(err)
	require.NotNil(result)
	require.Len(result.UsageEvents, 1)
	event := result.UsageEvents[0]
	assert.Equal("session", event.Source)
	assert.Equal(90, event.InputTokens)
	assert.Equal(10, event.OutputTokens)
	assert.Equal(8, event.CacheReadInputTokens)
	assert.Equal(2, event.CacheCreationInputTokens)
	require.NotNil(event.Cost)
	assert.Equal(money.Money{Microdollars: 20_000}, *event.Cost)
	assert.Equal("goose-accumulated", event.CostSource)
}

func TestGooseChangedPathWorkStaysProportionalToNewRows(t *testing.T) {
	for _, sessionCount := range []int{2, 200} {
		t.Run(strconv.Itoa(sessionCount), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			fixture := newGooseTestFixture(t)
			for i := range sessionCount {
				id := fmt.Sprintf("session-%03d", i)
				fixture.insertSession(t, id, id, "user", "")
				fixture.insertMessage(t, id, "user", `[{"type":"text","text":"seed"}]`, 1_700_000_000)
			}
			factory, ok := ProviderFactoryByType(AgentGoose)
			require.True(ok)
			provider := factory.NewProvider(ProviderConfig{
				Roots: []string{fixture.pathRoot}, Machine: "devbox",
			})
			scans := 0
			ctx := WithSharedContainerScanObserver(t.Context(), func() { scans++ })
			_, err := provider.Discover(ctx)
			require.NoError(err)
			require.Equal(1, scans, "discovery must report its full container scan")
			scans = 0

			fixture.insertMessage(t, "session-000", "assistant", `[{"type":"text","text":"changed"}]`, 1_700_000_001)
			// The engine creates short-lived providers; the factory must retain
			// the discovery cursor rather than making each event a cold scan.
			provider = factory.NewProvider(ProviderConfig{Roots: []string{fixture.pathRoot}})
			sources, err := provider.SourcesForChangedPath(
				ctx, ChangedPathRequest{
					Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
				},
			)
			require.NoError(err)
			require.Len(sources, 1)
			assert.Equal(fixture.dbPath+"#session-000", sources[0].DisplayPath)
			assert.Zero(scans, "a warm watcher event must not enumerate the container")
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
				_, err := fixture.database.ExecContext(t.Context(), `DELETE FROM sessions WHERE id = 'session-tail'`)
				require.NoError(t, err)
			},
		},
		{
			name: "message",
			delete: func(t *testing.T, fixture *gooseTestFixture) {
				t.Helper()
				_, err := fixture.database.ExecContext(t.Context(), `DELETE FROM messages WHERE session_id = 'session-tail'`)
				require.NoError(t, err)
			},
		},
		{
			name: "usage",
			delete: func(t *testing.T, fixture *gooseTestFixture) {
				t.Helper()
				_, err := fixture.database.ExecContext(t.Context(), `DELETE FROM usage_ledger WHERE session_id = 'session-tail'`)
				require.NoError(t, err)
			},
		},
	}

	for _, sessionCount := range []int{2, 200} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%d/%s", sessionCount, test.name), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				fixture := newGooseTestFixture(t)
				for i := range sessionCount - 1 {
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
				require.True(ok)
				scans := 0
				ctx := WithSharedContainerScanObserver(t.Context(), func() { scans++ })
				_, err := provider.Discover(ctx)
				require.NoError(err)
				require.Equal(1, scans)
				scans = 0

				test.delete(t, fixture)
				sources, err := provider.SourcesForChangedPath(
					ctx, ChangedPathRequest{
						Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
					},
				)
				require.NoError(err)
				assert.Empty(sources,
					"tail deletion must wait for reconciliation instead of enumerating the archive")
				assert.Zero(scans)
			})
		}
	}
}

func TestGooseTailDeleteKeepsInsertsFromOtherTablesInSameWindow(t *testing.T) {
	require := require.New(t)

	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session-keep", "keep", "user", "")
	fixture.insertMessage(t, "session-keep", "user", `[{"type":"text","text":"seed"}]`, 1_700_000_000)
	fixture.insertSession(t, "session-tail", "tail", "user", "")

	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{fixture.pathRoot}, Machine: "devbox",
	})
	require.True(ok)
	_, err := provider.Discover(t.Context())
	require.NoError(err)

	// One debounce window: the newest session row disappears (sessions cursor
	// invalidates) while another session gains a message.
	_, err = fixture.database.ExecContext(t.Context(), `DELETE FROM sessions WHERE id = 'session-tail'`)
	require.NoError(err)
	fixture.insertMessage(t, "session-keep", "assistant", `[{"type":"text","text":"new"}]`, 1_700_000_001)

	sources, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{
			Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
		},
	)
	require.NoError(err)
	require.Len(sources, 1,
		"inserts on tables with intact cursors must survive another table's tail delete")
	assert.Equal(t, fixture.dbPath+"#session-keep", sources[0].DisplayPath)
}

func TestGooseColdWatcherEventCommitsCursorAfterFullEnumeration(t *testing.T) {
	require := require.New(t)

	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session-a", "a", "user", "")
	fixture.insertSession(t, "session-b", "b", "user", "")
	fixture.insertMessage(t, "session-a", "user", `[{"type":"text","text":"seed"}]`, 1_700_000_000)

	// No Discover first: the tracker is cold, so the first watcher event must
	// fall back to full enumeration.
	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{fixture.pathRoot}, Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{
			Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
		},
	)
	require.NoError(err)
	require.Len(sources, 2)

	// The successful cold pass published its watermark, so the next event is
	// bounded to the newly inserted rows.
	fixture.insertMessage(t, "session-b", "assistant", `[{"type":"text","text":"new"}]`, 1_700_000_001)
	sources, err = provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{
			Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
		},
	)
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(t, fixture.dbPath+"#session-b", sources[0].DisplayPath)
}

func TestGooseDiscoveryWatermarkDoesNotRetreatWatcherCursor(t *testing.T) {
	require := require.New(t)

	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session", "Race", "user", "")
	fixture.insertMessage(t, "session", "user", `[{"type":"text","text":"seed"}]`, 1_700_000_000)
	_, err := fixture.database.ExecContext(t.Context(), "PRAGMA journal_mode=WAL")
	require.NoError(err)

	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{fixture.pathRoot}, Machine: "devbox",
	})
	require.True(ok)
	_, err = provider.Discover(t.Context())
	require.NoError(err)

	// A watcher event lands mid-discovery: the row it processes must not be
	// re-listed after the older discovery watermark is stored.
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(ok)
	err = discoverer.DiscoverEach(t.Context(), func(SourceRef) error {
		fixture.insertMessage(t, "session", "assistant", `[{"type":"text","text":"mid"}]`, 1_700_000_001)
		sources, err := provider.SourcesForChangedPath(
			t.Context(), ChangedPathRequest{
				Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
			},
		)
		require.NoError(err)
		require.Len(sources, 1)
		return nil
	})
	require.NoError(err)

	sources, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{
			Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
		},
	)
	require.NoError(err)
	assert.Empty(t, sources,
		"rows already delivered to the watcher must not be re-listed after discovery stores its watermark")
}

func TestGooseDiscoveryLeavesConcurrentRowsForWatcherProcessing(t *testing.T) {
	require := require.New(t)

	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session", "Race", "user", "")
	fixture.insertMessage(t, "session", "user", `[
		{"type":"text","text":"Initial prompt"}
	]`, 1_700_000_000)
	_, err := fixture.database.ExecContext(t.Context(), "PRAGMA journal_mode=WAL")
	require.NoError(err)

	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{fixture.pathRoot}, Machine: "devbox",
	})
	require.True(ok)
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(ok)
	err = discoverer.DiscoverEach(t.Context(), func(SourceRef) error {
		fixture.insertMessage(t, "session", "assistant", `[
			{"type":"text","text":"Committed during discovery"}
		]`, 1_700_000_001)
		return nil
	})
	require.NoError(err)

	sources, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{
			Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
		},
	)
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(t, fixture.dbPath+"#session", sources[0].DisplayPath)
}

func TestGooseFailedDiscoveryDoesNotPublishWatermark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session-a", "a", "user", "")
	fixture.insertSession(t, "session-b", "b", "user", "")
	provider, ok := NewProvider(AgentGoose, ProviderConfig{Roots: []string{fixture.pathRoot}})
	require.True(ok)
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(ok)
	stop := errors.New("consumer stopped discovery")
	err := discoverer.DiscoverEach(t.Context(), func(SourceRef) error { return stop })
	require.ErrorIs(err, stop)

	// A failed enumeration must leave the next event cold, even without new rows.
	sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: fixture.dbPath + "-wal", WatchRoot: fixture.sessionDir,
	})
	require.NoError(err)
	require.Len(sources, 2)
	assert.Equal(fixture.dbPath+"#session-a", sources[0].DisplayPath)
	assert.Equal(fixture.dbPath+"#session-b", sources[1].DisplayPath)
}

func TestGooseChangedSchemaReenumeratesOnce(t *testing.T) {
	for _, change := range []struct{ name, sql string }{
		{"version", "UPDATE schema_version SET version = 16"},
		{"optional usage table", "DROP TABLE usage_ledger"},
	} {
		t.Run(change.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			fixture := newGooseTestFixture(t)
			fixture.insertSession(t, "session-a", "a", "user", "")
			fixture.insertSession(t, "session-b", "b", "user", "")
			provider, ok := NewProvider(AgentGoose, ProviderConfig{Roots: []string{fixture.pathRoot}})
			require.True(ok)
			_, err := provider.Discover(t.Context())
			require.NoError(err)
			_, err = fixture.database.ExecContext(t.Context(), change.sql)
			require.NoError(err)
			scans := 0
			ctx := WithSharedContainerScanObserver(t.Context(), func() { scans++ })
			req := ChangedPathRequest{Path: fixture.dbPath, WatchRoot: fixture.sessionDir}
			sources, err := provider.SourcesForChangedPath(ctx, req)
			require.NoError(err)
			require.Len(sources, 2)
			assert.Equal(1, scans)

			fixture.insertMessage(t, "session-b", "user", `[{"type":"text","text":"new"}]`, 1_700_000_001)
			sources, err = provider.SourcesForChangedPath(ctx, req)
			require.NoError(err)
			require.Len(sources, 1)
			assert.Equal(fixture.dbPath+"#session-b", sources[0].DisplayPath)
			assert.Equal(1, scans, "the new schema's cursor must keep later events bounded")
		})
	}
}

func TestGooseDiscoveryRejectsUnsupportedMessagesSchema(t *testing.T) {
	require := require.New(t)

	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session", "Old", "user", "")
	_, err := fixture.database.ExecContext(t.Context(), `ALTER TABLE messages DROP COLUMN metadata_json`)
	require.NoError(err)

	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{fixture.pathRoot}, Machine: "devbox",
	})
	require.True(ok)
	_, err = provider.Discover(t.Context())
	require.ErrorContains(err, "unsupported goose messages schema")
	assert.ErrorContains(t, err, "metadata_json")
}

func TestGooseParsesSessionsSchemaWithoutOptionalColumns(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	pathRoot := t.TempDir()
	sessionDir := filepath.Join(pathRoot, "data", "sessions")
	require.NoError(os.MkdirAll(sessionDir, 0o755))
	dbPath := filepath.Join(sessionDir, GooseDBName)
	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })
	_, err = database.ExecContext(t.Context(), `
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
	require.NoError(err)

	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{pathRoot}, Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	assert.Len(fingerprint.Hash, 64)

	result, err := parseGooseSession(t.Context(), dbPath, "bare", "devbox", false)
	require.NoError(err)
	require.NotNil(result)
	assert.Equal("goose:bare", result.Session.ID)
	assert.Equal("Old schema prompt.", result.Session.FirstMessage)
	assert.Equal("goose-sqlite-v0", result.Session.SourceVersion)
	assert.Empty(result.Session.ParentSessionID)
	assert.Empty(result.UsageEvents)
	require.Len(result.Messages, 1)
	assert.Equal("Old schema prompt.", result.Messages[0].Content)
}

func TestGooseUnknownToolResponseStatusIsNotAHumanMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session", "Pending", "user", "")
	fixture.insertMessage(t, "session", "user", `[{"type":"text","text":"Run the tool."}]`, 1_700_000_000)
	fixture.insertMessage(t, "session", "user", `[
		{"type":"toolResponse","id":"call-1","toolResult":{"status":"pending"}}
	]`, 1_700_000_001)

	result, err := parseGooseSession(t.Context(), fixture.dbPath, "session", "devbox", false)
	require.NoError(err)
	require.NotNil(result)
	assert.Equal(1, result.Session.UserMessageCount,
		"a tool-response carrier with an unknown status must not count as a human message")
	require.Len(result.Messages, 2)
	require.Len(result.Messages[1].ToolResults, 1)
	assert.Equal("call-1", result.Messages[1].ToolResults[0].ToolUseID)
}

func TestGooseLedgerModelFallsBackToSessionModel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "session", "Carried", "user", "")
	fixture.insertMessage(t, "session", "user", `[{"type":"text","text":"hi"}]`, 1_700_000_000)
	// Upstream inserts carried_forward rows without a model.
	_, err := fixture.database.ExecContext(t.Context(), `
		INSERT INTO usage_ledger (
			session_id, created_timestamp, model, input_tokens, output_tokens,
			total_tokens, cache_read_tokens, cache_write_tokens,
			cost, cost_source, is_compaction
		) VALUES ('session', 1700000010, NULL, 50, 5, 55, 0, 0, 0.01, 'carried_forward', 0)
	`)
	require.NoError(err)

	result, err := parseGooseSession(t.Context(), fixture.dbPath, "session", "devbox", false)
	require.NoError(err)
	require.NotNil(result)
	require.Len(result.UsageEvents, 1)
	event := result.UsageEvents[0]
	assert.Equal("claude-sonnet-4-6", event.Model,
		"model-less ledger rows must inherit the session model so aggregates keep them")
	assert.Equal("unknown", event.CostStatus)
	assert.Equal("goose-carried-forward", event.CostSource)
	assert.Equal(50, event.InputTokens)
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
	assert := assert.New(t)
	require := require.New(t)

	sourceDB := os.Getenv("GOOSE_SOURCE_DB")
	if sourceDB == "" {
		t.Skip("set GOOSE_SOURCE_DB to an isolated Goose sessions.db copy")
	}
	raw, err := os.ReadFile(sourceDB)
	require.NoError(err)
	dbPath := filepath.Join(t.TempDir(), GooseDBName)
	require.NoError(os.WriteFile(dbPath, raw, 0o600))
	provider, ok := NewProvider(AgentGoose, ProviderConfig{
		Roots: []string{filepath.Dir(dbPath)}, Machine: "observed-fixture",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.NotEmpty(sources)
	for _, source := range sources {
		fingerprint, err := provider.Fingerprint(t.Context(), source)
		require.NoError(err)
		outcome, err := provider.Parse(t.Context(), ParseRequest{
			Source: source, Fingerprint: fingerprint, Machine: "observed-fixture",
		})
		require.NoError(err)
		require.Len(outcome.Results, 1)
		result := outcome.Results[0].Result
		assert.Equal(AgentGoose, result.Session.Agent)
		assert.Equal(len(result.Messages), result.Session.MessageCount)
		assert.NotEmpty(result.Session.SourceSessionID)
	}
}
