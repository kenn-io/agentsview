package parser

import (
	"database/sql"
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const shelleySchema = `
CREATE TABLE conversations (
	conversation_id TEXT PRIMARY KEY,
	slug TEXT,
	user_initiated BOOLEAN NOT NULL DEFAULT TRUE,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	cwd TEXT,
	archived BOOLEAN NOT NULL DEFAULT FALSE,
	parent_conversation_id TEXT,
	model TEXT,
	current_generation INTEGER NOT NULL DEFAULT 1,
	is_draft BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE TABLE messages (
	message_id TEXT PRIMARY KEY,
	conversation_id TEXT NOT NULL,
	sequence_id INTEGER NOT NULL,
	type TEXT NOT NULL,
	llm_data TEXT,
	user_data TEXT,
	usage_data TEXT,
	display_data TEXT,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	generation INTEGER NOT NULL DEFAULT 1,
	excluded_from_context BOOLEAN NOT NULL DEFAULT FALSE
);
`

func newShelleyTestDB(t *testing.T) (string, string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, shelleyDBName)
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open shelley test db")
	t.Cleanup(func() { db.Close() })
	_, err = db.ExecContext(t.Context(), shelleySchema)
	require.NoError(t, err, "create shelley schema")
	return dir, path, db
}

func newShelleyTestDBWithSchema(
	t *testing.T, schema string,
) (string, string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, shelleyDBName)
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open shelley test db")
	t.Cleanup(func() { db.Close() })
	_, err = db.ExecContext(t.Context(), schema)
	require.NoError(t, err, "create shelley schema")
	return dir, path, db
}

func seedShelleyConversation(
	t *testing.T, db *sql.DB,
	id, slug, cwd, model, parent string, userInitiated bool,
	createdAt, updatedAt string,
) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO conversations
			(conversation_id, slug, user_initiated, created_at,
			 updated_at, cwd, parent_conversation_id, model)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, slug, userInitiated, createdAt, updatedAt, cwd,
		nullable(parent), nullable(model),
	)
	require.NoError(t, err, "seed shelley conversation %s", id)
}

func seedShelleyMessage(
	t *testing.T, db *sql.DB,
	convID string, seq int, generation int, msgType,
	llmData, userData, usageData, createdAt string,
) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO messages
			(message_id, conversation_id, sequence_id, generation,
			 type, llm_data, user_data, usage_data, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		convID+"-m"+itoa(seq), convID, seq, generation, msgType,
		nullable(llmData), nullable(userData), nullable(usageData),
		createdAt,
	)
	require.NoError(t, err, "seed shelley message %s/%d", convID, seq)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func itoa(i int) string {
	return string(rune('0' + i))
}

// seedShelleyMainConversation seeds a conversation that exercises text,
// thinking, a tool call, a tool result, per-message token usage, and a
// second-generation (post-compaction) message.
func seedShelleyMainConversation(t *testing.T, db *sql.DB) {
	t.Helper()
	seedShelleyConversation(
		t, db, "cMAIN1", "Add Shelley parser",
		"/home/user/dev/myapp", "claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:05:00Z",
	)
	seedShelleyMessage(t, db, "cMAIN1", 1, 1, "user",
		`{"Role":0,"Content":[{"Type":2,"Text":"Add a Shelley parser"}]}`,
		"", "", "2026-06-15T10:00:00Z")
	seedShelleyMessage(t, db, "cMAIN1", 2, 1, "agent",
		`{"Role":1,"Content":[`+
			`{"Type":3,"Thinking":"Plan it."},`+
			`{"Type":2,"Text":"On it."},`+
			`{"ID":"toolu_1","Type":5,"ToolName":"bash","ToolInput":{"cmd":"ls"}}]}`,
		"",
		`{"input_tokens":1000,"cache_creation_input_tokens":200,`+
			`"cache_read_input_tokens":50,"output_tokens":300,`+
			`"cost_usd":0.012,"model":"claude-sonnet-4-6"}`,
		"2026-06-15T10:00:05Z")
	seedShelleyMessage(t, db, "cMAIN1", 3, 1, "tool",
		`{"Role":0,"Content":[{"Type":6,"ToolUseID":"toolu_1",`+
			`"ToolResult":[{"Type":2,"Text":"file1\nfile2"}],"ToolError":false}]}`,
		"", "", "2026-06-15T10:00:06Z")
	seedShelleyMessage(t, db, "cMAIN1", 4, 1, "agent",
		`{"Role":1,"Content":[{"Type":2,"Text":"Done."}]}`,
		"",
		`{"input_tokens":1500,"cache_creation_input_tokens":0,`+
			`"cache_read_input_tokens":0,"output_tokens":120,`+
			`"cost_usd":0.004,"model":"claude-sonnet-4-6"}`,
		"2026-06-15T10:00:10Z")
	// Second generation: a post-compaction continuation that must still
	// be included in the full history view.
	seedShelleyMessage(t, db, "cMAIN1", 5, 2, "agent",
		`{"Role":1,"Content":[{"Type":2,"Text":"Continued after compaction."}]}`,
		"",
		`{"input_tokens":800,"cache_creation_input_tokens":0,`+
			`"cache_read_input_tokens":0,"output_tokens":90,`+
			`"model":"claude-sonnet-4-6"}`,
		"2026-06-15T10:05:00Z")
}

// parseShelleyConversationDirectForTest exercises the Shelley provider's
// single-conversation parse in isolation, mirroring the legacy direct-parse
// entrypoint the fold removed.
func parseShelleyConversationDirectForTest(
	t *testing.T, dbPath, rawID, machine string, _ os.FileInfo,
) (*ParseResult, error) {
	t.Helper()
	return shelleyParseMember(t.Context(),
		multiSessionSource{Container: dbPath, MemberID: rawID},
		ParseRequest{Machine: machine},
	)
}

func TestParseShelleyConversation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)

	info, err := os.Stat(dbPath)
	require.NoError(err, "stat db")

	result, err := parseShelleyConversationDirectForTest(
		t, dbPath, "cMAIN1", "test-machine", info,
	)
	require.NoError(err, "parseConversationDirect")
	require.NotNil(result, "expected result")

	sess := result.Session
	assert.Equal("shelley:cMAIN1", sess.ID, "ID")
	assert.Equal(AgentShelley, sess.Agent, "Agent")
	assert.Equal("test-machine", sess.Machine, "Machine")
	assert.Equal("myapp", sess.Project, "Project")
	assert.Equal("/home/user/dev/myapp", sess.Cwd, "Cwd")
	assert.Equal("Add Shelley parser", sess.SessionName, "SessionName")
	assert.Equal("Add a Shelley parser", sess.FirstMessage, "FirstMessage")
	assert.Equal(5, sess.MessageCount, "MessageCount")
	assert.Equal(1, sess.UserMessageCount, "UserMessageCount")
	assert.Equal(dbPath+"#cMAIN1", sess.File.Path, "File.Path")
	assert.Empty(sess.ParentSessionID, "ParentSessionID")

	// Session token aggregates: peak context is a MAX, total output a SUM.
	assert.Equal(1500, sess.PeakContextTokens, "PeakContextTokens")
	assert.Equal(510, sess.TotalOutputTokens, "TotalOutputTokens")
	assert.True(sess.HasPeakContextTokens, "HasPeakContextTokens")
	assert.True(sess.HasTotalOutputTokens, "HasTotalOutputTokens")

	msgs := result.Messages
	require.Len(msgs, 5, "messages len")

	// Ordinals come straight from sequence_id.
	for i, m := range msgs {
		assert.Equalf(i+1, m.Ordinal, "msg[%d].Ordinal", i)
	}

	// User message.
	assert.Equal(RoleUser, msgs[0].Role, "msg[0].Role")
	assert.Equal("Add a Shelley parser", msgs[0].Content, "msg[0].Content")

	// Agent message: text + thinking + tool call + tokens.
	a := msgs[1]
	assert.Equal(RoleAssistant, a.Role, "msg[1].Role")
	assert.Equal("On it.", a.Content, "msg[1].Content")
	assert.True(a.HasThinking, "msg[1].HasThinking")
	assert.Equal("Plan it.", a.ThinkingText, "msg[1].ThinkingText")
	assert.True(a.HasToolUse, "msg[1].HasToolUse")
	require.Len(a.ToolCalls, 1, "msg[1].ToolCalls len")
	assert.Equal("bash", a.ToolCalls[0].ToolName, "tool name")
	assert.Equal("Bash", a.ToolCalls[0].Category, "tool category")
	assert.Equal("toolu_1", a.ToolCalls[0].ToolUseID, "tool use id")
	assert.JSONEq(`{"cmd":"ls"}`, a.ToolCalls[0].InputJSON, "tool input")
	assert.Equal(1250, a.ContextTokens, "msg[1].ContextTokens")
	assert.Equal(300, a.OutputTokens, "msg[1].OutputTokens")
	assert.True(a.HasContextTokens, "msg[1].HasContextTokens")
	assert.True(a.HasOutputTokens, "msg[1].HasOutputTokens")
	assert.Equal("claude-sonnet-4-6", a.Model, "msg[1].Model")
	assert.NotEmpty(a.TokenUsage, "msg[1].TokenUsage raw")

	// Tool result message: user role, empty content, paired by ToolUseID.
	tr := msgs[2]
	assert.Equal(RoleUser, tr.Role, "msg[2].Role")
	assert.Empty(tr.Content, "msg[2].Content")
	require.Len(tr.ToolResults, 1, "msg[2].ToolResults len")
	assert.Equal("toolu_1", tr.ToolResults[0].ToolUseID, "result tool use id")
	assert.Equal("file1\nfile2",
		DecodeContent(tr.ToolResults[0].ContentRaw), "decoded tool result")

	// Final first-generation agent message.
	assert.Equal("Done.", msgs[3].Content, "msg[3].Content")
	assert.Equal(120, msgs[3].OutputTokens, "msg[3].OutputTokens")

	// Second-generation message is included in the history.
	assert.Equal("Continued after compaction.", msgs[4].Content, "msg[4].Content")
}

func TestParseShelleySubagentRelationship(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)
	seedShelleyConversation(
		t, db, "cSUB01", "do subtask",
		"/home/user/dev/myapp", "claude-sonnet-4-6", "cMAIN1", false,
		"2026-06-15T10:01:00Z", "2026-06-15T10:01:30Z",
	)
	seedShelleyMessage(t, db, "cSUB01", 1, 1, "user",
		`{"Role":0,"Content":[{"Type":2,"Text":"do the subtask"}]}`,
		"", "", "2026-06-15T10:01:00Z")
	seedShelleyMessage(t, db, "cSUB01", 2, 1, "agent",
		`{"Role":1,"Content":[{"Type":2,"Text":"subtask done"}]}`,
		"", "", "2026-06-15T10:01:30Z")

	info, err := os.Stat(dbPath)
	require.NoError(err, "stat db")

	result, err := parseShelleyConversationDirectForTest(
		t, dbPath, "cSUB01", "test-machine", info,
	)
	require.NoError(err, "parse subagent")
	require.NotNil(result, "expected subagent result")
	assert.Equal("shelley:cSUB01", result.Session.ID, "subagent ID")
	assert.Equal("shelley:cMAIN1", result.Session.ParentSessionID, "parent")
	assert.Equal(RelSubagent, result.Session.RelationshipType, "relationship")
}

func TestDiscoverAndFindShelley(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)

	// Discovery and source lookup are provider-owned after the fold; the
	// physical DB still surfaces as a single source and a raw conversation
	// ID resolves to its virtual path.
	provider, ok := NewProvider(AgentShelley, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1, "discovered sources")
	assert.Equal(dbPath, sources[0].DisplayPath, "discovered db path")
	assert.Equal(AgentShelley, sources[0].Provider, "discovered provider")

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "cMAIN1",
	})
	require.NoError(err)
	require.True(ok, "find existing")
	assert.Equal(dbPath+"#cMAIN1", found.DisplayPath, "find existing path")

	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "cNOPE0",
	})
	require.NoError(err)
	assert.False(ok, "find missing")

	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "../escape",
	})
	require.NoError(err)
	assert.False(ok, "reject path-like id")

	assert.True(ShelleyConversationExists(t.Context(), dbPath, "cMAIN1"), "exists")
	assert.False(ShelleyConversationExists(t.Context(), dbPath, "cNOPE0"), "not exists")

	// Empty root yields no discovery.
	emptyProvider, ok := NewProvider(AgentShelley, ProviderConfig{Roots: []string{t.TempDir()}})
	require.True(ok)
	emptySources, err := emptyProvider.Discover(t.Context())
	require.NoError(err)
	assert.Empty(emptySources, "empty dir discovery")
}

func TestParseShelleyVirtualPath(t *testing.T) {
	assert := assert.New(t)

	dbPath := "/home/user/.config/shelley/shelley.db"
	got, id, ok := parseShelleyVirtualPath(dbPath + "#cABC123")
	require.True(t, ok, "valid virtual path")
	assert.Equal(dbPath, got, "db path")
	assert.Equal("cABC123", id, "conversation id")

	_, _, ok = parseShelleyVirtualPath("/x/other.db#id")
	assert.False(ok, "wrong db name")
	_, _, ok = parseShelleyVirtualPath(dbPath)
	assert.False(ok, "no separator")
	_, _, ok = parseShelleyVirtualPath(dbPath + "#")
	assert.False(ok, "empty id")
}

func TestAgentByPrefixShelley(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByPrefix("shelley:cMAIN1")
	require.True(ok, "shelley prefix matches")
	assert.Equal(AgentShelley, def.Type, "shelley type")

	def, ok = AgentByPrefix("host~shelley:cMAIN1")
	require.True(ok, "host-prefixed shelley matches")
	assert.Equal(AgentShelley, def.Type, "host-prefixed type")

	// A colon-free ID must fall back to Claude, never Shelley.
	def, ok = AgentByPrefix("cMAIN1")
	require.True(ok, "colon-free id matches Claude fallback")
	assert.Equal(AgentClaude, def.Type, "colon-free routes to Claude")
}

func TestShelleyTokenCount(t *testing.T) {
	tests := []struct {
		name string
		in   jsontext.Value
		want int
	}{
		{"empty", jsontext.Value(""), 0},
		{"plain", jsontext.Value("1234"), 1234},
		{"zero", jsontext.Value("0"), 0},
		{"negative", jsontext.Value("-5"), 0},
		{"float", jsontext.Value("42.0"), 42},
		{"garbage", jsontext.Value("abc"), 0},
		{"implausible", jsontext.Value("9999999999999"), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shelleyTokenCount(tt.in))
		})
	}
}

// TestParseShelleyTimestampFormats verifies the real on-disk DATETIME
// format Shelley writes (space-separated, no T/Z) parses into non-zero
// timestamps. Shelley relies on SQLite's DEFAULT CURRENT_TIMESTAMP, so
// stored values look like "2026-06-15 10:00:00".
func TestParseShelleyTimestampFormats(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, dbPath, db := newShelleyTestDB(t)
	seedShelleyConversation(
		t, db, "cTIME1", "ts", "/home/user/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15 10:00:00", "2026-06-15 10:00:30",
	)
	seedShelleyMessage(t, db, "cTIME1", 1, 1, "user",
		`{"Role":0,"Content":[{"Type":2,"Text":"hi"}]}`,
		"", "", "2026-06-15 10:00:00")

	info, err := os.Stat(dbPath)
	require.NoError(err, "stat db")
	result, err := parseShelleyConversationDirectForTest(t, dbPath, "cTIME1", "m", info)
	require.NoError(err)
	require.NotNil(result)
	assert.False(result.Session.StartedAt.IsZero(), "StartedAt parsed")
	assert.False(result.Session.EndedAt.IsZero(), "EndedAt parsed")
	assert.Positive(result.Session.File.Mtime, "File.Mtime positive")
}

// TestParseShelleyRobustContent verifies graceful handling of unknown
// content types, redacted thinking, malformed llm_data, and token
// capture on errored assistant turns (type="error").
func TestParseShelleyRobustContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, dbPath, db := newShelleyTestDB(t)
	seedShelleyConversation(
		t, db, "cROB1", "robust", "/home/user/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:40Z",
	)
	seedShelleyMessage(t, db, "cROB1", 1, 1, "user",
		`{"Role":0,"Content":[{"Type":2,"Text":"go"}]}`,
		"", "", "2026-06-15T10:00:00Z")
	// Unknown content type (99) is ignored; redacted thinking (2) is
	// surfaced as a placeholder; the text block survives.
	seedShelleyMessage(t, db, "cROB1", 2, 1, "agent",
		`{"Role":1,"Content":[{"Type":4},{"Type":99,"Text":"ignored"},`+
			`{"Type":2,"Text":"real text"}]}`,
		"", "", "2026-06-15T10:00:05Z")
	// Malformed llm_data with no user_data fallback is dropped.
	seedShelleyMessage(t, db, "cROB1", 3, 1, "agent",
		`{not json`, "", "", "2026-06-15T10:00:06Z")
	// Errored assistant turn stored as type="error" still carries usage.
	seedShelleyMessage(t, db, "cROB1", 4, 1, "error",
		`{"Role":1,"Content":[{"Type":2,"Text":"request failed"}]}`,
		"",
		`{"input_tokens":700,"output_tokens":40,"model":"claude-sonnet-4-6"}`,
		"2026-06-15T10:00:40Z")

	info, err := os.Stat(dbPath)
	require.NoError(err, "stat db")
	result, err := parseShelleyConversationDirectForTest(t, dbPath, "cROB1", "m", info)
	require.NoError(err, "must not error on robust content")
	require.NotNil(result)

	// user + agent(real text) + error message; the malformed row dropped.
	require.Len(result.Messages, 3, "messages len")
	agentMsg := result.Messages[1]
	assert.Equal("real text", agentMsg.Content, "unknown type ignored, text kept")
	assert.Contains(agentMsg.ThinkingText, "redacted", "redacted thinking placeholder")

	errMsg := result.Messages[2]
	assert.True(errMsg.IsSystem, "error message flagged system")
	assert.Equal("request failed", errMsg.Content, "error text preserved")
	assert.Equal(40, errMsg.OutputTokens, "errored-turn output tokens captured")
	assert.Equal(700, errMsg.ContextTokens, "errored-turn input tokens captured")

	// Errored-turn tokens roll up into the session totals.
	assert.Equal(40, result.Session.TotalOutputTokens, "session output total")
}

func TestParseShelleyUsageOnlyRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, dbPath, db := newShelleyTestDB(t)
	seedShelleyConversation(
		t, db, "cUSG1", "usage only", "/home/user/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:40Z",
	)
	seedShelleyMessage(t, db, "cUSG1", 1, 1, "user",
		`{"Role":0,"Content":[{"Type":2,"Text":"go"}]}`,
		"", "", "2026-06-15T10:00:00Z")
	seedShelleyMessage(t, db, "cUSG1", 2, 1, "error",
		`{not json`, "",
		`{"input_tokens":123,"output_tokens":33,"model":"claude-sonnet-4-6"}`,
		"2026-06-15T10:00:40Z")

	info, err := os.Stat(dbPath)
	require.NoError(err, "stat db")
	result, err := parseShelleyConversationDirectForTest(t, dbPath, "cUSG1", "m", info)
	require.NoError(err, "must not error on usage-only row")
	require.NotNil(result)

	require.Len(result.Messages, 2, "messages len")
	usageOnly := result.Messages[1]
	assert.True(usageOnly.IsSystem, "usage-only row is metadata")
	assert.Empty(usageOnly.Content, "usage-only content")
	assert.Equal(123, usageOnly.ContextTokens, "context tokens")
	assert.Equal(33, usageOnly.OutputTokens, "output tokens")
	assert.True(usageOnly.HasContextTokens, "has context tokens")
	assert.True(usageOnly.HasOutputTokens, "has output tokens")
	assert.NotEmpty(usageOnly.TokenUsage, "raw token usage")
	assert.Equal(123, result.Session.PeakContextTokens, "session peak context")
	assert.Equal(33, result.Session.TotalOutputTokens, "session output total")
}

// TestParseShelleyWebSearchToolResult verifies that a server-side web
// search turn is preserved: the web_search call (Type 7) becomes a tool
// call, and the web_search_tool_result (Type 8) whose nested
// web_search_result blocks (Type 9) carry Title/URL instead of Text is
// stored with readable content rather than dropped empty.
func TestParseShelleyWebSearchToolResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, dbPath, db := newShelleyTestDB(t)
	seedShelleyConversation(
		t, db, "cWEB1", "search the web", "/home/user/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:30Z",
	)
	seedShelleyMessage(t, db, "cWEB1", 1, 1, "user",
		`{"Role":0,"Content":[{"Type":2,"Text":"what is iota"}]}`,
		"", "", "2026-06-15T10:00:00Z")
	seedShelleyMessage(t, db, "cWEB1", 2, 1, "agent",
		`{"Role":1,"Content":[`+
			`{"ID":"srvtoolu_1","Type":7,"ToolName":"web_search",`+
			`"ToolInput":{"query":"golang iota"}},`+
			`{"Type":8,"ToolUseID":"srvtoolu_1","ToolResult":[`+
			`{"Type":9,"Title":"Go iota explained","URL":"https://go.dev/iota"},`+
			`{"Type":9,"Title":"Effective Go",`+
			`"URL":"https://go.dev/doc/effective_go"}]}]}`,
		"", "", "2026-06-15T10:00:05Z")

	info, err := os.Stat(dbPath)
	require.NoError(err, "stat db")
	result, err := parseShelleyConversationDirectForTest(t, dbPath, "cWEB1", "m", info)
	require.NoError(err, "parse web search conversation")
	require.NotNil(result, "expected result")

	require.Len(result.Messages, 2, "messages len")
	agent := result.Messages[1]

	// The server-side web_search call is captured as a tool call.
	require.Len(agent.ToolCalls, 1, "web_search tool call captured")
	assert.Equal("web_search", agent.ToolCalls[0].ToolName, "tool name")

	// The web_search_tool_result is paired to the server tool call and its
	// nested Title/URL result blocks are preserved, not stored empty.
	require.Len(agent.ToolResults, 1, "web_search tool result captured")
	assert.Equal("srvtoolu_1",
		agent.ToolResults[0].ToolUseID, "result tool use id")
	decoded := DecodeContent(agent.ToolResults[0].ContentRaw)
	assert.Equal("Go iota explained https://go.dev/iota\n"+
		"Effective Go https://go.dev/doc/effective_go",
		decoded, "web search result title/url preserved")
	assert.Positive(agent.ToolResults[0].ContentLength,
		"content length nonzero")
}

// TestShelleySameSecondChangeSignal verifies the split change signal: a
// message appended in the same wall-clock second (so updated_at is
// unchanged) leaves File.Mtime fixed at the conversation's real
// timestamp but shifts the content fingerprint (stored in file_hash), so
// the sync skip still re-parses. The watcher-only ShelleySourceMtime,
// compared for inequality, must also change. The stored values and the
// meta skip query must agree, or unchanged conversations would re-parse
// forever.
func TestShelleySameSecondChangeSignal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, dbPath, db := newShelleyTestDB(t)
	seedShelleyConversation(
		t, db, "cSEC1", "same second", "/home/user/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:00Z",
	)
	seedShelleyMessage(t, db, "cSEC1", 1, 1, "user",
		`{"Role":0,"Content":[{"Type":2,"Text":"first"}]}`,
		"", "", "2026-06-15T10:00:00Z")

	info, err := os.Stat(dbPath)
	require.NoError(err, "stat db")

	conn, err := OpenShelleyDB(dbPath)
	require.NoError(err, "open shelley db")
	defer conn.Close()

	first, err := parseShelleyConversationDirectForTest(t, dbPath, "cSEC1", "m", info)
	require.NoError(err)
	require.NotNil(first)
	mtime1 := first.Session.File.Mtime
	hash1 := first.Session.File.Hash

	// File.Mtime is the conversation's real timestamp, so it stays a valid
	// value for modified-between range queries (never a synthetic future).
	base := parseTimestamp("2026-06-15T10:00:00Z").UnixNano()
	assert.Equal(base, mtime1, "File.Mtime is the real updated_at")
	assert.NotEmpty(hash1, "content fingerprint set")

	metas1, err := ListShelleyConversationMetas(conn, dbPath)
	require.NoError(err)
	require.Len(metas1, 1)
	assert.Equal(mtime1, metas1[0].FileMtime,
		"stored File.Mtime must match the meta skip timestamp")
	assert.Equal(hash1, metas1[0].Fingerprint,
		"stored file_hash must match the meta skip fingerprint")

	srcMtime1, err := ShelleySourceMtime(t.Context(), dbPath+"#cSEC1")
	require.NoError(err)
	assert.Positive(srcMtime1, "SourceMtime resolves the conversation")

	// Append a second message in the SAME second: updated_at is unchanged,
	// sequence_id advances (1 -> 2) and the payload adds content bytes.
	seedShelleyMessage(t, db, "cSEC1", 2, 1, "agent",
		`{"Role":1,"Content":[{"Type":2,"Text":"second"}]}`,
		"", "", "2026-06-15T10:00:00Z")

	second, err := parseShelleyConversationDirectForTest(t, dbPath, "cSEC1", "m", info)
	require.NoError(err)
	require.NotNil(second)

	assert.Equal(mtime1, second.Session.File.Mtime,
		"same-second append leaves the real timestamp unchanged")
	assert.NotEqual(hash1, second.Session.File.Hash,
		"a same-second append must change the content fingerprint")

	metas2, err := ListShelleyConversationMetas(conn, dbPath)
	require.NoError(err)
	require.Len(metas2, 1)
	assert.Equal(second.Session.File.Hash, metas2[0].Fingerprint,
		"meta fingerprint tracks the same-second append")

	srcMtime2, err := ShelleySourceMtime(t.Context(), dbPath+"#cSEC1")
	require.NoError(err)
	assert.NotEqual(srcMtime1, srcMtime2,
		"watcher SourceMtime tracks the same-second append")
}

func TestShelleyNumericUserInitiatedScans(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	schema := strings.Replace(
		shelleySchema,
		"user_initiated BOOLEAN NOT NULL DEFAULT TRUE",
		"user_initiated INTEGER NOT NULL DEFAULT 1",
		1,
	)
	_, dbPath, db := newShelleyTestDBWithSchema(t, schema)
	seedShelleyConversation(
		t, db, "cNUM1", "numeric user_initiated",
		"/home/user/dev/app", "claude-sonnet-4-6", "cPARENT",
		false, "2026-06-15T10:00:00Z",
		"2026-06-15T10:00:10Z",
	)
	seedShelleyMessage(t, db, "cNUM1", 1, 1, "user",
		`{"Role":0,"Content":[{"Type":2,"Text":"hello"}]}`,
		"", "", "2026-06-15T10:00:00Z")

	info, err := os.Stat(dbPath)
	require.NoError(err, "stat db")
	result, err := parseShelleyConversationDirectForTest(
		t, dbPath, "cNUM1", "m", info,
	)
	require.NoError(err)
	require.NotNil(result)
	assert.Equal(RelSubagent, result.Session.RelationshipType,
		"numeric false user_initiated should map to subagent")

	conn, err := OpenShelleyDB(dbPath)
	require.NoError(err, "open shelley db")
	defer conn.Close()
	metas, err := ListShelleyConversationMetas(conn, dbPath)
	require.NoError(err)
	require.Len(metas, 1)
	assert.Equal(result.Session.File.Hash, metas[0].Fingerprint,
		"parse and meta paths should convert user_initiated the same way")
}

func TestApplyShelleyUsageTolerant(t *testing.T) {
	assert := assert.New(t)

	var msg ParsedMessage
	applyShelleyUsage(&msg,
		`{"input_tokens":"-5","cache_read_input_tokens":"100",`+
			`"output_tokens":"42","model":""}`, "fallback-model")
	assert.Equal(100, msg.ContextTokens, "ContextTokens (negative input clamped)")
	assert.Equal(42, msg.OutputTokens, "OutputTokens")
	assert.True(msg.HasContextTokens, "HasContextTokens")
	assert.True(msg.HasOutputTokens, "HasOutputTokens")
	assert.Equal("fallback-model", msg.Model, "falls back to conversation model")
}
