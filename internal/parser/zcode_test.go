package parser

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
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
	_, err = database.Exec(zcodeTestSchema)
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
	_, err := f.database.Exec(`
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
	_, err := f.database.Exec(`
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
	_, err := f.database.Exec(`
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
	_, err := f.database.Exec(`
		INSERT INTO part (
			id, message_id, session_id, time_created, data
		) VALUES (?, ?, ?, ?, ?)
	`, id, messageID, sessionID, createdAt, data)
	require.NoError(t, err)
}

func TestZCodeProviderCapabilities(t *testing.T) {
	caps := zcodeProviderCapabilities()
	assert.Equal(t, CapabilitySupported, caps.Content.FirstMessage)
	assert.Equal(t, CapabilitySupported, caps.Content.SessionName)
	assert.Equal(t, CapabilitySupported, caps.Content.Cwd)
	assert.Equal(t, CapabilitySupported, caps.Content.Thinking)
	assert.Equal(t, CapabilitySupported, caps.Content.ToolCalls)
	assert.Equal(t, CapabilitySupported, caps.Content.ToolResults)
	assert.Equal(t, CapabilitySupported, caps.Content.AggregateUsageEvents)
	assert.Equal(t, CapabilitySupported, caps.Content.Model)
}

func TestZCodeParsesReportedIntegerTimestamps(t *testing.T) {
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
	require.NoError(t, os.Chtimes(fixture.DBPath, oldDBMtime, oldDBMtime))

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{fixture.CLIRoot},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, int64(1783352401000000000), outcome.Results[0].Result.Session.StartedAt.UnixNano())
	assert.Equal(t, int64(1783352700000000000), outcome.Results[0].Result.Session.EndedAt.UnixNano())
	assert.Equal(t, int64(1783352700000000000), outcome.Results[0].Result.Session.File.Mtime)
}

func TestZCodeProviderSourceMethodsAndParse(t *testing.T) {
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
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, fixture.DBDir, plan.Roots[0].Path)
	assert.Contains(t, plan.Roots[0].IncludeGlobs, zcodeDBName)
	assert.Contains(t, plan.Roots[0].IncludeGlobs, zcodeDBName+"-*")

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	source := sources[0]
	assert.Equal(t, AgentZCode, source.Provider)
	assert.Equal(t, fixture.DBPath+"#session-001", source.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, fixture.DBPath+"#session-001", fingerprint.Key)
	assert.NotZero(t, fingerprint.MTimeNS)

	foundSource, found, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID:  "session-001",
		FullSessionID: "zcode:session-001",
	})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, source.DisplayPath, foundSource.DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      foundSource,
		Fingerprint: fingerprint,
		Machine:     "devbox",
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)

	result := outcome.Results[0]
	sess := result.Result.Session
	assert.Equal(t, "zcode:session-001", sess.ID)
	assert.Equal(t, AgentZCode, sess.Agent)
	assert.Equal(t, "devbox", sess.Machine)
	assert.Equal(t, "acme_app", sess.Project)
	assert.Equal(t, "/Users/alice/code/acme-app", sess.Cwd)
	assert.Equal(t, "Acme session", sess.SessionName)
	assert.Equal(t, "Acme session", sess.FirstMessage)
	assert.Equal(t, 3, sess.MessageCount)
	assert.Equal(t, 2, sess.UserMessageCount)
	assert.Equal(t, fixture.DBPath+"#session-001", sess.File.Path)
	assert.NotZero(t, sess.File.Size)
	require.Len(t, result.Result.Messages, 3)
	assert.Equal(t, RoleUser, result.Result.Messages[0].Role)
	assert.Equal(t, "Inspect the login flow.", result.Result.Messages[0].Content)
	assert.Equal(t, RoleAssistant, result.Result.Messages[1].Role)
	assert.True(t, result.Result.Messages[1].HasThinking)
	assert.True(t, result.Result.Messages[1].HasToolUse)
	assert.Equal(t, "claude-sonnet-4-6", result.Result.Messages[1].Model)
	assert.Equal(
		t,
		"[Thinking]\nI should read the auth code first.\n[/Thinking]\nI'll inspect the auth code first.",
		result.Result.Messages[1].Content,
	)
	require.Len(t, result.Result.Messages[1].ToolCalls, 1)
	assert.Equal(t, "Read", result.Result.Messages[1].ToolCalls[0].Category)
	assert.Equal(t, RoleUser, result.Result.Messages[2].Role)
	require.Len(t, result.Result.Messages[2].ToolResults, 1)
	assert.Equal(t, "package auth", DecodeContent(result.Result.Messages[2].ToolResults[0].ContentRaw))
	assert.Equal(t, 2, len(result.Result.UsageEvents))
	assert.Equal(t, 275, sess.TotalOutputTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 1075, sess.PeakContextTokens)
	assert.True(t, sess.HasPeakContextTokens)

	first := result.Result.UsageEvents[0]
	second := result.Result.UsageEvents[1]
	require.NotNil(t, first.MessageOrdinal)
	require.NotNil(t, second.MessageOrdinal)
	assert.Equal(t, 1, *first.MessageOrdinal)
	assert.Equal(t, 2, *second.MessageOrdinal)
	assert.Equal(t, "zcode:session-001", first.SessionID)
	assert.Equal(t, "session", first.Source)
	assert.Equal(t, "claude-sonnet-4-6", first.Model)
	assert.Equal(t, 1000, first.InputTokens)
	assert.Equal(t, 200, first.OutputTokens)
	assert.Equal(t, 40, first.ReasoningTokens)
	assert.Equal(t, 50, first.CacheCreationInputTokens)
	assert.Equal(t, 25, first.CacheReadInputTokens)
	assert.Equal(t, "zcode:session-001", second.SessionID)
	assert.NotEqual(t, first.DedupKey, second.DedupKey)
	assert.Contains(t, first.DedupKey, "session:zcode:session-001")
	assert.Contains(t, first.DedupKey, "turn=1")
	assert.Contains(t, first.DedupKey, "provider=builtin:bigmodel-coding-plan")
	assert.Contains(t, first.DedupKey, "model=claude-sonnet-4-6")
	assert.Contains(t, first.DedupKey, "input_tokens=1000")
}

func TestZCodeUsageEventMapping(t *testing.T) {
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
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: SourceFingerprint{Key: fixture.DBPath + "#session-usage"},
		Machine:     "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	result := outcome.Results[0]
	require.Len(t, result.Result.UsageEvents, 1)
	event := result.Result.UsageEvents[0]
	assert.Equal(t, "zcode:session-usage", event.SessionID)
	assert.Nil(t, event.MessageOrdinal)
	assert.Equal(t, "claude-sonnet-4-6", event.Model)
	assert.Equal(t, 1000, event.InputTokens)
	assert.Equal(t, 200, event.OutputTokens)
	assert.Equal(t, 40, event.ReasoningTokens)
	assert.Equal(t, 50, event.CacheCreationInputTokens)
	assert.Equal(t, 25, event.CacheReadInputTokens)
	assert.Contains(t, event.DedupKey, "session:zcode:session-usage")
	assert.Contains(t, event.DedupKey, "turn=turn-alpha")
	assert.Contains(t, event.DedupKey, "provider=builtin:bigmodel-coding-plan")
	assert.Contains(t, event.DedupKey, "model=claude-sonnet-4-6")
}

func TestZCodeIngestsTranscriptMessages(t *testing.T) {
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

	result, err := parseZCodeSession(fixture.DBPath, "session-transcript", "devbox")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 2, result.Session.MessageCount)
	assert.Equal(t, 1, result.Session.UserMessageCount)
	require.Len(t, result.Messages, 2)
	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, "Fix the login bug.", result.Messages[0].Content)
	assert.Equal(t, RoleAssistant, result.Messages[1].Role)
	assert.True(t, result.Messages[1].HasThinking)
	assert.Equal(t, "I should inspect the auth flow.", result.Messages[1].ThinkingText)
	assert.Equal(
		t,
		"[Thinking]\nI should inspect the auth flow.\n[/Thinking]\nI'll inspect the auth flow.",
		result.Messages[1].Content,
	)
	assert.Equal(t, "claude-sonnet-4-6", result.Messages[1].Model)
}

func TestZCodeToolCallsAndResults(t *testing.T) {
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

	result, err := parseZCodeSession(fixture.DBPath, "session-tools", "devbox")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Messages, 2)

	assistant := result.Messages[0]
	assert.Equal(t, RoleAssistant, assistant.Role)
	assert.True(t, assistant.HasToolUse)
	require.Len(t, assistant.ToolCalls, 1)
	assert.Equal(t, "call-read", assistant.ToolCalls[0].ToolUseID)
	assert.Equal(t, "Read", assistant.ToolCalls[0].ToolName)
	assert.Equal(t, "Read", assistant.ToolCalls[0].Category)
	assert.Equal(t, `{"file_path":"auth.go"}`, assistant.ToolCalls[0].InputJSON)

	toolResult := result.Messages[1]
	assert.Equal(t, RoleUser, toolResult.Role)
	require.Len(t, toolResult.ToolResults, 1)
	assert.Equal(t, "call-read", toolResult.ToolResults[0].ToolUseID)
	assert.Equal(t, len("package auth"), toolResult.ToolResults[0].ContentLength)
	assert.Equal(t, "package auth", DecodeContent(toolResult.ToolResults[0].ContentRaw))
}

func TestZCodeOpenCodeStyleReasoningAndToolParts(t *testing.T) {
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

	result, err := parseZCodeSession(fixture.DBPath, "session-opencode-parts", "devbox")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Messages, 2)

	assistant := result.Messages[1]
	assert.Equal(t, RoleAssistant, assistant.Role)
	assert.True(t, assistant.HasThinking)
	assert.True(t, assistant.HasToolUse)
	assert.Equal(t, "I should inspect the auth flow.", assistant.ThinkingText)
	assert.Equal(
		t,
		"[Thinking]\nI should inspect the auth flow.\n[/Thinking]\nI'll inspect the auth flow.",
		assistant.Content,
	)
	require.Len(t, assistant.ToolCalls, 1)
	assert.Equal(t, "call-read", assistant.ToolCalls[0].ToolUseID)
	assert.Equal(t, "Read", assistant.ToolCalls[0].ToolName)
	assert.Equal(t, `{"file_path":"auth.go"}`, assistant.ToolCalls[0].InputJSON)
	require.Len(t, assistant.ToolResults, 1)
	assert.Equal(t, "call-read", assistant.ToolResults[0].ToolUseID)
	assert.Equal(t, len("package auth"), assistant.ToolResults[0].ContentLength)
	assert.Equal(t, "package auth", DecodeContent(assistant.ToolResults[0].ContentRaw))
}

func TestZCodeOpenCodeStyleFailedToolPart(t *testing.T) {
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

	result, err := parseZCodeSession(fixture.DBPath, "session-tool-error", "devbox")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Messages, 1)

	assistant := result.Messages[0]
	assert.Equal(t, RoleAssistant, assistant.Role)
	assert.True(t, assistant.HasToolUse)
	require.Len(t, assistant.ToolCalls, 1)
	require.Len(t, assistant.ToolResults, 1)
	assert.Equal(t, "call-read", assistant.ToolResults[0].ToolUseID)
	assert.Equal(t, len("permission denied"), assistant.ToolResults[0].ContentLength)
	assert.Equal(t, "permission denied", DecodeContent(assistant.ToolResults[0].ContentRaw))
}

func TestZCodeFingerprintTracksUsageMtime(t *testing.T) {
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
	require.NoError(t, os.Chtimes(fixture.DBPath, oldDBMtime, oldDBMtime))

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{fixture.CLIRoot},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	expected := time.Date(2026, 7, 6, 13, 10, 0, 0, time.UTC).UnixNano()
	assert.Equal(t, expected, fingerprint.MTimeNS)
}

func TestZCodeFingerprintTracksDBMtimeForUsageOnlyChanges(t *testing.T) {
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
	require.NoError(t, os.Chtimes(fixture.DBPath, dbMtime, dbMtime))

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{fixture.CLIRoot},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	assert.Equal(t, dbMtime.UnixNano(), fingerprint.MTimeNS)
}

func TestZCodeFallsBackToDBMtimeWhenTimestampsAreMissing(t *testing.T) {
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
	require.NoError(t, os.Chtimes(fixture.DBPath, dbMtime, dbMtime))

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{fixture.CLIRoot},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, dbMtime.UnixNano(), outcome.Results[0].Result.Session.File.Mtime)
}

func TestZCodeParsesSessionWhenUsageTableIsMissing(t *testing.T) {
	fixture := newZCodeTestFixture(t)
	_, err := fixture.database.Exec(`DROP TABLE model_usage`)
	require.NoError(t, err)
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
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	result := outcome.Results[0].Result
	assert.Equal(t, "zcode:session-no-usage-table", result.Session.ID)
	assert.Equal(t, 0, result.Session.MessageCount)
	assert.Empty(t, result.UsageEvents)
	assert.False(t, result.Session.HasTotalOutputTokens)
	assert.False(t, result.Session.HasPeakContextTokens)
}

func TestZCodeMissingTranscriptTables(t *testing.T) {
	fixture := newZCodeTestFixture(t)
	_, err := fixture.database.Exec(`DROP TABLE part`)
	require.NoError(t, err)
	_, err = fixture.database.Exec(`DROP TABLE message`)
	require.NoError(t, err)
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

	result, err := parseZCodeSession(fixture.DBPath, "session-no-transcript", "devbox")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 0, result.Session.MessageCount)
	assert.Equal(t, 0, result.Session.UserMessageCount)
	assert.Empty(t, result.Messages)
	require.Len(t, result.UsageEvents, 1)
	assert.Equal(t, 200, result.Session.TotalOutputTokens)
	assert.True(t, result.Session.HasTotalOutputTokens)
}

func TestZCodeProviderRootWithoutDB(t *testing.T) {
	root := t.TempDir()
	cliRoot := filepath.Join(root, ".zcode", "cli")
	require.NoError(t, os.MkdirAll(cliRoot, 0o755))

	provider, ok := NewProvider(AgentZCode, ProviderConfig{
		Roots:   []string{cliRoot},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func nullableZCodeString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func TestZCodeSystemMessagesAreMarkedSystem(t *testing.T) {
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

	result, err := parseZCodeSession(fixture.DBPath, "session-system", "devbox")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, RoleSystem, result.Messages[0].Role)
	assert.True(t, result.Messages[0].IsSystem)
}
