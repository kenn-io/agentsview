package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newHermesTestProvider builds a concrete hermesProvider for the given roots so
// package tests can exercise the folded parse, discovery, and source-lookup
// behavior directly through provider methods.
func newHermesTestProvider(t *testing.T, roots ...string) *hermesProvider {
	t.Helper()
	provider, ok := NewProvider(AgentHermes, ProviderConfig{
		Roots:   roots,
		Machine: "local",
	})
	require.True(t, ok)
	hp, ok := provider.(*hermesProvider)
	require.True(t, ok)
	return hp
}

// parseHermesTestSession parses a Hermes transcript at path through the
// provider-owned parse method, replacing the removed package-level
// ParseHermesSession entrypoint.
func parseHermesTestSession(
	t *testing.T, path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()
	return newHermesTestProvider(t).parseSession(path, project, machine)
}

// parseHermesTestArchive parses a Hermes archive root through the provider-owned
// archive method, replacing the removed package-level ParseHermesArchive
// entrypoint.
func parseHermesTestArchive(
	t *testing.T, root, project, machine string,
) ([]ParseResult, error) {
	t.Helper()
	return newHermesTestProvider(t).parseArchive(root, project, machine)
}

// discoverHermesTestSessions discovers Hermes sources under root through the
// provider source set, replacing the removed package-level
// DiscoverHermesSessions entrypoint.
func discoverHermesTestSessions(t *testing.T, root string) []DiscoveredFile {
	t.Helper()
	return discoverHermesSessions(root)
}

// findHermesTestSourceFile resolves a Hermes session ID to a transcript path
// through the provider source set, replacing the removed package-level
// FindHermesSourceFile entrypoint.
func findHermesTestSourceFile(t *testing.T, sessionsDir, sessionID string) string {
	t.Helper()
	return findHermesSourceFile(sessionsDir, sessionID)
}

func runHermesJSONLTest(
	t *testing.T, filename, content string,
) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	if filename == "" {
		filename = "20260403_153620_5a3e2ff1.jsonl"
	}
	path := createTestFile(t, filename, content)
	sess, msgs, err := parseHermesTestSession(
		t, path, "", "local",
	)
	require.NoError(t, err)
	return sess, msgs
}

func runHermesJSONTest(
	t *testing.T, filename, content string,
) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	if filename == "" {
		filename = "session_20260403_153620_5a3e2ff1.json"
	}
	path := createTestFile(t, filename, content)
	sess, msgs, err := parseHermesTestSession(
		t, path, "", "local",
	)
	require.NoError(t, err)
	return sess, msgs
}

func createHermesStateDB(t *testing.T, root string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(root, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
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
}

func TestParseHermesArchive_StateDBMetadataUsageAndTranscriptChoice(
	t *testing.T,
) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	createHermesStateDB(t, root)
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionsDir, "session_child.json"),
		[]byte(`{
			"platform":"discord",
			"session_start":"2026-05-14T10:00:00Z",
			"last_updated":"2026-05-14T10:20:00Z",
			"messages":[
				{"role":"user","content":"hello from transcript","timestamp":"2026-05-14T10:01:00Z"},
				{"role":"assistant","content":"reply from transcript","timestamp":"2026-05-14T10:02:00Z"}
			]
		}`),
		0o644,
	))

	results, err := parseHermesTestArchive(t, root, "", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)

	res := results[0]
	assert.Equal(t, "hermes:child", res.Session.ID)
	assert.Equal(t, "hermes:parent", res.Session.ParentSessionID)
	assert.Equal(t, RelContinuation, res.Session.RelationshipType)
	assert.Equal(t, "Child Session", res.Session.SessionName)
	assert.Equal(t, "hermes-discord", res.Session.Project)
	assert.Equal(t, "child", res.Session.SourceSessionID)
	assert.Equal(t, "hermes-state-db", res.Session.SourceVersion)
	require.Len(t, res.Messages, 2)
	assert.Equal(t, "hello from transcript", res.Messages[0].Content)
	require.Len(t, res.UsageEvents, 1)
	assert.Equal(t, "gpt-5.4", res.UsageEvents[0].Model)
	assert.Equal(t, 300, res.UsageEvents[0].InputTokens)
	assert.Equal(t, 70, res.UsageEvents[0].OutputTokens)
	assert.Equal(t, 20, res.UsageEvents[0].CacheReadInputTokens)
	assert.Equal(t, 5, res.UsageEvents[0].CacheCreationInputTokens)
	assert.Equal(t, 9, res.UsageEvents[0].ReasoningTokens)
}

func TestParseHermesArchive_FallsBackToTranscriptsWhenStateDBUnreadable(
	t *testing.T,
) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "state.db"),
		[]byte("not sqlite"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionsDir, "session_child.json"),
		[]byte(`{
			"platform":"discord",
			"session_start":"2026-05-14T10:00:00Z",
			"last_updated":"2026-05-14T10:20:00Z",
			"messages":[
				{"role":"user","content":"hello from transcript","timestamp":"2026-05-14T10:01:00Z"},
				{"role":"assistant","content":"reply from transcript","timestamp":"2026-05-14T10:02:00Z"}
			]
		}`),
		0o644,
	))

	results, err := parseHermesTestArchive(t, root, "override-project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)

	res := results[0]
	assert.Equal(t, "hermes:child", res.Session.ID)
	assert.Equal(t, "override-project", res.Session.Project)
	assert.Equal(t, "hello from transcript", res.Session.FirstMessage)
	assert.Len(t, res.Messages, 2)
	assert.Empty(t, res.UsageEvents)
}

func TestParseHermesArchive_UsesStateMessagesWhenJSONLIsLowerQuality(
	t *testing.T,
) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	createHermesStateDB(t, root)
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionsDir, "child.jsonl"),
		[]byte(strings.Join([]string{
			`{"role":"session_meta","platform":"discord","timestamp":"2026-05-14T10:00:00Z"}`,
			`{"role":"user","content":"x","timestamp":"2026-05-14T10:01:00Z"}`,
		}, "\n")),
		0o644,
	))

	results, err := parseHermesTestArchive(t, root, "", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)

	res := results[0]
	require.Len(t, res.Messages, 1)
	assert.Equal(t, "state db only has one message", res.Messages[0].Content)
	assert.Equal(t, "hermes-state-db", res.Session.SourceVersion)
}

func TestParseHermesArchiveIncludesTranscriptsMissingFromStateDB(
	t *testing.T,
) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	createHermesStateDB(t, root)
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionsDir, "session_extra.json"),
		[]byte(`{
			"platform":"linux",
			"session_start":"2026-05-14T11:00:00Z",
			"messages":[
				{"role":"user","content":"extra transcript","timestamp":"2026-05-14T11:01:00Z"}
			]
		}`),
		0o644,
	))

	results, err := parseHermesTestArchive(t, root, "", "local")
	require.NoError(t, err)
	require.Len(t, results, 2)

	ids := []string{results[0].Session.ID, results[1].Session.ID}
	assert.Contains(t, ids, "hermes:child")
	assert.Contains(t, ids, "hermes:extra")
}

func TestBuildHermesStateResultKeepsUsageOnlySessions(t *testing.T) {
	res, ok := buildHermesStateResult(
		hermesStateSession{
			id:          "usage-only",
			source:      "cli",
			model:       "gpt-5.4",
			startedAt:   time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
			inputTokens: 10,
		},
		nil, t.TempDir(), "state.db", "", "local",
	)
	require.True(t, ok)
	assert.Equal(t, "hermes:usage-only", res.Session.ID)
	assert.Empty(t, res.Messages)
	require.Len(t, res.UsageEvents, 1)
	assert.Equal(t, 10, res.UsageEvents[0].InputTokens)
}

func TestBuildHermesStateResultPopulatesSessionAggregateTokens(t *testing.T) {
	res, ok := buildHermesStateResult(
		hermesStateSession{
			id:              "agg",
			source:          "cli",
			model:           "gpt-5.5",
			startedAt:       time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
			inputTokens:     300,
			outputTokens:    70,
			cacheReadTokens: 20,
		},
		nil, t.TempDir(), "state.db", "", "local",
	)
	require.True(t, ok)
	// Session-aggregate columns (session list / detail / stats portfolio)
	// must reflect Hermes's own authoritative accounting, not stay at 0.
	assert.True(t, res.Session.HasTotalOutputTokens)
	assert.Equal(t, 70, res.Session.TotalOutputTokens)
	assert.True(t, res.Session.HasPeakContextTokens)
	assert.Equal(t, 320, res.Session.PeakContextTokens)
}

func TestHermesUsageEvents_UnknownCostStatusLeavesCostUnpriced(t *testing.T) {
	// estimated_cost_usd is a literal 0 with cost_status "unknown":
	// Hermes does not actually know the cost, so the event must leave
	// CostUSD nil and let agentsview price it from the catalog rather
	// than asserting a false known $0.
	events := hermesUsageEvents(hermesStateSession{
		model:         "gpt-5.5",
		inputTokens:   100,
		outputTokens:  50,
		estimatedCost: sql.NullFloat64{Float64: 0, Valid: true},
		costStatus:    "unknown",
	}, "hermes:unknown-cost")
	require.Len(t, events, 1)
	assert.Nil(t, events[0].CostUSD)
}

func TestHermesUsageEvents_IncludedCostStatusIsKnownZero(t *testing.T) {
	// cost_status "included" backed by a real cost_source means the
	// usage is genuinely covered/free, so a known $0 is correct even
	// when no estimated cost is recorded.
	events := hermesUsageEvents(hermesStateSession{
		model:        "openrouter/owl-alpha",
		inputTokens:  100,
		outputTokens: 50,
		costStatus:   "included",
		costSource:   "provider_models_api",
	}, "hermes:included-cost")
	require.Len(t, events, 1)
	require.NotNil(t, events[0].CostUSD)
	assert.Equal(t, 0.0, *events[0].CostUSD)
}

func TestHermesUsageEvents_IncludedWithoutCostSourceLeavesCostUnpriced(t *testing.T) {
	// Hermes marks models it does not price (e.g. gpt-5.5) cost_status
	// "included" with cost_source "none". That is a default placeholder,
	// not a confident free-usage signal, so the event must leave CostUSD
	// nil and let agentsview price it from the catalog instead of
	// reporting a false $0.
	for _, src := range []string{"none", ""} {
		events := hermesUsageEvents(hermesStateSession{
			model:         "gpt-5.5",
			inputTokens:   100,
			outputTokens:  50,
			estimatedCost: sql.NullFloat64{Float64: 0, Valid: true},
			costStatus:    "included",
			costSource:    src,
		}, "hermes:included-no-source")
		require.Len(t, events, 1)
		assert.Nilf(t, events[0].CostUSD,
			"cost_source %q must not produce a confident $0", src)
	}
}

func TestHermesUsageEvents_ActualCostTakesPrecedence(t *testing.T) {
	events := hermesUsageEvents(hermesStateSession{
		model:         "gpt-5.5",
		inputTokens:   100,
		actualCost:    sql.NullFloat64{Float64: 1.23, Valid: true},
		estimatedCost: sql.NullFloat64{Float64: 0, Valid: true},
		costStatus:    "unknown",
	}, "hermes:actual-cost")
	require.Len(t, events, 1)
	require.NotNil(t, events[0].CostUSD)
	assert.Equal(t, 1.23, *events[0].CostUSD)
}

func TestHermesUsageEvents_PositiveEstimateUsedWhenStatusEmpty(t *testing.T) {
	events := hermesUsageEvents(hermesStateSession{
		model:         "gpt-5.5",
		inputTokens:   100,
		estimatedCost: sql.NullFloat64{Float64: 0.5, Valid: true},
		costStatus:    "",
	}, "hermes:estimate-cost")
	require.Len(t, events, 1)
	require.NotNil(t, events[0].CostUSD)
	assert.Equal(t, 0.5, *events[0].CostUSD)
}

func TestBuildHermesStateResultLeavesAggregatesUnsetWhenNoTokens(t *testing.T) {
	res, ok := buildHermesStateResult(
		hermesStateSession{
			id:        "no-usage",
			source:    "cli",
			model:     "gpt-5.5",
			startedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
			// No token counts: a session with messages but no recorded usage.
		},
		[]hermesStateMessage{{
			role:      "user",
			content:   "hi",
			timestamp: time.Date(2026, 5, 14, 12, 0, 1, 0, time.UTC),
		}},
		t.TempDir(), "state.db", "", "local",
	)
	require.True(t, ok)
	assert.False(t, res.Session.HasTotalOutputTokens)
	assert.Zero(t, res.Session.TotalOutputTokens)
	assert.False(t, res.Session.HasPeakContextTokens)
	assert.Zero(t, res.Session.PeakContextTokens)
}

func TestCountHermesUsersSkipsToolResultOnlyMessages(t *testing.T) {
	got := countHermesUsers([]ParsedMessage{
		{Role: RoleUser, Content: "real prompt"},
		{Role: RoleUser, ToolResults: []ParsedToolResult{{ToolUseID: "tc1"}}},
		{Role: RoleUser, Content: "system", IsSystem: true},
	})
	assert.Equal(t, 1, got)
}

func TestDiscoverHermesSessionsFindsTranscriptOnlyRoot(
	t *testing.T,
) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	path := filepath.Join(sessionsDir, "session_child.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"messages":[]}`), 0o644))

	files := discoverHermesTestSessions(t, root)
	require.Len(t, files, 1)
	assert.Equal(t, path, files[0].Path)
}

func TestParseHermesSession_CompactionBoundaryIsSystem(t *testing.T) {
	sess, msgs := runHermesJSONTest(t, "", `{
		"platform":"darwin",
		"session_start":"2026-05-14T10:00:00Z",
		"last_updated":"2026-05-14T10:02:00Z",
		"messages":[
			{"role":"user","content":"[CONTEXT COMPACTION - REFERENCE ONLY]\nold context","timestamp":"2026-05-14T10:01:00Z"},
			{"role":"user","content":"real prompt","timestamp":"2026-05-14T10:02:00Z"}
		]
	}`)

	require.Len(t, msgs, 2)
	assert.True(t, msgs[0].IsSystem)
	assert.True(t, msgs[0].IsCompactBoundary)
	assert.Equal(t, "system", msgs[0].SourceType)
	assert.Equal(t, "compact_boundary", msgs[0].SourceSubtype)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "real prompt", sess.FirstMessage)
}

// --- JSONL format tests ---

func TestParseHermesSession_JSONL_Basic(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta","platform":"linux","model":"gpt-4","timestamp":"2026-04-03T15:27:00.000000"}`,
		`{"role":"user","content":"Fix the tests","timestamp":"2026-04-03T15:27:21.014566"}`,
		`{"role":"assistant","content":"I will fix them now.","timestamp":"2026-04-03T15:27:25.123456"}`,
	}, "\n")

	sess, msgs := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)

	assertSessionMeta(t, sess,
		"hermes:20260403_153620_5a3e2ff1",
		"hermes-linux", AgentHermes,
	)
	assert.Equal(t, "Fix the tests", sess.FirstMessage)
	assertMessageCount(t, sess.MessageCount, 2)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "local", sess.Machine)

	require.Len(t, msgs, 2)
	assertMessage(t, msgs[0], RoleUser, "Fix the tests")
	assertMessage(t, msgs[1], RoleAssistant, "I will fix them now.")
	assert.Equal(t, 0, msgs[0].Ordinal)
	assert.Equal(t, 1, msgs[1].Ordinal)
}

func TestParseHermesSession_JSONL_ToolCalls(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta","platform":"darwin","timestamp":"2026-04-03T15:00:00.000000"}`,
		`{"role":"user","content":"Read main.go","timestamp":"2026-04-03T15:00:01.000000"}`,
		`{"role":"assistant","content":"","reasoning":"Let me read it.","finish_reason":"tool_calls","tool_calls":[{"id":"tc1","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}],"timestamp":"2026-04-03T15:00:02.000000"}`,
		`{"role":"tool","content":"package main\n","tool_call_id":"tc1","timestamp":"2026-04-03T15:00:03.000000"}`,
	}, "\n")

	sess, msgs := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)

	assertMessageCount(t, sess.MessageCount, 3)
	assert.Equal(t, 1, sess.UserMessageCount)

	// Assistant message with reasoning and tool call.
	assert.True(t, msgs[1].HasThinking)
	assert.True(t, msgs[1].HasToolUse)
	assert.Contains(t, msgs[1].Content, "[Thinking]")
	assert.Contains(t, msgs[1].Content, "Let me read it.")
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "read_file", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].Category)
	assert.Equal(t, "tc1", msgs[1].ToolCalls[0].ToolUseID)

	// Tool result message.
	assert.Equal(t, RoleUser, msgs[2].Role)
	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "tc1", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal(t,
		"package main\n",
		DecodeContent(msgs[2].ToolResults[0].ContentRaw),
	)
}

func TestParseHermesSession_JSONL_MultipleToolCalls(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta","timestamp":"2026-04-03T15:00:00.000000"}`,
		`{"role":"user","content":"Check both files","timestamp":"2026-04-03T15:00:01.000000"}`,
		`{"role":"assistant","content":"Reading both.","tool_calls":[{"id":"tc1","function":{"name":"read_file","arguments":"{}"}},{"id":"tc2","function":{"name":"search_files","arguments":"{}"}}],"timestamp":"2026-04-03T15:00:02.000000"}`,
	}, "\n")

	_, msgs := runHermesJSONLTest(t, "", content)
	require.Len(t, msgs[1].ToolCalls, 2)
	assert.Equal(t, "read_file", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].Category)
	assert.Equal(t, "search_files", msgs[1].ToolCalls[1].ToolName)
	assert.Equal(t, "Grep", msgs[1].ToolCalls[1].Category)
}

func TestParseHermesSession_JSONL_NoPlatform(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta","model":"gpt-4"}`,
		`{"role":"user","content":"hello","timestamp":"2026-04-03T15:00:00.000000"}`,
	}, "\n")

	sess, _ := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)
	assert.Equal(t, "hermes", sess.Project)
}

func TestParseHermesSession_JSONL_ExplicitProject(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta","platform":"darwin"}`,
		`{"role":"user","content":"hello","timestamp":"2026-04-03T15:00:00.000000"}`,
	}, "\n")

	path := createTestFile(
		t, "20260403_153620_abc.jsonl", content,
	)
	sess, _, err := parseHermesTestSession(
		t, path, "my-project", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "my-project", sess.Project)
}

func TestParseHermesSession_JSONL_EmptyMessages(t *testing.T) {
	content := `{"role":"session_meta","platform":"linux"}`
	sess, msgs := runHermesJSONLTest(t, "", content)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func TestParseHermesSession_JSONL_EmptyUserContent(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta"}`,
		`{"role":"user","content":"","timestamp":"2026-04-03T15:00:00.000000"}`,
		`{"role":"user","content":"   ","timestamp":"2026-04-03T15:00:01.000000"}`,
		`{"role":"user","content":"real message","timestamp":"2026-04-03T15:00:02.000000"}`,
	}, "\n")

	sess, msgs := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)
	assertMessageCount(t, sess.MessageCount, 1)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "real message", sess.FirstMessage)
	require.Len(t, msgs, 1)
}

func TestParseHermesSession_JSONL_EmptyAssistant(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta"}`,
		`{"role":"user","content":"hi","timestamp":"2026-04-03T15:00:00.000000"}`,
		`{"role":"assistant","content":"","timestamp":"2026-04-03T15:00:01.000000"}`,
	}, "\n")

	sess, msgs := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)
	// Empty assistant with no tool calls is skipped.
	assertMessageCount(t, sess.MessageCount, 1)
	require.Len(t, msgs, 1)
}

func TestParseHermesSession_JSONL_ToolResultNoID(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta"}`,
		`{"role":"user","content":"hi","timestamp":"2026-04-03T15:00:00.000000"}`,
		`{"role":"tool","content":"result","tool_call_id":"","timestamp":"2026-04-03T15:00:01.000000"}`,
	}, "\n")

	sess, msgs := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)
	// Tool result without ID is skipped.
	assertMessageCount(t, sess.MessageCount, 1)
	require.Len(t, msgs, 1)
}

func TestParseHermesSession_JSONL_InvalidJSON(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta"}`,
		`not valid json`,
		`{"role":"user","content":"hello","timestamp":"2026-04-03T15:00:00.000000"}`,
	}, "\n")

	sess, msgs := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)
	// Invalid line is skipped, valid messages are parsed.
	assertMessageCount(t, sess.MessageCount, 1)
	require.Len(t, msgs, 1)
}

func TestParseHermesSession_JSONL_Timestamps(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta","timestamp":"2026-04-03T10:00:00.000000"}`,
		`{"role":"user","content":"first","timestamp":"2026-04-03T10:00:05.000000"}`,
		`{"role":"assistant","content":"reply","timestamp":"2026-04-03T10:05:00.000000"}`,
	}, "\n")

	sess, _ := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)

	wantStart := time.Date(
		2026, 4, 3, 10, 0, 0, 0, time.Local,
	)
	wantEnd := time.Date(
		2026, 4, 3, 10, 5, 0, 0, time.Local,
	)
	assertTimestamp(t, sess.StartedAt, wantStart)
	assertTimestamp(t, sess.EndedAt, wantEnd)
}

func TestParseHermesSession_JSONL_FirstMessageTruncation(t *testing.T) {
	longMsg := strings.Repeat("a", 400)
	content := strings.Join([]string{
		`{"role":"session_meta"}`,
		`{"role":"user","content":"` + longMsg + `","timestamp":"2026-04-03T15:00:00.000000"}`,
	}, "\n")

	sess, _ := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)
	// truncate clips at 300 + 3 ellipsis = 303.
	assert.Equal(t, 303, len(sess.FirstMessage))
}

func TestParseHermesSession_JSONL_Errors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, _, err := parseHermesTestSession(
			t, "/nonexistent/file.jsonl", "", "local",
		)
		assert.Error(t, err)
	})
}

// --- JSON format tests ---

func TestParseHermesSession_JSON_Basic(t *testing.T) {
	content := `{
		"platform": "darwin",
		"session_start": "2026-04-03T15:00:00.000000",
		"last_updated": "2026-04-03T15:05:00.000000",
		"messages": [
			{"role": "user", "content": "Deploy the app", "timestamp": "2026-04-03T15:00:01.000000"},
			{"role": "assistant", "content": "Deploying now.", "timestamp": "2026-04-03T15:00:05.000000"}
		]
	}`

	sess, msgs := runHermesJSONTest(t, "", content)
	require.NotNil(t, sess)

	assertSessionMeta(t, sess,
		"hermes:20260403_153620_5a3e2ff1",
		"hermes-darwin", AgentHermes,
	)
	assert.Equal(t, "Deploy the app", sess.FirstMessage)
	assertMessageCount(t, sess.MessageCount, 2)
	assert.Equal(t, 1, sess.UserMessageCount)

	require.Len(t, msgs, 2)
	assertMessage(t, msgs[0], RoleUser, "Deploy the app")
	assertMessage(t, msgs[1], RoleAssistant, "Deploying now.")
}

func TestParseHermesSession_JSON_ToolCalls(t *testing.T) {
	content := `{
		"session_start": "2026-04-03T15:00:00.000000",
		"messages": [
			{"role": "user", "content": "Edit the file", "timestamp": "2026-04-03T15:00:01.000000"},
			{
				"role": "assistant",
				"content": "Editing.",
				"reasoning": "I need to patch it.",
				"tool_calls": [
					{"id": "tc1", "function": {"name": "patch", "arguments": "{\"file\":\"main.go\"}"}}
				],
				"timestamp": "2026-04-03T15:00:02.000000"
			},
			{"role": "tool", "content": "patched", "tool_call_id": "tc1", "timestamp": "2026-04-03T15:00:03.000000"}
		]
	}`

	sess, msgs := runHermesJSONTest(t, "", content)
	require.NotNil(t, sess)
	assertMessageCount(t, sess.MessageCount, 3)

	assert.True(t, msgs[1].HasThinking)
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "patch", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Edit", msgs[1].ToolCalls[0].Category)

	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "tc1", msgs[2].ToolResults[0].ToolUseID)
}

func TestParseHermesSession_JSON_ReasoningDetails(t *testing.T) {
	content := `{
		"messages": [
			{"role": "user", "content": "think hard", "timestamp": "2026-04-03T15:00:00.000000"},
			{"role": "assistant", "content": "done", "reasoning_details": "deep thought", "timestamp": "2026-04-03T15:00:01.000000"}
		]
	}`

	_, msgs := runHermesJSONTest(t, "", content)
	// reasoning_details is a fallback for reasoning.
	assert.True(t, msgs[1].HasThinking)
	assert.Contains(t, msgs[1].Content, "deep thought")
}

func TestParseHermesSession_JSON_EmptyMessages(t *testing.T) {
	content := `{"platform":"linux","messages":[]}`
	sess, msgs := runHermesJSONTest(t, "", content)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func TestParseHermesSession_JSON_NoPlatform(t *testing.T) {
	content := `{
		"messages": [
			{"role": "user", "content": "hi", "timestamp": "2026-04-03T15:00:00.000000"}
		]
	}`
	sess, _ := runHermesJSONTest(t, "", content)
	require.NotNil(t, sess)
	assert.Equal(t, "hermes", sess.Project)
}

func TestParseHermesSession_JSON_MessageTimestampsExtendBounds(
	t *testing.T,
) {
	// Per-message timestamps can extend session bounds beyond
	// the envelope session_start/last_updated.
	content := `{
		"session_start": "2026-04-03T15:00:00.000000",
		"last_updated": "2026-04-03T15:05:00.000000",
		"messages": [
			{"role": "user", "content": "early", "timestamp": "2026-04-03T14:50:00.000000"},
			{"role": "assistant", "content": "late", "timestamp": "2026-04-03T15:10:00.000000"}
		]
	}`

	sess, _ := runHermesJSONTest(t, "", content)
	require.NotNil(t, sess)

	wantStart := time.Date(
		2026, 4, 3, 14, 50, 0, 0, time.Local,
	)
	wantEnd := time.Date(
		2026, 4, 3, 15, 10, 0, 0, time.Local,
	)
	assertTimestamp(t, sess.StartedAt, wantStart)
	assertTimestamp(t, sess.EndedAt, wantEnd)
}

func TestParseHermesSession_JSON_Errors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, _, err := parseHermesTestSession(
			t, "/nonexistent/file.json", "", "local",
		)
		assert.Error(t, err)
	})

	t.Run("not an object", func(t *testing.T) {
		path := createTestFile(
			t, "session_bad.json", `"just a string"`,
		)
		_, _, err := parseHermesTestSession(t, path, "", "local")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid JSON")
	})
}

// --- Routing test ---

func TestParseHermesSession_RoutesOnExtension(t *testing.T) {
	jsonlContent := strings.Join([]string{
		`{"role":"session_meta","platform":"linux"}`,
		`{"role":"user","content":"jsonl path","timestamp":"2026-04-03T15:00:00.000000"}`,
	}, "\n")
	jsonContent := `{
		"platform": "darwin",
		"messages": [
			{"role":"user","content":"json path","timestamp":"2026-04-03T15:00:00.000000"}
		]
	}`

	t.Run("jsonl", func(t *testing.T) {
		sess, _ := runHermesJSONLTest(
			t, "20260403_test.jsonl", jsonlContent,
		)
		require.NotNil(t, sess)
		assert.Equal(t, "hermes-linux", sess.Project)
	})

	t.Run("json", func(t *testing.T) {
		sess, _ := runHermesJSONTest(
			t, "session_test.json", jsonContent,
		)
		require.NotNil(t, sess)
		assert.Equal(t, "hermes-darwin", sess.Project)
	})
}

// --- HermesSessionID ---

func TestHermesSessionID(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"20260403_153620_5a3e2ff1.jsonl", "20260403_153620_5a3e2ff1"},
		{"20260403_153620_5a3e2ff1.json", "20260403_153620_5a3e2ff1"},
		{"session_20260403_abc.json", "20260403_abc"},
		{"session_20260403_abc.jsonl", "20260403_abc"},
		{"plain_name.jsonl", "plain_name"},
		{"no_ext", "no_ext"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HermesSessionID(tt.name)
			if got != tt.want {
				t.Errorf(
					"HermesSessionID(%q) = %q, want %q",
					tt.name, got, tt.want,
				)
			}
		})
	}
}

// --- parseHermesTimestamp ---

func TestParseHermesTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  time.Time
	}{
		{
			"microseconds",
			"2026-04-03T15:27:21.014566",
			time.Date(
				2026, 4, 3, 15, 27, 21, 14566000,
				time.Local,
			),
		},
		{
			"no fractional seconds",
			"2026-04-03T15:27:21",
			time.Date(
				2026, 4, 3, 15, 27, 21, 0,
				time.Local,
			),
		},
		{
			"RFC3339 with timezone",
			"2026-04-03T15:27:21Z",
			time.Date(
				2026, 4, 3, 15, 27, 21, 0, time.UTC,
			),
		},
		{
			"RFC3339 with offset",
			"2026-04-03T15:27:21+05:30",
			time.Date(
				2026, 4, 3, 15, 27, 21, 0,
				time.FixedZone("", 5*3600+30*60),
			),
		},
		{
			"empty string",
			"",
			time.Time{},
		},
		{
			"garbage",
			"not-a-timestamp",
			time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHermesTimestamp(tt.input)
			if tt.want.IsZero() {
				assertZeroTimestamp(t, got, "timestamp")
			} else {
				assertTimestamp(t, got, tt.want)
			}
		})
	}
}

// --- stripHermesSkillPrefix ---

func TestStripHermesSkillPrefix(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"no prefix",
			"Fix the bug in main.go",
			"Fix the bug in main.go",
		},
		{
			"skill with user instruction",
			`[SYSTEM: The user has invoked the "commit" skill. Please follow the instructions below.]` +
				"\n\n---\nname: commit\n---\ncommit stuff\n\n" +
				"The user has provided the following instruction alongside the skill invocation: Please commit my changes",
			"Please commit my changes",
		},
		{
			"skill without user instruction",
			`[SYSTEM: The user has invoked the "review" skill. Please follow the instructions.]` +
				"\n\n---\nname: review\n---\nreview stuff\n\n",
			"[Skill: review]",
		},
		{
			"skill with runtime note stripped",
			`[SYSTEM: The user has invoked the "debug" skill. Follow instructions.]` +
				"\n\nThe user has provided the following instruction alongside the skill invocation: Fix it" +
				"\n\n[Runtime note: some internal detail]",
			"Fix it",
		},
		{
			"empty user instruction falls back to skill name",
			`[SYSTEM: The user has invoked the "test" skill. Follow instructions.]` +
				"\n\nThe user has provided the following instruction alongside the skill invocation:   ",
			"[Skill: test]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripHermesSkillPrefix(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// --- DiscoverHermesSessions ---

func TestDiscoverHermesSessions(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		wantFiles []string
	}{
		{
			name: "JSONL only",
			files: map[string]string{
				"20260403_153620_aaa.jsonl": "{}",
				"20260404_100000_bbb.jsonl": "{}",
			},
			wantFiles: []string{
				"20260403_153620_aaa.jsonl",
				"20260404_100000_bbb.jsonl",
			},
		},
		{
			name: "JSON only",
			files: map[string]string{
				"session_20260403_aaa.json": "{}",
			},
			wantFiles: []string{
				"session_20260403_aaa.json",
			},
		},
		{
			name: "JSONL takes priority over JSON",
			files: map[string]string{
				"20260403_153620_aaa.jsonl":        "{}",
				"session_20260403_153620_aaa.json": "{}",
			},
			wantFiles: []string{
				"20260403_153620_aaa.jsonl",
			},
		},
		{
			name: "non-session JSON ignored",
			files: map[string]string{
				"config.json":    "{}",
				"random.json":    "{}",
				"notes.txt":      "hi",
				"session_a.json": "{}",
			},
			wantFiles: []string{
				"session_a.json",
			},
		},
		{
			name: "directories ignored",
			files: map[string]string{
				"20260403_aaa.jsonl":  "{}",
				"subdir/nested.jsonl": "{}",
			},
			wantFiles: []string{
				"20260403_aaa.jsonl",
			},
		},
		{
			name:      "empty dir",
			files:     map[string]string{},
			wantFiles: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)
			files := discoverHermesTestSessions(t, dir)
			assertDiscoveredFiles(
				t, files, tt.wantFiles, AgentHermes,
			)
		})
	}

	t.Run("empty string dir", func(t *testing.T) {
		files := discoverHermesTestSessions(t, "")
		assert.Nil(t, files)
	})

	t.Run("nonexistent dir", func(t *testing.T) {
		files := discoverHermesTestSessions(
			t, filepath.Join(t.TempDir(), "nope"),
		)
		assert.Nil(t, files)
	})
}

// --- FindHermesSourceFile ---

func TestFindHermesSourceFile(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		sessionID string
		wantFile  string
	}{
		{
			name:      "finds JSONL",
			files:     map[string]string{"20260403_aaa.jsonl": "{}"},
			sessionID: "20260403_aaa",
			wantFile:  "20260403_aaa.jsonl",
		},
		{
			name:      "finds JSON",
			files:     map[string]string{"session_20260403_aaa.json": "{}"},
			sessionID: "20260403_aaa",
			wantFile:  "session_20260403_aaa.json",
		},
		{
			name: "prefers JSONL over JSON",
			files: map[string]string{
				"20260403_aaa.jsonl":        "{}",
				"session_20260403_aaa.json": "{}",
			},
			sessionID: "20260403_aaa",
			wantFile:  "20260403_aaa.jsonl",
		},
		{
			name:      "not found",
			files:     map[string]string{"20260403_aaa.jsonl": "{}"},
			sessionID: "nonexistent",
			wantFile:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)

			got := findHermesTestSourceFile(t, dir, tt.sessionID)
			want := ""
			if tt.wantFile != "" {
				want = filepath.Join(dir, tt.wantFile)
			}
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}

	t.Run("invalid session IDs", func(t *testing.T) {
		dir := t.TempDir()
		setupFileSystem(t, dir, map[string]string{
			"20260403_aaa.jsonl": "{}",
		})
		for _, id := range []string{"", "../etc/passwd", "a/b", "a b"} {
			got := findHermesTestSourceFile(t, dir, id)
			if got != "" {
				t.Errorf(
					"FindHermesSourceFile(%q) = %q, want empty",
					id, got,
				)
			}
		}
	})
}

// --- Taxonomy ---

func TestHermesToolTaxonomy(t *testing.T) {
	tests := []struct {
		tool     string
		category string
	}{
		{"read_file", "Read"},
		{"write_file", "Write"},
		{"edit_file", "Edit"},
		{"search_files", "Grep"},
		{"run_command", "Bash"},
		{"execute_command", "Bash"},
		{"patch", "Edit"},
		{"terminal", "Bash"},
		{"execute_code", "Bash"},
		{"vision_analyze", "Read"},
		{"delegate_task", "Task"},
		{"browser_navigate", "Tool"},
		{"browser_click", "Tool"},
		{"todo", "Tool"},
		{"memory", "Tool"},
		{"skill_view", "Tool"},
	}

	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			got := NormalizeToolCategory(tt.tool)
			if got != tt.category {
				t.Errorf(
					"NormalizeToolCategory(%q) = %q, want %q",
					tt.tool, got, tt.category,
				)
			}
		})
	}
}

// --- Registry ---

func TestHermesRegistryEntry(t *testing.T) {
	var found *AgentDef
	for i := range Registry {
		if Registry[i].Type == AgentHermes {
			found = &Registry[i]
			break
		}
	}
	require.NotNil(t, found, "AgentHermes not in Registry")

	assert.Equal(t, "Hermes Agent", found.DisplayName)
	assert.Equal(t, "HERMES_SESSIONS_DIR", found.EnvVar)
	assert.Equal(t, "hermes_sessions_dirs", found.ConfigKey)
	assert.Equal(t, "hermes:", found.IDPrefix)
	assert.True(t, found.FileBased)
	assert.Contains(t, found.DefaultDirs, ".hermes/sessions")
	// Hermes is provider-authoritative: discovery and source lookup live on the
	// hermesProvider, not on legacy AgentDef hooks. The watch-root resolvers
	// stay because they are provider-owned and consumed by watcher setup.
	assert.Nil(t, found.DiscoverFunc)
	assert.Nil(t, found.FindSourceFunc)
	assert.NotNil(t, found.WatchRootsFunc)
	assert.NotNil(t, found.ShallowWatchRootsFunc)
}

// --- File info ---

func TestParseHermesSession_FileInfo(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta"}`,
		`{"role":"user","content":"hi","timestamp":"2026-04-03T15:00:00.000000"}`,
	}, "\n")

	path := createTestFile(
		t, "20260403_test.jsonl", content,
	)
	info, err := os.Stat(path)
	require.NoError(t, err)

	sess, _, err := parseHermesTestSession(t, path, "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Equal(t, path, sess.File.Path)
	assert.Equal(t, info.Size(), sess.File.Size)
	assert.Equal(t, info.ModTime().UnixNano(), sess.File.Mtime)
}
