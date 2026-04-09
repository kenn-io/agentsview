package parser

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
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
	if err != nil {
		s.t.Fatalf("add conversation: %v", err)
	}
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
	if err != nil {
		s.t.Fatalf("add exchange: %v", err)
	}
}

func newWarpTestDB(t *testing.T) (string, *WarpSeeder, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "warp.sqlite")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if _, err := db.Exec(warpSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
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

	sessions, err := ParseWarpDB(dbPath, "testmachine")
	if err != nil {
		t.Fatalf("ParseWarpDB: %v", err)
	}

	assertEq(t, "sessions len", len(sessions), 1)

	s := sessions[0]
	assertEq(t, "ID", s.Session.ID, "warp:conv-001")
	assertEq(t, "Agent", s.Session.Agent, AgentWarp)
	assertEq(t, "Machine", s.Session.Machine, "testmachine")
	assertEq(t, "Project", s.Session.Project, "myproject")
	assertEq(t, "UserMessageCount", s.Session.UserMessageCount, 2)
	assertEq(t, "FirstMessage",
		s.Session.FirstMessage,
		"Fix the JSON parsing bug in parser.go",
	)

	wantPath := dbPath + "#conv-001"
	assertEq(t, "File.Path", s.Session.File.Path, wantPath)

	// Token usage from conversation_data
	assertEq(t, "HasTotalOutputTokens",
		s.Session.HasTotalOutputTokens, true)
	assertEq(t, "TotalOutputTokens",
		s.Session.TotalOutputTokens, 100000)

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
	assertEq(t, "userMsgs", userMsgs, 2)
	// 3 run_command + 2 read_files + 1 grep + 1 apply_file_diff = 7
	assertEq(t, "toolMsgs", toolMsgs, 7)
}

func TestParseWarpSession_SingleConversation(t *testing.T) {
	dbPath, seeder, db := newWarpTestDB(t)
	defer db.Close()
	seedWarpConversation(t, seeder)

	sess, msgs, err := ParseWarpSession(
		dbPath, "conv-001", "testmachine",
	)
	if err != nil {
		t.Fatalf("ParseWarpSession: %v", err)
	}
	if sess == nil {
		t.Fatal("expected non-nil session")
	}

	assertEq(t, "ID", sess.ID, "warp:conv-001")
	assertEq(t, "Agent", sess.Agent, AgentWarp)

	// First user message
	assertEq(t, "msgs[0].Role", msgs[0].Role, RoleUser)
	assertEq(t, "msgs[0].Content", msgs[0].Content,
		"Fix the JSON parsing bug in parser.go")
	// Second user message
	assertEq(t, "msgs[1].Role", msgs[1].Role, RoleUser)
	assertEq(t, "msgs[1].Content", msgs[1].Content,
		"Now add a test for that fix")
}

func TestListWarpSessionMeta(t *testing.T) {
	dbPath, seeder, db := newWarpTestDB(t)
	defer db.Close()
	seedWarpConversation(t, seeder)

	metas, err := ListWarpSessionMeta(dbPath)
	if err != nil {
		t.Fatalf("ListWarpSessionMeta: %v", err)
	}

	assertEq(t, "metas len", len(metas), 1)
	assertEq(t, "SessionID", metas[0].SessionID, "conv-001")
	assertEq(t, "VirtualPath",
		metas[0].VirtualPath, dbPath+"#conv-001")
	if metas[0].FileMtime == 0 {
		t.Error("expected non-zero FileMtime")
	}
}

func TestParseWarpDB_EmptyConversation(t *testing.T) {
	dbPath, seeder, db := newWarpTestDB(t)
	defer db.Close()

	seeder.AddConversation(
		"conv-empty", "{}", "2026-04-07 10:00:00",
	)

	sessions, err := ParseWarpDB(dbPath, "m")
	if err != nil {
		t.Fatalf("ParseWarpDB: %v", err)
	}
	assertEq(t, "sessions len", len(sessions), 0)
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

	sessions, err := ParseWarpDB(dbPath, "m")
	if err != nil {
		t.Fatalf("ParseWarpDB: %v", err)
	}
	assertEq(t, "sessions len", len(sessions), 0)
}

func TestParseWarpDB_NonExistent(t *testing.T) {
	sessions, err := ParseWarpDB(
		"/nonexistent/warp.sqlite", "m",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sessions != nil {
		t.Error("expected nil sessions for non-existent db")
	}
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
			assertEq(t, "text", got, tc.want)
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
			if !ts.IsZero() {
				t.Errorf("expected zero time for %q", tc.input)
			}
		} else if ts.Year() != tc.year {
			t.Errorf("year = %d, want %d for %q",
				ts.Year(), tc.year, tc.input)
		}
	}
}

func TestFindWarpDBPath(t *testing.T) {
	// Create a temp dir with warp.sqlite
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "warp.sqlite")

	// Before creating the file
	assertEq(t, "not found", FindWarpDBPath(dir), "")

	// Create the file (sql.Open is lazy; Ping forces creation)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	db.Close()

	assertEq(t, "found", FindWarpDBPath(dir), dbPath)
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
	assertEq(t, "totalTokens", meta.totalTokens, 1700)
	assertEq(t, "RunCommand", meta.toolStats.RunCommand, 5)
	assertEq(t, "ReadFiles", meta.toolStats.ReadFiles, 3)
	assertEq(t, "Grep", meta.toolStats.Grep, 2)
	assertEq(t, "ApplyFileDiff", meta.toolStats.ApplyFileDiff, 1)
}

func TestParseWarpConversationMeta_Empty(t *testing.T) {
	meta := parseWarpConversationMeta("{}")
	assertEq(t, "totalTokens", meta.totalTokens, 0)
	assertEq(t, "RunCommand", meta.toolStats.RunCommand, 0)
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

	assertEq(t, "msgs len", len(msgs), 3) // 2 + 1
	assertEq(t, "ordinal after", ordinal, 3)

	// All should be assistant messages with tool use
	for _, m := range msgs {
		assertEq(t, "Role", m.Role, RoleAssistant)
		if !m.HasToolUse {
			t.Error("expected HasToolUse=true")
		}
		if len(m.ToolCalls) != 1 {
			t.Errorf("expected 1 tool call, got %d",
				len(m.ToolCalls))
		}
	}

	// Check categories
	assertEq(t, "tc[0].Category",
		msgs[0].ToolCalls[0].Category, "Bash")
	assertEq(t, "tc[2].Category",
		msgs[2].ToolCalls[0].Category, "Read")
}
