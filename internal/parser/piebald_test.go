package parser

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newPiebaldTestDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "app.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open test db")
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
		_, err := db.Exec(stmt)
		require.NoError(t, err, "exec schema")
	}
	return dbPath
}

func execPiebaldTestSQL(t *testing.T, dbPath, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open test db")
	defer db.Close()
	_, err = db.Exec(stmt, args...)
	require.NoError(t, err, "exec %q", stmt)
}

func withPiebaldTestTx(t *testing.T, dbPath string, fn func(*sql.Tx)) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open test db")
	defer db.Close()
	tx, err := db.Begin()
	require.NoError(t, err, "begin test tx")
	defer tx.Rollback()
	fn(tx)
	require.NoError(t, tx.Commit(), "commit test tx")
}

func execPiebaldTestTx(t *testing.T, tx *sql.Tx, stmt string, args ...any) {
	t.Helper()
	_, err := tx.Exec(stmt, args...)
	require.NoError(t, err, "exec %q", stmt)
}

type piebaldTextPartSeed struct {
	partID   int64
	msgID    int64
	idx      int
	text     string
	thinking bool
}

func seedPiebaldTextPart(t *testing.T, dbPath string, partID, msgID int64, idx int, text string, thinking bool) {
	t.Helper()
	seedPiebaldTextParts(t, dbPath, piebaldTextPartSeed{
		partID: partID, msgID: msgID, idx: idx, text: text, thinking: thinking,
	})
}

func seedPiebaldTextParts(t *testing.T, dbPath string, parts ...piebaldTextPartSeed) {
	t.Helper()
	withPiebaldTestTx(t, dbPath, func(tx *sql.Tx) {
		for _, part := range parts {
			execPiebaldTestTx(t, tx,
				`INSERT INTO message_parts (id, parent_chat_message_id, part_index, part_type)
				 VALUES (?, ?, ?, 'text')`, part.partID, part.msgID, part.idx)
			execPiebaldTestTx(t, tx,
				`INSERT INTO message_part_text (message_part_id, is_thinking) VALUES (?, ?)`, part.partID, part.thinking)
			execPiebaldTestTx(t, tx,
				`INSERT INTO message_content_nodes (id, parent_text_part_id, node_index, node_type)
				 VALUES (?, ?, 0, 'text')`, part.partID+1000, part.partID)
			execPiebaldTestTx(t, tx,
				`INSERT INTO message_node_text (node_id, content) VALUES (?, ?)`, part.partID+1000, part.text)
		}
	})
}

func seedPiebaldToolPart(t *testing.T, dbPath string, partID, msgID int64, idx int) {
	t.Helper()
	withPiebaldTestTx(t, dbPath, func(tx *sql.Tx) {
		execPiebaldTestTx(t, tx,
			`INSERT INTO message_parts (id, parent_chat_message_id, part_index, part_type)
			 VALUES (?, ?, ?, 'tool_call')`, partID, msgID, idx)
		execPiebaldTestTx(t, tx,
			`INSERT INTO message_part_tool_call
			 (message_part_id, provider_tool_use_id, tool_name, tool_input, tool_result, tool_state)
			 VALUES (?, 'toolu_1', 'Read', '{"path":"README.md"}', 'file contents', 'completed')`, partID)
	})
}

func seedPiebaldSubagentToolPart(t *testing.T, dbPath string, partID, msgID int64, idx int, subAgentChatID int64) {
	t.Helper()
	withPiebaldTestTx(t, dbPath, func(tx *sql.Tx) {
		execPiebaldTestTx(t, tx,
			`INSERT INTO message_parts (id, parent_chat_message_id, part_index, part_type)
			 VALUES (?, ?, ?, 'tool_call')`, partID, msgID, idx)
		execPiebaldTestTx(t, tx,
			`INSERT INTO message_part_tool_call
			 (message_part_id, provider_tool_use_id, tool_name, tool_input, tool_result, tool_state, sub_agent_chat_id)
			 VALUES (?, 'toolu_sub', 'LaunchSubagent', '{"prompt":"research"}', 'done', 'completed', ?)`, partID, subAgentChatID)
	})
}

func TestPiebaldDBPath(t *testing.T) {
	dir := t.TempDir()
	assert.Empty(t, piebaldDBPath(dir), "empty dir path")
	dbPath := filepath.Join(dir, "app.db")
	execPiebaldTestSQL(t, dbPath, `CREATE TABLE x (id INTEGER)`)
	assert.Equal(t, dbPath, piebaldDBPath(dir))
}

// parsePiebaldOneSession parses a single Piebald chat through the per-session
// entrypoint the provider uses (parsePiebaldSessionResults) and returns the
// primary session, mirroring the behavior tests relied on before the
// ParsePiebaldSession wrapper was folded onto the provider.
func parsePiebaldOneSession(
	t *testing.T, dbPath, chatID, machine string,
) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	results, err := parsePiebaldSessionResults(dbPath, chatID, machine)
	require.NoError(t, err, "parsePiebaldSessionResults")
	require.NotEmpty(t, results, "expected at least one parsed session")
	return &results[0].Session, results[0].Messages
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

	sess, msgs := parsePiebaldOneSession(t, dbPath, "42", "machine")
	require.NotNil(t, sess, "expected session")
	assert.Equal(t, "piebald:42", sess.ID)
	assert.Equal(t, AgentPiebald, sess.Agent)
	assert.Equal(t, "app", sess.Project)
	assert.Equal(t, "/repo/app", sess.Cwd)
	assert.Equal(t, "main", sess.GitBranch)
	assert.Equal(t, "Please fix this", sess.FirstMessage)
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Please fix this", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "I fixed it", msgs[1].Content)
	assert.Equal(t, "claude-test", msgs[1].Model)
	assert.True(t, msgs[1].HasContextTokens)
	assert.Equal(t, 15, msgs[1].ContextTokens)
	assert.True(t, msgs[1].HasOutputTokens)
	assert.Equal(t, 20, msgs[1].OutputTokens)
	require.NotEmpty(t, msgs[1].TokenUsage, "TokenUsage empty")
	assert.Equal(t, int64(10), gjson.GetBytes(msgs[1].TokenUsage, "input_tokens").Int(), "input_tokens")
	assert.Equal(t, int64(20), gjson.GetBytes(msgs[1].TokenUsage, "output_tokens").Int(), "output_tokens")
	assert.Equal(t, int64(5), gjson.GetBytes(msgs[1].TokenUsage, "cache_read_input_tokens").Int(), "cache_read_input_tokens")
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

	sess, msgs := parsePiebaldOneSession(t, dbPath, "7", "machine")
	require.NotNil(t, sess)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].ToolCalls, 1)
	call := msgs[0].ToolCalls[0]
	assert.Equal(t, "toolu_1", call.ToolUseID)
	assert.Equal(t, "Read", call.ToolName)
	assert.Equal(t, "Read", call.Category)
	require.Len(t, msgs[0].ToolResults, 1)
	assert.Equal(t, len("file contents"), msgs[0].ToolResults[0].ContentLength)
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

	sess, msgs := parsePiebaldOneSession(t, dbPath, "7", "machine")
	require.NotNil(t, sess)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].ToolCalls, 1)
	call := msgs[0].ToolCalls[0]
	assert.Equal(t, "LaunchSubagent", call.ToolName)
	assert.Equal(t, "Task", call.Category)
	assert.Equal(t, "piebald:99", call.SubagentSessionID)
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
	seedPiebaldTextParts(t, dbPath,
		piebaldTextPartSeed{1000, 100, 0, "main start", false},
		piebaldTextPartSeed{1001, 101, 0, "main first answer", false},
		piebaldTextPartSeed{1002, 102, 0, "main followup", false},
		piebaldTextPartSeed{1003, 103, 0, "main second answer", false},
		piebaldTextPartSeed{2000, 200, 0, "fork question", false},
		piebaldTextPartSeed{2001, 201, 0, "fork answer", false},
	)

	results, err := parsePiebaldSessionResults(dbPath, "42", "machine")
	require.NoError(t, err, "parsePiebaldSessionResults")
	require.Len(t, results, 2)
	main := results[0]
	assert.Equal(t, "piebald:42", main.Session.ID)
	assert.Empty(t, main.Session.ParentSessionID)
	assert.Equal(t, RelNone, main.Session.RelationshipType)
	require.Len(t, main.Messages, 4)
	assert.Equal(t, "main followup", main.Messages[2].Content)
	fork := results[1]
	assert.Equal(t, "piebald:42-200", fork.Session.ID)
	assert.Equal(t, "piebald:42", fork.Session.ParentSessionID)
	assert.Equal(t, RelFork, fork.Session.RelationshipType)
	require.Len(t, fork.Messages, 2)
	assert.Equal(t, "fork question", fork.Messages[0].Content)
	assert.Equal(t, 0, fork.Messages[0].Ordinal)
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
	seedPiebaldTextParts(t, dbPath,
		piebaldTextPartSeed{1100, 100, 0, "main start", false},
		piebaldTextPartSeed{1101, 101, 0, "main answer", false},
		piebaldTextPartSeed{1102, 102, 0, "main followup", false},
		piebaldTextPartSeed{1103, 103, 0, "main final", false},
		piebaldTextPartSeed{1200, 200, 0, "outer fork question", false},
		piebaldTextPartSeed{1201, 201, 0, "outer fork answer", false},
		piebaldTextPartSeed{1202, 202, 0, "outer fork followup", false},
		piebaldTextPartSeed{1203, 203, 0, "outer fork final", false},
		piebaldTextPartSeed{1300, 300, 0, "nested fork question", false},
		piebaldTextPartSeed{1301, 301, 0, "nested fork answer", false},
	)

	results, err := parsePiebaldSessionResults(dbPath, "42", "machine")
	require.NoError(t, err, "parsePiebaldSessionResults")
	require.Len(t, results, 3, "main + outer fork + nested fork")

	byID := make(map[string]ParseResult, len(results))
	for _, r := range results {
		byID[r.Session.ID] = r
	}

	main, ok := byID["piebald:42"]
	require.True(t, ok, "missing main session piebald:42")
	assert.Equal(t, RelNone, main.Session.RelationshipType)
	assert.Empty(t, main.Session.ParentSessionID)
	assert.Len(t, main.Messages, 4)

	outer, ok := byID["piebald:42-200"]
	require.True(t, ok, "missing outer fork session piebald:42-200")
	assert.Equal(t, RelFork, outer.Session.RelationshipType)
	assert.Equal(t, "piebald:42", outer.Session.ParentSessionID)
	assert.Len(t, outer.Messages, 4)

	nested, ok := byID["piebald:42-300"]
	require.True(t, ok, "missing nested fork session piebald:42-300 (lost by append/walk evaluation order bug)")
	assert.Equal(t, RelFork, nested.Session.RelationshipType)
	assert.Equal(t, "piebald:42-200", nested.Session.ParentSessionID)
	assert.Len(t, nested.Messages, 2)
}

func TestListPiebaldSessionMetaSkipsDeletedAndEmpty(t *testing.T) {
	dbPath := newPiebaldTestDB(t)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO chats (id, title, created_at, updated_at, is_deleted, message_count)
		 VALUES (1, 'active', '2026-05-01T10:00:00Z', '2026-05-01T10:01:00Z', 0, 1),
		        (2, 'empty', '2026-05-01T10:00:00Z', '2026-05-01T10:01:00Z', 0, 0),
		        (3, 'deleted', '2026-05-01T10:00:00Z', '2026-05-01T10:01:00Z', 1, 1)`)
	metas, err := ListPiebaldSessionMeta(dbPath)
	require.NoError(t, err, "ListPiebaldSessionMeta")
	require.Len(t, metas, 1)
	assert.Equal(t, "1", metas[0].SessionID)
	assert.Equal(t, dbPath+"#1", metas[0].VirtualPath)
}
