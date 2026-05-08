package parser

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
)

func newPiebaldTestDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "app.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE projects (
			id INTEGER PRIMARY KEY,
			directory TEXT NOT NULL,
			name TEXT NOT NULL
		)`,
		`CREATE TABLE chats (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			is_deleted BOOLEAN NOT NULL DEFAULT 0,
			message_count INTEGER NOT NULL DEFAULT 0,
			current_directory TEXT,
			worktree_path TEXT,
			branch_name TEXT,
			project_id INTEGER
		)`,
		`CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			parent_chat_id INTEGER NOT NULL,
			parent_message_id INTEGER,
			role TEXT NOT NULL,
			model TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			input_tokens BIGINT,
			output_tokens BIGINT,
			reasoning_tokens BIGINT,
			cache_read_tokens BIGINT,
			cache_write_tokens BIGINT,
			status TEXT NOT NULL,
			finish_reason TEXT,
			error TEXT,
			enabled INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE message_parts (
			id INTEGER PRIMARY KEY,
			parent_chat_message_id INTEGER NOT NULL,
			part_index INTEGER NOT NULL,
			part_type TEXT NOT NULL
		)`,
		`CREATE TABLE message_part_text (
			message_part_id INTEGER PRIMARY KEY,
			is_thinking BOOLEAN NOT NULL DEFAULT FALSE
		)`,
		`CREATE TABLE message_content_nodes (
			id INTEGER PRIMARY KEY,
			parent_text_part_id INTEGER NOT NULL,
			node_index INTEGER NOT NULL,
			node_type TEXT NOT NULL
		)`,
		`CREATE TABLE message_node_text (
			node_id INTEGER PRIMARY KEY,
			content TEXT NOT NULL
		)`,
		`CREATE TABLE message_part_tool_call (
			message_part_id INTEGER PRIMARY KEY,
			provider_tool_use_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			tool_input TEXT NOT NULL,
			tool_result TEXT,
			tool_error TEXT,
			tool_state TEXT NOT NULL DEFAULT 'pending',
			sub_agent_chat_id INTEGER
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec schema: %v", err)
		}
	}
	return dbPath
}

func execPiebaldTestSQL(t *testing.T, dbPath, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

func seedPiebaldTextPart(t *testing.T, dbPath string, partID, msgID int64, idx int, text string, thinking bool) {
	t.Helper()
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO message_parts (id, parent_chat_message_id, part_index, part_type)
		 VALUES (?, ?, ?, 'text')`, partID, msgID, idx)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO message_part_text (message_part_id, is_thinking) VALUES (?, ?)`, partID, thinking)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO message_content_nodes (id, parent_text_part_id, node_index, node_type)
		 VALUES (?, ?, 0, 'text')`, partID+1000, partID)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO message_node_text (node_id, content) VALUES (?, ?)`, partID+1000, text)
}

func seedPiebaldToolPart(t *testing.T, dbPath string, partID, msgID int64, idx int) {
	t.Helper()
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO message_parts (id, parent_chat_message_id, part_index, part_type)
		 VALUES (?, ?, ?, 'tool_call')`, partID, msgID, idx)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO message_part_tool_call
		 (message_part_id, provider_tool_use_id, tool_name, tool_input, tool_result, tool_state)
		 VALUES (?, 'toolu_1', 'Read', '{"path":"README.md"}', 'file contents', 'completed')`, partID)
}

func seedPiebaldSubagentToolPart(t *testing.T, dbPath string, partID, msgID int64, idx int, subAgentChatID int64) {
	t.Helper()
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO message_parts (id, parent_chat_message_id, part_index, part_type)
		 VALUES (?, ?, ?, 'tool_call')`, partID, msgID, idx)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO message_part_tool_call
		 (message_part_id, provider_tool_use_id, tool_name, tool_input, tool_result, tool_state, sub_agent_chat_id)
		 VALUES (?, 'toolu_sub', 'LaunchSubagent', '{"prompt":"research"}', 'done', 'completed', ?)`, partID, subAgentChatID)
}

func TestFindPiebaldDBPath(t *testing.T) {
	dir := t.TempDir()
	if got := FindPiebaldDBPath(dir); got != "" {
		t.Fatalf("empty dir path = %q, want empty", got)
	}
	dbPath := filepath.Join(dir, "app.db")
	execPiebaldTestSQL(t, dbPath, `CREATE TABLE x (id INTEGER)`)
	if got := FindPiebaldDBPath(dir); got != dbPath {
		t.Fatalf("FindPiebaldDBPath = %q, want %q", got, dbPath)
	}
}

func TestParsePiebaldSessionBasic(t *testing.T) {
	dbPath := newPiebaldTestDB(t)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO projects (id, directory, name) VALUES (1, '/repo/app', 'app')`)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO chats
		 (id, title, created_at, updated_at, is_deleted, message_count, current_directory, branch_name, project_id)
		 VALUES (42, 'Fix bug', '2026-05-01T10:00:00Z', '2026-05-01T10:05:00Z', 0, 2, '/repo/app', 'main', 1)`)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO messages
		 (id, parent_chat_id, role, model, created_at, updated_at, status)
		 VALUES (100, 42, 'user', '', '2026-05-01T10:00:01Z', '2026-05-01T10:00:01Z', 'completed')`)
	seedPiebaldTextPart(t, dbPath, 200, 100, 0, "Please fix this", false)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO messages
		 (id, parent_chat_id, role, model, created_at, updated_at, input_tokens, output_tokens, cache_read_tokens, status, finish_reason)
		 VALUES (101, 42, 'assistant', 'claude-test', '2026-05-01T10:00:02Z', '2026-05-01T10:00:03Z', 10, 20, 5, 'completed', 'end_turn')`)
	seedPiebaldTextPart(t, dbPath, 201, 101, 0, "I fixed it", false)

	sess, msgs, err := ParsePiebaldSession(dbPath, "42", "machine")
	if err != nil {
		t.Fatalf("ParsePiebaldSession: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session")
	}
	if sess.ID != "piebald:42" || sess.Agent != AgentPiebald || sess.Project != "app" {
		t.Fatalf("bad session meta: %#v", sess)
	}
	if sess.Cwd != "/repo/app" || sess.GitBranch != "main" || sess.FirstMessage != "Please fix this" {
		t.Fatalf("bad session details: %#v", sess)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2", len(msgs))
	}
	if msgs[0].Role != RoleUser || msgs[0].Content != "Please fix this" {
		t.Fatalf("bad first message: %#v", msgs[0])
	}
	if msgs[1].Role != RoleAssistant || msgs[1].Content != "I fixed it" || msgs[1].Model != "claude-test" {
		t.Fatalf("bad assistant message: %#v", msgs[1])
	}
	if !msgs[1].HasContextTokens || msgs[1].ContextTokens != 15 || !msgs[1].HasOutputTokens || msgs[1].OutputTokens != 20 {
		t.Fatalf("bad token usage: %#v", msgs[1])
	}
	if len(msgs[1].TokenUsage) == 0 {
		t.Fatal("TokenUsage empty, want normalized usage JSON")
	}
	if got := gjson.GetBytes(msgs[1].TokenUsage, "input_tokens").Int(); got != 10 {
		t.Fatalf("input_tokens = %d, want 10", got)
	}
	if got := gjson.GetBytes(msgs[1].TokenUsage, "output_tokens").Int(); got != 20 {
		t.Fatalf("output_tokens = %d, want 20", got)
	}
	if got := gjson.GetBytes(msgs[1].TokenUsage, "cache_read_input_tokens").Int(); got != 5 {
		t.Fatalf("cache_read_input_tokens = %d, want 5", got)
	}
}

func TestParsePiebaldSessionToolCall(t *testing.T) {
	dbPath := newPiebaldTestDB(t)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO chats (id, title, created_at, updated_at, is_deleted, message_count)
		 VALUES (7, 'Tools', '2026-05-01T10:00:00Z', '2026-05-01T10:01:00Z', 0, 1)`)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO messages (id, parent_chat_id, role, created_at, updated_at, status)
		 VALUES (70, 7, 'assistant', '2026-05-01T10:00:01Z', '2026-05-01T10:00:01Z', 'completed')`)
	seedPiebaldToolPart(t, dbPath, 700, 70, 0)

	sess, msgs, err := ParsePiebaldSession(dbPath, "7", "machine")
	if err != nil {
		t.Fatalf("ParsePiebaldSession: %v", err)
	}
	if sess == nil || len(msgs) != 1 {
		t.Fatalf("session/messages = %#v %d", sess, len(msgs))
	}
	if len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(msgs[0].ToolCalls))
	}
	call := msgs[0].ToolCalls[0]
	if call.ToolUseID != "toolu_1" || call.ToolName != "Read" || call.Category != "Read" {
		t.Fatalf("bad tool call: %#v", call)
	}
	if len(msgs[0].ToolResults) != 1 || msgs[0].ToolResults[0].ContentLength != len("file contents") {
		t.Fatalf("bad tool result: %#v", msgs[0].ToolResults)
	}
}

func TestParsePiebaldSessionSubagentToolCall(t *testing.T) {
	dbPath := newPiebaldTestDB(t)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO chats (id, title, created_at, updated_at, is_deleted, message_count)
		 VALUES (7, 'Tools', '2026-05-01T10:00:00Z', '2026-05-01T10:01:00Z', 0, 1),
		        (99, 'Subagent', '2026-05-01T10:00:02Z', '2026-05-01T10:00:03Z', 0, 1)`)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO messages (id, parent_chat_id, role, created_at, updated_at, status)
		 VALUES (70, 7, 'assistant', '2026-05-01T10:00:01Z', '2026-05-01T10:00:01Z', 'completed')`)
	seedPiebaldSubagentToolPart(t, dbPath, 700, 70, 0, 99)

	sess, msgs, err := ParsePiebaldSession(dbPath, "7", "machine")
	if err != nil {
		t.Fatalf("ParsePiebaldSession: %v", err)
	}
	if sess == nil || len(msgs) != 1 {
		t.Fatalf("session/messages = %#v %d", sess, len(msgs))
	}
	if len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(msgs[0].ToolCalls))
	}
	call := msgs[0].ToolCalls[0]
	if call.ToolName != "LaunchSubagent" || call.Category != "Task" {
		t.Fatalf("bad subagent tool category: %#v", call)
	}
	if call.SubagentSessionID != "piebald:99" {
		t.Fatalf("SubagentSessionID = %q, want piebald:99", call.SubagentSessionID)
	}
}

func TestParsePiebaldSessionResultsSplitsForks(t *testing.T) {
	dbPath := newPiebaldTestDB(t)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO chats (id, title, created_at, updated_at, is_deleted, message_count)
		 VALUES (42, 'Branches', '2026-05-01T10:00:00Z', '2026-05-01T10:05:00Z', 0, 5)`)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO messages (id, parent_chat_id, parent_message_id, role, created_at, updated_at, status, enabled)
		 VALUES (100, 42, NULL, 'user', '2026-05-01T10:00:01Z', '2026-05-01T10:00:01Z', 'completed', 1),
		        (101, 42, 100, 'assistant', '2026-05-01T10:00:02Z', '2026-05-01T10:00:02Z', 'completed', 1),
		        (102, 42, 101, 'user', '2026-05-01T10:00:03Z', '2026-05-01T10:00:03Z', 'completed', 1),
		        (103, 42, 102, 'assistant', '2026-05-01T10:00:04Z', '2026-05-01T10:00:04Z', 'completed', 1),
		        (200, 42, 101, 'user', '2026-05-01T10:01:00Z', '2026-05-01T10:01:00Z', 'completed', 0),
		        (201, 42, 200, 'assistant', '2026-05-01T10:01:01Z', '2026-05-01T10:01:01Z', 'completed', 1)`)
	seedPiebaldTextPart(t, dbPath, 1000, 100, 0, "main start", false)
	seedPiebaldTextPart(t, dbPath, 1001, 101, 0, "main first answer", false)
	seedPiebaldTextPart(t, dbPath, 1002, 102, 0, "main followup", false)
	seedPiebaldTextPart(t, dbPath, 1003, 103, 0, "main second answer", false)
	seedPiebaldTextPart(t, dbPath, 2000, 200, 0, "fork question", false)
	seedPiebaldTextPart(t, dbPath, 2001, 201, 0, "fork answer", false)

	results, err := ParsePiebaldSessionResults(dbPath, "42", "machine")
	if err != nil {
		t.Fatalf("ParsePiebaldSessionResults: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	main := results[0]
	if main.Session.ID != "piebald:42" || main.Session.ParentSessionID != "" || main.Session.RelationshipType != RelNone {
		t.Fatalf("bad main session: %#v", main.Session)
	}
	if len(main.Messages) != 4 || main.Messages[2].Content != "main followup" {
		t.Fatalf("bad main messages: %#v", main.Messages)
	}
	fork := results[1]
	if fork.Session.ID != "piebald:42-200" {
		t.Fatalf("fork session ID = %q, want piebald:42-200", fork.Session.ID)
	}
	if fork.Session.ParentSessionID != "piebald:42" || fork.Session.RelationshipType != RelFork {
		t.Fatalf("bad fork relationship: %#v", fork.Session)
	}
	if len(fork.Messages) != 2 || fork.Messages[0].Content != "fork question" || fork.Messages[0].Ordinal != 0 {
		t.Fatalf("bad fork messages: %#v", fork.Messages)
	}
}

func TestParsePiebaldSessionResultsHandlesNestedForks(t *testing.T) {
	dbPath := newPiebaldTestDB(t)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO chats (id, title, created_at, updated_at, is_deleted, message_count)
		 VALUES (42, 'Nested', '2026-05-01T10:00:00Z', '2026-05-01T10:10:00Z', 0, 10)`)
	// Tree:
	//   100 (user)
	//   └── 101 (assistant)
	//       ├── 102 (main child of 101)         enabled=1
	//       │   └── 103
	//       └── 200 (fork at 101)               enabled=0
	//           └── 201 (assistant)
	//               ├── 202 (main child of 201) enabled=1
	//               │   └── 203
	//               └── 300 (nested fork at 201) enabled=0
	//                   └── 301
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO messages (id, parent_chat_id, parent_message_id, role, created_at, updated_at, status, enabled)
		 VALUES (100, 42, NULL, 'user',      '2026-05-01T10:00:01Z', '2026-05-01T10:00:01Z', 'completed', 1),
		        (101, 42, 100,  'assistant', '2026-05-01T10:00:02Z', '2026-05-01T10:00:02Z', 'completed', 1),
		        (102, 42, 101,  'user',      '2026-05-01T10:00:03Z', '2026-05-01T10:00:03Z', 'completed', 1),
		        (103, 42, 102,  'assistant', '2026-05-01T10:00:04Z', '2026-05-01T10:00:04Z', 'completed', 1),
		        (200, 42, 101,  'user',      '2026-05-01T10:01:00Z', '2026-05-01T10:01:00Z', 'completed', 0),
		        (201, 42, 200,  'assistant', '2026-05-01T10:01:01Z', '2026-05-01T10:01:01Z', 'completed', 1),
		        (202, 42, 201,  'user',      '2026-05-01T10:01:02Z', '2026-05-01T10:01:02Z', 'completed', 1),
		        (203, 42, 202,  'assistant', '2026-05-01T10:01:03Z', '2026-05-01T10:01:03Z', 'completed', 1),
		        (300, 42, 201,  'user',      '2026-05-01T10:02:00Z', '2026-05-01T10:02:00Z', 'completed', 0),
		        (301, 42, 300,  'assistant', '2026-05-01T10:02:01Z', '2026-05-01T10:02:01Z', 'completed', 1)`)
	seedPiebaldTextPart(t, dbPath, 1100, 100, 0, "main start", false)
	seedPiebaldTextPart(t, dbPath, 1101, 101, 0, "main answer", false)
	seedPiebaldTextPart(t, dbPath, 1102, 102, 0, "main followup", false)
	seedPiebaldTextPart(t, dbPath, 1103, 103, 0, "main final", false)
	seedPiebaldTextPart(t, dbPath, 1200, 200, 0, "outer fork question", false)
	seedPiebaldTextPart(t, dbPath, 1201, 201, 0, "outer fork answer", false)
	seedPiebaldTextPart(t, dbPath, 1202, 202, 0, "outer fork followup", false)
	seedPiebaldTextPart(t, dbPath, 1203, 203, 0, "outer fork final", false)
	seedPiebaldTextPart(t, dbPath, 1300, 300, 0, "nested fork question", false)
	seedPiebaldTextPart(t, dbPath, 1301, 301, 0, "nested fork answer", false)

	results, err := ParsePiebaldSessionResults(dbPath, "42", "machine")
	if err != nil {
		t.Fatalf("ParsePiebaldSessionResults: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results len = %d, want 3 (main + outer fork + nested fork)", len(results))
	}

	byID := make(map[string]ParseResult, len(results))
	for _, r := range results {
		byID[r.Session.ID] = r
	}

	main, ok := byID["piebald:42"]
	if !ok {
		t.Fatal("missing main session piebald:42")
	}
	if main.Session.RelationshipType != RelNone || main.Session.ParentSessionID != "" {
		t.Fatalf("bad main relationship: %#v", main.Session)
	}
	if len(main.Messages) != 4 {
		t.Fatalf("main messages = %d, want 4", len(main.Messages))
	}

	outer, ok := byID["piebald:42-200"]
	if !ok {
		t.Fatal("missing outer fork session piebald:42-200")
	}
	if outer.Session.RelationshipType != RelFork || outer.Session.ParentSessionID != "piebald:42" {
		t.Fatalf("bad outer fork relationship: %#v", outer.Session)
	}
	if len(outer.Messages) != 4 {
		t.Fatalf("outer fork messages = %d, want 4", len(outer.Messages))
	}

	nested, ok := byID["piebald:42-300"]
	if !ok {
		t.Fatal("missing nested fork session piebald:42-300 (lost by append/walk evaluation order bug)")
	}
	if nested.Session.RelationshipType != RelFork || nested.Session.ParentSessionID != "piebald:42-200" {
		t.Fatalf("bad nested fork relationship: %#v", nested.Session)
	}
	if len(nested.Messages) != 2 {
		t.Fatalf("nested fork messages = %d, want 2", len(nested.Messages))
	}
}

func TestListPiebaldSessionMetaSkipsDeletedAndEmpty(t *testing.T) {
	dbPath := newPiebaldTestDB(t)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO chats (id, title, created_at, updated_at, is_deleted, message_count)
		 VALUES (1, 'active', '2026-05-01T10:00:00Z', '2026-05-01T10:01:00Z', 0, 1),
		        (2, 'empty', '2026-05-01T10:00:00Z', '2026-05-01T10:01:00Z', 0, 0),
		        (3, 'deleted', '2026-05-01T10:00:00Z', '2026-05-01T10:01:00Z', 1, 1)`)
	metas, err := ListPiebaldSessionMeta(dbPath)
	if err != nil {
		t.Fatalf("ListPiebaldSessionMeta: %v", err)
	}
	if len(metas) != 1 || metas[0].SessionID != "1" || metas[0].VirtualPath != dbPath+"#1" {
		t.Fatalf("metas = %#v", metas)
	}
}
