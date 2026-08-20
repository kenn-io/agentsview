package parser

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseOpenCodeAll parses every session in an OpenCode SQLite database using
// the same per-session primitives the provider uses (ListOpenCodeSessionMeta +
// parseOpenCodeDBSession), reproducing the deleted ParseOpenCodeDB
// whole-database free function for the retained parse tests.
func parseOpenCodeAll(dbPath, machine string) ([]ParseResult, error) {
	metas, err := ListOpenCodeSessionMeta(dbPath)
	if err != nil {
		return nil, err
	}
	var out []ParseResult
	for _, m := range metas {
		sess, msgs, err := parseOpenCodeDBSession(dbPath, m.SessionID, machine)
		if err != nil {
			return nil, err
		}
		if sess == nil {
			continue
		}
		out = append(out, ParseResult{Session: *sess, Messages: msgs})
	}
	return out, nil
}

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
	directory TEXT NOT NULL DEFAULT '',
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

-- SQLite does not index a foreign key automatically. Production OpenCode
-- declares these, and the per-session freshness lookups depend on them, so the
-- fixture must carry them or plan assertions prove nothing.
CREATE INDEX message_session_time_created_id_idx
	ON message (session_id, time_created, id);
CREATE INDEX part_session_idx ON part (session_id);
CREATE INDEX part_message_id_id_idx ON part (message_id, id);
`

func assertEq[T comparable](t *testing.T, name string, got, want T) {
	t.Helper()
	assert.Equal(t, want, got, name)
}

type openCodeSeedExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

type OpenCodeSeeder struct {
	db   *sql.DB
	exec openCodeSeedExecer
	t    *testing.T
}

func (s *OpenCodeSeeder) executor() openCodeSeedExecer {
	if s.exec != nil {
		return s.exec
	}
	return s.db
}

func (s *OpenCodeSeeder) InTransaction(seed func(*OpenCodeSeeder)) {
	s.t.Helper()
	tx, err := s.db.Begin()
	require.NoError(s.t, err, "begin seed transaction")
	defer func() { _ = tx.Rollback() }()

	seed(&OpenCodeSeeder{db: s.db, exec: tx, t: s.t})
	require.NoError(s.t, tx.Commit(), "commit seed transaction")
}

func (s *OpenCodeSeeder) AddProject(id, worktree string) {
	s.t.Helper()
	_, err := s.executor().Exec(`INSERT INTO project (id, worktree) VALUES (?, ?)`, id, worktree)
	require.NoError(s.t, err, "add project")
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

	// Omit directory so the same helper works on legacy schemas that
	// lack the column; modern fixtures default directory to ''.
	_, err := s.executor().Exec(
		`INSERT INTO session
			(id, project_id, parent_id, title, time_created, time_updated)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, projectID, pID, tStr, timeCreated, timeUpdated,
	)
	require.NoError(s.t, err, "add session")
}

func (s *OpenCodeSeeder) AddSessionDirectory(
	id, projectID, parentID, title, directory string,
	timeCreated, timeUpdated int64,
) {
	s.t.Helper()

	var pID, tStr any
	if parentID != "" {
		pID = parentID
	}
	if title != "" {
		tStr = title
	}

	_, err := s.executor().Exec(
		`INSERT INTO session
			(id, project_id, parent_id, title, directory,
			 time_created, time_updated)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, projectID, pID, tStr, directory, timeCreated, timeUpdated,
	)
	require.NoError(s.t, err, "add session with directory")
}

func (s *OpenCodeSeeder) AddMessage(id, sessionID string, timeCreated, timeUpdated int64, data string) {
	s.t.Helper()
	_, err := s.executor().Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		id, sessionID, timeCreated, timeUpdated, data)
	require.NoError(s.t, err, "add message")
}

func (s *OpenCodeSeeder) AddPart(id, messageID, sessionID string, timeCreated, timeUpdated int64, data string) {
	s.t.Helper()
	_, err := s.executor().Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
		id, messageID, sessionID, timeCreated, timeUpdated, data)
	require.NoError(s.t, err, "add part")
}

func newTestDB(t *testing.T) (string, *OpenCodeSeeder, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	copyOpenCodeSchemaTemplate(t, dbPath)
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open test db")
	// Close before TempDir cleanup: Windows cannot delete a database file
	// that still has an open handle. Close is idempotent, so tests that
	// close the writer themselves are unaffected.
	t.Cleanup(func() { _ = db.Close() })

	seeder := &OpenCodeSeeder{db: db, t: t}
	return dbPath, seeder, db
}

var (
	openCodeSchemaTemplateOnce  sync.Once
	openCodeSchemaTemplateBytes []byte
	openCodeSchemaTemplateErr   error
)

func copyOpenCodeSchemaTemplate(t *testing.T, dbPath string) {
	t.Helper()
	openCodeSchemaTemplateOnce.Do(func() {
		openCodeSchemaTemplateBytes, openCodeSchemaTemplateErr =
			buildOpenCodeSchemaTemplate()
	})
	require.NoError(t, openCodeSchemaTemplateErr)
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755),
		"mkdir opencode test db dir")
	require.NoError(t, os.WriteFile(dbPath, openCodeSchemaTemplateBytes, 0o644),
		"copy opencode schema template")
}

func buildOpenCodeSchemaTemplate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "agentsview-opencode-schema-*")
	if err != nil {
		return nil, fmt.Errorf("create opencode schema template dir: %w", err)
	}
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "opencode.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open opencode schema template: %w", err)
	}
	if _, err = db.Exec(openCodeSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create opencode schema template: %w", err)
	}
	if err = db.Close(); err != nil {
		return nil, fmt.Errorf("close opencode schema template: %w", err)
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		return nil, fmt.Errorf("read opencode schema template: %w", err)
	}
	return raw, nil
}

// seedHybridSQLiteDB creates an OpenCode-shaped SQLite DB at
// dbPath containing a single session row with the given ID. Used
// by tests that exercise OpenCode-format source lookup in hybrid and
// pure-SQLite roots, where a real DB file (not just a marker) is
// required.
func seedHybridSQLiteDB(t *testing.T, dbPath, sessionID string) {
	t.Helper()
	copyOpenCodeSchemaTemplate(t, dbPath)
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open hybrid db")
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(
		`INSERT INTO project (id, worktree)
		 VALUES (?, ?)`,
		"prj_seed", "/tmp/seed",
	)
	require.NoError(t, err, "seed project")
	_, err = db.Exec(
		`INSERT INTO session
			(id, project_id, time_created, time_updated)
		 VALUES (?, ?, ?, ?)`,
		sessionID, "prj_seed", int64(1), int64(2),
	)
	require.NoError(t, err, "seed session")
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
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755),
		"mkdir %s", filepath.Dir(path))
	raw, err := json.Marshal(data)
	require.NoError(t, err, "marshal %s", path)
	require.NoError(t, os.WriteFile(path, raw, 0o644), "write %s", path)
}

func BenchmarkOpenCodeStorageSessionFingerprint(b *testing.B) {
	for _, messageCount := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("messages_%d", messageCount), func(b *testing.B) {
			root := b.TempDir()
			sessionPath := filepath.Join(
				root, "storage", "session", "benchmark-project", "ses_benchmark.json",
			)
			writeFile := func(path string, data []byte) {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o644); err != nil {
					b.Fatal(err)
				}
			}
			writeFile(sessionPath, []byte(`{"id":"ses_benchmark","directory":"/work/benchmark","title":"Benchmark","time":{"created":1700000000000,"updated":1700000060000}}`))
			for messageIndex := range messageCount {
				messageID := fmt.Sprintf("msg_%04d", messageIndex)
				writeFile(
					filepath.Join(root, "storage", "message", "ses_benchmark", messageID+".json"),
					fmt.Appendf(nil,
						`{"id":%q,"sessionID":"ses_benchmark","role":%q,"time":{"created":%d}}`,
						messageID,
						map[bool]string{true: "assistant", false: "user"}[messageIndex%2 == 1],
						1700000000000+int64(messageIndex)*1000,
					),
				)
				for partIndex := range 4 {
					partID := fmt.Sprintf("part_%04d_%d", messageIndex, partIndex)
					partType := "text"
					if partIndex == 3 {
						partType = "reasoning"
					}
					writeFile(
						filepath.Join(root, "storage", "part", messageID, partID+".json"),
						fmt.Appendf(nil,
							`{"id":%q,"sessionID":"ses_benchmark","messageID":%q,"type":%q,"text":"benchmark content","time":{"created":%d}}`,
							partID, messageID, partType,
							1700000000000+int64(messageIndex)*1000+int64(partIndex),
						),
					)
				}
			}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				hash, err := openCodeStorageSessionFingerprint(sessionPath)
				if err != nil {
					b.Fatal(err)
				}
				if hash == "" {
					b.Fatal("expected fingerprint")
				}
			}
		})
	}
}

func TestParseOpenCodeDB_StandardSession(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()
	seedStandardSession(t, seeder)

	sessions, err := parseOpenCodeAll(dbPath, "testmachine")
	require.NoError(t, err, "ParseOpenCodeDB")

	assertEq(t, "sessions len", len(sessions), 1)

	s := sessions[0]
	assertEq(t, "ID", s.Session.ID, "opencode:ses_abc")
	assertEq(t, "Agent", s.Session.Agent, AgentOpenCode)
	assertEq(t, "Machine", s.Session.Machine, "testmachine")
	assertEq(t, "Project", s.Session.Project, "myapp")
	assertEq(t, "Cwd", s.Session.Cwd, "/home/user/code/myapp")
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

func TestOpenOpenCodeDBDoesNotForceWALMode(t *testing.T) {
	dbPath, _, writer := newTestDB(t)
	require.NoError(t, writer.Close())

	reader, err := openOpenCodeDB(dbPath)
	require.NoError(t, err)
	defer reader.Close()
	_, err = reader.Exec("CREATE TABLE must_stay_read_only (id INTEGER)")
	require.Error(t, err, "OpenCode source databases must stay read-only")

	var journalMode string
	require.NoError(t, reader.QueryRow("PRAGMA journal_mode").Scan(&journalMode))
	assert.Equal(t, "delete", journalMode)
	assert.NoFileExists(t, dbPath+"-wal")
	assert.NoFileExists(t, dbPath+"-shm")
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

	sess, msgs, err := parseOpenCodeStorageFile(
		sessionPath, "testmachine",
	)
	require.NoError(t, err, "parseOpenCodeStorageFile")
	require.NotNil(t, sess, "expected non-nil session")

	assertEq(t, "ID", sess.ID, "opencode:ses_storage")
	assertEq(t, "Agent", sess.Agent, AgentOpenCode)
	assertEq(t, "Project", sess.Project, "myapp")
	assertEq(t, "Cwd", sess.Cwd, "/home/user/code/myapp")
	assertEq(t, "Machine", sess.Machine, "testmachine")
	assertEq(t, "MessageCount", sess.MessageCount, 2)
	assertEq(t, "FirstMessage", sess.FirstMessage, "Storage Session")
	assertEq(t, "File.Path", sess.File.Path, sessionPath)
	assertEq(t, "File.Mtime", sess.File.Mtime > 0, true)

	assertEq(t, "messages len", len(msgs), 2)
	fingerprint, err := openCodeStorageSessionFingerprint(sessionPath)
	require.NoError(t, err)
	assert.Equal(t, sess.File.Hash, fingerprint,
		"fingerprinting and parsing must stamp the same raw storage identity")
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

func TestOpenCodeStorageEmptySessionKeepsSkipAndFingerprint(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(root, "storage", "session", "global", "ses_empty.json")
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id": "ses_empty",
		"time": map[string]any{
			"created": int64(1700000000000),
			"updated": int64(1700000000000),
		},
	})

	sess, messages, err := parseOpenCodeStorageFile(sessionPath, "")
	require.NoError(t, err)
	assert.Nil(t, sess)
	assert.Empty(t, messages)

	fingerprint, err := openCodeStorageSessionFingerprint(sessionPath)
	require.NoError(t, err)
	assert.Empty(t, fingerprint)

	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_empty", "msg_in_progress.json",
	), map[string]any{
		"id": "msg_in_progress", "sessionID": "ses_empty", "role": "user",
		"time": map[string]any{"created": int64(1700000000000)},
	})
	sess, messages, err = parseOpenCodeStorageFile(sessionPath, "")
	require.NoError(t, err)
	assert.Nil(t, sess)
	assert.Empty(t, messages)
	fingerprint, err = openCodeStorageSessionFingerprint(sessionPath)
	require.NoError(t, err)
	assert.Empty(t, fingerprint)

	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_in_progress", "part_whitespace.json",
	), map[string]any{
		"id": "part_whitespace", "sessionID": "ses_empty",
		"messageID": "msg_in_progress", "type": "text", "text": "   ",
	})
	sess, messages, err = parseOpenCodeStorageFile(sessionPath, "")
	require.NoError(t, err)
	assert.Nil(t, sess)
	assert.Empty(t, messages)
	fingerprint, err = openCodeStorageSessionFingerprint(sessionPath)
	require.NoError(t, err)
	assert.Empty(t, fingerprint)
}

func TestOpenCodeStorageFingerprintSerializationIsStable(t *testing.T) {
	got := buildOpenCodeSessionFingerprint(
		openCodeSessionRow{
			id: "ses_fixed", timeCreated: 100, timeUpdated: 200,
		},
		"/work/fixed", "/work/fixed",
		[]openCodeMessageRow{{
			id: "msg_1", data: `{"role":"user"}`, timeCreated: 300,
		}},
		map[string][]openCodePartRow{
			"msg_1": {{
				id: "part_1", data: `{"type":"text","text":"hello"}`,
				timeCreated: 301,
			}},
		},
	)
	assert.Equal(t,
		`opencode-storage:v1:{"session":{"id":"ses_fixed","directory":"/work/fixed","worktree":"/work/fixed","time_created":100,"time_updated":200},"messages":[{"id":"msg_1","time":300,"hash":"6b3061507ef8cc2ca95320f790a9f9ccb3c850ae081df4b6f6e56e40d57203e4","parts":[{"id":"part_1","time":301,"hash":"59aeafa564efb2dec1ad26be01872d3ac132e96fc9cb3b53cba4c62cde0dc188"}]}]}`,
		got,
	)
}

func TestOpenCodeStorageFingerprintAvoidsNormalizedParseAllocations(
	t *testing.T,
) {
	root := t.TempDir()
	sessionPath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_allocations", "allocations-app", "Allocations",
	)

	fingerprintAllocs := testing.AllocsPerRun(5, func() {
		hash, err := openCodeStorageSessionFingerprint(sessionPath)
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		if hash == "" {
			t.Fatal("expected fingerprint")
		}
	})
	parseAllocs := testing.AllocsPerRun(5, func() {
		sess, _, err := parseOpenCodeStorageFile(sessionPath, "testmachine")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if sess == nil {
			t.Fatal("expected parsed session")
		}
	})

	assert.Less(t, fingerprintAllocs, parseAllocs,
		"raw fingerprinting should avoid normalized session allocation")
}

func TestParseOpenCodeFile_LegacySessionUsesProjectWorktree(t *testing.T) {
	const projectID = "legacy-project"
	const sessionID = "ses_legacy"
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", projectID, sessionID+".json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id": sessionID, "title": "Legacy", "time": map[string]any{
			"created": int64(1700000000000), "updated": int64(1700000060000),
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "project", projectID+".json",
	), map[string]any{
		"id": projectID, "vcs": "git", "worktree": "/home/user/code/legacy-app",
	})
	writeOpenCodeStorageFile(t, filepath.Join(root, "storage", "message", "ses_legacy", "msg_1.json"), map[string]any{
		"id": "msg_1", "sessionID": "ses_legacy", "role": "user",
		"time": map[string]any{"created": int64(1700000000000)},
	})
	writeOpenCodeStorageFile(t, filepath.Join(root, "storage", "part", "msg_1", "part_1.json"), map[string]any{
		"id": "part_1", "sessionID": "ses_legacy", "messageID": "msg_1",
		"type": "text", "text": "hello", "time": map[string]any{"created": int64(1700000000000)},
	})

	sess, _, err := parseOpenCodeStorageFile(sessionPath, "machine")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "/home/user/code/legacy-app", sess.Cwd)
	assert.Equal(t, "legacy_app", sess.Project)

	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id": "ses_legacy", "directory": "/home/user/code/session-app", "title": "Legacy",
		"time": map[string]any{"created": int64(1700000000000), "updated": int64(1700000060000)},
	})
	sess, _, err = parseOpenCodeStorageFile(sessionPath, "machine")
	require.NoError(t, err)
	assert.Equal(t, "/home/user/code/session-app", sess.Cwd)
	assert.Equal(t, "session_app", sess.Project)
}

func TestOpenCodeStorageFingerprintTracksRawRows(t *testing.T) {
	root := t.TempDir()
	const (
		projectID = "fingerprint-project"
		sessionID = "ses_fingerprint"
	)
	sessionPath := filepath.Join(
		root, "storage", "session", projectID, sessionID+".json",
	)
	projectPath := filepath.Join(
		root, "storage", "project", projectID+".json",
	)
	messagePath := filepath.Join(
		root, "storage", "message", sessionID, "msg_1.json",
	)
	partPath := filepath.Join(
		root, "storage", "part", "msg_1", "part_1.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id": sessionID, "title": "Fingerprint session",
		"time": map[string]any{
			"created": int64(1700000000000),
			"updated": int64(1700000060000),
		},
	})
	writeOpenCodeStorageFile(t, projectPath, map[string]any{
		"id": projectID, "worktree": "/work/fingerprint-app",
	})
	writeOpenCodeStorageFile(t, messagePath, map[string]any{
		"id": "msg_1", "sessionID": sessionID, "role": "user",
		"time": map[string]any{"created": int64(1700000000000)},
	})
	writeOpenCodeStorageFile(t, partPath, map[string]any{
		"id": "part_1", "sessionID": sessionID, "messageID": "msg_1",
		"type": "text", "text": "fingerprint content",
		"time": map[string]any{"created": int64(1700000000000)},
	})

	baseline, err := openCodeStorageSessionFingerprint(sessionPath)
	require.NoError(t, err)
	require.NotEmpty(t, baseline)
	sess, _, err := parseOpenCodeStorageFile(sessionPath, "testmachine")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, baseline, sess.File.Hash)

	tests := []struct {
		name string
		path string
		data map[string]any
	}{
		{
			name: "session",
			path: sessionPath,
			data: map[string]any{
				"id": sessionID, "title": "Changed fingerprint session",
				"time": map[string]any{
					"created": int64(1700000000000),
					"updated": int64(1700000060000),
				},
			},
		},
		{
			name: "project",
			path: projectPath,
			data: map[string]any{
				"id": projectID, "worktree": "/work/changed-fingerprint-app",
			},
		},
		{
			name: "message",
			path: messagePath,
			data: map[string]any{
				"id": "msg_1", "sessionID": sessionID, "role": "assistant",
				"time": map[string]any{"created": int64(1700000000000)},
			},
		},
		{
			name: "part",
			path: partPath,
			data: map[string]any{
				"id": "part_1", "sessionID": sessionID, "messageID": "msg_1",
				"type": "text", "text": "changed fingerprint content",
				"time": map[string]any{"created": int64(1700000000000)},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original, err := os.ReadFile(tc.path)
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, os.WriteFile(tc.path, original, 0o644))
			})
			writeOpenCodeStorageFile(t, tc.path, tc.data)
			changed, err := openCodeStorageSessionFingerprint(sessionPath)
			require.NoError(t, err)
			assert.NotEqual(t, baseline, changed)
		})
	}
}

func TestResolveOpenCodeWorktreeUsesSharedPrecedence(t *testing.T) {
	tests := []struct {
		name             string
		sessionDirectory string
		projectWorktree  string
		want             string
	}{
		{
			name:             "session directory wins",
			sessionDirectory: "/work/session",
			projectWorktree:  "/work/project",
			want:             "/work/session",
		},
		{
			name:            "project fallback",
			projectWorktree: "/work/project",
			want:            "/work/project",
		},
		{
			name:             "trimmed root falls back to project",
			sessionDirectory: " / ",
			projectWorktree:  "/work/project",
			want:             "/work/project",
		},
		{
			name:            "global root is unusable",
			projectWorktree: "/",
			want:            "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, resolveOpenCodeWorktree(
				test.sessionDirectory, test.projectWorktree,
			))
		})
	}
}

func TestResolveOpenCodeStorageWorktreeTrimsSessionDirectory(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "legacy-project", "ses_legacy.json",
	)
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "project", "legacy-project.json",
	), map[string]any{
		"worktree": "/work/project",
	})

	got, err := resolveOpenCodeStorageWorktree(sessionPath, " / ")
	require.NoError(t, err)
	assert.Equal(t, "/work/project", got)
}

func TestParseOpenCodeFile_LegacySessionMissingProjectFallback(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(root, "storage", "session", "legacy-project", "ses_legacy.json")
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id": "ses_legacy", "title": "Legacy", "time": map[string]any{
			"created": int64(1700000000000), "updated": int64(1700000060000),
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(root, "storage", "message", "ses_legacy", "msg_1.json"), map[string]any{
		"id": "msg_1", "sessionID": "ses_legacy", "role": "user",
		"time": map[string]any{"created": int64(1700000000000)},
	})
	writeOpenCodeStorageFile(t, filepath.Join(root, "storage", "part", "msg_1", "part_1.json"), map[string]any{
		"id": "part_1", "sessionID": "ses_legacy", "messageID": "msg_1",
		"type": "text", "text": "hello", "time": map[string]any{"created": int64(1700000000000)},
	})

	sess, _, err := parseOpenCodeStorageFile(sessionPath, "machine")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Empty(t, sess.Cwd)
	assert.Equal(t, "unknown", sess.Project)
}

func TestParseOpenCodeFile_LegacySessionMalformedProjectReturnsScopedError(
	t *testing.T,
) {
	root := t.TempDir()
	sessionPath := filepath.Join(root, "storage", "session", "legacy-project", "ses_legacy.json")
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id": "ses_legacy", "title": "Legacy", "time": map[string]any{
			"created": int64(1700000000000), "updated": int64(1700000060000),
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(root, "storage", "message", "ses_legacy", "msg_1.json"), map[string]any{
		"id": "msg_1", "sessionID": "ses_legacy", "role": "user",
		"time": map[string]any{"created": int64(1700000000000)},
	})
	writeOpenCodeStorageFile(t, filepath.Join(root, "storage", "part", "msg_1", "part_1.json"), map[string]any{
		"id": "part_1", "sessionID": "ses_legacy", "messageID": "msg_1",
		"type": "text", "text": "hello", "time": map[string]any{"created": int64(1700000000000)},
	})
	require.NoError(t, os.MkdirAll(filepath.Dir(openCodeProjectPath(sessionPath)), 0o755))
	writeOpenCodeStorageFile(t, openCodeProjectPath(sessionPath), map[string]any{
		"id": "legacy-project", "worktree": "/home/user/code/legacy-app",
	})

	require.NoError(t, os.WriteFile(
		openCodeProjectPath(sessionPath), []byte("{"), 0o644,
	))

	sess, msgs, err := parseOpenCodeStorageFile(sessionPath, "machine")
	assert.Error(t, err)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
	_, fingerprintErr := openCodeStorageSessionFingerprint(sessionPath)
	assert.Error(t, fingerprintErr)
	assert.Contains(t, fingerprintErr.Error(), "decoding opencode project file")
}

func TestParseOpenCodeFile_LegacySessionUnreadableProjectReturnsScopedError(
	t *testing.T,
) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "legacy-project", "ses_legacy.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id": "ses_legacy", "title": "Legacy", "time": map[string]any{
			"created": int64(1700000000000), "updated": int64(1700000060000),
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_legacy", "msg_1.json",
	), map[string]any{
		"id": "msg_1", "sessionID": "ses_legacy", "role": "user",
		"time": map[string]any{"created": int64(1700000000000)},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "part_1.json",
	), map[string]any{
		"id": "part_1", "sessionID": "ses_legacy", "messageID": "msg_1",
		"type": "text", "text": "hello",
		"time": map[string]any{"created": int64(1700000000000)},
	})
	projectPath := openCodeProjectPath(sessionPath)
	require.NoError(t, os.MkdirAll(projectPath, 0o755))

	sess, msgs, err := parseOpenCodeStorageFile(sessionPath, "machine")
	assert.Error(t, err)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
	assert.Contains(t, err.Error(), "reading opencode project file")
	_, fingerprintErr := openCodeStorageSessionFingerprint(sessionPath)
	assert.Error(t, fingerprintErr)
	assert.Contains(t, fingerprintErr.Error(), "reading opencode project file")
}

func TestOpenCodeStorageMtimeUsesProjectOnlyForFallback(t *testing.T) {
	concreteRoot := t.TempDir()
	concretePath := writeOpenCodeProviderStorageSession(
		t, concreteRoot, "session", "ses_directory_mtime", "directory-app", "Directory",
	)
	concreteProjectPath := openCodeProjectPath(concretePath)
	writeOpenCodeStorageFile(t, concreteProjectPath, map[string]any{
		"id": "global", "worktree": "/work/unused",
	})
	concreteBefore, err := OpenCodeSourceMtime(concretePath)
	require.NoError(t, err)
	future := time.Unix(1810000000, 123456789)
	require.NoError(t, os.Chtimes(
		concreteProjectPath, future, future,
	))
	concreteAfter, err := OpenCodeSourceMtime(concretePath)
	require.NoError(t, err)
	assert.Equal(t, concreteBefore, concreteAfter,
		"unused project metadata must not refresh a session with a concrete directory")

	fallbackRoot := t.TempDir()
	fallbackPath := filepath.Join(
		fallbackRoot, "storage", "session", "global", "ses_fallback_mtime.json",
	)
	fallbackProjectPath := openCodeProjectPath(fallbackPath)
	writeOpenCodeStorageFile(t, fallbackPath, map[string]any{
		"id":   "ses_fallback_mtime",
		"time": map[string]any{"created": int64(1700000000000)},
	})
	writeOpenCodeStorageFile(t, fallbackProjectPath, map[string]any{
		"id": "global", "worktree": "/work/fallback",
	})
	fallbackBefore, err := OpenCodeSourceMtime(fallbackPath)
	require.NoError(t, err)
	future = time.Unix(1810000100, 123456789)
	require.NoError(t, os.Chtimes(
		fallbackProjectPath, future, future,
	))
	fallbackAfter, err := OpenCodeSourceMtime(fallbackPath)
	require.NoError(t, err)
	assert.Greater(t, fallbackAfter, fallbackBefore,
		"project metadata must refresh a legacy session without a directory")
}

func TestStatOpenCodeStorageSessionStateTracksProjectContentRewrite(
	t *testing.T,
) {
	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "legacy-project", "ses_state.json",
	)
	projectPath := filepath.Join(
		root, "storage", "project", "legacy-project.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id": "ses_state", "time": map[string]any{
			"created": int64(1700000000000), "updated": int64(1700000060000),
		},
	})
	writeOpenCodeStorageFile(t, projectPath, map[string]any{
		"id": "legacy-project", "worktree": "/home/user/code/old-app",
	})

	before, ok := StatOpenCodeStorageSessionState(sessionPath)
	require.True(t, ok)
	info, err := os.Stat(projectPath)
	require.NoError(t, err)
	writeOpenCodeStorageFile(t, projectPath, map[string]any{
		"id": "legacy-project", "worktree": "/home/user/code/new-app",
	})
	afterInfo, err := os.Stat(projectPath)
	require.NoError(t, err)
	require.Equal(t, info.Size(), afterInfo.Size())
	require.NoError(t, os.Chtimes(
		projectPath, info.ModTime(), info.ModTime(),
	))

	after, ok := StatOpenCodeStorageSessionState(sessionPath)
	require.True(t, ok)
	assert.NotEqual(t, before, after,
		"project content must invalidate equal-size, preserved-mtime state")
	t.Logf("project state changed after equal-size rewrite with preserved mtime")
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
	require.NoError(t, os.MkdirAll(filepath.Join(
		root, "storage", "message", "ses_storage",
	), 0o755), "mkdir invalid message dir")
	require.NoError(t, os.WriteFile(filepath.Join(
		root, "storage", "message", "ses_storage", "msg_bad.json",
	), []byte(`{"id":"msg_bad"`), 0o644), "write invalid message")
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
	require.NoError(t, os.MkdirAll(filepath.Join(
		root, "storage", "part", "msg_1",
	), 0o755), "mkdir invalid part dir")
	require.NoError(t, os.WriteFile(filepath.Join(
		root, "storage", "part", "msg_1", "prt_bad.json",
	), []byte(`{"id":"prt_bad"`), 0o644), "write invalid part")

	sess, msgs, err := parseOpenCodeStorageFile(
		sessionPath, "testmachine",
	)
	require.Error(t, err, "expected parseOpenCodeStorageFile error")
	assert.Nil(t, sess, "session, want nil")
	assert.Nil(t, msgs, "msgs, want nil")
	_, fingerprintErr := openCodeStorageSessionFingerprint(sessionPath)
	assert.Error(t, fingerprintErr)
	assert.Contains(t, fingerprintErr.Error(), "decoding opencode message file")
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

	sess, msgs, err := parseOpenCodeStorageFile(
		sessionPath, "testmachine",
	)
	require.NoError(t, err, "parseOpenCodeStorageFile")
	assert.Nil(t, sess, "session, want nil")
	assert.Nil(t, msgs, "msgs, want nil")
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

	sess, msgs, err := parseOpenCodeStorageFile(
		sessionPath, "testmachine",
	)
	require.Error(t, err, "expected parseOpenCodeStorageFile error")
	assert.Nil(t, sess, "session, want nil")
	assert.Nil(t, msgs, "msgs, want nil")
	_, fingerprintErr := openCodeStorageSessionFingerprint(sessionPath)
	assert.Error(t, fingerprintErr)
	assert.Contains(t, fingerprintErr.Error(), "missing id")
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

	sess, msgs, err := parseOpenCodeStorageFile(
		sessionPath, "testmachine",
	)
	require.Error(t, err, "expected parseOpenCodeStorageFile error")
	assert.Nil(t, sess, "session, want nil")
	assert.Nil(t, msgs, "msgs, want nil")
	_, fingerprintErr := openCodeStorageSessionFingerprint(sessionPath)
	assert.Error(t, fingerprintErr)
	assert.Contains(t, fingerprintErr.Error(), "missing id")
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

	_, msgs, err := parseOpenCodeStorageFile(sessionPath, "testmachine")
	require.NoError(t, err, "parseOpenCodeStorageFile")
	require.Len(t, msgs, 1, "messages len")
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

	_, msgs, err := parseOpenCodeStorageFile(sessionPath, "testmachine")
	require.NoError(t, err, "parseOpenCodeStorageFile")
	require.Len(t, msgs, 1, "messages len")
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

	sess, msgs, err := parseOpenCodeStorageFile(sessionPath, "testmachine")
	require.NoError(t, err, "parseOpenCodeStorageFile")
	require.NotNil(t, sess, "want one parsed session")
	require.Len(t, msgs, 1, "messages")

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

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")
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

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")

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

// TestParseOpenCodeDB_SkillTool verifies that a "skill" tool part
// populates ParsedToolCall.SkillName straight from the tool's own
// input, without going through the read-file/shell-command
// inference heuristics. Before this fix extractOpenCodeToolCall
// never set SkillName at all (#1040).
func TestParseOpenCodeDB_SkillTool(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/tmp/proj")
	seeder.AddSession("ses_skill", "prj_1", "", "", 1700000000000, 1700000030000)

	seeder.AddMessage("msg_u", "ses_skill", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_u", "msg_u", "ses_skill", 1700000000000, 1700000000000, `{"type":"text","text":"use the doc-writer skill"}`)

	seeder.AddMessage("msg_a", "ses_skill", 1700000010000, 1700000010000, `{"role":"assistant"}`)
	seeder.AddPart("prt_t", "msg_a", "ses_skill", 1700000010000, 1700000010000,
		`{"type":"tool","tool":"skill","callID":"call_skill","state":{"input":{"name":"doc-writer"}}}`)

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")
	require.Len(t, sessions, 1, "sessions len")

	msgs := sessions[0].Messages
	require.Len(t, msgs, 2, "messages len")

	ast := msgs[1]
	require.Len(t, ast.ToolCalls, 1, "tool calls len")
	assertEq(t, "SkillName", ast.ToolCalls[0].SkillName, "doc-writer")
}

// TestParseOpenCodeDB_InvalidToolCall verifies that an invalid
// tool call (tool:"invalid") populates ResultEvents with
// Status:"errored" so the signal engine detects it as a failure.
func TestParseOpenCodeDB_InvalidToolCall(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/tmp/proj")
	seeder.AddSession("ses_inv", "prj_1", "", "", 1700000000000, 1700000030000)

	seeder.AddMessage("msg_u", "ses_inv", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_u", "msg_u", "ses_inv", 1700000000000, 1700000000000, `{"type":"text","text":"do something"}`)

	seeder.AddMessage("msg_a", "ses_inv", 1700000010000, 1700000010000, `{"role":"assistant"}`)
	seeder.AddPart("prt_t", "msg_a", "ses_inv", 1700000010000, 1700000010000,
		`{"type":"tool","tool":"invalid","callID":"call_inv","state":{"input":{"tool":"nonexistent_tool","error":"Model tried to call unavailable tool 'nonexistent_tool'"}}}`)

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")
	require.Len(t, sessions, 1, "sessions len")

	msgs := sessions[0].Messages
	require.Len(t, msgs, 2, "messages len")

	ast := msgs[1]
	require.Len(t, ast.ToolCalls, 1, "tool calls len")
	require.Len(t, ast.ToolCalls[0].ResultEvents, 1, "result events len")
	assertEq(t, "ResultEvents[0].Status", ast.ToolCalls[0].ResultEvents[0].Status, "errored")
}

// TestParseOpenCodeDB_BashExitFailure verifies that a bash tool whose
// state metadata records a non-zero exit is reported as a failure even
// when the output text lacks an "exit status N" marker, and that a
// successful or exit-less part stays clean.
func TestParseOpenCodeDB_BashExitFailure(t *testing.T) {
	tests := []struct {
		name        string
		tool        string
		state       string
		wantErrored bool
	}{
		{
			name:        "non-zero exit without exit-status text",
			tool:        "bash",
			state:       `{"input":{"command":"build"},"output":"error: command failed","metadata":{"exit":1}}`,
			wantErrored: true,
		},
		{
			name:        "non-zero exit with empty output",
			tool:        "bash",
			state:       `{"input":{"command":"build"},"output":"","metadata":{"exit":127}}`,
			wantErrored: true,
		},
		{
			name:        "zero exit is not a failure",
			tool:        "bash",
			state:       `{"input":{"command":"build"},"output":"ok","metadata":{"exit":0}}`,
			wantErrored: false,
		},
		{
			name:        "metadata without an exit key is not a failure",
			tool:        "bash",
			state:       `{"input":{"command":"build"},"output":"ok","metadata":{"truncated":false}}`,
			wantErrored: false,
		},
		{
			name:        "no metadata is not a failure",
			tool:        "bash",
			state:       `{"input":{"command":"build"},"output":"ok"}`,
			wantErrored: false,
		},
		{
			name:        "non-bash metadata exit is not a failure",
			tool:        "mcp_lookup",
			state:       `{"input":{"query":"exit routes"},"output":"route 1","metadata":{"exit":1}}`,
			wantErrored: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbPath, seeder, db := newTestDB(t)
			defer db.Close()

			seeder.AddProject("prj_1", "/tmp/proj")
			seeder.AddSession("ses_bexit", "prj_1", "", "", 1700000000000, 1700000030000)

			seeder.AddMessage("msg_u", "ses_bexit", 1700000000000, 1700000000000, `{"role":"user"}`)
			seeder.AddPart("prt_u", "msg_u", "ses_bexit", 1700000000000, 1700000000000, `{"type":"text","text":"build"}`)

			seeder.AddMessage("msg_a", "ses_bexit", 1700000010000, 1700000010000, `{"role":"assistant"}`)
			seeder.AddPart("prt_t", "msg_a", "ses_bexit", 1700000010000, 1700000010000,
				`{"type":"tool","tool":"`+tt.tool+`","callID":"call_exit","state":`+tt.state+`}`)

			sessions, err := parseOpenCodeAll(dbPath, "m")
			require.NoError(t, err, "ParseOpenCodeDB")
			require.Len(t, sessions, 1, "sessions len")

			msgs := sessions[0].Messages
			require.Len(t, msgs, 2, "messages len")

			ast := msgs[1]
			require.Len(t, ast.ToolCalls, 1, "tool calls len")
			if !tt.wantErrored {
				assert.Empty(t, ast.ToolCalls[0].ResultEvents, "result events")
				return
			}
			require.Len(t, ast.ToolCalls[0].ResultEvents, 1, "result events len")
			assertEq(t, "ResultEvents[0].Status", ast.ToolCalls[0].ResultEvents[0].Status, "errored")
		})
	}
}

// TestParseOpenCodeDB_SkillNameFromReadTool verifies that a
// "read" tool part whose input points at a real on-disk SKILL.md
// infers the skill name from the file's frontmatter, matching the
// Cursor/Codex read-file heuristic shared via inferOpenCodeSkillName.
func TestParseOpenCodeDB_SkillNameFromReadTool(t *testing.T) {
	path := writeTestSkill(t, "foo", "foo")

	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/tmp/proj")
	seeder.AddSession("ses_read", "prj_1", "", "", 1700000000000, 1700000030000)

	seeder.AddMessage("msg_u", "ses_read", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_u", "msg_u", "ses_read", 1700000000000, 1700000000000, `{"type":"text","text":"read the skill file"}`)

	seeder.AddMessage("msg_a", "ses_read", 1700000010000, 1700000010000, `{"role":"assistant"}`)
	seeder.AddPart("prt_t", "msg_a", "ses_read", 1700000010000, 1700000010000,
		`{"type":"tool","tool":"read","callID":"call_read","state":{"input":{"file_path":`+
			quoteJSON(t, path)+`}}}`)

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")
	require.Len(t, sessions, 1, "sessions len")

	msgs := sessions[0].Messages
	require.Len(t, msgs, 2, "messages len")

	ast := msgs[1]
	require.Len(t, ast.ToolCalls, 1, "tool calls len")
	assertEq(t, "SkillName", ast.ToolCalls[0].SkillName, "foo")
}

// TestParseOpenCodeDB_SkillNameFromReadToolRelativePath verifies
// that a read tool with a relative SKILL.md path resolves against
// the session worktree and reads frontmatter instead of falling
// back to the parent directory name.
func TestParseOpenCodeDB_SkillNameFromReadToolRelativePath(t *testing.T) {
	// Frontmatter name intentionally differs from the folder name
	// ("renamed") to prove we read frontmatter, not the directory.
	path := writeTestSkill(t, "renamed", "actual-skill")
	directory := filepath.Dir(filepath.Dir(filepath.Dir(path)))

	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", filepath.Join(t.TempDir(), "project-root"))
	seeder.AddSessionDirectory(
		"ses_rel", "prj_1", "", "", directory,
		1700000000000, 1700000030000,
	)

	seeder.AddMessage("msg_u", "ses_rel", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_u", "msg_u", "ses_rel", 1700000000000, 1700000000000, `{"type":"text","text":"read the skill"}`)

	seeder.AddMessage("msg_a", "ses_rel", 1700000010000, 1700000010000, `{"role":"assistant"}`)
	seeder.AddPart("prt_t", "msg_a", "ses_rel", 1700000010000, 1700000010000,
		`{"type":"tool","tool":"read","callID":"call_read","state":{"input":{"file_path":"skills/renamed/SKILL.md"}}}`)

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")
	require.Len(t, sessions, 1, "sessions len")

	msgs := sessions[0].Messages
	require.Len(t, msgs, 2, "messages len")

	ast := msgs[1]
	require.Len(t, ast.ToolCalls, 1, "tool calls len")
	assertEq(t, "SkillName", ast.ToolCalls[0].SkillName, "actual-skill")
}

// TestParseOpenCodeDB_SkillNameFromShellCommandRelativePath
// verifies that a "bash" tool part running a relative-path
// SKILL.md read is resolved against the session's project
// worktree (threaded through buildOpenCodeMessage), matching the
// Codex shell-command heuristic.
func TestParseOpenCodeDB_SkillNameFromShellCommandRelativePath(t *testing.T) {
	path := writeTestSkill(t, "foo", "foo")
	worktree := filepath.Dir(filepath.Dir(filepath.Dir(path)))

	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", worktree)
	seeder.AddSession("ses_bash", "prj_1", "", "", 1700000000000, 1700000030000)

	seeder.AddMessage("msg_u", "ses_bash", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_u", "msg_u", "ses_bash", 1700000000000, 1700000000000, `{"type":"text","text":"cat the skill file"}`)

	seeder.AddMessage("msg_a", "ses_bash", 1700000010000, 1700000010000, `{"role":"assistant"}`)
	seeder.AddPart("prt_t", "msg_a", "ses_bash", 1700000010000, 1700000010000,
		`{"type":"tool","tool":"bash","callID":"call_bash","state":{"input":{"command":"cat skills/foo/SKILL.md"}}}`)

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")
	require.Len(t, sessions, 1, "sessions len")

	msgs := sessions[0].Messages
	require.Len(t, msgs, 2, "messages len")

	ast := msgs[1]
	require.Len(t, ast.ToolCalls, 1, "tool calls len")
	assertEq(t, "SkillName", ast.ToolCalls[0].SkillName, "foo")
}

func TestParseOpenCodeDB_EmptySession(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/tmp/proj")
	seeder.AddSession("ses_empty", "prj_1", "", "", 1700000000000, 1700000000000)

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")

	assertEq(t, "sessions len", len(sessions), 0)
}

func TestParseOpenCodeDB_NonexistentDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nonexistent.db")

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "expected nil error")
	assert.Nil(t, sessions, "expected nil sessions")
}

func TestParseOpenCodeDB_ProjectFromWorktree(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	// Create a temp dir that looks like a git repo so
	// ExtractProjectFromCwd resolves it.
	repoDir := filepath.Join(t.TempDir(), "my-project")
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755))

	seeder.AddProject("prj_git", repoDir)
	seeder.AddSession("ses_git", "prj_git", "", "", 1700000000000, 1700000010000)
	seeder.AddMessage("msg_1", "ses_git", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_1", "msg_1", "ses_git", 1700000000000, 1700000000000, `{"type":"text","text":"hello"}`)

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")
	assertEq(t, "sessions len", len(sessions), 1)

	assertEq(t, "Project", sessions[0].Session.Project, "my_project")
}

func TestResolveOpenCodeWorktree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		session string
		project string
		want    string
	}{
		{
			name:    "prefers concrete session directory over global project",
			session: "/home/user/code/myapp",
			project: "/",
			want:    "/home/user/code/myapp",
		},
		{
			name:    "falls back when session directory empty",
			session: "",
			project: "/home/user/code/myapp",
			want:    "/home/user/code/myapp",
		},
		{
			name:    "falls back when session directory is root",
			session: "/",
			project: "/home/user/code/myapp",
			want:    "/home/user/code/myapp",
		},
		{
			name:    "trims session directory whitespace",
			session: "  /home/user/code/myapp  ",
			project: "/",
			want:    "/home/user/code/myapp",
		},
		{
			name:    "normalizes project root when session unusable",
			session: "/",
			project: "/",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveOpenCodeWorktree(tt.session, tt.project)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseOpenCodeDB_PrefersSessionDirectoryOverGlobalProject(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	// OpenCode's synthetic global project uses worktree="/".
	seeder.AddProject("global", "/")
	seeder.AddSessionDirectory(
		"ses_global", "global", "", "Global Session",
		"/home/user/code/lonely-app",
		1700000000000, 1700000010000,
	)
	seeder.AddMessage("msg_1", "ses_global", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart(
		"prt_1", "msg_1", "ses_global",
		1700000000000, 1700000000000,
		`{"type":"text","text":"hello from global project"}`,
	)

	sessions, err := parseOpenCodeAll(dbPath, "testmachine")
	require.NoError(t, err, "ParseOpenCodeDB")
	require.Len(t, sessions, 1)

	s := sessions[0].Session
	assert.Equal(t, "/home/user/code/lonely-app", s.Cwd)
	assert.Equal(t, "lonely_app", s.Project)
	assert.NotEqual(t, "unknown", s.Project)
}

func TestParseOpenCodeDB_ProjectFromUnavailableProjectWorktree(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	worktree := filepath.Join(t.TempDir(), "unavailable-repo")
	directory := filepath.Join(worktree, "subdir")
	_, err := os.Stat(worktree)
	require.Error(t, err)
	require.True(t, os.IsNotExist(err), "project checkout must be unavailable")

	seeder.AddProject("prj_unavailable", worktree)
	seeder.AddSessionDirectory(
		"ses_subdir", "prj_unavailable", "", "Unavailable Checkout",
		directory, 1700000000000, 1700000010000,
	)
	seeder.AddMessage(
		"msg_1", "ses_subdir", 1700000000000, 1700000000000,
		`{"role":"user"}`,
	)
	seeder.AddPart(
		"prt_1", "msg_1", "ses_subdir",
		1700000000000, 1700000000000,
		`{"type":"text","text":"hello"}`,
	)

	sessions, err := parseOpenCodeAll(dbPath, "testmachine")
	require.NoError(t, err)
	require.Len(t, sessions, 1)

	assert.Equal(t, directory, sessions[0].Session.Cwd)
	assert.Equal(t, "unavailable_repo", sessions[0].Session.Project)
}

func TestParseOpenCodeDB_EmptySessionDirectoryUsesProjectWorktree(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/home/user/code/myapp")
	seeder.AddSessionDirectory(
		"ses_empty_dir", "prj_1", "", "No Directory",
		"",
		1700000000000, 1700000010000,
	)
	seeder.AddMessage("msg_1", "ses_empty_dir", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart(
		"prt_1", "msg_1", "ses_empty_dir",
		1700000000000, 1700000000000,
		`{"type":"text","text":"hello"}`,
	)

	sessions, err := parseOpenCodeAll(dbPath, "testmachine")
	require.NoError(t, err, "ParseOpenCodeDB")
	require.Len(t, sessions, 1)

	s := sessions[0].Session
	assert.Equal(t, "/home/user/code/myapp", s.Cwd)
	assert.Equal(t, "myapp", s.Project)
}

func TestParseOpenCodeDB_EmptySessionDirectoryPreservesSQLiteRootWorktree(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_global", "/")
	seeder.AddSessionDirectory(
		"ses_global", "prj_global", "", "Global Project", "",
		1700000000000, 1700000010000,
	)
	seeder.AddMessage("msg_global", "ses_global", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart(
		"prt_global", "msg_global", "ses_global", 1700000000000, 1700000000000,
		`{"type":"text","text":"hello"}`,
	)

	sessions, err := parseOpenCodeAll(dbPath, "testmachine")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "/", sessions[0].Session.Cwd)
}

func TestParseOpenCodeDB_RootSessionDirectoryPreservesSQLiteRootWorktree(
	t *testing.T,
) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_global", "/")
	seeder.AddSessionDirectory(
		"ses_global_root", "prj_global", "", "Global Project", "/",
		1700000000000, 1700000010000,
	)
	seeder.AddMessage(
		"msg_global_root", "ses_global_root", 1700000000000,
		1700000000000, `{"role":"user"}`,
	)
	seeder.AddPart(
		"prt_global_root", "msg_global_root", "ses_global_root",
		1700000000000, 1700000000000,
		`{"type":"text","text":"hello"}`,
	)

	sessions, err := parseOpenCodeAll(dbPath, "testmachine")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "/", sessions[0].Session.Cwd)
}

// openCodeSchemaLegacy omits session.directory, matching older OpenCode-family
// SQLite layouts still used by Kilo/MiMoCode/ICodeMate archives.
const openCodeSchemaLegacy = `
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

func newLegacyOpenCodeTestDB(t *testing.T) (string, *OpenCodeSeeder, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "opencode-legacy.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open legacy test db")
	_, err = db.Exec(openCodeSchemaLegacy)
	require.NoError(t, err, "create legacy schema")
	return dbPath, &OpenCodeSeeder{db: db, t: t}, db
}

func TestParseOpenCodeDB_LegacySchemaWithoutDirectoryUsesProjectWorktree(t *testing.T) {
	dbPath, seeder, db := newLegacyOpenCodeTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_legacy", "/home/user/code/legacy-app")
	// AddSession inserts without directory; legacy schema has no such column.
	seeder.AddSession(
		"ses_legacy", "prj_legacy", "", "Legacy Session",
		1700000000000, 1700000010000,
	)
	seeder.AddMessage("msg_1", "ses_legacy", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart(
		"prt_1", "msg_1", "ses_legacy",
		1700000000000, 1700000000000,
		`{"type":"text","text":"hello from legacy schema"}`,
	)

	// Confirm the column is actually absent so the test would fail closed
	// if the modern SELECT path were used.
	hasDir, err := openCodeSessionTableHasDirectory(db)
	require.NoError(t, err)
	require.False(t, hasDir, "legacy fixture must omit session.directory")

	sessions, err := parseOpenCodeAll(dbPath, "testmachine")
	require.NoError(t, err, "ParseOpenCodeDB on legacy schema")
	require.Len(t, sessions, 1)

	s := sessions[0].Session
	assert.Equal(t, "/home/user/code/legacy-app", s.Cwd)
	assert.Equal(t, "legacy_app", s.Project)
}

func TestParseOpenCodeDB_ModernSchemaDirectoryColumnDetected(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()
	seedStandardSession(t, seeder)

	hasDir, err := openCodeSessionHasDirectoryCached(db, dbPath)
	require.NoError(t, err)
	assert.True(t, hasDir, "modern fixture must include session.directory")

	sessions, err := parseOpenCodeAll(dbPath, "testmachine")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "/home/user/code/myapp", sessions[0].Session.Cwd)
}

func TestParseOpenCodeSession_SingleSession(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()
	seedStandardSession(t, seeder)

	sess, msgs, err := parseOpenCodeDBSession(dbPath, "ses_abc", "testmachine")
	require.NoError(t, err, "parseOpenCodeDBSession")
	require.NotNil(t, sess, "expected non-nil session")

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

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")
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

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")

	var child *ParseResult
	for i := range sessions {
		if sessions[i].Session.ID == "opencode:ses_child" {
			child = &sessions[i]
		}
	}
	require.NotNil(t, child, "child session not found")
	assertEq(t, "ParentSessionID", child.Session.ParentSessionID, "opencode:ses_parent")
}

func TestListOpenCodeSessionMeta(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()
	seedStandardSession(t, seeder)

	metas, err := ListOpenCodeSessionMeta(dbPath)
	require.NoError(t, err, "ListOpenCodeSessionMeta")
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
	require.NoError(t, err, "unexpected error")
	assertEq(t, "metas len", len(metas), 0)
}

// TestListOpenCodeSessionWatermarkMeta pins the bounded changed-path listing:
// on a composite-capable container it carries only the session-row watermark
// (session and project time_updated, never child times) with no digest, so
// listing every session touches no message or part rows.
func TestListOpenCodeSessionWatermarkMeta(t *testing.T) {
	dbPath, seeder, db := newTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_1", "/home/user/code/app")
	seeder.AddSession(
		"ses_wm", "prj_1", "", "Watermark", 1700000000000, 1700000060000,
	)
	seeder.AddMessage(
		"msg_1", "ses_wm", 1700000000000, 1700099999000, `{"role":"user"}`,
	)
	seeder.AddPart(
		"prt_1", "msg_1", "ses_wm", 1700000000000, 1700099999000,
		`{"type":"text","text":"hi"}`,
	)
	// Project row above the session row: the watermark is MAX(session,
	// project). Child rows sit above both and must NOT be reflected.
	_, err := db.Exec(
		"UPDATE project SET time_updated = ? WHERE id = ?",
		1700000070000, "prj_1",
	)
	require.NoError(t, err, "raise project time")

	metas, err := ListOpenCodeSessionWatermarkMeta(dbPath)
	require.NoError(t, err, "ListOpenCodeSessionWatermarkMeta")
	require.Len(t, metas, 1)

	m := metas[0]
	assert.Equal(t, "ses_wm", m.SessionID)
	assert.Equal(t, dbPath+"#ses_wm", m.VirtualPath)
	assert.True(t, m.WatermarkOnly, "composite container must list watermark-only")
	assert.True(t, m.CompositeMtime)
	assert.Empty(t, m.ChildDigest, "watermark listing must not resolve the child digest")
	assert.Equal(t, int64(1700000070000)*1_000_000, m.FileMtime,
		"watermark must be MAX(session, project) and exclude child times")
}

// TestOpenCodeChildDigestMetadataWatermarkNS pins the digest round-trip the
// watcher's like-for-like comparison depends on: the session/project times a
// digest embeds must come back out as the metadata watermark, and every
// other hash shape must be rejected so callers fall back to the composite.
func TestOpenCodeChildDigestMetadataWatermarkNS(t *testing.T) {
	agg := openCodeChildAggregate{
		watermark:    1700000099000,
		sessionTime:  1700000060000,
		projectTime:  1700000070000,
		messages:     2,
		parts:        5,
		messageIdent: "m1:1",
		partIdent:    "p1:1",
	}
	got, ok := OpenCodeChildDigestMetadataWatermarkNS(agg.digest(true))
	require.True(t, ok, "digest must round-trip its metadata watermark")
	assert.Equal(t, int64(1700000070000)*1_000_000, got,
		"metadata watermark must be MAX(session, project), not the composite")

	for _, hash := range []string{
		"",
		agg.digest(false),
		openCodeStorageFingerprintPrefix + "abcdef",
		"opencode-child:v2:1:2:3:4:5:aabb",
		"opencode-child:v1:1:2:3",
		"opencode-child:v1:x:2:3:4:5:aabb",
		"opencode-child:v1:1:x:3:4:5:aabb",
		"opencode-child:v1:1:2:x:4:5:aabb",
	} {
		_, ok := OpenCodeChildDigestMetadataWatermarkNS(hash)
		assert.False(t, ok, "hash %q must be rejected", hash)
	}
}

// TestListOpenCodeSessionWatermarkMeta_LegacySchema pins that containers
// without composite support keep the full listing's shape: session-only
// mtime, no composite, and no watermark-only marker, so the engine never
// watermark-skips a session whose only change signal is the container size.
func TestListOpenCodeSessionWatermarkMeta_LegacySchema(t *testing.T) {
	dbPath, seeder, db := newLegacyOpenCodeTestDB(t)
	defer db.Close()

	seeder.AddProject("prj_legacy", "/home/user/code/legacy-app")
	seeder.AddSession(
		"ses_legacy", "prj_legacy", "", "Legacy", 1700000000000, 1700000060000,
	)

	metas, err := ListOpenCodeSessionWatermarkMeta(dbPath)
	require.NoError(t, err, "ListOpenCodeSessionWatermarkMeta legacy")
	require.Len(t, metas, 1)

	full, err := ListOpenCodeSessionMeta(dbPath)
	require.NoError(t, err, "ListOpenCodeSessionMeta legacy")
	require.Len(t, full, 1)

	assert.False(t, metas[0].WatermarkOnly,
		"legacy containers must not be marked watermark-only")
	assert.Equal(t, full[0], metas[0],
		"legacy watermark listing must match the full listing")
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

	sessions, err := parseOpenCodeAll(dbPath, "testmachine")
	require.NoError(t, err, "ParseOpenCodeDB")
	require.Len(t, sessions, 1, "sessions len")
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
	require.NotNil(t, asst1, "missing gpt-5.2-codex assistant message")
	require.NotNil(t, asst2, "missing claude-sonnet assistant message")

	checkUsage := func(name string, m *ParsedMessage,
		wantIn, wantOut, wantCacheRead, wantCacheCreate int) {
		t.Helper()
		require.NotEmpty(t, m.TokenUsage, "%s: TokenUsage empty", name)
		var got map[string]int
		require.NoError(t, json.Unmarshal(m.TokenUsage, &got),
			"%s: unmarshal TokenUsage", name)
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
	assert.True(t, s.Session.HasTotalOutputTokens, "session HasTotalOutputTokens")
	assertEq(t, "TotalOutputTokens",
		s.Session.TotalOutputTokens, 137) // 35 + 102
	assert.True(t, s.Session.HasPeakContextTokens, "session HasPeakContextTokens")
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

			sessions, err := parseOpenCodeAll(dbPath, "m")
			require.NoError(t, err, "ParseOpenCodeDB")
			require.Len(t, sessions, 1, "sessions len")

			var asst *ParsedMessage
			for i := range sessions[0].Messages {
				if sessions[0].Messages[i].Role == RoleAssistant {
					asst = &sessions[0].Messages[i]
					break
				}
			}
			require.NotNil(t, asst, "missing assistant message")
			assertEq(t, "Model", asst.Model, "gpt-5.4")
			assert.Empty(t, asst.TokenUsage,
				"TokenUsage = %q, want empty", string(asst.TokenUsage))
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

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")
	require.Len(t, sessions, 1, "sessions len")

	var asst *ParsedMessage
	for i := range sessions[0].Messages {
		if sessions[0].Messages[i].Role == RoleAssistant {
			asst = &sessions[0].Messages[i]
			break
		}
	}
	require.NotNil(t, asst, "missing assistant message")
	assertEq(t, "Model", asst.Model, "gpt-5.2-chat-latest")
	require.NotEmpty(t, asst.TokenUsage, "TokenUsage empty; want zero-valued JSON preserved")
	var got map[string]int
	require.NoError(t, json.Unmarshal(asst.TokenUsage, &got), "unmarshal TokenUsage")
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

	sessions, err := parseOpenCodeAll(dbPath, "m")
	require.NoError(t, err, "ParseOpenCodeDB")
	require.Len(t, sessions, 1, "sessions len")

	var asst *ParsedMessage
	for i := range sessions[0].Messages {
		if sessions[0].Messages[i].Role == RoleAssistant {
			asst = &sessions[0].Messages[i]
			break
		}
	}
	require.NotNil(t, asst, "missing assistant message")
	assertEq(t, "Model", asst.Model, "gpt-5.4")
	assert.Empty(t, asst.TokenUsage, "TokenUsage = %q, want empty", string(asst.TokenUsage))
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

	assert.True(t, OpenCodeStorageFingerprintMissing(stored, current),
		"expected content rewrite to invalidate fingerprint")
}
