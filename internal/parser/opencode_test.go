package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// openCodeSchema matches the real OpenCode database schema.
// Role and part type live inside the JSON data columns.
const openCodeSchema = `
CREATE TABLE project (
	id TEXT PRIMARY KEY,
	worktree TEXT NOT NULL,
	time_created INTEGER NOT NULL DEFAULT 0,
	time_updated INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE session (
	id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL,
	parent_id TEXT,
	title TEXT,
	time_created INTEGER NOT NULL,
	time_updated INTEGER NOT NULL,
	FOREIGN KEY (project_id) REFERENCES project(id)
);

CREATE TABLE message (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	time_created INTEGER NOT NULL,
	time_updated INTEGER NOT NULL,
	data TEXT NOT NULL,
	FOREIGN KEY (session_id) REFERENCES session(id)
);

CREATE TABLE part (
	id TEXT PRIMARY KEY,
	message_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	time_created INTEGER NOT NULL,
	time_updated INTEGER NOT NULL,
	data TEXT NOT NULL,
	FOREIGN KEY (message_id) REFERENCES message(id)
);
`

func assertEq[T comparable](t *testing.T, name string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

type OpenCodeSeeder struct {
	db *sql.DB
	t  *testing.T
}

func (s *OpenCodeSeeder) AddProject(id, worktree string) {
	s.t.Helper()
	_, err := s.db.Exec(`INSERT INTO project (id, worktree) VALUES (?, ?)`, id, worktree)
	if err != nil {
		s.t.Fatalf("add project: %v", err)
	}
}

func (s *OpenCodeSeeder) AddSession(id, projectID, parentID, title string, timeCreated, timeUpdated int64) {
	s.t.Helper()

	var pID, tStr any
	if parentID != "" {
		pID = parentID
	}
	if title != "" {
		tStr = title
	}

	_, err := s.db.Exec(`INSERT INTO session (id, project_id, parent_id, title, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?)`,
		id, projectID, pID, tStr, timeCreated, timeUpdated)
	if err != nil {
		s.t.Fatalf("add session: %v", err)
	}
}

func (s *OpenCodeSeeder) AddMessage(id, sessionID string, timeCreated, timeUpdated int64, data string) {
	s.t.Helper()
	_, err := s.db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		id, sessionID, timeCreated, timeUpdated, data)
	if err != nil {
		s.t.Fatalf("add message: %v", err)
	}
}

func (s *OpenCodeSeeder) AddPart(id, messageID, sessionID string, timeCreated, timeUpdated int64, data string) {
	s.t.Helper()
	_, err := s.db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
		id, messageID, sessionID, timeCreated, timeUpdated, data)
	if err != nil {
		s.t.Fatalf("add part: %v", err)
	}
}

func newTestDB(t *testing.T) (string, *OpenCodeSeeder, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}

	if _, err := db.Exec(openCodeSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	seeder := &OpenCodeSeeder{db: db, t: t}
	return dbPath, seeder, db
}

func seedStandardSession(t *testing.T, seeder *OpenCodeSeeder) {
	t.Helper()
	seeder.AddProject("prj_1", "/home/user/code/myapp")
	seeder.AddSession("ses_abc", "prj_1", "", "Test Session", 1700000000000, 1700000060000)

	seeder.AddMessage("msg_1", "ses_abc", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_1", "msg_1", "ses_abc", 1700000000000, 1700000000000, `{"type":"text","text":"Hello, help me with Go"}`)

	seeder.AddMessage("msg_2", "ses_abc", 1700000010000, 1700000010000, `{"role":"assistant"}`)
	seeder.AddPart("prt_2", "msg_2", "ses_abc", 1700000010000, 1700000010000, `{"type":"text","text":"Sure, I can help with Go."}`)
}

func TestParseOpenCodeDB_StandardSession(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()
	seedStandardSession(t, seeder)

	sessions, err := ParseOpenCodeDB(dbPath, "testmachine")
	if err != nil {
		t.Fatalf("ParseOpenCodeDB: %v", err)
	}

	assertEq(t, "sessions len", len(sessions), 1)

	s := sessions[0]
	assertEq(t, "ID", s.Session.ID, "opencode:ses_abc")
	assertEq(t, "Agent", s.Session.Agent, AgentOpenCode)
	assertEq(t, "Machine", s.Session.Machine, "testmachine")
	assertEq(t, "Project", s.Session.Project, "myapp")
	assertEq(t, "MessageCount", s.Session.MessageCount, 2)
	assertEq(t, "FirstMessage", s.Session.FirstMessage, "Test Session")

	wantPath := dbPath + "#ses_abc"
	assertEq(t, "File.Path", s.Session.File.Path, wantPath)

	wantMtime := int64(1700000060000) * 1_000_000
	assertEq(t, "File.Mtime", s.Session.File.Mtime, wantMtime)

	assertEq(t, "Messages len", len(s.Messages), 2)
	assertEq(t, "msg[0].Role", s.Messages[0].Role, RoleUser)
	assertEq(t, "msg[1].Role", s.Messages[1].Role, RoleAssistant)
	assertEq(t, "msg[1].Content", s.Messages[1].Content, "Sure, I can help with Go.")
}

func TestParseOpenCodeDB_TitleFallback(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/tmp/proj")

	// Empty title: should use first user message.
	seeder.AddSession("ses_empty", "prj_1", "", "",
		1700000000000, 1700000010000)
	seeder.AddMessage("msg_1", "ses_empty",
		1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_1", "msg_1", "ses_empty",
		1700000000000, 1700000000000,
		`{"type":"text","text":"Help me debug this crash"}`)

	// Placeholder title: should also use first user message.
	seeder.AddSession("ses_default", "prj_1", "",
		"New session - 2026-03-22T10:00:00.000Z",
		1700000020000, 1700000030000)
	seeder.AddMessage("msg_2", "ses_default",
		1700000020000, 1700000020000, `{"role":"user"}`)
	seeder.AddPart("prt_2", "msg_2", "ses_default",
		1700000020000, 1700000020000,
		`{"type":"text","text":"Refactor the auth module"}`)

	sessions, err := ParseOpenCodeDB(dbPath, "m")
	if err != nil {
		t.Fatalf("ParseOpenCodeDB: %v", err)
	}
	assertEq(t, "sessions len", len(sessions), 2)

	for _, s := range sessions {
		switch s.Session.ID {
		case "opencode:ses_empty":
			assertEq(t, "empty title fallback",
				s.Session.FirstMessage,
				"Help me debug this crash")
		case "opencode:ses_default":
			assertEq(t, "placeholder title fallback",
				s.Session.FirstMessage,
				"Refactor the auth module")
		}
	}
}

func TestParseOpenCodeDB_ToolParts(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/tmp/proj")
	seeder.AddSession("ses_tools", "prj_1", "", "", 1700000000000, 1700000030000)

	seeder.AddMessage("msg_u", "ses_tools", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_u", "msg_u", "ses_tools", 1700000000000, 1700000000000, `{"type":"text","text":"read my file"}`)

	seeder.AddMessage("msg_a", "ses_tools", 1700000010000, 1700000012000, `{"role":"assistant"}`)
	seeder.AddPart("prt_r", "msg_a", "ses_tools", 1700000010000, 1700000010000, `{"type":"reasoning","text":"Let me think about this..."}`)
	seeder.AddPart("prt_t", "msg_a", "ses_tools", 1700000011000, 1700000011000, `{"type":"tool","tool":"read","callID":"call_1","state":{"input":{"file_path":"main.go"}}}`)
	seeder.AddPart("prt_txt", "msg_a", "ses_tools", 1700000012000, 1700000012000, `{"type":"text","text":"Here is the file content."}`)

	sessions, err := ParseOpenCodeDB(dbPath, "m")
	if err != nil {
		t.Fatalf("ParseOpenCodeDB: %v", err)
	}

	assertEq(t, "sessions len", len(sessions), 1)

	msgs := sessions[0].Messages
	assertEq(t, "messages len", len(msgs), 2)

	ast := msgs[1]
	assertEq(t, "HasThinking", ast.HasThinking, true)
	assertEq(t, "HasToolUse", ast.HasToolUse, true)

	assertToolCalls(t, ast.ToolCalls, []ParsedToolCall{{
		ToolName:  "read",
		Category:  "Read",
		ToolUseID: "call_1",
		InputJSON: `{"file_path":"main.go"}`,
	}})
}

func TestParseOpenCodeDB_EmptySession(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/tmp/proj")
	seeder.AddSession("ses_empty", "prj_1", "", "", 1700000000000, 1700000000000)

	sessions, err := ParseOpenCodeDB(dbPath, "m")
	if err != nil {
		t.Fatalf("ParseOpenCodeDB: %v", err)
	}

	assertEq(t, "sessions len", len(sessions), 0)
}

func TestParseOpenCodeDB_NonexistentDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nonexistent.db")

	sessions, err := ParseOpenCodeDB(dbPath, "m")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if sessions != nil {
		t.Errorf("expected nil sessions, got %d", len(sessions))
	}
}

func TestParseOpenCodeDB_ProjectFromWorktree(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	// Create a temp dir that looks like a git repo so
	// ExtractProjectFromCwd resolves it.
	repoDir := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	seeder.AddProject("prj_git", repoDir)
	seeder.AddSession("ses_git", "prj_git", "", "", 1700000000000, 1700000010000)
	seeder.AddMessage("msg_1", "ses_git", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_1", "msg_1", "ses_git", 1700000000000, 1700000000000, `{"type":"text","text":"hello"}`)

	sessions, err := ParseOpenCodeDB(dbPath, "m")
	if err != nil {
		t.Fatalf("ParseOpenCodeDB: %v", err)
	}
	assertEq(t, "sessions len", len(sessions), 1)

	assertEq(t, "Project", sessions[0].Session.Project, "my_project")
}

func TestParseOpenCodeSession_SingleSession(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()
	seedStandardSession(t, seeder)

	sess, msgs, err := ParseOpenCodeSession(dbPath, "ses_abc", "testmachine")
	if err != nil {
		t.Fatalf("ParseOpenCodeSession: %v", err)
	}
	if sess == nil {
		t.Fatal("expected non-nil session")
		return
	}

	assertEq(t, "ID", sess.ID, "opencode:ses_abc")
	assertEq(t, "messages len", len(msgs), 2)
}

func TestParseOpenCodeDB_OrdinalContinuity(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/tmp/proj")
	seeder.AddSession("ses_ord", "prj_1", "", "", 1700000000000, 1700000050000)

	// msg 0: user (kept, ordinal 0)
	seeder.AddMessage("msg_1", "ses_ord", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_1", "msg_1", "ses_ord", 1700000000000, 1700000000000, `{"type":"text","text":"first"}`)

	// msg 1: system (skipped role)
	seeder.AddMessage("msg_2", "ses_ord", 1700000010000, 1700000010000, `{"role":"system"}`)
	seeder.AddPart("prt_2", "msg_2", "ses_ord", 1700000010000, 1700000010000, `{"type":"text","text":"system msg"}`)

	// msg 2: user with empty content (skipped)
	seeder.AddMessage("msg_3", "ses_ord", 1700000020000, 1700000020000, `{"role":"user"}`)
	seeder.AddPart("prt_3", "msg_3", "ses_ord", 1700000020000, 1700000020000, `{"type":"text","text":""}`)

	// msg 3: assistant (kept, ordinal 1)
	seeder.AddMessage("msg_4", "ses_ord", 1700000030000, 1700000030000, `{"role":"assistant"}`)
	seeder.AddPart("prt_4", "msg_4", "ses_ord", 1700000030000, 1700000030000, `{"type":"text","text":"response"}`)

	// msg 4: user (kept, ordinal 2)
	seeder.AddMessage("msg_5", "ses_ord", 1700000040000, 1700000040000, `{"role":"user"}`)
	seeder.AddPart("prt_5", "msg_5", "ses_ord", 1700000040000, 1700000040000, `{"type":"text","text":"follow up"}`)

	sessions, err := ParseOpenCodeDB(dbPath, "m")
	if err != nil {
		t.Fatalf("ParseOpenCodeDB: %v", err)
	}
	assertEq(t, "sessions len", len(sessions), 1)

	msgs := sessions[0].Messages
	assertEq(t, "messages len", len(msgs), 3)

	for i, m := range msgs {
		assertEq(t, "Ordinal", m.Ordinal, i)
	}

	assertEq(t, "msgs[0].Content", msgs[0].Content, "first")
	assertEq(t, "msgs[1].Content", msgs[1].Content, "response")
	assertEq(t, "msgs[2].Content", msgs[2].Content, "follow up")
}

func TestParseOpenCodeDB_ParentSession(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/tmp/proj")
	seeder.AddSession("ses_parent", "prj_1", "", "", 1700000000000, 1700000010000)
	seeder.AddSession("ses_child", "prj_1", "ses_parent", "", 1700000020000, 1700000030000)

	// Add messages to both so they aren't skipped
	seeder.AddMessage("msg_p", "ses_parent", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_p", "msg_p", "ses_parent", 1700000000000, 1700000000000, `{"type":"text","text":"parent msg"}`)

	seeder.AddMessage("msg_c", "ses_child", 1700000020000, 1700000020000, `{"role":"user"}`)
	seeder.AddPart("prt_c", "msg_c", "ses_child", 1700000020000, 1700000020000, `{"type":"text","text":"child msg"}`)

	sessions, err := ParseOpenCodeDB(dbPath, "m")
	if err != nil {
		t.Fatalf("ParseOpenCodeDB: %v", err)
	}

	var child *OpenCodeSession
	for i := range sessions {
		if sessions[i].Session.ID == "opencode:ses_child" {
			child = &sessions[i]
		}
	}
	if child == nil {
		t.Fatal("child session not found")
		return
	}
	assertEq(t, "ParentSessionID", child.Session.ParentSessionID, "opencode:ses_parent")
}

func TestListOpenCodeSessionMeta(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()
	seedStandardSession(t, seeder)

	metas, err := ListOpenCodeSessionMeta(dbPath)
	if err != nil {
		t.Fatalf("ListOpenCodeSessionMeta: %v", err)
	}
	assertEq(t, "metas len", len(metas), 1)

	m := metas[0]
	assertEq(t, "SessionID", m.SessionID, "ses_abc")

	wantPath := dbPath + "#ses_abc"
	assertEq(t, "VirtualPath", m.VirtualPath, wantPath)

	wantMtime := int64(1700000060000) * 1_000_000
	assertEq(t, "FileMtime", m.FileMtime, wantMtime)
}

func TestListOpenCodeSessionMeta_NonexistentDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nope.db")
	metas, err := ListOpenCodeSessionMeta(dbPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertEq(t, "metas len", len(metas), 0)
}
