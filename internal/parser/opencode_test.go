package parser

import (
	"database/sql"
	"encoding/json"
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

func writeOpenCodeStorageFile(
	t *testing.T, path string, data any,
) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
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

func TestParseOpenCodeFile_StorageSession(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_storage.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_storage",
		"directory": "/home/user/code/myapp",
		"title":     "Storage Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_storage", "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": "ses_storage",
		"role":      "user",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_storage", "msg_2.json",
	), map[string]any{
		"id":        "msg_2",
		"sessionID": "ses_storage",
		"role":      "assistant",
		"modelID":   "gpt-5.2-codex",
		"tokens": map[string]any{
			"input":  11,
			"output": 7,
			"cache": map[string]any{
				"read":  3,
				"write": 2,
			},
		},
		"time": map[string]any{
			"created": 1700000010000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "prt_1.json",
	), map[string]any{
		"id":        "prt_1",
		"sessionID": "ses_storage",
		"messageID": "msg_1",
		"type":      "text",
		"text":      "Hello from storage",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_2", "prt_2.json",
	), map[string]any{
		"id":        "prt_2",
		"sessionID": "ses_storage",
		"messageID": "msg_2",
		"type":      "tool",
		"tool":      "read",
		"callID":    "call_storage",
		"state": map[string]any{
			"input": map[string]any{
				"file_path": "main.go",
			},
		},
		"time": map[string]any{
			"created": 1700000010000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_2", "prt_3.json",
	), map[string]any{
		"id":        "prt_3",
		"sessionID": "ses_storage",
		"messageID": "msg_2",
		"type":      "text",
		"text":      "Here is the file.",
		"time": map[string]any{
			"created": 1700000011000,
		},
	})

	sess, msgs, err := ParseOpenCodeFile(
		sessionPath, "testmachine",
	)
	if err != nil {
		t.Fatalf("ParseOpenCodeFile: %v", err)
	}
	if sess == nil {
		t.Fatal("expected non-nil session")
	}

	assertEq(t, "ID", sess.ID, "opencode:ses_storage")
	assertEq(t, "Agent", sess.Agent, AgentOpenCode)
	assertEq(t, "Project", sess.Project, "myapp")
	assertEq(t, "Machine", sess.Machine, "testmachine")
	assertEq(t, "MessageCount", sess.MessageCount, 2)
	assertEq(t, "FirstMessage", sess.FirstMessage, "Storage Session")
	assertEq(t, "File.Path", sess.File.Path, sessionPath)
	assertEq(t, "File.Mtime", sess.File.Mtime > 0, true)

	assertEq(t, "messages len", len(msgs), 2)
	assertEq(t, "msg[0].Role", msgs[0].Role, RoleUser)
	assertEq(t, "msg[0].Content", msgs[0].Content, "Hello from storage")
	assertEq(t, "msg[1].Role", msgs[1].Role, RoleAssistant)
	assertEq(t, "msg[1].Model", msgs[1].Model, "gpt-5.2-codex")
	assertEq(t, "msg[1].HasToolUse", msgs[1].HasToolUse, true)
	assertEq(t, "msg[1].Content", msgs[1].Content, "Here is the file.")
	assertEq(t, "msg[1].HasOutputTokens", msgs[1].HasOutputTokens, true)
	assertEq(t, "msg[1].OutputTokens", msgs[1].OutputTokens, 7)

	assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{
		ToolName:  "read",
		Category:  "Read",
		ToolUseID: "call_storage",
		InputJSON: `{"file_path":"main.go"}`,
	}})
}

func TestParseOpenCodeFile_StorageSessionInvalidChildFails(
	t *testing.T,
) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_storage.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_storage",
		"directory": "/home/user/code/myapp",
		"title":     "Storage Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_storage", "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": "ses_storage",
		"role":      "user",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	if err := os.MkdirAll(filepath.Join(
		root, "storage", "message", "ses_storage",
	), 0o755); err != nil {
		t.Fatalf("mkdir invalid message dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(
		root, "storage", "message", "ses_storage", "msg_bad.json",
	), []byte(`{"id":"msg_bad"`), 0o644); err != nil {
		t.Fatalf("write invalid message: %v", err)
	}
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "prt_1.json",
	), map[string]any{
		"id":        "prt_1",
		"sessionID": "ses_storage",
		"messageID": "msg_1",
		"type":      "text",
		"text":      "Hello from storage",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	if err := os.MkdirAll(filepath.Join(
		root, "storage", "part", "msg_1",
	), 0o755); err != nil {
		t.Fatalf("mkdir invalid part dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(
		root, "storage", "part", "msg_1", "prt_bad.json",
	), []byte(`{"id":"prt_bad"`), 0o644); err != nil {
		t.Fatalf("write invalid part: %v", err)
	}

	sess, msgs, err := ParseOpenCodeFile(
		sessionPath, "testmachine",
	)
	if err == nil {
		t.Fatal("expected ParseOpenCodeFile error")
	}
	if sess != nil {
		t.Fatalf("session = %#v, want nil", sess)
	}
	if msgs != nil {
		t.Fatalf("msgs = %#v, want nil", msgs)
	}
}

func TestParseOpenCodeFile_MissingPartDirAllowed(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_storage.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_storage",
		"directory": "/home/user/code/myapp",
		"title":     "Storage Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_storage", "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": "ses_storage",
		"role":      "assistant",
		"modelID":   "gpt-5.2-codex",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})

	sess, msgs, err := ParseOpenCodeFile(
		sessionPath, "testmachine",
	)
	if err != nil {
		t.Fatalf("ParseOpenCodeFile: %v", err)
	}
	if sess != nil {
		t.Fatalf("session = %#v, want nil", sess)
	}
	if msgs != nil {
		t.Fatalf("msgs = %#v, want nil", msgs)
	}
}

func TestParseOpenCodeFile_StorageMessageMissingIDFails(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_storage.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_storage",
		"directory": "/home/user/code/myapp",
		"title":     "Storage Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_storage", "msg_1.json",
	), map[string]any{
		"sessionID": "ses_storage",
		"role":      "assistant",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})

	sess, msgs, err := ParseOpenCodeFile(
		sessionPath, "testmachine",
	)
	if err == nil {
		t.Fatal("expected ParseOpenCodeFile error")
	}
	if sess != nil {
		t.Fatalf("session = %#v, want nil", sess)
	}
	if msgs != nil {
		t.Fatalf("msgs = %#v, want nil", msgs)
	}
}

func TestParseOpenCodeFile_StoragePartMissingIDFails(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_storage.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_storage",
		"directory": "/home/user/code/myapp",
		"title":     "Storage Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_storage", "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": "ses_storage",
		"role":      "assistant",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "part_1.json",
	), map[string]any{
		"messageID": "msg_1",
		"type":      "text",
		"text":      "hello",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})

	sess, msgs, err := ParseOpenCodeFile(
		sessionPath, "testmachine",
	)
	if err == nil {
		t.Fatal("expected ParseOpenCodeFile error")
	}
	if sess != nil {
		t.Fatalf("session = %#v, want nil", sess)
	}
	if msgs != nil {
		t.Fatalf("msgs = %#v, want nil", msgs)
	}
}

func TestParseOpenCodeFile_StoragePartOrderingUsesStartTime(
	t *testing.T,
) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_storage.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_storage",
		"directory": "/home/user/code/myapp",
		"title":     "Storage Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_storage", "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": "ses_storage",
		"role":      "assistant",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "part_1.json",
	), map[string]any{
		"id":        "part_1",
		"sessionID": "ses_storage",
		"messageID": "msg_1",
		"type":      "text",
		"text":      "second",
		"time": map[string]any{
			"start": 1700000002000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "part_2.json",
	), map[string]any{
		"id":        "part_2",
		"sessionID": "ses_storage",
		"messageID": "msg_1",
		"type":      "text",
		"text":      "first",
		"time": map[string]any{
			"start": 1700000001000,
		},
	})

	_, msgs, err := ParseOpenCodeFile(sessionPath, "testmachine")
	if err != nil {
		t.Fatalf("ParseOpenCodeFile: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
	assertEq(t, "msg[0].Content", msgs[0].Content, "first\nsecond")
}

func TestParseOpenCodeFile_StoragePartOrderingPrefersStartOverCreated(
	t *testing.T,
) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_storage.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_storage",
		"directory": "/home/user/code/myapp",
		"title":     "Storage Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_storage", "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": "ses_storage",
		"role":      "assistant",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "part_1.json",
	), map[string]any{
		"id":        "part_1",
		"sessionID": "ses_storage",
		"messageID": "msg_1",
		"type":      "text",
		"text":      "second",
		"time": map[string]any{
			"start":   1700000002000,
			"created": 1700000001000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "part_2.json",
	), map[string]any{
		"id":        "part_2",
		"sessionID": "ses_storage",
		"messageID": "msg_1",
		"type":      "text",
		"text":      "first",
		"time": map[string]any{
			"start":   1700000001000,
			"created": 1700000002000,
		},
	})

	_, msgs, err := ParseOpenCodeFile(sessionPath, "testmachine")
	if err != nil {
		t.Fatalf("ParseOpenCodeFile: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
	assertEq(t, "msg[0].Content", msgs[0].Content, "first\nsecond")
}

func TestParseOpenCodeFile_StorageStepFinishTokens(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_storage.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_storage",
		"directory": "/home/user/code/myapp",
		"title":     "Storage Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_storage", "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": "ses_storage",
		"role":      "assistant",
		"modelID":   "gpt-5.2-codex",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "prt_1.json",
	), map[string]any{
		"id":        "prt_1",
		"sessionID": "ses_storage",
		"messageID": "msg_1",
		"type":      "text",
		"text":      "reply from storage",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "prt_2.json",
	), map[string]any{
		"id":        "prt_2",
		"sessionID": "ses_storage",
		"messageID": "msg_1",
		"type":      "step-finish",
		"tokens": map[string]any{
			"input":  11,
			"output": 7,
			"cache": map[string]any{
				"read":  3,
				"write": 2,
			},
		},
		"time": map[string]any{
			"created": 1700000001000,
		},
	})

	sess, msgs, err := ParseOpenCodeFile(sessionPath, "testmachine")
	if err != nil {
		t.Fatalf("ParseOpenCodeFile: %v", err)
	}
	if sess == nil || len(msgs) != 1 {
		t.Fatalf(
			"got session=%#v messages=%d, want one parsed session",
			sess, len(msgs),
		)
	}

	assertEq(t, "msg[0].Model", msgs[0].Model, "gpt-5.2-codex")
	assertEq(t, "msg[0].HasOutputTokens", msgs[0].HasOutputTokens, true)
	assertEq(t, "msg[0].OutputTokens", msgs[0].OutputTokens, 7)
	assertEq(t, "msg[0].HasContextTokens", msgs[0].HasContextTokens, true)
	assertEq(t, "msg[0].ContextTokens", msgs[0].ContextTokens, 16)
	assertEq(
		t, "session HasTotalOutputTokens",
		sess.HasTotalOutputTokens, true,
	)
	assertEq(t, "session TotalOutputTokens", sess.TotalOutputTokens, 7)
	assertEq(
		t, "session HasPeakContextTokens",
		sess.HasPeakContextTokens, true,
	)
	assertEq(t, "session PeakContextTokens", sess.PeakContextTokens, 16)
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

// TestParseOpenCodeDB_TokenUsage verifies that an assistant
// message with modelID and tokens populates ParsedMessage.Model
// and TokenUsage in the agentsview-native key shape, and that
// session totals roll up. Without this fix the usage dashboard
// reports $0 for every OpenCode session.
func TestParseOpenCodeDB_TokenUsage(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/home/user/code/myapp")
	seeder.AddSession("ses_usage", "prj_1", "", "Usage Test",
		1700000000000, 1700000060000)

	seeder.AddMessage("msg_user", "ses_usage",
		1700000000000, 1700000000000,
		`{"role":"user"}`)
	seeder.AddPart("prt_user", "msg_user", "ses_usage",
		1700000000000, 1700000000000,
		`{"type":"text","text":"hi"}`)

	// Assistant message with full token blob: input, output,
	// cache.read, cache.write. Mirrors the real OpenCode shape
	// for OpenAI and Anthropic providers.
	assistantData := `{"role":"assistant","modelID":"gpt-5.2-codex",` +
		`"providerID":"openai","cost":0.0186375,` +
		`"tokens":{"input":10370,"output":35,"reasoning":0,` +
		`"cache":{"read":0,"write":0}}}`
	seeder.AddMessage("msg_asst", "ses_usage",
		1700000010000, 1700000010000, assistantData)
	seeder.AddPart("prt_asst", "msg_asst", "ses_usage",
		1700000010000, 1700000010000,
		`{"type":"text","text":"answer"}`)

	// Second assistant with cache read+write to verify the
	// nested cache.{read,write} fields map onto the
	// cache_{read,creation}_input_tokens keys.
	cacheData := `{"role":"assistant","modelID":"claude-sonnet-4-20250514",` +
		`"providerID":"anthropic","cost":0.04641675,` +
		`"tokens":{"input":1,"output":102,"reasoning":0,` +
		`"cache":{"read":500,"write":11969}}}`
	seeder.AddMessage("msg_asst2", "ses_usage",
		1700000020000, 1700000020000, cacheData)
	seeder.AddPart("prt_asst2", "msg_asst2", "ses_usage",
		1700000020000, 1700000020000,
		`{"type":"text","text":"answer2"}`)

	sessions, err := ParseOpenCodeDB(dbPath, "testmachine")
	if err != nil {
		t.Fatalf("ParseOpenCodeDB: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1", len(sessions))
	}
	s := sessions[0]

	var asst1, asst2 *ParsedMessage
	for i := range s.Messages {
		m := &s.Messages[i]
		if m.Role != RoleAssistant {
			continue
		}
		switch m.Model {
		case "gpt-5.2-codex":
			asst1 = m
		case "claude-sonnet-4-20250514":
			asst2 = m
		}
	}
	if asst1 == nil {
		t.Fatal("missing gpt-5.2-codex assistant message")
	}
	if asst2 == nil {
		t.Fatal("missing claude-sonnet assistant message")
	}

	checkUsage := func(name string, m *ParsedMessage,
		wantIn, wantOut, wantCacheRead, wantCacheCreate int) {
		t.Helper()
		if len(m.TokenUsage) == 0 {
			t.Fatalf("%s: TokenUsage empty", name)
		}
		var got map[string]int
		if err := json.Unmarshal(m.TokenUsage, &got); err != nil {
			t.Fatalf("%s: unmarshal TokenUsage: %v", name, err)
		}
		assertEq(t, name+" input_tokens",
			got["input_tokens"], wantIn)
		assertEq(t, name+" output_tokens",
			got["output_tokens"], wantOut)
		assertEq(t, name+" cache_read_input_tokens",
			got["cache_read_input_tokens"], wantCacheRead)
		assertEq(t, name+" cache_creation_input_tokens",
			got["cache_creation_input_tokens"], wantCacheCreate)
		assertEq(t, name+" OutputTokens",
			m.OutputTokens, wantOut)
		assertEq(t, name+" HasOutputTokens",
			m.HasOutputTokens, wantOut > 0)
		wantCtx := wantIn + wantCacheRead + wantCacheCreate
		assertEq(t, name+" ContextTokens",
			m.ContextTokens, wantCtx)
		assertEq(t, name+" HasContextTokens",
			m.HasContextTokens, wantCtx > 0)
	}

	checkUsage("gpt", asst1, 10370, 35, 0, 0)
	checkUsage("claude", asst2, 1, 102, 500, 11969)

	// Session-level rollups via accumulateMessageTokenUsage.
	if !s.Session.HasTotalOutputTokens {
		t.Fatal("session HasTotalOutputTokens=false, want true")
	}
	assertEq(t, "TotalOutputTokens",
		s.Session.TotalOutputTokens, 137) // 35 + 102
	if !s.Session.HasPeakContextTokens {
		t.Fatal("session HasPeakContextTokens=false, want true")
	}
	assertEq(t, "PeakContextTokens",
		s.Session.PeakContextTokens, 12470) // 1 + 500 + 11969
}

// TestParseOpenCodeDB_UnknownTokensShape verifies that a
// present but unrecognized `tokens` object (empty {} or a
// foreign schema) leaves TokenUsage empty so the usage query
// filter skips the row, rather than fabricating a zero-valued
// record that pollutes the dashboard.
func TestParseOpenCodeDB_UnknownTokensShape(t *testing.T) {
	cases := []struct {
		name      string
		tokensRaw string
	}{
		{"empty object", `{}`},
		{"foreign keys only", `{"totalTokens":42,"promptCount":3}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath, seeder, db := newTestDB(t)
			defer db.Close()

			seeder.AddProject("prj_1", "/tmp/proj")
			seeder.AddSession("ses_u", "prj_1", "", "Unknown",
				1700000000000, 1700000010000)
			seeder.AddMessage("msg_u", "ses_u",
				1700000000000, 1700000000000, `{"role":"user"}`)
			seeder.AddPart("prt_u", "msg_u", "ses_u",
				1700000000000, 1700000000000,
				`{"type":"text","text":"hi"}`)

			data := `{"role":"assistant","modelID":"gpt-5.4",` +
				`"providerID":"openai","tokens":` + tc.tokensRaw + `}`
			seeder.AddMessage("msg_a", "ses_u",
				1700000005000, 1700000005000, data)
			seeder.AddPart("prt_a", "msg_a", "ses_u",
				1700000005000, 1700000005000,
				`{"type":"text","text":"answer"}`)

			sessions, err := ParseOpenCodeDB(dbPath, "m")
			if err != nil {
				t.Fatalf("ParseOpenCodeDB: %v", err)
			}
			if len(sessions) != 1 {
				t.Fatalf("sessions len = %d, want 1", len(sessions))
			}

			var asst *ParsedMessage
			for i := range sessions[0].Messages {
				if sessions[0].Messages[i].Role == RoleAssistant {
					asst = &sessions[0].Messages[i]
					break
				}
			}
			if asst == nil {
				t.Fatal("missing assistant message")
			}
			assertEq(t, "Model", asst.Model, "gpt-5.4")
			if len(asst.TokenUsage) != 0 {
				t.Fatalf("TokenUsage = %q, want empty",
					string(asst.TokenUsage))
			}
			assertEq(t, "HasOutputTokens",
				asst.HasOutputTokens, false)
			assertEq(t, "HasContextTokens",
				asst.HasContextTokens, false)
		})
	}
}

// TestParseOpenCodeDB_ZeroTokens verifies that an explicit
// tokens block with every counter set to zero is preserved as
// "known zero" rather than collapsed to "unknown". The
// normalized token_usage row is still written and both
// coverage flags are set, so downstream rollups can
// distinguish an errored request from a missing usage blob.
func TestParseOpenCodeDB_ZeroTokens(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/tmp/proj")
	seeder.AddSession("ses_zero", "prj_1", "", "Zero",
		1700000000000, 1700000010000)

	seeder.AddMessage("msg_u", "ses_zero",
		1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_u", "msg_u", "ses_zero",
		1700000000000, 1700000000000,
		`{"type":"text","text":"hi"}`)

	// Errored assistant request: OpenCode still records the
	// tokens object with every field set to zero. Non-empty
	// content keeps the row out of the "empty message" filter
	// so the usage extraction path is actually exercised.
	seeder.AddMessage("msg_a", "ses_zero",
		1700000005000, 1700000005000,
		`{"role":"assistant","modelID":"gpt-5.2-chat-latest",`+
			`"providerID":"openai","cost":0,`+
			`"tokens":{"input":0,"output":0,"reasoning":0,`+
			`"cache":{"read":0,"write":0}}}`)
	seeder.AddPart("prt_a", "msg_a", "ses_zero",
		1700000005000, 1700000005000,
		`{"type":"text","text":"sorry, request failed"}`)

	sessions, err := ParseOpenCodeDB(dbPath, "m")
	if err != nil {
		t.Fatalf("ParseOpenCodeDB: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1", len(sessions))
	}

	var asst *ParsedMessage
	for i := range sessions[0].Messages {
		if sessions[0].Messages[i].Role == RoleAssistant {
			asst = &sessions[0].Messages[i]
			break
		}
	}
	if asst == nil {
		t.Fatal("missing assistant message")
	}
	assertEq(t, "Model", asst.Model, "gpt-5.2-chat-latest")
	if len(asst.TokenUsage) == 0 {
		t.Fatal("TokenUsage empty; want zero-valued JSON preserved")
	}
	var got map[string]int
	if err := json.Unmarshal(asst.TokenUsage, &got); err != nil {
		t.Fatalf("unmarshal TokenUsage: %v", err)
	}
	assertEq(t, "input_tokens", got["input_tokens"], 0)
	assertEq(t, "output_tokens", got["output_tokens"], 0)
	assertEq(t, "cache_read_input_tokens",
		got["cache_read_input_tokens"], 0)
	assertEq(t, "cache_creation_input_tokens",
		got["cache_creation_input_tokens"], 0)
	assertEq(t, "HasOutputTokens", asst.HasOutputTokens, true)
	assertEq(t, "HasContextTokens", asst.HasContextTokens, true)
	assertEq(t, "OutputTokens", asst.OutputTokens, 0)
	assertEq(t, "ContextTokens", asst.ContextTokens, 0)
}

// TestParseOpenCodeDB_NoTokenUsage verifies that assistant
// messages with no tokens block (e.g. errored requests) leave
// TokenUsage empty so they are filtered out by the usage query.
func TestParseOpenCodeDB_NoTokenUsage(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/tmp/proj")
	seeder.AddSession("ses_err", "prj_1", "", "Errored",
		1700000000000, 1700000010000)

	seeder.AddMessage("msg_u", "ses_err",
		1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_u", "msg_u", "ses_err",
		1700000000000, 1700000000000,
		`{"type":"text","text":"hi"}`)

	// No tokens block at all (errored request).
	seeder.AddMessage("msg_a", "ses_err",
		1700000005000, 1700000005000,
		`{"role":"assistant","modelID":"gpt-5.4","providerID":"openai"}`)
	seeder.AddPart("prt_a", "msg_a", "ses_err",
		1700000005000, 1700000005000,
		`{"type":"text","text":"oops"}`)

	sessions, err := ParseOpenCodeDB(dbPath, "m")
	if err != nil {
		t.Fatalf("ParseOpenCodeDB: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1", len(sessions))
	}

	var asst *ParsedMessage
	for i := range sessions[0].Messages {
		if sessions[0].Messages[i].Role == RoleAssistant {
			asst = &sessions[0].Messages[i]
			break
		}
	}
	if asst == nil {
		t.Fatal("missing assistant message")
	}
	assertEq(t, "Model", asst.Model, "gpt-5.4")
	if len(asst.TokenUsage) != 0 {
		t.Fatalf("TokenUsage = %q, want empty", string(asst.TokenUsage))
	}
}

func TestOpenCodeStorageFingerprintMissingDetectsContentRewrite(
	t *testing.T,
) {
	stored := buildOpenCodeStorageFingerprint(
		[]openCodeMessageRow{{
			id:          "msg-1",
			data:        `{"role":"assistant","modelID":"gpt-5"}`,
			timeCreated: 100,
		}},
		map[string][]openCodePartRow{
			"msg-1": {{
				id:          "part-1",
				messageID:   "msg-1",
				data:        `{"type":"text","text":"complete"}`,
				timeCreated: 101,
			}},
		},
	)
	current := buildOpenCodeStorageFingerprint(
		[]openCodeMessageRow{{
			id:          "msg-1",
			data:        `{"role":"assistant","modelID":"gpt-5"}`,
			timeCreated: 100,
		}},
		map[string][]openCodePartRow{
			"msg-1": {{
				id:          "part-1",
				messageID:   "msg-1",
				data:        `{"type":"text","text":"truncated"}`,
				timeCreated: 101,
			}},
		},
	)

	if !OpenCodeStorageFingerprintMissing(
		stored, current,
	) {
		t.Fatal("expected content rewrite to invalidate fingerprint")
	}
}
