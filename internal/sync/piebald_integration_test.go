package sync_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/wesm/agentsview/internal/sync"
)

type piebaldTestDB struct {
	path string
	db   *sql.DB
}

func createPiebaldDB(t *testing.T, dir string) *piebaldTestDB {
	t.Helper()
	path := filepath.Join(dir, "app.db")
	d, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("opening piebald test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	schema := `
		CREATE TABLE projects (
			id INTEGER PRIMARY KEY,
			directory TEXT NOT NULL,
			name TEXT NOT NULL
		);
		CREATE TABLE chats (
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
		);
		CREATE TABLE messages (
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
		);
		CREATE TABLE message_parts (
			id INTEGER PRIMARY KEY,
			parent_chat_message_id INTEGER NOT NULL,
			part_index INTEGER NOT NULL,
			part_type TEXT NOT NULL
		);
		CREATE TABLE message_part_text (
			message_part_id INTEGER PRIMARY KEY,
			is_thinking BOOLEAN NOT NULL DEFAULT FALSE
		);
		CREATE TABLE message_content_nodes (
			id INTEGER PRIMARY KEY,
			parent_text_part_id INTEGER NOT NULL,
			node_index INTEGER NOT NULL,
			node_type TEXT NOT NULL
		);
		CREATE TABLE message_node_text (
			node_id INTEGER PRIMARY KEY,
			content TEXT NOT NULL
		);
		CREATE TABLE message_part_tool_call (
			message_part_id INTEGER PRIMARY KEY,
			provider_tool_use_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			tool_input TEXT NOT NULL,
			tool_result TEXT,
			tool_error TEXT,
			tool_state TEXT NOT NULL DEFAULT 'pending',
			sub_agent_chat_id INTEGER
		);
	`
	if _, err := d.Exec(schema); err != nil {
		t.Fatalf("creating piebald schema: %v", err)
	}
	return &piebaldTestDB{path: path, db: d}
}

func (p *piebaldTestDB) mustExec(t *testing.T, msg, query string, args ...any) {
	t.Helper()
	if _, err := p.db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
}

func (p *piebaldTestDB) addChat(t *testing.T, id int64, title, prompt, answer, updatedAt string) {
	t.Helper()
	p.mustExec(t, "insert project",
		`INSERT OR IGNORE INTO projects (id, directory, name) VALUES (1, '/repo/app', 'app')`,
	)
	p.mustExec(t, "insert chat",
		`INSERT INTO chats
			(id, title, created_at, updated_at, is_deleted, message_count, current_directory, branch_name, project_id)
		 VALUES (?, ?, '2026-05-01T10:00:00Z', ?, 0, 2, '/repo/app', 'main', 1)`,
		id, title, updatedAt,
	)
	userID := id*100 + 1
	assistantID := id*100 + 2
	p.mustExec(t, "insert user message",
		`INSERT INTO messages (id, parent_chat_id, role, model, created_at, updated_at, status)
		 VALUES (?, ?, 'user', '', '2026-05-01T10:00:01Z', '2026-05-01T10:00:01Z', 'completed')`,
		userID, id,
	)
	p.addTextPart(t, userID*10, userID, 0, prompt, false)
	p.mustExec(t, "insert assistant message",
		`INSERT INTO messages
			(id, parent_chat_id, role, model, created_at, updated_at,
			 input_tokens, output_tokens, cache_read_tokens, status, finish_reason)
		 VALUES (?, ?, 'assistant', 'claude-test', '2026-05-01T10:00:02Z', '2026-05-01T10:00:03Z',
			 10, 20, 5, 'completed', 'end_turn')`,
		assistantID, id,
	)
	p.addTextPart(t, assistantID*10, assistantID, 0, answer, false)
	p.addToolPart(t, assistantID*10+1, assistantID, 1)
}

func (p *piebaldTestDB) addTextPart(t *testing.T, partID, msgID int64, idx int, text string, thinking bool) {
	t.Helper()
	p.mustExec(t, "insert text part",
		`INSERT INTO message_parts (id, parent_chat_message_id, part_index, part_type)
		 VALUES (?, ?, ?, 'text')`, partID, msgID, idx)
	p.mustExec(t, "insert text part metadata",
		`INSERT INTO message_part_text (message_part_id, is_thinking) VALUES (?, ?)`, partID, thinking)
	p.mustExec(t, "insert text node",
		`INSERT INTO message_content_nodes (id, parent_text_part_id, node_index, node_type)
		 VALUES (?, ?, 0, 'text')`, partID+100000, partID)
	p.mustExec(t, "insert text node content",
		`INSERT INTO message_node_text (node_id, content) VALUES (?, ?)`, partID+100000, text)
}

func (p *piebaldTestDB) addToolPart(t *testing.T, partID, msgID int64, idx int) {
	t.Helper()
	p.mustExec(t, "insert tool part",
		`INSERT INTO message_parts (id, parent_chat_message_id, part_index, part_type)
		 VALUES (?, ?, ?, 'tool_call')`, partID, msgID, idx)
	p.mustExec(t, "insert tool call",
		`INSERT INTO message_part_tool_call
			(message_part_id, provider_tool_use_id, tool_name, tool_input, tool_result, tool_state)
		 VALUES (?, 'toolu_1', 'ReadFile', '{"path":"README.md"}', 'file contents', 'completed')`,
		partID,
	)
}

func (p *piebaldTestDB) addChatWithFork(t *testing.T, chatID int64) {
	t.Helper()
	p.mustExec(t, "insert project",
		`INSERT OR IGNORE INTO projects (id, directory, name) VALUES (1, '/repo/app', 'app')`)
	p.mustExec(t, "insert chat",
		`INSERT INTO chats
			(id, title, created_at, updated_at, is_deleted, message_count, current_directory, branch_name, project_id)
		 VALUES (?, 'Forked chat', '2026-05-01T10:00:00Z', '2026-05-01T10:05:00Z', 0, 6, '/repo/app', 'main', 1)`,
		chatID)
	p.mustExec(t, "insert messages",
		`INSERT INTO messages (id, parent_chat_id, parent_message_id, role, model, created_at, updated_at, status, enabled)
		 VALUES (100, ?, NULL, 'user',      '',             '2026-05-01T10:00:01Z', '2026-05-01T10:00:01Z', 'completed', 1),
		        (101, ?, 100,  'assistant', 'claude-test',  '2026-05-01T10:00:02Z', '2026-05-01T10:00:02Z', 'completed', 1),
		        (102, ?, 101,  'user',      '',             '2026-05-01T10:00:03Z', '2026-05-01T10:00:03Z', 'completed', 1),
		        (103, ?, 102,  'assistant', 'claude-test',  '2026-05-01T10:00:04Z', '2026-05-01T10:00:04Z', 'completed', 1),
		        (200, ?, 101,  'user',      '',             '2026-05-01T10:01:00Z', '2026-05-01T10:01:00Z', 'completed', 0),
		        (201, ?, 200,  'assistant', 'claude-test',  '2026-05-01T10:01:01Z', '2026-05-01T10:01:01Z', 'completed', 1)`,
		chatID, chatID, chatID, chatID, chatID, chatID)
	p.addTextPart(t, 1100, 100, 0, "Main start", false)
	p.addTextPart(t, 1101, 101, 0, "Main answer", false)
	p.addTextPart(t, 1102, 102, 0, "Main followup", false)
	p.addTextPart(t, 1103, 103, 0, "Main final", false)
	p.addTextPart(t, 1200, 200, 0, "Fork question", false)
	p.addTextPart(t, 1201, 201, 0, "Fork answer", false)
}

func TestSyncSingleSessionPiebaldFork(t *testing.T) {
	env := setupTestEnv(t)
	piebald := createPiebaldDB(t, env.piebaldDir)
	piebald.addChatWithFork(t, 42)

	if err := env.engine.SyncSingleSession("piebald:42-200"); err != nil {
		t.Fatalf("SyncSingleSession(fork): %v", err)
	}
	assertSessionMessageCount(t, env.db, "piebald:42-200", 2)
	assertSessionMessageCount(t, env.db, "piebald:42", 4)

	src := env.engine.FindSourceFile("piebald:42-200")
	wantSrc := filepath.Join(env.piebaldDir, "app.db")
	if src != wantSrc {
		t.Fatalf("FindSourceFile(fork) = %q, want %q", src, wantSrc)
	}

	if mtime := env.engine.SourceMtime("piebald:42-200"); mtime == 0 {
		t.Fatal("SourceMtime(fork) returned zero")
	}
}

func TestSyncEnginePiebaldBulkSync(t *testing.T) {
	env := setupTestEnv(t)
	piebald := createPiebaldDB(t, env.piebaldDir)
	piebald.addChat(t, 42, "Piebald Bulk Sync", "Please add Piebald support.", "Added Piebald support.", "2026-05-01T10:05:00Z")

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})
	assertSessionProject(t, env.db, "piebald:42", "app")
	assertSessionMessageCount(t, env.db, "piebald:42", 2)
	assertMessageRoles(t, env.db, "piebald:42", "user", "assistant")
	assertToolCallCount(t, env.db, "piebald:42", 1)
	assertMessageContent(t, env.db, "piebald:42", "Please add Piebald support.", "Added Piebald support.")

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 0, Synced: 0, Skipped: 0})
}

func TestSyncSingleSessionPiebald(t *testing.T) {
	env := setupTestEnv(t)
	piebald := createPiebaldDB(t, env.piebaldDir)
	piebald.addChat(t, 7, "Single Piebald", "One chat.", "One answer.", "2026-05-01T10:05:00Z")

	if err := env.engine.SyncSingleSession("piebald:7"); err != nil {
		t.Fatalf("SyncSingleSession: %v", err)
	}
	assertSessionProject(t, env.db, "piebald:7", "app")
	assertSessionMessageCount(t, env.db, "piebald:7", 2)

	src := env.engine.FindSourceFile("piebald:7")
	wantSrc := filepath.Join(env.piebaldDir, "app.db")
	if src != wantSrc {
		t.Fatalf("FindSourceFile() = %q, want %q", src, wantSrc)
	}

	mtime := env.engine.SourceMtime("piebald:7")
	if mtime == 0 {
		t.Fatal("SourceMtime returned zero")
	}

	_, storedMtime, ok := env.db.GetSessionFileInfo("piebald:7")
	if !ok {
		t.Fatal("session file info not found")
	}
	if storedMtime != mtime {
		t.Fatalf("stored mtime = %d, want %d", storedMtime, mtime)
	}
}

func TestSyncPiebaldMultiChatIncremental(t *testing.T) {
	env := setupTestEnv(t)
	piebald := createPiebaldDB(t, env.piebaldDir)
	piebald.addChat(t, 1, "Chat A", "Prompt A.", "Answer A.", "2026-05-01T10:01:00Z")
	piebald.addChat(t, 2, "Chat B", "Prompt B.", "Answer B.", "2026-05-01T10:02:00Z")

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2, Synced: 2, Skipped: 0})
	_, storedMtimeA, okA := env.db.GetSessionFileInfo("piebald:1")
	if !okA {
		t.Fatal("session A file info not found after initial sync")
	}

	piebald.mustExec(t, "update B updated_at",
		`UPDATE chats SET updated_at = '2026-05-01T10:03:00Z' WHERE id = 2`,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})
	_, storedMtimeA2, okA2 := env.db.GetSessionFileInfo("piebald:1")
	if !okA2 {
		t.Fatal("session A file info not found after partial sync")
	}
	if storedMtimeA != storedMtimeA2 {
		t.Errorf("A's stored mtime changed: was %d, now %d", storedMtimeA, storedMtimeA2)
	}
}
