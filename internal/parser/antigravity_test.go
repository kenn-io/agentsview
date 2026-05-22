package parser

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
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

// TestAgProtoLengthOverflow feeds a length-delimited field whose
// declared length is near uint64-max. The pre-fix code computed
// pos+ln in uint64 and wrapped, then sliced with int(ln) which
// panicked. The fix compares ln against (len(data)-pos) without
// addition.
func TestAgProtoLengthOverflow(t *testing.T) {
	// Tag for field 1, wire 2 (length-delimited).
	tag := []byte{0x0A}
	// Encode the largest uvarint (10 bytes, value 2^64-1).
	huge := make([]byte, 10)
	for i := range 9 {
		huge[i] = 0xFF
	}
	huge[9] = 0x01
	payload := append(append([]byte{}, tag...), huge...)
	payload = append(payload, []byte("only-a-few-bytes")...)

	// Must return an error rather than panicking or returning a
	// bogus slice.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("agProtoParse panicked: %v", r)
		}
	}()
	if _, err := agProtoParse(payload); err == nil {
		t.Fatalf("expected error for oversized length, got nil")
	}
}

// TestAgProtoLooksLikePrefix exercises the prefix-tolerant
// validator used by the decryption retry loop. It must accept a
// well-formed prefix followed by a truncated final field, but
// reject random bytes.
func TestAgProtoLooksLikePrefix(t *testing.T) {
	complete := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 42},
		{num: 2, wire: pbWireBytes, bytes: []byte("hello there")},
	})
	if !agProtoLooksLikePrefix(complete) {
		t.Fatalf("complete message rejected")
	}

	// Append a length-delimited field whose declared length runs
	// past the end of the buffer — agProtoParse rejects this, but
	// the prefix-tolerant check should accept since at least one
	// full field decoded cleanly first.
	truncated := append(append([]byte{}, complete...),
		// tag for field 3, wire 2; length 100; only 3 actual bytes
		0x1A, 0x64, 0x41, 0x42, 0x43,
	)
	if agProtoLooksLikePrefix(truncated) != true {
		t.Fatalf("truncated tail rejected; want accepted")
	}
	if _, err := agProtoParse(truncated); err == nil {
		t.Fatalf("agProtoParse should still reject truncated tail")
	}

	// Pure garbage with zero clean fields → reject.
	if agProtoLooksLikePrefix([]byte{0x00, 0x00, 0x00}) {
		t.Fatalf("zero-field-number garbage accepted")
	}
	if agProtoLooksLikePrefix(nil) {
		t.Fatalf("empty input accepted")
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

// ---- crypto: cipher round-trips -------------------------------

// TestDecryptAesGCMRoundTrip encrypts a payload with stdlib AES-GCM
// in the same layout decryptAesGCM expects (12-byte nonce prefix +
// ciphertext-with-tag) and confirms recovery. GCM is Antigravity's
// primary cipher per the handoff.
func TestDecryptAesGCMRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	plaintext := []byte("hello antigravity gcm world")

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("new gcm: %v", err)
	}
	nonce := bytes.Repeat([]byte{0x01}, 12)
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	data := append(append([]byte{}, nonce...), ct...)

	got := decryptAesGCM(data, key, 0)
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("decrypt: got %q want %q", got, plaintext)
	}

	// Wrong key → nil (auth tag fails).
	bad := bytes.Repeat([]byte{0x43}, 32)
	if out := decryptAesGCM(data, bad, 0); out != nil {
		t.Fatalf("wrong key should fail, got %q", out)
	}

	// Too-short input → nil, not panic.
	if out := decryptAesGCM([]byte{0x00}, key, 0); out != nil {
		t.Fatalf("short input should return nil, got %q", out)
	}
}

// TestDecryptAesGCMSkip confirms the leading-bytes skip works as
// documented (the brute-forcer tries 0/1/2/4/8 byte prefixes).
func TestDecryptAesGCMSkip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	plaintext := []byte("with leading junk bytes")

	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{0x02}, 12)
	ct := gcm.Seal(nil, nonce, plaintext, nil)

	prefix := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	data := append(append([]byte{}, prefix...), nonce...)
	data = append(data, ct...)

	got := decryptAesGCM(data, key, len(prefix))
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("decrypt with skip: got %q want %q", got, plaintext)
	}
}

func TestStripPKCS7(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{
			name: "valid one-byte pad",
			in:   []byte{0x41, 0x42, 0x43, 0x01},
			want: []byte{0x41, 0x42, 0x43},
		},
		{
			name: "valid four-byte pad",
			in: []byte{
				0x41, 0x42, 0x43, 0x44,
				0x04, 0x04, 0x04, 0x04,
			},
			want: []byte{0x41, 0x42, 0x43, 0x44},
		},
		{
			name: "empty input passes through",
			in:   []byte{},
			want: []byte{},
		},
		{
			name: "pad byte zero is invalid → unchanged",
			in:   []byte{0x41, 0x00},
			want: []byte{0x41, 0x00},
		},
		{
			name: "pad larger than block size → unchanged",
			in:   []byte{0x41, 0x42, 0xFF},
			want: []byte{0x41, 0x42, 0xFF},
		},
		{
			name: "inconsistent pad bytes → unchanged",
			in:   []byte{0x41, 0x02, 0x03},
			want: []byte{0x41, 0x02, 0x03},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripPKCS7(tc.in); !bytes.Equal(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

// ---- CLI parser: discovery edges ------------------------------

// TestAntigravityCLIDiscoverImplicit confirms .pb files under
// implicit/ are discovered alongside conversations/.
func TestAntigravityCLIDiscoverImplicit(t *testing.T) {
	root := t.TempDir()
	convID := "aaaaaaaa-1111-2222-3333-444444444444"
	implID := "bbbbbbbb-5555-6666-7777-888888888888"

	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "implicit"))
	mustWrite(t,
		filepath.Join(root, "conversations", convID+".pb"),
		[]byte("x"))
	mustWrite(t,
		filepath.Join(root, "implicit", implID+".pb"),
		[]byte("x"))

	files := DiscoverAntigravityCLISessions(root)
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2 (one per subdir)", len(files))
	}
	var sawConv, sawImpl bool
	for _, f := range files {
		switch filepath.Base(filepath.Dir(f.Path)) {
		case "conversations":
			sawConv = true
		case "implicit":
			sawImpl = true
		}
	}
	if !sawConv || !sawImpl {
		t.Fatalf("missing subdir: conv=%v impl=%v",
			sawConv, sawImpl)
	}

	// FindAntigravityCLISourceFile routes implicit-tagged ids to
	// the implicit/ subdir; bare ids resolve under conversations/.
	wantImpl := filepath.Join("implicit", implID+".pb")
	if got := FindAntigravityCLISourceFile(
		root, "implicit-"+implID,
	); got == "" || !strings.HasSuffix(got, wantImpl) {
		t.Fatalf("find implicit: %q", got)
	}
	wantConv := filepath.Join("conversations", convID+".pb")
	if got := FindAntigravityCLISourceFile(root, convID); got == "" ||
		!strings.HasSuffix(got, wantConv) {
		t.Fatalf("find conv: %q", got)
	}
	// A bare implicit-only UUID must NOT resolve under conversations/.
	if got := FindAntigravityCLISourceFile(root, implID); got != "" {
		t.Fatalf("bare implicit id should not resolve: %q", got)
	}
}

// TestAntigravityCLIImplicitSessionIDDistinct ensures a UUID that
// appears under both conversations/ and implicit/ produces two
// distinct storage IDs, so one record doesn't overwrite the other.
func TestAntigravityCLIImplicitSessionIDDistinct(t *testing.T) {
	root := t.TempDir()
	id := "cccccccc-9999-aaaa-bbbb-dddddddddddd"

	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "implicit"))
	convPath := filepath.Join(root, "conversations", id+".pb")
	implPath := filepath.Join(root, "implicit", id+".pb")
	mustWrite(t, convPath, []byte("x"))
	mustWrite(t, implPath, []byte("x"))

	convSess, _, err := ParseAntigravityCLISession(convPath, "", "m")
	if err != nil {
		t.Fatalf("parse conv: %v", err)
	}
	implSess, _, err := ParseAntigravityCLISession(implPath, "", "m")
	if err != nil {
		t.Fatalf("parse impl: %v", err)
	}
	if convSess.ID == implSess.ID {
		t.Fatalf("session ids collide: conv=%q impl=%q",
			convSess.ID, implSess.ID)
	}
	if convSess.ID != "antigravity-cli:"+id {
		t.Fatalf("conv id: %q", convSess.ID)
	}
	if implSess.ID != "antigravity-cli:implicit-"+id {
		t.Fatalf("impl id: %q", implSess.ID)
	}

	// Round-trip: each storage id resolves back to its own file.
	if got := FindAntigravityCLISourceFile(root, id); got != convPath {
		t.Fatalf("round-trip conv: %q", got)
	}
	if got := FindAntigravityCLISourceFile(
		root, "implicit-"+id,
	); got != implPath {
		t.Fatalf("round-trip impl: %q", got)
	}
}

func TestBuildAntigravityProjectMapRobust(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "history.jsonl")

	// Missing file → empty map, no error.
	if m := buildAntigravityProjectMap(path); len(m) != 0 {
		t.Fatalf("missing file: got %d entries", len(m))
	}

	// Mix of valid rows, blank lines, garbage, and rows missing
	// one of the two required fields. Only the valid rows survive.
	mustWrite(t, path, []byte(
		`{"conversationId":"id-1","workspace":"/tmp/a"}`+"\n"+
			""+"\n"+
			`not json at all`+"\n"+
			`{"conversationId":"id-2"}`+"\n"+
			`{"workspace":"/tmp/orphan"}`+"\n"+
			`{"conversationId":"id-3","workspace":"/tmp/c"}`+"\n",
	))
	m := buildAntigravityProjectMap(path)
	if len(m) != 2 {
		t.Fatalf("got %d entries, want 2: %#v", len(m), m)
	}
	if m["id-1"] != "/tmp/a" || m["id-3"] != "/tmp/c" {
		t.Fatalf("unexpected map: %#v", m)
	}
	if _, ok := m["id-2"]; ok {
		t.Fatalf("id-2 had no workspace, should be absent")
	}
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
