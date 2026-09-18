package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const zcodeTestSchema = `
	CREATE TABLE session (
		id TEXT PRIMARY KEY NOT NULL,
		project_id TEXT,
		workspace_id TEXT,
		directory TEXT,
		title TEXT,
		time_created INTEGER,
		time_updated INTEGER
	);
	CREATE TABLE model_usage (
		session_id TEXT NOT NULL,
		turn_id TEXT,
		provider_id TEXT,
		model_id TEXT,
		status TEXT,
		input_tokens INTEGER,
		output_tokens INTEGER,
		reasoning_tokens INTEGER,
		cache_creation_input_tokens INTEGER,
		cache_read_input_tokens INTEGER,
		computed_total_tokens INTEGER,
		started_at INTEGER,
		completed_at INTEGER,
		duration_ms INTEGER,
		tool_call_count INTEGER
	);
	CREATE TABLE message (
		id TEXT PRIMARY KEY NOT NULL,
		session_id TEXT NOT NULL,
		time_created TEXT,
		data TEXT
	);
	CREATE TABLE part (
		id TEXT PRIMARY KEY NOT NULL,
		message_id TEXT NOT NULL,
		session_id TEXT NOT NULL,
		time_created TEXT,
		data TEXT
	);
`

type zcodeTestFixture struct {
	Root     string
	CLIRoot  string
	DBDir    string
	DBPath   string
	database *sql.DB
}

func newZCodeTestFixture(t *testing.T) *zcodeTestFixture {
	t.Helper()
	root := t.TempDir()
	cliRoot := filepath.Join(root, ".zcode", "cli")
	dbDir := filepath.Join(cliRoot, "db")
	require.NoError(t, os.MkdirAll(dbDir, 0o755))
	dbPath := filepath.Join(dbDir, zcodeDBName)

	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	_, err = database.ExecContext(t.Context(), zcodeTestSchema)
	require.NoError(t, err)

	return &zcodeTestFixture{
		Root:     root,
		CLIRoot:  cliRoot,
		DBDir:    dbDir,
		DBPath:   dbPath,
		database: database,
	}
}

func (f *zcodeTestFixture) insertSession(
	t *testing.T,
	id, directory, title string,
	createdAt, updatedAt any,
	projectID, workspaceID string,
) {
	t.Helper()
	_, err := f.database.ExecContext(t.Context(), `
		INSERT INTO session (
			id, project_id, workspace_id, directory, title,
			time_created, time_updated
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, id, nullableZCodeString(projectID), nullableZCodeString(workspaceID), directory, title, createdAt, updatedAt)
	require.NoError(t, err)
}

func (f *zcodeTestFixture) insertUsage(
	t *testing.T,
	sessionID string,
	turnID string,
	providerID string,
	modelID, status string,
	inputTokens, outputTokens, reasoningTokens,
	cacheCreationTokens, cacheReadTokens, computedTotalTokens int64,
	startedAt, completedAt string,
	durationMS, toolCallCount int64,
) {
	t.Helper()
	_, err := f.database.ExecContext(t.Context(), `
		INSERT INTO model_usage (
			session_id, turn_id, provider_id, model_id, status,
			input_tokens, output_tokens, reasoning_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			computed_total_tokens, started_at, completed_at,
			duration_ms, tool_call_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, sessionID, nullableZCodeString(turnID), nullableZCodeString(providerID), modelID, status,
		inputTokens, outputTokens, reasoningTokens,
		cacheCreationTokens, cacheReadTokens, computedTotalTokens,
		startedAt, completedAt, durationMS, toolCallCount)
	require.NoError(t, err)
}

func (f *zcodeTestFixture) insertMessage(
	t *testing.T,
	id, sessionID string,
	createdAt any,
	data string,
) {
	t.Helper()
	_, err := f.database.ExecContext(t.Context(), `
		INSERT INTO message (
			id, session_id, time_created, data
		) VALUES (?, ?, ?, ?)
	`, id, sessionID, createdAt, data)
	require.NoError(t, err)
}

func (f *zcodeTestFixture) insertPart(
	t *testing.T,
	id, messageID, sessionID string,
	data string,
) {
	t.Helper()
	f.insertPartAt(t, id, messageID, sessionID, nil, data)
}

func (f *zcodeTestFixture) insertPartAt(
	t *testing.T,
	id, messageID, sessionID string,
	createdAt any,
	data string,
) {
	t.Helper()
	_, err := f.database.ExecContext(t.Context(), `
		INSERT INTO part (
			id, message_id, session_id, time_created, data
		) VALUES (?, ?, ?, ?, ?)
	`, id, messageID, sessionID, createdAt, data)
	require.NoError(t, err)
}

func TestZCodeProviderCapabilities(t *testing.T) {
	assert := assert.New(t)

	caps := zcodeProviderCapabilities()
	assert.Equal(CapabilitySupported, caps.Content.FirstMessage)
	assert.Equal(CapabilitySupported, caps.Content.SessionName)
	assert.Equal(CapabilitySupported, caps.Content.Cwd)
	assert.Equal(CapabilitySupported, caps.Content.Thinking)
	assert.Equal(CapabilitySupported, caps.Content.ToolCalls)
	assert.Equal(CapabilitySupported, caps.Content.ToolResults)
	assert.Equal(CapabilitySupported, caps.Content.AggregateUsageEvents)
	assert.Equal(CapabilitySupported, caps.Content.Model)
}

func TestZCodeParsesReportedIntegerTimestamps(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t,
		"session-ms",
		"/Users/alice/code/ms-app",
		"Integer timestamps",
		int64(1783352401000),
		int64(1783352700000),
		"",
		"",
	)
	oldDBMtime := time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)
	require.NoError(os.Chtimes(fixture.DBPath, oldDBMtime, oldDBMtime))

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{fixture.CLIRoot},
		Machine: "devbox",
	})
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal(int64(1783352401000000000), outcome.Results[0].Result.Session.StartedAt.UnixNano())
	assert.Equal(int64(1783352700000000000), outcome.Results[0].Result.Session.EndedAt.UnixNano())
	assert.Equal(int64(1783352700000000000), outcome.Results[0].Result.Session.File.Mtime)
}

func TestZCodeProviderFindsRawDatabaseInDirectRoot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// A materialized raw snapshot places db.sqlite directly inside the given
	// root, so stable-snapshot root normalization must accept that layout
	// instead of always appending a "db" child.
	dir := t.TempDir()
	require.NoError(os.WriteFile(
		filepath.Join(dir, ZCodeDBName), []byte("sqlite\n"), 0o600,
	))
	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:                 []string{dir},
		StableSourceSnapshots: true,
	})
	require.True(ok)

	discovery, err := DiscoverRawCaptureSources(t.Context(), provider)

	require.NoError(err)
	require.True(discovery.Complete)
	require.Len(discovery.Sources, 1)
	assert.Equal(ZCodeDBName, discovery.Sources[0].Key)
	assert.Equal(filepath.Join(dir, ZCodeDBName), discovery.Sources[0].DisplayPath)
}

func TestZCodeDiscoveryPreservesSessionsWhenUsageTableCorrupt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	fixture.insertSession(t, "readable", "/work/app", "Readable session",
		1700000000, 1700000001, "", "")
	// Keep the producer schema and session data intact while damaging the
	// usage table's page. Discovery must retain session identities; the usage
	// failure belongs to parsing each source, as in ordinary local sync.
	var page, pageSize int
	require.NoError(fixture.database.QueryRowContext(t.Context(),
		"SELECT rootpage FROM sqlite_schema WHERE name = 'model_usage'",
	).Scan(&page))
	require.NoError(fixture.database.QueryRowContext(t.Context(), "PRAGMA page_size").Scan(&pageSize))
	require.NoError(fixture.database.Close())
	contents, err := os.ReadFile(fixture.DBPath)
	require.NoError(err)
	contents[(page-1)*pageSize] = 0
	require.NoError(os.WriteFile(fixture.DBPath, contents, 0o600))

	provider, ok := NewProvider(AgentZCode, ProviderConfig{Roots: []string{fixture.CLIRoot}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(fixture.DBPath+"#readable", sources[0].DisplayPath)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	assert.NotZero(fingerprint.MTimeNS)
	_, err = provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
	var sqliteErr sqlite3.Error
	require.ErrorAs(err, &sqliteErr)
	assert.Equal(sqlite3.ErrCorrupt, sqliteErr.Code)
}

func TestZCodeConfiguredRootKeepsEstablishedDBLayoutPrecedence(t *testing.T) {
	require := require.New(t)

	// A configured root that carries both a stray top-level db.sqlite and the
	// established db/db.sqlite layout must keep resolving to db/db.sqlite:
	// direct-root acceptance exists only for hosted stable snapshots.
	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "db"), 0o755))
	directPath := filepath.Join(root, ZCodeDBName)
	establishedPath := filepath.Join(root, "db", ZCodeDBName)
	require.NoError(os.WriteFile(directPath, []byte("direct"), 0o600))
	db, err := sql.Open("sqlite3", establishedPath)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), zcodeTestSchema)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO session (id, project_id, workspace_id, directory, title,
			time_created, time_updated)
		VALUES ('session-001', NULL, NULL, '/work/app', 'Established',
			'2026-07-06T13:00:01Z', '2026-07-06T13:05:00Z')`)
	require.NoError(err)
	require.NoError(db.Close())

	provider, ok := NewProvider(AgentZCode, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	sources, err := provider.Discover(t.Context())

	require.NoError(err)
	require.Len(sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Machine: "devbox",
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal(t, "zcode:session-001", outcome.Results[0].Result.Session.ID,
		"discovery must use the established db/db.sqlite, not the stray top-level copy")
}

func TestZCodeConfiguredRootWithoutDBDirKeepsAppendingDB(t *testing.T) {
	// Without the stable-snapshot flag a root that only carries a stray
	// top-level database keeps the historical mapping onto root/db, so local
	// discovery behavior is unchanged by the hosted direct-root support.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, ZCodeDBName), []byte("not a sqlite database"), 0o600,
	))

	assert.Equal(t, filepath.Join(root, "db"), normalizeZCodeRoot(root, false))
	assert.Equal(t, root, normalizeZCodeRoot(root, true))
}

func TestZCodeProviderSourceMethodsAndParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t,
		"session-001",
		"/Users/alice/code/acme-app",
		"Acme session",
		"2026-07-06T13:00:01Z",
		"2026-07-06T13:05:00Z",
		"project-7",
		"workspace-19",
	)
	fixture.insertUsage(
		t,
		"session-001",
		"1",
		"builtin:bigmodel-coding-plan",
		"claude-sonnet-4-6",
		"done",
		1000, 200, 40, 50, 25, 1315,
		"2026-07-06T13:00:02Z",
		"2026-07-06T13:00:03Z",
		1000,
		2,
	)
	fixture.insertUsage(
		t,
		"session-001",
		"2",
		"builtin:bigmodel-coding-plan",
		"claude-sonnet-4-6",
		"done",
		800, 75, 5, 10, 0, 890,
		"2026-07-06T13:04:00Z",
		"2026-07-06T13:05:00Z",
		600,
		0,
	)
	fixture.insertMessage(
		t,
		"msg-1",
		"session-001",
		"2026-07-06T13:00:01Z",
		`{"role":"user"}`,
	)
	fixture.insertPart(
		t,
		"part-1",
		"msg-1",
		"session-001",
		`{"type":"text","text":"Inspect the login flow."}`,
	)
	fixture.insertMessage(
		t,
		"msg-2",
		"session-001",
		"2026-07-06T13:00:02Z",
		`{"role":"assistant","model":{"modelID":"claude-sonnet-4-6"}}`,
	)
	fixture.insertPartAt(
		t,
		"part-z",
		"msg-2",
		"session-001",
		"2026-07-06T13:00:02Z",
		`{"type":"thinking","thinking":"I should read the auth code first."}`,
	)
	fixture.insertPartAt(
		t,
		"part-a",
		"msg-2",
		"session-001",
		"2026-07-06T13:00:03Z",
		`{"type":"text","text":"I'll inspect the auth code first."}`,
	)
	fixture.insertPartAt(
		t,
		"part-4",
		"msg-2",
		"session-001",
		"2026-07-06T13:00:04Z",
		`{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"auth.go"}}`,
	)
	fixture.insertMessage(
		t,
		"msg-3",
		"session-001",
		"2026-07-06T13:00:03Z",
		`{"role":"user"}`,
	)
	fixture.insertPart(
		t,
		"part-5",
		"msg-3",
		"session-001",
		`{"type":"tool_result","tool_use_id":"call-1","content":"package auth"}`,
	)

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots: []string{
			fixture.CLIRoot,
			fixture.DBDir,
		},
		Machine: "devbox",
	})
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(fixture.DBDir, plan.Roots[0].Path)
	assert.Contains(plan.Roots[0].IncludeGlobs, zcodeDBName)
	assert.Contains(plan.Roots[0].IncludeGlobs, zcodeDBName+"-*")

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	source := sources[0]
	assert.Equal(AgentZCode, source.Provider)
	assert.Equal(fixture.DBPath+"#session-001", source.DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.Equal(fixture.DBPath+"#session-001", fingerprint.Key)
	assert.NotZero(fingerprint.MTimeNS)

	foundSource, found, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID:  "session-001",
		FullSessionID: "zcode:session-001",
	})
	require.NoError(err)
	require.True(found)
	assert.Equal(source.DisplayPath, foundSource.DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      foundSource,
		Fingerprint: fingerprint,
		Machine:     "devbox",
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.True(outcome.ForceReplace)
	require.Len(outcome.Results, 1)

	result := outcome.Results[0]
	sess := result.Result.Session
	assert.Equal("zcode:session-001", sess.ID)
	assert.Equal(AgentZCode, sess.Agent)
	assert.Equal("devbox", sess.Machine)
	assert.Equal("acme_app", sess.Project)
	assert.Equal("/Users/alice/code/acme-app", sess.Cwd)
	assert.Equal("Acme session", sess.SessionName)
	assert.Equal("Acme session", sess.FirstMessage)
	assert.Equal(3, sess.MessageCount)
	assert.Equal(2, sess.UserMessageCount)
	assert.Equal(fixture.DBPath+"#session-001", sess.File.Path)
	assert.NotZero(sess.File.Size)
	require.Len(result.Result.Messages, 3)
	assert.Equal(RoleUser, result.Result.Messages[0].Role)
	assert.Equal("Inspect the login flow.", result.Result.Messages[0].Content)
	assert.Equal(RoleAssistant, result.Result.Messages[1].Role)
	assert.True(result.Result.Messages[1].HasThinking)
	assert.True(result.Result.Messages[1].HasToolUse)
	assert.Equal("claude-sonnet-4-6", result.Result.Messages[1].Model)
	assert.Equal(
		"[Thinking]\nI should read the auth code first.\n[/Thinking]\nI'll inspect the auth code first.",
		result.Result.Messages[1].Content,
	)
	require.Len(result.Result.Messages[1].ToolCalls, 1)
	assert.Equal("Read", result.Result.Messages[1].ToolCalls[0].Category)
	assert.Equal(RoleUser, result.Result.Messages[2].Role)
	require.Len(result.Result.Messages[2].ToolResults, 1)
	assert.Equal("package auth", DecodeContent(result.Result.Messages[2].ToolResults[0].ContentRaw))
	assert.Len(result.Result.UsageEvents, 2)
	assert.Equal(275, sess.TotalOutputTokens)
	assert.True(sess.HasTotalOutputTokens)
	assert.Equal(1075, sess.PeakContextTokens)
	assert.True(sess.HasPeakContextTokens)

	first := result.Result.UsageEvents[0]
	second := result.Result.UsageEvents[1]
	require.NotNil(first.MessageOrdinal)
	require.NotNil(second.MessageOrdinal)
	assert.Equal(1, *first.MessageOrdinal)
	assert.Equal(2, *second.MessageOrdinal)
	assert.Equal("zcode:session-001", first.SessionID)
	assert.Equal("session", first.Source)
	assert.Equal("claude-sonnet-4-6", first.Model)
	assert.Equal(1000, first.InputTokens)
	assert.Equal(200, first.OutputTokens)
	assert.Equal(40, first.ReasoningTokens)
	assert.Equal(50, first.CacheCreationInputTokens)
	assert.Equal(25, first.CacheReadInputTokens)
	assert.Equal("zcode:session-001", second.SessionID)
	assert.NotEqual(first.DedupKey, second.DedupKey)
	assert.Contains(first.DedupKey, "session:zcode:session-001")
	assert.Contains(first.DedupKey, "turn=1")
	assert.Contains(first.DedupKey, "provider=builtin:bigmodel-coding-plan")
	assert.Contains(first.DedupKey, "model=claude-sonnet-4-6")
	assert.Contains(first.DedupKey, "input_tokens=1000")
}

func TestZCodeUsageEventMapping(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t,
		"session-usage",
		"/Users/alice/code/acme-app",
		"Usage session",
		"2026-07-06T13:00:01Z",
		"2026-07-06T13:05:00Z",
		"",
		"",
	)
	fixture.insertUsage(
		t,
		"session-usage",
		"turn-alpha",
		"builtin:bigmodel-coding-plan",
		"claude-sonnet-4-6",
		"done",
		1000, 200, 40, 50, 25, 1315,
		"2026-07-06T13:00:02Z",
		"2026-07-06T13:00:03Z",
		1000,
		2,
	)

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{fixture.CLIRoot},
		Machine: "devbox",
	})
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: SourceFingerprint{Key: fixture.DBPath + "#session-usage"},
		Machine:     "devbox",
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)

	result := outcome.Results[0]
	require.Len(result.Result.UsageEvents, 1)
	event := result.Result.UsageEvents[0]
	assert.Equal("zcode:session-usage", event.SessionID)
	assert.Nil(event.MessageOrdinal)
	assert.Equal("claude-sonnet-4-6", event.Model)
	assert.Equal(1000, event.InputTokens)
	assert.Equal(200, event.OutputTokens)
	assert.Equal(40, event.ReasoningTokens)
	assert.Equal(50, event.CacheCreationInputTokens)
	assert.Equal(25, event.CacheReadInputTokens)
	assert.Contains(event.DedupKey, "session:zcode:session-usage")
	assert.Contains(event.DedupKey, "turn=turn-alpha")
	assert.Contains(event.DedupKey, "provider=builtin:bigmodel-coding-plan")
	assert.Contains(event.DedupKey, "model=claude-sonnet-4-6")
}

func TestZCodeIngestsTranscriptMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t,
		"session-transcript",
		"/Users/alice/code/acme-app",
		"Transcript session",
		"2026-07-06T13:00:01Z",
		"2026-07-06T13:05:00Z",
		"",
		"",
	)
	fixture.insertMessage(
		t,
		"msg-1",
		"session-transcript",
		"2026-07-06T13:00:01Z",
		`{"role":"user"}`,
	)
	fixture.insertPart(
		t,
		"part-1",
		"msg-1",
		"session-transcript",
		`{"type":"text","text":"Fix the login bug."}`,
	)
	fixture.insertMessage(
		t,
		"msg-2",
		"session-transcript",
		"2026-07-06T13:00:02Z",
		`{"role":"assistant","model":{"modelID":"claude-sonnet-4-6"}}`,
	)
	fixture.insertPartAt(
		t,
		"part-z",
		"msg-2",
		"session-transcript",
		"2026-07-06T13:00:02Z",
		`{"type":"thinking","thinking":"I should inspect the auth flow."}`,
	)
	fixture.insertPartAt(
		t,
		"part-a",
		"msg-2",
		"session-transcript",
		"2026-07-06T13:00:03Z",
		`{"type":"text","text":"I'll inspect the auth flow."}`,
	)

	result, err := parseZCodeSession(t.Context(), fixture.DBPath, "session-transcript", "devbox", false)
	require.NoError(err)
	require.NotNil(result)
	assert.Equal(2, result.Session.MessageCount)
	assert.Equal(1, result.Session.UserMessageCount)
	require.Len(result.Messages, 2)
	assert.Equal(RoleUser, result.Messages[0].Role)
	assert.Equal("Fix the login bug.", result.Messages[0].Content)
	assert.Equal(RoleAssistant, result.Messages[1].Role)
	assert.True(result.Messages[1].HasThinking)
	assert.Equal("I should inspect the auth flow.", result.Messages[1].ThinkingText)
	assert.Equal(
		"[Thinking]\nI should inspect the auth flow.\n[/Thinking]\nI'll inspect the auth flow.",
		result.Messages[1].Content,
	)
	assert.Equal("claude-sonnet-4-6", result.Messages[1].Model)
}

func TestZCodeToolCallsAndResults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t,
		"session-tools",
		"/Users/alice/code/acme-app",
		"Tool session",
		"2026-07-06T13:00:01Z",
		"2026-07-06T13:05:00Z",
		"",
		"",
	)
	fixture.insertMessage(
		t,
		"msg-1",
		"session-tools",
		"2026-07-06T13:00:01Z",
		`{"role":"assistant","modelID":"claude-sonnet-4-6"}`,
	)
	fixture.insertPart(
		t,
		"part-1",
		"msg-1",
		"session-tools",
		`{"type":"tool_use","id":"call-read","name":"Read","input":{"file_path":"auth.go"}}`,
	)
	fixture.insertMessage(
		t,
		"msg-2",
		"session-tools",
		"2026-07-06T13:00:02Z",
		`{"role":"user"}`,
	)
	fixture.insertPart(
		t,
		"part-2",
		"msg-2",
		"session-tools",
		`{"type":"tool_result","tool_use_id":"call-read","content":"package auth"}`,
	)

	result, err := parseZCodeSession(t.Context(), fixture.DBPath, "session-tools", "devbox", false)
	require.NoError(err)
	require.NotNil(result)
	require.Len(result.Messages, 2)

	assistant := result.Messages[0]
	assert.Equal(RoleAssistant, assistant.Role)
	assert.True(assistant.HasToolUse)
	require.Len(assistant.ToolCalls, 1)
	assert.Equal("call-read", assistant.ToolCalls[0].ToolUseID)
	assert.Equal("Read", assistant.ToolCalls[0].ToolName)
	assert.Equal("Read", assistant.ToolCalls[0].Category)
	assert.JSONEq(`{"file_path":"auth.go"}`, assistant.ToolCalls[0].InputJSON)

	toolResult := result.Messages[1]
	assert.Equal(RoleUser, toolResult.Role)
	require.Len(toolResult.ToolResults, 1)
	assert.Equal("call-read", toolResult.ToolResults[0].ToolUseID)
	assert.Equal(len("package auth"), toolResult.ToolResults[0].ContentLength)
	assert.Equal("package auth", DecodeContent(toolResult.ToolResults[0].ContentRaw))
}

func TestZCodeOpenCodeStyleReasoningAndToolParts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t,
		"session-opencode-parts",
		"/Users/alice/code/acme-app",
		"OpenCode-style parts",
		"2026-07-06T13:00:01Z",
		"2026-07-06T13:05:00Z",
		"",
		"",
	)
	fixture.insertMessage(
		t,
		"msg-1",
		"session-opencode-parts",
		"2026-07-06T13:00:01Z",
		`{"role":"user"}`,
	)
	fixture.insertPart(
		t,
		"part-1",
		"msg-1",
		"session-opencode-parts",
		`{"type":"text","text":"Inspect the auth flow."}`,
	)
	fixture.insertMessage(
		t,
		"msg-2",
		"session-opencode-parts",
		"2026-07-06T13:00:02Z",
		`{"role":"assistant","modelID":"claude-sonnet-4-6"}`,
	)
	fixture.insertPartAt(
		t,
		"part-b",
		"msg-2",
		"session-opencode-parts",
		"2026-07-06T13:00:02Z",
		`{"type":"reasoning","text":"I should inspect the auth flow."}`,
	)
	fixture.insertPartAt(
		t,
		"part-c",
		"msg-2",
		"session-opencode-parts",
		"2026-07-06T13:00:03Z",
		`{"type":"text","text":"I'll inspect the auth flow."}`,
	)
	fixture.insertPartAt(
		t,
		"part-a",
		"msg-2",
		"session-opencode-parts",
		"2026-07-06T13:00:04Z",
		`{"type":"tool","tool":"Read","callID":"call-read","state":{"input":{"file_path":"auth.go"},"output":"package auth"}}`,
	)

	result, err := parseZCodeSession(t.Context(), fixture.DBPath, "session-opencode-parts", "devbox", false)
	require.NoError(err)
	require.NotNil(result)
	require.Len(result.Messages, 2)

	assistant := result.Messages[1]
	assert.Equal(RoleAssistant, assistant.Role)
	assert.True(assistant.HasThinking)
	assert.True(assistant.HasToolUse)
	assert.Equal("I should inspect the auth flow.", assistant.ThinkingText)
	assert.Equal(
		"[Thinking]\nI should inspect the auth flow.\n[/Thinking]\nI'll inspect the auth flow.",
		assistant.Content,
	)
	require.Len(assistant.ToolCalls, 1)
	assert.Equal("call-read", assistant.ToolCalls[0].ToolUseID)
	assert.Equal("Read", assistant.ToolCalls[0].ToolName)
	assert.JSONEq(`{"file_path":"auth.go"}`, assistant.ToolCalls[0].InputJSON)
	require.Len(assistant.ToolResults, 1)
	assert.Equal("call-read", assistant.ToolResults[0].ToolUseID)
	assert.Equal(len("package auth"), assistant.ToolResults[0].ContentLength)
	assert.Equal("package auth", DecodeContent(assistant.ToolResults[0].ContentRaw))
}

func TestZCodeOpenCodeStyleFailedToolPart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t,
		"session-tool-error",
		"/Users/alice/code/acme-app",
		"Tool error",
		"2026-07-06T13:00:01Z",
		"2026-07-06T13:05:00Z",
		"",
		"",
	)
	fixture.insertMessage(
		t,
		"msg-1",
		"session-tool-error",
		"2026-07-06T13:00:02Z",
		`{"role":"assistant","modelID":"claude-sonnet-4-6"}`,
	)
	fixture.insertPartAt(
		t,
		"part-1",
		"msg-1",
		"session-tool-error",
		"2026-07-06T13:00:03Z",
		`{"type":"tool","tool":"Read","callID":"call-read","state":{"input":{"file_path":"auth.go"},"error":"permission denied"}}`,
	)

	result, err := parseZCodeSession(t.Context(), fixture.DBPath, "session-tool-error", "devbox", false)
	require.NoError(err)
	require.NotNil(result)
	require.Len(result.Messages, 1)

	assistant := result.Messages[0]
	assert.Equal(RoleAssistant, assistant.Role)
	assert.True(assistant.HasToolUse)
	require.Len(assistant.ToolCalls, 1)
	require.Len(assistant.ToolResults, 1)
	assert.Equal("call-read", assistant.ToolResults[0].ToolUseID)
	assert.Equal(len("permission denied"), assistant.ToolResults[0].ContentLength)
	assert.Equal("permission denied", DecodeContent(assistant.ToolResults[0].ContentRaw))
}

func TestZCodeFingerprintTracksUsageMtime(t *testing.T) {
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t,
		"session-usage-mtime",
		"/Users/alice/code/acme-app",
		"Usage mtime",
		"2026-07-06T13:00:01Z",
		"2026-07-06T13:01:00Z",
		"",
		"",
	)
	fixture.insertUsage(
		t,
		"session-usage-mtime",
		"turn-alpha",
		"builtin:bigmodel-coding-plan",
		"claude-sonnet-4-6",
		"done",
		1000, 200, 40, 50, 25, 1315,
		"2026-07-06T13:09:00Z",
		"2026-07-06T13:10:00Z",
		1000,
		2,
	)
	oldDBMtime := time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)
	require.NoError(os.Chtimes(fixture.DBPath, oldDBMtime, oldDBMtime))

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{fixture.CLIRoot},
		Machine: "devbox",
	})
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	expected := time.Date(2026, 7, 6, 13, 10, 0, 0, time.UTC).UnixNano()
	assert.Equal(t, expected, fingerprint.MTimeNS)
}

func TestZCodeFingerprintTracksDBMtimeForUsageOnlyChanges(t *testing.T) {
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t,
		"session-db-mtime",
		"/Users/alice/code/acme-app",
		"DB mtime",
		"2026-07-06T13:00:01Z",
		"2026-07-06T13:10:00Z",
		"",
		"",
	)
	fixture.insertUsage(
		t,
		"session-db-mtime",
		"turn-alpha",
		"builtin:bigmodel-coding-plan",
		"claude-sonnet-4-6",
		"done",
		1000, 200, 40, 50, 25, 1315,
		"2026-07-06T13:04:00Z",
		"2026-07-06T13:05:00Z",
		1000,
		2,
	)
	dbMtime := time.Date(2026, 7, 6, 13, 20, 0, 0, time.UTC)
	require.NoError(os.Chtimes(fixture.DBPath, dbMtime, dbMtime))

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{fixture.CLIRoot},
		Machine: "devbox",
	})
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	assert.Equal(t, dbMtime.UnixNano(), fingerprint.MTimeNS)
}

func TestZCodeFingerprintIgnoresShmIndexMtime(t *testing.T) {
	require := require.New(t)

	// Readers rewrite the -shm index, this provider's own connection
	// included, so its mtime must not move the fingerprint or every scan
	// would report the whole container as changed.
	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t,
		"session-shm",
		"/Users/alice/code/acme-app",
		"SHM",
		"2026-07-06T13:00:01Z",
		"2026-07-06T13:10:00Z",
		"",
		"",
	)
	dbMtime := time.Date(2026, 7, 6, 13, 20, 0, 0, time.UTC)
	require.NoError(os.Chtimes(fixture.DBPath, dbMtime, dbMtime))
	shmPath := fixture.DBPath + "-shm"
	require.NoError(os.WriteFile(shmPath, make([]byte, 32), 0o644))
	shmMtime := dbMtime.Add(time.Hour)
	require.NoError(os.Chtimes(shmPath, shmMtime, shmMtime))

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{fixture.CLIRoot},
		Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	assert.Equal(t, dbMtime.UnixNano(), fingerprint.MTimeNS)
}

func TestZCodeFallsBackToDBMtimeWhenTimestampsAreMissing(t *testing.T) {
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t,
		"session-no-timestamps",
		"/Users/alice/code/acme-app",
		"No timestamps",
		nil,
		nil,
		"",
		"",
	)
	dbMtime := time.Date(2026, 7, 6, 13, 20, 0, 0, time.UTC)
	require.NoError(os.Chtimes(fixture.DBPath, dbMtime, dbMtime))

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{fixture.CLIRoot},
		Machine: "devbox",
	})
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal(t, dbMtime.UnixNano(), outcome.Results[0].Result.Session.File.Mtime)
}

func TestZCodeParsesSessionWhenUsageTableIsMissing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	_, err := fixture.database.ExecContext(t.Context(), `DROP TABLE model_usage`)
	require.NoError(err)
	fixture.insertSession(
		t,
		"session-no-usage-table",
		"/Users/alice/code/acme-app",
		"No usage table",
		"2026-07-06T13:00:01Z",
		"2026-07-06T13:05:00Z",
		"",
		"",
	)

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{fixture.CLIRoot},
		Machine: "devbox",
	})
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)

	result := outcome.Results[0].Result
	assert.Equal("zcode:session-no-usage-table", result.Session.ID)
	assert.Equal(0, result.Session.MessageCount)
	assert.Empty(result.UsageEvents)
	assert.False(result.Session.HasTotalOutputTokens)
	assert.False(result.Session.HasPeakContextTokens)
}

func TestZCodeMissingTranscriptTables(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	_, err := fixture.database.ExecContext(t.Context(), `DROP TABLE part`)
	require.NoError(err)
	_, err = fixture.database.ExecContext(t.Context(), `DROP TABLE message`)
	require.NoError(err)
	fixture.insertSession(
		t,
		"session-no-transcript",
		"/Users/alice/code/acme-app",
		"No transcript",
		"2026-07-06T13:00:01Z",
		"2026-07-06T13:05:00Z",
		"",
		"",
	)
	fixture.insertUsage(
		t,
		"session-no-transcript",
		"1",
		"builtin:bigmodel-coding-plan",
		"claude-sonnet-4-6",
		"done",
		1000, 200, 40, 50, 25, 1315,
		"2026-07-06T13:00:02Z",
		"2026-07-06T13:00:03Z",
		1000,
		2,
	)

	result, err := parseZCodeSession(t.Context(), fixture.DBPath, "session-no-transcript", "devbox", false)
	require.NoError(err)
	require.NotNil(result)
	assert.Equal(0, result.Session.MessageCount)
	assert.Equal(0, result.Session.UserMessageCount)
	assert.Empty(result.Messages)
	require.Len(result.UsageEvents, 1)
	assert.Equal(200, result.Session.TotalOutputTokens)
	assert.True(result.Session.HasTotalOutputTokens)
}

func TestZCodeProviderRootWithoutDB(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	cliRoot := filepath.Join(root, ".zcode", "cli")
	require.NoError(os.MkdirAll(cliRoot, 0o755))

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{cliRoot},
		Machine: "devbox",
	})
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	assert.Empty(t, sources)
}

func nullableZCodeString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func TestZCodeSystemMessagesAreMarkedSystem(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t,
		"session-system",
		"/Users/alice/code/acme-app",
		"System session",
		"2026-07-06T13:00:01Z",
		"2026-07-06T13:05:00Z",
		"",
		"",
	)
	fixture.insertMessage(
		t,
		"msg-system",
		"session-system",
		"2026-07-06T13:00:01Z",
		`{"role":"system"}`,
	)
	fixture.insertPart(
		t,
		"part-system",
		"msg-system",
		"session-system",
		`{"type":"text","text":"You are a code reviewer."}`,
	)

	result, err := parseZCodeSession(t.Context(), fixture.DBPath, "session-system", "devbox", false)
	require.NoError(err)
	require.NotNil(result)
	require.Len(result.Messages, 1)
	assert.Equal(RoleSystem, result.Messages[0].Role)
	assert.True(result.Messages[0].IsSystem)
}
