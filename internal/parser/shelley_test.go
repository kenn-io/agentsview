package parser

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
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
	_, err = db.Exec(shelleySchema)
	require.NoError(t, err, "create shelley schema")
	return dir, path, db
}

func seedShelleyConversation(
	t *testing.T, db *sql.DB,
	id, slug, cwd, model, parent string, userInitiated bool,
	createdAt, updatedAt string,
) {
	t.Helper()
	_, err := db.Exec(
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
	_, err := db.Exec(
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
		`{"Role":0,"Content":[{"Type":0,"Text":"Add a Shelley parser"}]}`,
		"", "", "2026-06-15T10:00:00Z")
	seedShelleyMessage(t, db, "cMAIN1", 2, 1, "agent",
		`{"Role":1,"Content":[`+
			`{"Type":1,"Thinking":"Plan it."},`+
			`{"Type":0,"Text":"On it."},`+
			`{"ID":"toolu_1","Type":3,"ToolName":"bash","ToolInput":{"cmd":"ls"}}]}`,
		"",
		`{"input_tokens":1000,"cache_creation_input_tokens":200,`+
			`"cache_read_input_tokens":50,"output_tokens":300,`+
			`"cost_usd":0.012,"model":"claude-sonnet-4-6"}`,
		"2026-06-15T10:00:05Z")
	seedShelleyMessage(t, db, "cMAIN1", 3, 1, "tool",
		`{"Role":0,"Content":[{"Type":4,"ToolUseID":"toolu_1",`+
			`"ToolResult":[{"Type":0,"Text":"file1\nfile2"}],"ToolError":false}]}`,
		"", "", "2026-06-15T10:00:06Z")
	seedShelleyMessage(t, db, "cMAIN1", 4, 1, "agent",
		`{"Role":1,"Content":[{"Type":0,"Text":"Done."}]}`,
		"",
		`{"input_tokens":1500,"cache_creation_input_tokens":0,`+
			`"cache_read_input_tokens":0,"output_tokens":120,`+
			`"cost_usd":0.004,"model":"claude-sonnet-4-6"}`,
		"2026-06-15T10:00:10Z")
	// Second generation: a post-compaction continuation that must still
	// be included in the full history view.
	seedShelleyMessage(t, db, "cMAIN1", 5, 2, "agent",
		`{"Role":1,"Content":[{"Type":0,"Text":"Continued after compaction."}]}`,
		"",
		`{"input_tokens":800,"cache_creation_input_tokens":0,`+
			`"cache_read_input_tokens":0,"output_tokens":90,`+
			`"model":"claude-sonnet-4-6"}`,
		"2026-06-15T10:05:00Z")
}

func TestParseShelleyConversation(t *testing.T) {
	_, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)

	info, err := os.Stat(dbPath)
	require.NoError(t, err, "stat db")

	result, err := ParseShelleyConversationDirect(
		dbPath, "cMAIN1", "test-machine", info,
	)
	require.NoError(t, err, "ParseShelleyConversationDirect")
	require.NotNil(t, result, "expected result")

	sess := result.Session
	assert.Equal(t, "shelley:cMAIN1", sess.ID, "ID")
	assert.Equal(t, AgentShelley, sess.Agent, "Agent")
	assert.Equal(t, "test-machine", sess.Machine, "Machine")
	assert.Equal(t, "myapp", sess.Project, "Project")
	assert.Equal(t, "/home/user/dev/myapp", sess.Cwd, "Cwd")
	assert.Equal(t, "Add Shelley parser", sess.SessionName, "SessionName")
	assert.Equal(t, "Add a Shelley parser", sess.FirstMessage, "FirstMessage")
	assert.Equal(t, 5, sess.MessageCount, "MessageCount")
	assert.Equal(t, 1, sess.UserMessageCount, "UserMessageCount")
	assert.Equal(t, dbPath+"#cMAIN1", sess.File.Path, "File.Path")
	assert.Empty(t, sess.ParentSessionID, "ParentSessionID")

	// Session token aggregates: peak context is a MAX, total output a SUM.
	assert.Equal(t, 1500, sess.PeakContextTokens, "PeakContextTokens")
	assert.Equal(t, 510, sess.TotalOutputTokens, "TotalOutputTokens")
	assert.True(t, sess.HasPeakContextTokens, "HasPeakContextTokens")
	assert.True(t, sess.HasTotalOutputTokens, "HasTotalOutputTokens")

	msgs := result.Messages
	require.Len(t, msgs, 5, "messages len")

	// Ordinals come straight from sequence_id.
	for i, m := range msgs {
		assert.Equalf(t, i+1, m.Ordinal, "msg[%d].Ordinal", i)
	}

	// User message.
	assert.Equal(t, RoleUser, msgs[0].Role, "msg[0].Role")
	assert.Equal(t, "Add a Shelley parser", msgs[0].Content, "msg[0].Content")

	// Agent message: text + thinking + tool call + tokens.
	a := msgs[1]
	assert.Equal(t, RoleAssistant, a.Role, "msg[1].Role")
	assert.Equal(t, "On it.", a.Content, "msg[1].Content")
	assert.True(t, a.HasThinking, "msg[1].HasThinking")
	assert.Equal(t, "Plan it.", a.ThinkingText, "msg[1].ThinkingText")
	assert.True(t, a.HasToolUse, "msg[1].HasToolUse")
	require.Len(t, a.ToolCalls, 1, "msg[1].ToolCalls len")
	assert.Equal(t, "bash", a.ToolCalls[0].ToolName, "tool name")
	assert.Equal(t, "Bash", a.ToolCalls[0].Category, "tool category")
	assert.Equal(t, "toolu_1", a.ToolCalls[0].ToolUseID, "tool use id")
	assert.JSONEq(t, `{"cmd":"ls"}`, a.ToolCalls[0].InputJSON, "tool input")
	assert.Equal(t, 1250, a.ContextTokens, "msg[1].ContextTokens")
	assert.Equal(t, 300, a.OutputTokens, "msg[1].OutputTokens")
	assert.True(t, a.HasContextTokens, "msg[1].HasContextTokens")
	assert.True(t, a.HasOutputTokens, "msg[1].HasOutputTokens")
	assert.Equal(t, "claude-sonnet-4-6", a.Model, "msg[1].Model")
	assert.NotEmpty(t, a.TokenUsage, "msg[1].TokenUsage raw")

	// Tool result message: user role, empty content, paired by ToolUseID.
	tr := msgs[2]
	assert.Equal(t, RoleUser, tr.Role, "msg[2].Role")
	assert.Empty(t, tr.Content, "msg[2].Content")
	require.Len(t, tr.ToolResults, 1, "msg[2].ToolResults len")
	assert.Equal(t, "toolu_1", tr.ToolResults[0].ToolUseID, "result tool use id")
	assert.Equal(t, "file1\nfile2",
		DecodeContent(tr.ToolResults[0].ContentRaw), "decoded tool result")

	// Final first-generation agent message.
	assert.Equal(t, "Done.", msgs[3].Content, "msg[3].Content")
	assert.Equal(t, 120, msgs[3].OutputTokens, "msg[3].OutputTokens")

	// Second-generation message is included in the history.
	assert.Equal(t, "Continued after compaction.", msgs[4].Content, "msg[4].Content")
}

func TestParseShelleySubagentRelationship(t *testing.T) {
	_, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)
	seedShelleyConversation(
		t, db, "cSUB01", "do subtask",
		"/home/user/dev/myapp", "claude-sonnet-4-6", "cMAIN1", false,
		"2026-06-15T10:01:00Z", "2026-06-15T10:01:30Z",
	)
	seedShelleyMessage(t, db, "cSUB01", 1, 1, "user",
		`{"Role":0,"Content":[{"Type":0,"Text":"do the subtask"}]}`,
		"", "", "2026-06-15T10:01:00Z")
	seedShelleyMessage(t, db, "cSUB01", 2, 1, "agent",
		`{"Role":1,"Content":[{"Type":0,"Text":"subtask done"}]}`,
		"", "", "2026-06-15T10:01:30Z")

	info, err := os.Stat(dbPath)
	require.NoError(t, err, "stat db")

	result, err := ParseShelleyConversationDirect(
		dbPath, "cSUB01", "test-machine", info,
	)
	require.NoError(t, err, "parse subagent")
	require.NotNil(t, result, "expected subagent result")
	assert.Equal(t, "shelley:cSUB01", result.Session.ID, "subagent ID")
	assert.Equal(t, "shelley:cMAIN1", result.Session.ParentSessionID, "parent")
	assert.Equal(t, RelSubagent, result.Session.RelationshipType, "relationship")
}

func TestDiscoverAndFindShelley(t *testing.T) {
	root, dbPath, db := newShelleyTestDB(t)
	seedShelleyMainConversation(t, db)

	files := DiscoverShelleySessions(root)
	require.Len(t, files, 1, "discovered files")
	assert.Equal(t, dbPath, files[0].Path, "discovered db path")
	assert.Equal(t, AgentShelley, files[0].Agent, "discovered agent")

	assert.Equal(t, dbPath+"#cMAIN1",
		FindShelleySourceFile(root, "cMAIN1"), "find existing")
	assert.Empty(t, FindShelleySourceFile(root, "cNOPE0"), "find missing")
	assert.Empty(t, FindShelleySourceFile(root, "../escape"), "reject path-like id")
	assert.True(t, ShelleyConversationExists(dbPath, "cMAIN1"), "exists")
	assert.False(t, ShelleyConversationExists(dbPath, "cNOPE0"), "not exists")

	// Empty root yields no discovery and no source file.
	assert.Nil(t, DiscoverShelleySessions(t.TempDir()), "empty dir discovery")
}

func TestParseShelleyVirtualPath(t *testing.T) {
	dbPath := "/home/user/.config/shelley/shelley.db"
	got, id, ok := ParseShelleyVirtualPath(dbPath + "#cABC123")
	require.True(t, ok, "valid virtual path")
	assert.Equal(t, dbPath, got, "db path")
	assert.Equal(t, "cABC123", id, "conversation id")

	_, _, ok = ParseShelleyVirtualPath("/x/other.db#id")
	assert.False(t, ok, "wrong db name")
	_, _, ok = ParseShelleyVirtualPath(dbPath)
	assert.False(t, ok, "no separator")
	_, _, ok = ParseShelleyVirtualPath(dbPath + "#")
	assert.False(t, ok, "empty id")
}

func TestAgentByPrefixShelley(t *testing.T) {
	def, ok := AgentByPrefix("shelley:cMAIN1")
	require.True(t, ok, "shelley prefix matches")
	assert.Equal(t, AgentShelley, def.Type, "shelley type")

	def, ok = AgentByPrefix("host~shelley:cMAIN1")
	require.True(t, ok, "host-prefixed shelley matches")
	assert.Equal(t, AgentShelley, def.Type, "host-prefixed type")

	// A colon-free ID must fall back to Claude, never Shelley.
	def, ok = AgentByPrefix("cMAIN1")
	require.True(t, ok, "colon-free id matches Claude fallback")
	assert.Equal(t, AgentClaude, def.Type, "colon-free routes to Claude")
}

func TestShelleyTokenCount(t *testing.T) {
	tests := []struct {
		name string
		in   json.Number
		want int
	}{
		{"empty", json.Number(""), 0},
		{"plain", json.Number("1234"), 1234},
		{"zero", json.Number("0"), 0},
		{"negative", json.Number("-5"), 0},
		{"float", json.Number("42.0"), 42},
		{"garbage", json.Number("abc"), 0},
		{"implausible", json.Number("9999999999999"), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shelleyTokenCount(tt.in))
		})
	}
}

func TestApplyShelleyUsageTolerant(t *testing.T) {
	var msg ParsedMessage
	applyShelleyUsage(&msg,
		`{"input_tokens":"-5","cache_read_input_tokens":"100",`+
			`"output_tokens":"42","model":""}`, "fallback-model")
	assert.Equal(t, 100, msg.ContextTokens, "ContextTokens (negative input clamped)")
	assert.Equal(t, 42, msg.OutputTokens, "OutputTokens")
	assert.True(t, msg.HasContextTokens, "HasContextTokens")
	assert.True(t, msg.HasOutputTokens, "HasOutputTokens")
	assert.Equal(t, "fallback-model", msg.Model, "falls back to conversation model")
}
