package parser

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// warpSchema matches the relevant tables from Warp's SQLite database.
const warpSchema = `
CREATE TABLE agent_conversations (
    id INTEGER PRIMARY KEY NOT NULL,
    conversation_id TEXT NOT NULL,
    conversation_data TEXT NOT NULL,
    last_modified_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX ux_agent_conversations_conversation_id
    ON agent_conversations (conversation_id);

CREATE TABLE ai_queries (
    id INTEGER PRIMARY KEY NOT NULL,
    exchange_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    start_ts DATETIME NOT NULL,
    input TEXT NOT NULL,
    working_directory TEXT,
    output_status TEXT NOT NULL,
    model_id TEXT NOT NULL DEFAULT '',
    planning_model_id TEXT NOT NULL DEFAULT '',
    coding_model_id TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX ux_ai_queries_exchange_id
    ON ai_queries(exchange_id);
`

type WarpSeeder struct {
	db *sql.DB
	t  *testing.T
}

func (s *WarpSeeder) AddConversation(
	conversationID, conversationData, lastModified string,
) {
	s.t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO agent_conversations
		 (conversation_id, conversation_data, last_modified_at)
		 VALUES (?, ?, ?)`,
		conversationID, conversationData, lastModified,
	)
	require.NoError(s.t, err, "add conversation")
}

func (s *WarpSeeder) AddExchange(
	exchangeID, conversationID, startTS, input,
	workingDir, outputStatus, modelID string,
) {
	s.t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO ai_queries
		 (exchange_id, conversation_id, start_ts, input,
		  working_directory, output_status, model_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		exchangeID, conversationID, startTS, input,
		workingDir, outputStatus, modelID,
	)
	require.NoError(s.t, err, "add exchange")
}

func newWarpTestDB(t *testing.T) (string, *WarpSeeder, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "warp.sqlite")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open test db")
	_, err = db.Exec(warpSchema)
	require.NoError(t, err, "create schema")
	seeder := &WarpSeeder{db: db, t: t}
	return dbPath, seeder, db
}

func seedWarpConversation(t *testing.T, seeder *WarpSeeder) {
	t.Helper()

	convData := `{
		"conversation_usage_metadata":{
			"token_usage":[
				{"model_id":"Claude Opus 4","warp_tokens":100000,"byok_tokens":0}
			],
			"tool_usage_metadata":{
				"run_command_stats":{"count":3,"commands_executed":3},
				"read_files_stats":{"count":2},
				"search_codebase_stats":{"count":0},
				"grep_stats":{"count":1},
				"file_glob_stats":{"count":0},
				"apply_file_diff_stats":{"count":1,"lines_added":5,"lines_removed":2,"files_changed":1},
				"write_to_long_running_shell_command_stats":{"count":0},
				"read_mcp_resource_stats":{"count":0},
				"call_mcp_tool_stats":{"count":0},
				"suggest_plan_stats":{"count":0},
				"suggest_create_plan_stats":{"count":0},
				"read_shell_command_output_stats":{"count":0},
				"use_computer_stats":{"count":0}
			}
		}
	}`

	seeder.AddConversation(
		"conv-001", convData, "2026-04-07 10:00:00",
	)

	// User message with query text
	seeder.AddExchange(
		"ex-001", "conv-001",
		"2026-04-07 09:50:00.000000",
		`[{"Query":{"text":"Fix the JSON parsing bug in parser.go","context":[]}}]`,
		"/Users/alice/code/myproject",
		`"Completed"`, "auto-genius",
	)
	// Intermediate exchange (tool call, no user input)
	seeder.AddExchange(
		"ex-002", "conv-001",
		"2026-04-07 09:50:05.000000",
		`[]`,
		"/Users/alice/code/myproject",
		`"Completed"`, "auto-genius",
	)
	// Follow-up user message
	seeder.AddExchange(
		"ex-003", "conv-001",
		"2026-04-07 09:51:00.000000",
		`[{"Query":{"text":"Now add a test for that fix","context":[]}}]`,
		"/Users/alice/code/myproject",
		`"Completed"`, "auto-genius",
	)
}

func TestParseWarpDB_StandardConversation(t *testing.T) {
	dbPath, seeder, db := newWarpTestDB(t)
	defer db.Close()
	seedWarpConversation(t, seeder)

	sessions, err := parseWarpAll(dbPath, "testmachine")
	require.NoError(t, err, "ParseWarpDB")

	require.Len(t, sessions, 1, "sessions len")

	s := sessions[0]
	assert.Equal(t, "warp:conv-001", s.Session.ID, "ID")
	assert.Equal(t, AgentWarp, s.Session.Agent, "Agent")
	assert.Equal(t, "testmachine", s.Session.Machine, "Machine")
	assert.Equal(t, "myproject", s.Session.Project, "Project")
	assert.Equal(t, 2, s.Session.UserMessageCount, "UserMessageCount")
	assert.Equal(t, "Fix the JSON parsing bug in parser.go", s.Session.FirstMessage, "FirstMessage")

	wantPath := dbPath + "#conv-001"
	assert.Equal(t, wantPath, s.Session.File.Path, "File.Path")

	// Token usage from conversation_data
	assert.True(t, s.Session.HasTotalOutputTokens, "HasTotalOutputTokens")
	assert.Equal(t, 100000, s.Session.TotalOutputTokens, "TotalOutputTokens")

	// Check user messages
	var userMsgs, toolMsgs int
	for _, m := range s.Messages {
		if m.Role == RoleUser {
			userMsgs++
		}
		if m.HasToolUse {
			toolMsgs++
		}
	}
	assert.Equal(t, 2, userMsgs, "userMsgs")
	// 3 run_command + 2 read_files + 1 grep + 1 apply_file_diff = 7
	assert.Equal(t, 7, toolMsgs, "toolMsgs")
}

func TestParseWarpSession_SingleConversation(t *testing.T) {
	dbPath, seeder, db := newWarpTestDB(t)
	defer db.Close()
	seedWarpConversation(t, seeder)

	sess, msgs, err := parseWarpSession(
		dbPath, "conv-001", "testmachine",
	)
	require.NoError(t, err, "parseWarpSession")
	require.NotNil(t, sess, "expected non-nil session")

	assert.Equal(t, "warp:conv-001", sess.ID, "ID")
	assert.Equal(t, AgentWarp, sess.Agent, "Agent")

	// First user message
	assert.Equal(t, RoleUser, msgs[0].Role, "msgs[0].Role")
	assert.Equal(t, "Fix the JSON parsing bug in parser.go", msgs[0].Content, "msgs[0].Content")
	// Second user message
	assert.Equal(t, RoleUser, msgs[1].Role, "msgs[1].Role")
	assert.Equal(t, "Now add a test for that fix", msgs[1].Content, "msgs[1].Content")
}

func TestListWarpSessionMeta(t *testing.T) {
	dbPath, seeder, db := newWarpTestDB(t)
	defer db.Close()
	seedWarpConversation(t, seeder)

	metas, err := ListWarpSessionMeta(dbPath)
	require.NoError(t, err, "ListWarpSessionMeta")

	require.Len(t, metas, 1, "metas len")
	assert.Equal(t, "conv-001", metas[0].SessionID, "SessionID")
	assert.Equal(t, dbPath+"#conv-001", metas[0].VirtualPath, "VirtualPath")
	assert.NotZero(t, metas[0].FileMtime, "expected non-zero FileMtime")
}

func TestParseWarpDB_EmptyConversation(t *testing.T) {
	dbPath, seeder, db := newWarpTestDB(t)
	defer db.Close()

	seeder.AddConversation(
		"conv-empty", "{}", "2026-04-07 10:00:00",
	)

	sessions, err := parseWarpAll(dbPath, "m")
	require.NoError(t, err, "ParseWarpDB")
	assert.Empty(t, sessions, "sessions len")
}

func TestParseWarpDB_NoQueryText(t *testing.T) {
	dbPath, seeder, db := newWarpTestDB(t)
	defer db.Close()

	seeder.AddConversation(
		"conv-notext", "{}", "2026-04-07 10:00:00",
	)
	// Only empty exchanges
	seeder.AddExchange(
		"ex-x1", "conv-notext",
		"2026-04-07 09:50:00",
		`[]`, "/tmp", `"Completed"`, "auto",
	)

	sessions, err := parseWarpAll(dbPath, "m")
	require.NoError(t, err, "ParseWarpDB")
	assert.Empty(t, sessions, "sessions len")
}

func TestParseWarpDB_NonExistent(t *testing.T) {
	sessions, err := parseWarpAll(
		"/nonexistent/warp.sqlite", "m",
	)
	require.NoError(t, err)
	assert.Nil(t, sessions, "expected nil sessions for non-existent db")
}

func TestExtractWarpQueryText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"empty array", "[]", ""},
		{"with text", `[{"Query":{"text":"hello world","context":[]}}]`, "hello world"},
		{"no query key", `[{"Other":{}}]`, ""},
		{"invalid json", `not json`, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractWarpQueryText(tc.input)
			assert.Equal(t, tc.want, got, "text")
		})
	}
}

func TestParseWarpTimestamp(t *testing.T) {
	tests := []struct {
		input string
		year  int
	}{
		{"2026-04-07 08:55:40", 2026},
		{"2026-04-07 08:55:40.412505", 2026},
		{"", 0},
	}

	for _, tc := range tests {
		ts := parseWarpTimestamp(tc.input)
		if tc.year == 0 {
			assert.True(t, ts.IsZero(), "expected zero time for %q", tc.input)
		} else {
			assert.Equal(t, tc.year, ts.Year(), "year for %q", tc.input)
		}
	}
}

func TestWarpDBPath(t *testing.T) {
	// Create a temp dir with warp.sqlite
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "warp.sqlite")

	// Before creating the file
	assert.Empty(t, warpDBPath(dir), "not found")

	// Create the file (sql.Open is lazy; Ping forces creation)
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	db.Close()

	assert.Equal(t, dbPath, warpDBPath(dir), "found")
}

func TestParseWarpConversationMeta(t *testing.T) {
	data := `{
		"conversation_usage_metadata":{
			"token_usage":[
				{"warp_tokens":1000,"byok_tokens":200},
				{"warp_tokens":500,"byok_tokens":0}
			],
			"tool_usage_metadata":{
				"run_command_stats":{"count":5},
				"read_files_stats":{"count":3},
				"grep_stats":{"count":2},
				"apply_file_diff_stats":{"count":1},
				"search_codebase_stats":{"count":0},
				"file_glob_stats":{"count":0},
				"write_to_long_running_shell_command_stats":{"count":0},
				"read_mcp_resource_stats":{"count":0},
				"call_mcp_tool_stats":{"count":0},
				"suggest_plan_stats":{"count":0},
				"suggest_create_plan_stats":{"count":0},
				"read_shell_command_output_stats":{"count":0},
				"use_computer_stats":{"count":0}
			}
		}
	}`

	meta := parseWarpConversationMeta(data)
	assert.Equal(t, 1700, meta.totalTokens, "totalTokens")
	assert.Equal(t, 5, meta.toolStats.RunCommand, "RunCommand")
	assert.Equal(t, 3, meta.toolStats.ReadFiles, "ReadFiles")
	assert.Equal(t, 2, meta.toolStats.Grep, "Grep")
	assert.Equal(t, 1, meta.toolStats.ApplyFileDiff, "ApplyFileDiff")
}

func TestParseWarpConversationMeta_Empty(t *testing.T) {
	meta := parseWarpConversationMeta("{}")
	assert.Equal(t, 0, meta.totalTokens, "totalTokens")
	assert.Equal(t, 0, meta.toolStats.RunCommand, "RunCommand")
}

func TestSynthesizeWarpToolMessages(t *testing.T) {
	meta := warpConversationMeta{
		toolStats: warpToolStats{
			RunCommand: 2,
			ReadFiles:  1,
		},
	}

	ordinal := 0
	msgs := synthesizeWarpToolMessages(
		meta, parseWarpTimestamp("2026-04-07 10:00:00"),
		"auto", &ordinal,
	)

	require.Len(t, msgs, 3, "msgs len") // 2 + 1
	assert.Equal(t, 3, ordinal, "ordinal after")

	// All should be assistant messages with tool use
	for _, m := range msgs {
		assert.Equal(t, RoleAssistant, m.Role, "Role")
		assert.True(t, m.HasToolUse, "expected HasToolUse=true")
		assert.Len(t, m.ToolCalls, 1)
	}

	// Check categories
	assert.Equal(t, "Bash", msgs[0].ToolCalls[0].Category, "tc[0].Category")
	assert.Equal(t, "Read", msgs[2].ToolCalls[0].Category, "tc[2].Category")
}
