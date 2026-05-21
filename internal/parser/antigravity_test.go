package parser

import (
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ---- protobuf wire walker -------------------------------------

// agProtoEncode is a tiny test-only encoder used to hand-craft
// payloads for the wire walker. It supports varint, length-
// delimited bytes, and nested messages (re-encoded recursively).
type pbField struct {
	num    int
	wire   int
	varint uint64
	bytes  []byte
}

func encodeVarint(v uint64) []byte {
	buf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(buf, v)
	return buf[:n]
}

func encodePB(fields []pbField) []byte {
	var out []byte
	for _, f := range fields {
		tag := uint64(f.num<<3) | uint64(f.wire)
		out = append(out, encodeVarint(tag)...)
		switch f.wire {
		case pbWireVarint:
			out = append(out, encodeVarint(f.varint)...)
		case pbWireBytes:
			out = append(out, encodeVarint(uint64(len(f.bytes)))...)
			out = append(out, f.bytes...)
		}
	}
	return out
}

func TestAgProtoParseAndExtract(t *testing.T) {
	inner := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779326586},
		{num: 2, wire: pbWireVarint, varint: 12345},
	})
	payload := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 7},
		{
			num:   17,
			wire:  pbWireBytes,
			bytes: []byte("Hi, what's up next?"),
		},
		{num: 5, wire: pbWireBytes, bytes: inner},
	})

	fields, err := agProtoParse(payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(fields) != 3 {
		t.Fatalf("got %d fields, want 3", len(fields))
	}

	// Field 17 should be a UTF-8 string with no nested decoding.
	got, _ := agProtoFind(fields, 17)
	s, ok := agProtoString(got)
	if !ok || s != "Hi, what's up next?" {
		t.Fatalf("field 17: got %q ok=%v", s, ok)
	}

	// Field 5 should have nested fields parsed as a Timestamp.
	tsf, _ := agProtoFind(fields, 5)
	if tsf.Nested == nil {
		t.Fatalf("field 5 not parsed as nested")
	}
	sec, nanos, ok := agProtoTimestamp(tsf.Nested)
	if !ok || sec != 1779326586 || nanos != 12345 {
		t.Fatalf("timestamp: sec=%d nanos=%d ok=%v",
			sec, nanos, ok)
	}

	strs := agProtoCollectStrings(fields, 5)
	if len(strs) != 1 || strs[0] != "Hi, what's up next?" {
		t.Fatalf("collect strings: %#v", strs)
	}
}

func TestEarliestAntigravityTimestamp(t *testing.T) {
	older := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1700000000},
		{num: 2, wire: pbWireVarint, varint: 0},
	})
	newer := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779326586},
	})
	payload := encodePB([]pbField{
		{num: 3, wire: pbWireBytes, bytes: newer},
		{num: 4, wire: pbWireBytes, bytes: older},
	})
	fields, err := agProtoParse(payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := earliestAntigravityTimestamp(fields)
	if got.Unix() != 1700000000 {
		t.Fatalf("got %d, want 1700000000", got.Unix())
	}
}

// ---- CLI parser -----------------------------------------------

func TestAntigravityCLIDiscoverAndParse(t *testing.T) {
	root := t.TempDir()
	id := "11111111-2222-3333-4444-555555555555"

	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "implicit"))
	mustMkdir(t, filepath.Join(root, "brain", id))

	// Encrypted .pb stub (content does not matter without a key)
	mustWrite(t, filepath.Join(root, "conversations", id+".pb"),
		[]byte("encrypted-placeholder"))

	// brain artifact + metadata
	mustWrite(t, filepath.Join(root, "brain", id, "task.md"),
		[]byte("# Task\n\n- step one"))
	mustWrite(t,
		filepath.Join(root, "brain", id, "task.md.metadata.json"),
		[]byte(`{
			"artifactType": "ARTIFACT_TYPE_TASK",
			"summary": "Top task summary",
			"updatedAt": "2026-05-20T22:47:27.078Z"
		}`))

	// history.jsonl: one row for our session, one for another
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"hello world","timestamp":1779000000000,`+
			`"workspace":"/tmp/proj","conversationId":"`+id+`"}
{"display":"other","timestamp":1779000001000,"workspace":"/tmp/x","conversationId":"other-id"}`))

	// Discovery should return the .pb with the right project.
	files := DiscoverAntigravityCLISessions(root)
	if len(files) != 1 {
		t.Fatalf("discover: got %d files, want 1", len(files))
	}
	if files[0].Project != "/tmp/proj" {
		t.Fatalf("project: got %q want /tmp/proj", files[0].Project)
	}

	// Find by id should locate the same .pb.
	if got := FindAntigravityCLISourceFile(root, id); got !=
		files[0].Path {
		t.Fatalf("find: got %q want %q", got, files[0].Path)
	}

	sess, msgs, err := ParseAntigravityCLISession(
		files[0].Path, files[0].Project, "test-machine",
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sess.ID != "antigravity-cli:"+id {
		t.Fatalf("session id: %q", sess.ID)
	}
	// One user message from history + one assistant from brain.
	if len(msgs) != 2 {
		t.Fatalf("msgs: got %d want 2", len(msgs))
	}
	if msgs[0].Role != RoleUser ||
		!strings.Contains(msgs[0].Content, "hello world") {
		t.Fatalf("msg0: %+v", msgs[0])
	}
	if msgs[1].Role != RoleAssistant ||
		!strings.Contains(msgs[1].Content, "step one") ||
		!strings.Contains(msgs[1].Content, "Top task summary") {
		t.Fatalf("msg1: %+v", msgs[1])
	}
	if sess.MessageCount != 2 || sess.UserMessageCount != 1 {
		t.Fatalf(
			"counts: msg=%d user=%d",
			sess.MessageCount, sess.UserMessageCount,
		)
	}
	if sess.FirstMessage != "hello world" {
		t.Fatalf("first message: %q", sess.FirstMessage)
	}
	// StartedAt is the user message timestamp (epoch ms).
	if sess.StartedAt.UnixMilli() != 1779000000000 {
		t.Fatalf(
			"startedAt: %d want 1779000000000",
			sess.StartedAt.UnixMilli(),
		)
	}
}

func TestAntigravityCLIDiscoverIgnoresJunk(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "conversations"))
	// Non-.pb files in the conversations dir are ignored.
	mustWrite(t,
		filepath.Join(root, "conversations", "README.txt"),
		[]byte("x"))
	// .pb files whose stem isn't a valid session id (contains
	// characters outside [A-Za-z0-9_-]) are skipped.
	mustWrite(t,
		filepath.Join(root, "conversations", "bad.name.pb"),
		[]byte("x"))
	if files := DiscoverAntigravityCLISessions(root); len(files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(files))
	}
}

// ---- IDE parser -----------------------------------------------

func TestAntigravityIDEDiscoverAndParse(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "annotations"))
	mustMkdir(t, filepath.Join(root, "brain", id))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath)

	mustWrite(t,
		filepath.Join(root, "annotations", id+".pbtxt"),
		[]byte("last_user_view_time:{seconds:1779326586 nanos:0}\n"))
	mustWrite(t,
		filepath.Join(root, "brain", id, "plan.md"),
		[]byte("# Plan"))
	mustWrite(t,
		filepath.Join(root, "brain", id, "plan.md.metadata.json"),
		[]byte(`{"summary":"Plan summary","updatedAt":"2026-05-20T22:47:27Z"}`))

	files := DiscoverAntigravitySessions(root)
	if len(files) != 1 || files[0].Path != dbPath {
		t.Fatalf("discover: %#v", files)
	}
	if got := FindAntigravitySourceFile(root, id); got != dbPath {
		t.Fatalf("find: got %q want %q", got, dbPath)
	}

	sess, msgs, err := ParseAntigravitySession(
		dbPath, "", "test-machine",
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sess.ID != "antigravity:"+id {
		t.Fatalf("session id: %q", sess.ID)
	}
	// 2 step rows + 1 brain artifact = 3 messages
	if len(msgs) != 3 {
		t.Fatalf("msgs: %d", len(msgs))
	}
	// step_type=14 should be flagged as user
	var sawUser, sawAssistant bool
	for _, m := range msgs {
		if m.Role == RoleUser {
			sawUser = true
			if !strings.Contains(m.Content, "user prompt text") {
				t.Fatalf("user msg content: %q", m.Content)
			}
		}
		if m.Role == RoleAssistant &&
			strings.Contains(m.Content, "Plan summary") {
			sawAssistant = true
		}
	}
	if !sawUser || !sawAssistant {
		t.Fatalf("missing role(s): user=%v assistant=%v",
			sawUser, sawAssistant)
	}
	// Annotation overrides endedAt to 2026-05-20T... =
	// 1779326586
	if sess.EndedAt.Unix() != 1779326586 {
		t.Fatalf("endedAt: %d", sess.EndedAt.Unix())
	}
}

// ---- crypto: key loading --------------------------------------

func TestAntigravityKeyMissing(t *testing.T) {
	// loadAntigravityKey memoizes via sync.Once, so we test the
	// observable behavior via hasAntigravityKey on a process
	// without the env var. Set+unset to be explicit.
	t.Setenv("ANTIGRAVITY_KEY", "")
	// Cannot reset sync.Once without restructuring the source.
	// At minimum verify hasAntigravityKey doesn't panic.
	_ = hasAntigravityKey()
}

// ---- helpers --------------------------------------------------

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func mustWrite(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// createAntigravityTestDB writes a minimal antigravity IDE
// SQLite database with two synthetic steps: a user prompt
// (step_type=14) and an assistant step (step_type=17).
func createAntigravityTestDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE trajectory_meta (
		trajectory_id text, cascade_id text,
		trajectory_type integer, source integer,
		PRIMARY KEY (trajectory_id))`)
	mustExec(t, db, `CREATE TABLE steps (
		idx integer, step_type integer NOT NULL DEFAULT 0,
		status integer NOT NULL DEFAULT 0,
		has_subtrajectory numeric NOT NULL DEFAULT false,
		metadata blob, error_details blob,
		permissions blob, task_details blob,
		render_info blob, step_payload blob,
		step_format integer NOT NULL DEFAULT 0,
		PRIMARY KEY (idx))`)

	tsEarly := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779000000},
	})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsEarly},
		{
			num:   17,
			wire:  pbWireBytes,
			bytes: []byte("user prompt text goes here"),
		},
	})
	tsLate := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779000100},
	})
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsLate},
		{
			num:   17,
			wire:  pbWireBytes,
			bytes: []byte("assistant reply content body"),
		},
	})

	mustExec(t, db,
		`INSERT INTO steps (idx, step_type, step_payload) `+
			`VALUES (?, ?, ?)`,
		0, 14, userPayload)
	mustExec(t, db,
		`INSERT INTO steps (idx, step_type, step_payload) `+
			`VALUES (?, ?, ?)`,
		1, 17, asstPayload)
}

func mustExec(
	t *testing.T, db *sql.DB, q string, args ...any,
) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// silence unused warning on time import in case the file is
// trimmed in the future.
var _ = time.Time{}
