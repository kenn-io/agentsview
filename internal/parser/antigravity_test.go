package parser

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "parse")
	require.Len(t, fields, 3)

	// Field 17 should be a UTF-8 string with no nested decoding.
	got, _ := agProtoFind(fields, 17)
	s, ok := agProtoString(got)
	require.True(t, ok, "field 17 string ok")
	assert.Equal(t, "Hi, what's up next?", s, "field 17")

	// Field 5 should have nested fields parsed as a Timestamp.
	tsf, _ := agProtoFind(fields, 5)
	require.NotNil(t, tsf.Nested, "field 5 not parsed as nested")
	sec, nanos, ok := agProtoTimestamp(tsf.Nested)
	require.True(t, ok, "timestamp ok")
	assert.Equal(t, int64(1779326586), sec, "timestamp sec")
	assert.Equal(t, int32(12345), nanos, "timestamp nanos")

	strs := agProtoCollectStrings(fields, 5)
	require.Len(t, strs, 1)
	assert.Equal(t, "Hi, what's up next?", strs[0])
}

// TestAgProtoTimestampNanosRange pins the nanos guard: the protobuf
// Timestamp spec bounds nanos to [0, 1e9), and an out-of-range varint
// cast to int32 could go negative and shift time.Unix results outside
// the plausibility window callers check on seconds alone.
func TestAgProtoTimestampNanosRange(t *testing.T) {
	tests := []struct {
		name   string
		nanos  uint64
		wantOK bool
	}{
		{"max valid nanos", 999_999_999, true},
		{"one past the cap", 1_000_000_000, false},
		{"int32-overflowing nanos", 4_000_000_000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := encodePB([]pbField{
				{num: 1, wire: pbWireVarint, varint: 1_779_326_586},
				{num: 2, wire: pbWireVarint, varint: tt.nanos},
			})
			fields, err := agProtoParse(payload)
			require.NoError(t, err, "parse")
			sec, nanos, ok := agProtoTimestamp(fields)
			assert.Equal(t, tt.wantOK, ok, "timestamp ok")
			if tt.wantOK {
				assert.Equal(t, int64(1_779_326_586), sec, "timestamp sec")
				assert.Equal(t, int32(tt.nanos), nanos, "timestamp nanos")
			}
		})
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
	_, err := agProtoParse(payload)
	require.Error(t, err, "expected error for oversized length")
}

// TestAgProtoFieldBudget verifies the total-fields cap: a flat run
// of minimal two-byte varint fields amplifies into ~100-byte field
// structs, so without the budget an unbounded blob could allocate
// two orders of magnitude more memory than its size. Exhaustion
// truncates instead of failing, so a payload past the budget keeps
// its decoded prefix rather than losing all content.
func TestAgProtoFieldBudget(t *testing.T) {
	// One field per two bytes: tag 0x08 (field 1, varint), value 0.
	dense := func(fields int) []byte {
		return bytes.Repeat([]byte{0x08, 0x00}, fields)
	}

	within, err := agProtoParse(dense(1000))
	require.NoError(t, err)
	assert.Len(t, within, 1000)

	truncated, err := agProtoParse(dense(agProtoMaxFields + 10))
	require.NoError(t, err)
	assert.Len(t, truncated, agProtoMaxFields,
		"expected truncation at the field budget")

	// Speculative nested re-parses consume the shared budget too: a
	// small envelope around a dense payload must not bypass it. The
	// exhausted child stays opaque (no Nested) and the field after
	// it is truncated away, but the parse itself still succeeds.
	fields, err := agProtoParse(encodePB([]pbField{
		{num: 1, wire: pbWireBytes, bytes: dense(agProtoMaxFields)},
		{num: 2, wire: pbWireVarint, varint: 1},
	}))
	require.NoError(t, err)
	require.Len(t, fields, 1,
		"expected the post-exhaustion field to be truncated")
	assert.Nil(t, fields[0].Nested,
		"expected the budget-exhausted child to stay opaque")
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
	require.True(t, agProtoLooksLikePrefix(complete), "complete message rejected")

	// Append a length-delimited field whose declared length runs
	// past the end of the buffer - agProtoParse rejects this, but
	// the prefix-tolerant check should accept since at least one
	// full field decoded cleanly first.
	truncated := append(append([]byte{}, complete...),
		// tag for field 3, wire 2; length 100; only 3 actual bytes
		0x1A, 0x64, 0x41, 0x42, 0x43,
	)
	assert.True(t, agProtoLooksLikePrefix(truncated), "truncated tail rejected")
	_, err := agProtoParse(truncated)
	require.Error(t, err, "agProtoParse should still reject truncated tail")

	// Pure garbage with zero clean fields → reject.
	assert.False(t, agProtoLooksLikePrefix([]byte{0x00, 0x00, 0x00}), "zero-field-number garbage accepted")
	assert.False(t, agProtoLooksLikePrefix(nil), "empty input accepted")
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
	require.NoError(t, err, "parse")
	got := earliestAntigravityTimestamp(fields)
	assert.Equal(t, int64(1700000000), got.Unix())
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
	require.Len(t, files, 1, "discover")
	assert.Equal(t, "/tmp/proj", files[0].Project, "project")

	// Find by id should locate the same .pb.
	assert.Equal(t, files[0].Path, FindAntigravityCLISourceFile(root, id), "find")

	sess, msgs, err := ParseAntigravityCLISession(
		files[0].Path, files[0].Project, "test-machine",
	)
	require.NoError(t, err, "parse")
	assert.Equal(t, "antigravity-cli:"+id, sess.ID)
	// One user message from history + one assistant from brain.
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Contains(t, msgs[0].Content, "hello world")
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "step one")
	assert.Contains(t, msgs[1].Content, "Top task summary")
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "hello world", sess.FirstMessage)
	// StartedAt is the user message timestamp (epoch ms).
	assert.Equal(t, int64(1779000000000), sess.StartedAt.UnixMilli())
}

func TestAntigravityCLIDiscoverAndParseDB(t *testing.T) {
	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"

	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "brain", id))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath)
	mustWrite(t, filepath.Join(root, "conversations", id+".pb"),
		[]byte("old-encrypted-placeholder"))
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"db prompt fallback","timestamp":1779000000000,`+
			`"workspace":"/tmp/db-proj","conversationId":"`+id+`"}`))

	files := DiscoverAntigravityCLISessions(root)
	require.Len(t, files, 1, "discover")
	assert.Equal(t, dbPath, files[0].Path, "prefer db over pb")
	assert.Equal(t, "/tmp/db-proj", files[0].Project, "project")
	assert.Equal(t, dbPath, FindAntigravityCLISourceFile(root, id), "find")

	sess, msgs, err := ParseAntigravityCLISession(
		files[0].Path, files[0].Project, "test-machine",
	)
	require.NoError(t, err, "parse")
	assert.Equal(t, "antigravity-cli:"+id, sess.ID)
	assert.Equal(t, AgentAntigravityCLI, sess.Agent)
	assert.Equal(t, dbPath, sess.File.Path)
	assert.Equal(t, "/tmp/db-proj", sess.Project)
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "db prompt fallback", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "assistant reply content body")
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "db prompt fallback", sess.FirstMessage)
}

func TestAntigravityCLIProjectFallbackPromptAndProximity(t *testing.T) {
	root := t.TempDir()
	id := "f0f0f0f0-f1f1-f2f2-f3f3-f4f4f4f4f4f4"

	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "brain", id))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath) // user prompt: "user prompt text goes here", ts: 1779000000

	// Create history.jsonl with a row omitting conversationId, matching text, and close timestamp (1779000000000 ms)
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"  user prompt text goes here  ","timestamp":1779000010000,"workspace":"/tmp/fallback-proj"}`))

	sess, msgs, err := ParseAntigravityCLISession(dbPath, "", "m")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "/tmp/fallback-proj", sess.Project, "should successfully fallback infer project")
}

func TestAntigravityCLIProjectFallbackStrictWindow(t *testing.T) {
	root := t.TempDir()
	id := "e0e0e0e0-e1e1-e2e2-e3e3-e4e4e4e4e4e4"

	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "brain", id))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath) // user prompt: "user prompt text goes here", ts: 1779000000

	// Create history.jsonl with timestamp outside the 1-minute window (e.g., 65 seconds later)
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"user prompt text goes here","timestamp":1779000065000,"workspace":"/tmp/too-late-proj"}`))

	sess, _, err := ParseAntigravityCLISession(dbPath, "", "m")
	require.NoError(t, err)
	assert.Empty(t, sess.Project, "should reject match outside 1-minute window")
}

func TestAntigravityCLIProjectFallbackAmbiguous(t *testing.T) {
	root := t.TempDir()
	id := "d0d0d0d0-d1d1-d2d2-d3d3-d4d4d4d4d4d4"

	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "brain", id))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath) // user prompt: "user prompt text goes here", ts: 1779000000

	// Create history.jsonl with two rows having matching prompts, same timestamp difference, but different workspaces
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"user prompt text goes here","timestamp":1779000005000,"workspace":"/tmp/proj-a"}
{"display":"user prompt text goes here","timestamp":1779000005000,"workspace":"/tmp/proj-b"}`))

	sess, _, err := ParseAntigravityCLISession(dbPath, "", "m")
	require.NoError(t, err)
	assert.Empty(t, sess.Project, "should reject ambiguous match with different workspaces at same time closeness")
}

func TestAntigravityCLIProjectFallbackShortPrompt(t *testing.T) {
	root := t.TempDir()
	id := "c0c0c0c0-c1c1-c2c2-c3c3-c4c4c4c4c4c4"

	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "brain", id))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityOvershortPromptDB(t, dbPath) // user prompt: "hi", ts: 1779000000

	// Create history.jsonl with matching short prompt "hi"
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"hi","timestamp":1779000005000,"workspace":"/tmp/short-proj"}`))

	sess, _, err := ParseAntigravityCLISession(dbPath, "", "m")
	require.NoError(t, err)
	assert.Empty(t, sess.Project, "should reject matching short prompts")
}

func createAntigravityOvershortPromptDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open")
	defer db.Close()
	createAntigravityStepTables(t, db)

	tsEarly := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779000000},
	})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsEarly},
		{
			num:   17,
			wire:  pbWireBytes,
			bytes: []byte("hi"),
		},
	})
	mustExec(t, db,
		`INSERT INTO steps (idx, step_type, step_payload) `+
			`VALUES (?, ?, ?)`,
		0, 14, userPayload)
}

func TestAntigravityCLIDBFileInfoIncludesSQLiteSidecars(t *testing.T) {
	root := t.TempDir()
	id := "44444444-5555-6666-7777-888888888888"

	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")
	mustWrite(t, dbPath, []byte("db"))
	mustWrite(t, dbPath+"-wal", []byte("wal"))
	mustWrite(t, dbPath+"-shm", []byte("shm"))

	early := time.Unix(1779000000, 0)
	late := time.Unix(1779000300, 0)
	require.NoError(t, os.Chtimes(dbPath, early, early))
	require.NoError(t, os.Chtimes(dbPath+"-wal", late, late))
	require.NoError(t, os.Chtimes(dbPath+"-shm", early, early))

	info, err := AntigravityCLIFileInfo(dbPath)
	require.NoError(t, err)
	assert.Equal(t, int64(len("dbwalshm")), info.Size())
	assert.Equal(t, late.UnixNano(), info.ModTime().UnixNano())
}

// TestAntigravityCLIFileInfoIncludesBrainArtifacts pins brain
// artifacts into the CLI composite fingerprint: the parser renders
// brain/<id>/*.md (+ .metadata.json) as messages, so a brain-only
// add/edit/delete must change the effective file info or skip checks
// keep stale brain messages.
func TestAntigravityCLIFileInfoIncludesBrainArtifacts(t *testing.T) {
	early := time.Unix(1779000000, 0)
	late := time.Unix(1779000300, 0)

	t.Run("db session", func(t *testing.T) {
		root := t.TempDir()
		id := "14141414-2525-3636-4747-585858585858"
		mustMkdir(t, filepath.Join(root, "conversations"))
		dbPath := filepath.Join(root, "conversations", id+".db")
		mustWrite(t, dbPath, []byte("db"))

		brainDir := filepath.Join(root, "brain", id)
		mustMkdir(t, brainDir)
		mdPath := filepath.Join(brainDir, "task.md")
		mustWrite(t, mdPath, []byte("brain doc"))
		require.NoError(t, os.Chtimes(dbPath, early, early))
		require.NoError(t, os.Chtimes(mdPath, late, late))

		info, err := AntigravityCLIFileInfo(dbPath)
		require.NoError(t, err)
		assert.Equal(t, int64(len("db")+len("brain doc")), info.Size())
		assert.Equal(t, late.UnixNano(), info.ModTime().UnixNano())
	})

	t.Run("pb session", func(t *testing.T) {
		root := t.TempDir()
		id := "15151515-2626-3737-4848-595959595959"
		mustMkdir(t, filepath.Join(root, "implicit"))
		pbPath := filepath.Join(root, "implicit", id+".pb")
		mustWrite(t, pbPath, []byte("pb"))

		brainDir := filepath.Join(root, "brain", id)
		mustMkdir(t, brainDir)
		mdPath := filepath.Join(brainDir, "task.md")
		mustWrite(t, mdPath, []byte("brain doc"))
		require.NoError(t, os.Chtimes(pbPath, early, early))
		require.NoError(t, os.Chtimes(mdPath, late, late))

		info, err := AntigravityCLIFileInfo(pbPath)
		require.NoError(t, err)
		assert.Equal(t, int64(len("pb")+len("brain doc")), info.Size())
		assert.Equal(t, late.UnixNano(), info.ModTime().UnixNano())
	})
}

func TestAntigravityCLIDBInsertsShortHistoryPrompt(t *testing.T) {
	root := t.TempDir()
	id := "55555555-6666-7777-8888-999999999999"

	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityShortPromptDB(t, dbPath)
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"fix lint","timestamp":1779000000000,`+
			`"workspace":"/tmp/db-proj","conversationId":"`+id+`"}`))

	sess, msgs, err := ParseAntigravityCLISession(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "fix lint", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "assistant reply content body")
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "fix lint", sess.FirstMessage)
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
	assert.Empty(t, DiscoverAntigravityCLISessions(root))
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
	require.Len(t, files, 1)
	assert.Equal(t, dbPath, files[0].Path)
	assert.Equal(t, dbPath, FindAntigravitySourceFile(root, id))

	sess, msgs, _, err := ParseAntigravitySession(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err, "parse")
	assert.Equal(t, "antigravity:"+id, sess.ID)
	// 2 step rows + 1 brain artifact = 3 messages
	require.Len(t, msgs, 3)
	// step_type=14 should be flagged as user
	var sawUser, sawAssistant bool
	for _, m := range msgs {
		if m.Role == RoleUser {
			sawUser = true
			assert.Contains(t, m.Content, "user prompt text")
		}
		if m.Role == RoleAssistant &&
			strings.Contains(m.Content, "Plan summary") {
			sawAssistant = true
		}
	}
	assert.True(t, sawUser, "missing user role")
	assert.True(t, sawAssistant, "missing assistant role")
	// Annotation overrides endedAt to 2026-05-20T... =
	// 1779326586
	assert.Equal(t, int64(1779326586), sess.EndedAt.Unix())
}

func TestDecodeAntigravityStepFiltersInternalStrings(t *testing.T) {
	ts := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779000000},
	})
	payload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: ts},
		{
			num: 17, wire: pbWireBytes,
			bytes: []byte("67fdbde7-4a15-4599-a206-5ed536cf1fc4"),
		},
		{
			num: 18, wire: pbWireBytes,
			bytes: []byte("can you review the proposed issues before filing?"),
		},
		{
			num: 19, wire: pbWireBytes,
			bytes: []byte("can you review the proposed issues before filing?"),
		},
		{
			num: 20, wire: pbWireBytes,
			bytes: []byte("/home/mj/.gemini/antigravity-cli/skills"),
		},
	})

	msg, ok := decodeAntigravityStep(0, 14, payload)
	require.True(t, ok)
	assert.Equal(t, RoleUser, msg.Role)
	assert.Equal(t, "can you review the proposed issues before filing?", msg.Content)
	assert.NotContains(t, msg.Content, "[step")
	assert.NotContains(t, msg.Content, "67fdbde7")
	assert.NotContains(t, msg.Content, ".gemini")
}

func TestDecodeAntigravityStepKeepsCleanAssistantText(t *testing.T) {
	payload := encodePB([]pbField{
		{
			num: 17, wire: pbWireBytes,
			bytes: []byte("06b66779-eebe-4869-ba1b-ccf3d42be70b"),
		},
		{
			num: 18, wire: pbWireBytes,
			bytes: []byte("-3750763034362895579"),
		},
		{
			num: 19, wire: pbWireBytes,
			bytes: []byte("mYseaoyPDcS6qtsP7c6Z6QE"),
		},
		{
			num: 20, wire: pbWireBytes,
			bytes: []byte("I will start by listing the directory structure."),
		},
		{
			num: 21, wire: pbWireBytes,
			bytes: []byte(`{"DirectoryPath":"/tmp/project","toolAction":"Listing workspace files","toolSummary":"List directory contents"}`),
		},
		{
			num: 22, wire: pbWireBytes,
			bytes: []byte("MODEL_PLACEHOLDER_M20"),
		},
		{
			num: 23, wire: pbWireBytes,
			bytes: []byte("I will start by listing the directory structure."),
		},
	})

	msg, ok := decodeAntigravityStep(1, 15, payload)
	require.True(t, ok)
	assert.Equal(t, RoleAssistant, msg.Role)
	assert.Equal(t, "I will start by listing the directory structure.", msg.Content)
	assert.NotContains(t, msg.Content, "[step")
	assert.NotContains(t, msg.Content, "06b66779")
	assert.NotContains(t, msg.Content, "-3750763034362895579")
	assert.NotContains(t, msg.Content, "mYseaoyPDcS6qtsP7c6Z6QE")
	assert.NotContains(t, msg.Content, "toolAction")
	assert.NotContains(t, msg.Content, "MODEL_PLACEHOLDER")
}

// TestDecodeAntigravityStepSanitizesNUL verifies that NUL bytes in
// otherwise-valid content are replaced rather than the string (or
// the whole message) being dropped: NUL-delimited tool output such
// as `git ls-files -z` is realistic transcript content, while a NUL
// that leaks into persisted text breaks `pg push` (SQLSTATE 22021).
func TestDecodeAntigravityStepSanitizesNUL(t *testing.T) {
	payload := encodePB([]pbField{
		{
			num: 17, wire: pbWireBytes,
			bytes: []byte("file_a.go\x00file_b.go\x00file_c.go"),
		},
	})
	msg, ok := decodeAntigravityStep(0, 2, payload)
	require.True(t, ok, "NUL-bearing content must survive, not drop")
	assert.NotContains(t, msg.Content, "\x00")
	assert.Equal(t,
		"file_a.go�file_b.go�file_c.go", msg.Content)
}

func TestMergeAntigravityDBHistoryMessagesAppendsMissingPrompts(t *testing.T) {
	msgs := []ParsedMessage{
		{Role: RoleUser, Content: "first decoded prompt"},
		{Role: RoleAssistant, Content: "assistant"},
		{Role: RoleUser, Content: "second decoded prompt"},
	}
	history := []ParsedMessage{
		{Role: RoleUser, Content: "only tagged history prompt"},
	}

	got := mergeAntigravityDBHistoryMessages(msgs, history)

	require.Len(t, got, 4)
	assert.Equal(t, "first decoded prompt", got[0].Content)
	assert.Equal(t, "second decoded prompt", got[2].Content)
	assert.Equal(t, "only tagged history prompt", got[3].Content)
}

func TestMergeAntigravityDBHistoryMessagesIgnoresBlankHistoryRows(t *testing.T) {
	ts := time.Unix(1779000000, 0)
	msgs := []ParsedMessage{
		{Role: RoleUser, Content: "decoded prompt"},
		{Role: RoleAssistant, Content: "assistant"},
	}
	history := []ParsedMessage{
		{Role: RoleUser, Content: ""},
		{Role: RoleUser, Content: "history prompt", Timestamp: ts},
		{Role: RoleUser, Content: "   "},
	}

	got := mergeAntigravityDBHistoryMessages(msgs, history)

	assert.Equal(t, "history prompt", got[0].Content)
	assert.Equal(t, len("history prompt"), got[0].ContentLength)
	assert.Equal(t, ts, got[0].Timestamp)
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
	require.NoError(t, err, "new cipher")
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err, "new gcm")
	nonce := bytes.Repeat([]byte{0x01}, 12)
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	data := append(append([]byte{}, nonce...), ct...)

	got := decryptAesGCM(data, key, 0)
	assert.True(t, bytes.Equal(got, plaintext), "decrypt: got %q want %q", got, plaintext)

	// Wrong key → nil (auth tag fails).
	bad := bytes.Repeat([]byte{0x43}, 32)
	assert.Nil(t, decryptAesGCM(data, bad, 0), "wrong key should fail")

	// Too-short input → nil, not panic.
	assert.Nil(t, decryptAesGCM([]byte{0x00}, key, 0), "short input should return nil")
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
	assert.True(t, bytes.Equal(got, plaintext), "decrypt with skip: got %q want %q", got, plaintext)
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
			assert.Equal(t, tc.want, stripPKCS7(tc.in))
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
	require.Len(t, files, 2, "got files, want 2 (one per subdir)")
	var sawConv, sawImpl bool
	for _, f := range files {
		switch filepath.Base(filepath.Dir(f.Path)) {
		case "conversations":
			sawConv = true
		case "implicit":
			sawImpl = true
		}
	}
	assert.True(t, sawConv, "missing conv subdir")
	assert.True(t, sawImpl, "missing impl subdir")

	// FindAntigravityCLISourceFile routes implicit-tagged ids to
	// the implicit/ subdir; bare ids resolve under conversations/.
	wantImpl := filepath.Join("implicit", implID+".pb")
	gotImpl := FindAntigravityCLISourceFile(root, "implicit-"+implID)
	require.NotEmpty(t, gotImpl)
	assert.True(t, strings.HasSuffix(gotImpl, wantImpl), "find implicit: %q", gotImpl)
	wantConv := filepath.Join("conversations", convID+".pb")
	gotConv := FindAntigravityCLISourceFile(root, convID)
	require.NotEmpty(t, gotConv)
	assert.True(t, strings.HasSuffix(gotConv, wantConv), "find conv: %q", gotConv)
	// A bare implicit-only UUID must NOT resolve under conversations/.
	assert.Empty(t, FindAntigravityCLISourceFile(root, implID),
		"bare implicit id should not resolve")
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
	require.NoError(t, err, "parse conv")
	implSess, _, err := ParseAntigravityCLISession(implPath, "", "m")
	require.NoError(t, err, "parse impl")
	assert.NotEqual(t, implSess.ID, convSess.ID, "session ids collide")
	assert.Equal(t, "antigravity-cli:"+id, convSess.ID, "conv id")
	assert.Equal(t, "antigravity-cli:implicit-"+id, implSess.ID, "impl id")

	// Round-trip: each storage id resolves back to its own file.
	assert.Equal(t, convPath, FindAntigravityCLISourceFile(root, id), "round-trip conv")
	assert.Equal(t, implPath, FindAntigravityCLISourceFile(root, "implicit-"+id), "round-trip impl")
}

func TestBuildAntigravityProjectMapRobust(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "history.jsonl")

	// Missing file → empty map, no error.
	assert.Empty(t, buildAntigravityProjectMap(path), "missing file")

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
	require.Len(t, m, 2, "map entries")
	assert.Equal(t, "/tmp/a", m["id-1"])
	assert.Equal(t, "/tmp/c", m["id-3"])
	_, ok := m["id-2"]
	assert.False(t, ok, "id-2 had no workspace, should be absent")
}

// ---- helpers --------------------------------------------------

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(p, 0o755), "mkdir %s", p)
}

func mustWrite(t *testing.T, p string, b []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(p, b, 0o644), "write %s", p)
}

// createAntigravityTestDB writes a minimal antigravity IDE
// SQLite database with two synthetic steps: a user prompt
// (step_type=14) and an assistant step (step_type=17).
func createAntigravityTestDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open")
	defer db.Close()
	createAntigravityStepTables(t, db)

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

func createAntigravityShortPromptDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open")
	defer db.Close()
	createAntigravityStepTables(t, db)

	tsEarly := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779000000},
	})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsEarly},
		{
			num:   17,
			wire:  pbWireBytes,
			bytes: []byte("fix lint"),
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

func createAntigravityStepTables(t *testing.T, db *sql.DB) {
	t.Helper()
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
}

func mustExec(
	t *testing.T, db *sql.DB, q string, args ...any,
) {
	t.Helper()
	_, err := db.Exec(q, args...)
	require.NoError(t, err, "exec %q", q)
}

// createAntigravityToolCallDB creates a .db with two steps:
//   - step_type=14 (user): plain text prompt
//   - step_type=17 (assistant): payload containing the string "view_file"
//     embedded as a protobuf string field, simulating a planner response
//     that invoked the view_file tool.
func createAntigravityToolCallDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open")
	defer db.Close()
	createAntigravityStepTables(t, db)

	tsEarly := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779000000},
	})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsEarly},
		{num: 17, wire: pbWireBytes, bytes: []byte("please check the file")},
	})

	tsLate := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779000100},
	})
	// Embed "view_file" as a string field inside a nested message
	// that also contains a UUID-like tool-use ID. This mirrors the
	// protobuf shape observed in real Antigravity .db step payloads.
	toolCallNested := encodePB([]pbField{
		{num: 3, wire: pbWireBytes, bytes: []byte("view_file")},
		{num: 4, wire: pbWireBytes, bytes: []byte("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")},
	})
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsLate},
		{num: 17, wire: pbWireBytes, bytes: []byte("I will read the file for you")},
		{num: 8, wire: pbWireBytes, bytes: toolCallNested},
	})

	mustExec(t, db,
		`INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		0, 14, userPayload)
	mustExec(t, db,
		`INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		1, 17, asstPayload)
}

// TestDecodeAntigravityStepToolCall is the tracer bullet: parsing a .db
// session whose assistant step contains a known tool name ("view_file")
// produces a message with HasToolUse=true and one ToolCall entry.
func TestDecodeAntigravityStepToolCall(t *testing.T) {
	root := t.TempDir()
	id := "aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityToolCallDB(t, dbPath)

	sess, msgs, _, err := ParseAntigravitySession(dbPath, "/tmp/proj", "m")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Find the assistant message.
	var asstMsg *ParsedMessage
	for i := range msgs {
		if msgs[i].Role == RoleAssistant {
			asstMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, asstMsg, "expected an assistant message")

	assert.True(t, asstMsg.HasToolUse, "assistant message should have HasToolUse=true")
	require.Len(t, asstMsg.ToolCalls, 1, "expected one tool call")
	assert.Equal(t, "view_file", asstMsg.ToolCalls[0].ToolName)
	assert.Equal(t, "Read", asstMsg.ToolCalls[0].Category)
}

// silence unused warning on time import in case the file is
// trimmed in the future.
var _ = time.Time{}

// TestDecodeAntigravityStepUserNoToolCalls asserts that a user step
// (step_type=14) whose payload happens to contain a known tool name string
// does NOT produce any ToolCall entries - tool calls only belong on assistant
// messages.
func TestDecodeAntigravityStepUserNoToolCalls(t *testing.T) {
	root := t.TempDir()
	id := "bbbb2222-cccc-dddd-eeee-ffffffffffff"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")

	// Build a .db where the USER step payload contains "view_file".
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()
	createAntigravityStepTables(t, db)

	ts := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000000}})
	// User step (step_type=14) whose payload includes a tool name string.
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: ts},
		{num: 17, wire: pbWireBytes, bytes: []byte("please use view_file on that path")},
	})
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		0, 14, userPayload)

	sess, msgs, _, err := ParseAntigravitySession(dbPath, "/tmp/proj", "m")
	require.NoError(t, err)
	require.NotNil(t, sess)

	require.Len(t, msgs, 1)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.False(t, msgs[0].HasToolUse, "user step should not have HasToolUse=true")
	assert.Empty(t, msgs[0].ToolCalls, "user step should have no ToolCalls")
}

// TestDecodeAntigravityStepNoFalsePositives asserts that an assistant step
// whose payload contains only plain prose (no known tool name strings)
// produces no ToolCall entries and HasToolUse=false.
func TestDecodeAntigravityStepNoFalsePositives(t *testing.T) {
	root := t.TempDir()
	id := "cccc3333-dddd-eeee-ffff-000000000000"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()
	createAntigravityStepTables(t, db)

	ts := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000000}})
	// Assistant step with no known tool names - just prose.
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: ts},
		{num: 17, wire: pbWireBytes, bytes: []byte("Here is my analysis of the codebase and recommendations for improvement.")},
	})
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		0, 17, asstPayload)

	sess, msgs, _, err := ParseAntigravitySession(dbPath, "/tmp/proj", "m")
	require.NoError(t, err)
	require.NotNil(t, sess)

	require.Len(t, msgs, 1)
	assert.Equal(t, RoleAssistant, msgs[0].Role)
	assert.False(t, msgs[0].HasToolUse, "plain-prose step should not have HasToolUse=true")
	assert.Empty(t, msgs[0].ToolCalls, "plain-prose step should have no ToolCalls")
}

// TestDecodeAntigravityStepMultipleToolCalls asserts that an assistant step
// whose payload contains two distinct known tool names produces two ToolCall
// entries with the correct names and categories.
func TestDecodeAntigravityStepMultipleToolCalls(t *testing.T) {
	root := t.TempDir()
	id := "dddd4444-eeee-ffff-0000-111111111111"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()
	createAntigravityStepTables(t, db)

	ts := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000000}})
	// Two tool call nested messages in the same assistant step.
	tc1 := encodePB([]pbField{
		{num: 3, wire: pbWireBytes, bytes: []byte("view_file")},
		{num: 4, wire: pbWireBytes, bytes: []byte("11111111-2222-3333-4444-555555555555")},
	})
	tc2 := encodePB([]pbField{
		{num: 3, wire: pbWireBytes, bytes: []byte("run_command")},
		{num: 4, wire: pbWireBytes, bytes: []byte("66666666-7777-8888-9999-aaaaaaaaaaaa")},
	})
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: ts},
		{num: 17, wire: pbWireBytes, bytes: []byte("reading file and running command")},
		{num: 8, wire: pbWireBytes, bytes: tc1},
		{num: 9, wire: pbWireBytes, bytes: tc2},
	})
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		0, 17, asstPayload)

	sess, msgs, _, err := ParseAntigravitySession(dbPath, "/tmp/proj", "m")
	require.NoError(t, err)
	require.NotNil(t, sess)

	require.Len(t, msgs, 1)
	assert.True(t, msgs[0].HasToolUse)
	require.Len(t, msgs[0].ToolCalls, 2, "expected two tool calls")

	toolNames := []string{msgs[0].ToolCalls[0].ToolName, msgs[0].ToolCalls[1].ToolName}
	assert.ElementsMatch(t, []string{"view_file", "run_command"}, toolNames)

	for _, tc := range msgs[0].ToolCalls {
		switch tc.ToolName {
		case "view_file":
			assert.Equal(t, "Read", tc.Category)
		case "run_command":
			assert.Equal(t, "Bash", tc.Category)
		}
	}
}

func TestAntigravityCLITrajectoryParse(t *testing.T) {
	root := t.TempDir()
	id := "22222222-3333-4444-5555-666666666666"

	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "implicit"))

	// Create stub .pb file
	pbPath := filepath.Join(root, "conversations", id+".pb")
	mustWrite(t, pbPath, []byte("pb-stub"))

	// Create trajectory JSON sidecar
	trajectoryJSON := `{
		"trajectoryId": "traj-id",
		"cascadeId": "` + id + `",
		"steps": [
			{
				"type": "CORTEX_STEP_TYPE_USER_INPUT",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:40:00Z"
				},
				"userInput": {
					"userResponse": "check files please"
				}
			},
			{
				"type": "CORTEX_STEP_TYPE_PLANNER_RESPONSE",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:41:00Z"
				},
				"plannerResponse": {
					"thinking": "I should run a command",
					"response": "running command now",
					"toolCalls": [
						{
							"name": "run_command",
							"argumentsJson": "{\"command\":\"ls -la\"}",
							"id": "tc-1"
						}
					]
				}
			},
			{
				"type": "CORTEX_STEP_TYPE_RUN_COMMAND",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:42:00Z",
					"executionId": "tc-1"
				},
				"runCommand": {
					"commandLine": "ls -la",
					"cwd": "/tmp",
					"combinedOutput": "\"file1.txt\nfile2.txt\""
				}
			},
			{
				"type": "CORTEX_STEP_TYPE_SYSTEM_MESSAGE",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:43:00Z"
				},
				"systemMessage": {
					"message": "system warning: low memory"
				}
			},
			{
				"type": "CORTEX_STEP_TYPE_CHECKPOINT",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:44:00Z"
				},
				"checkpoint": {
					"userRequests": ["request1"],
					"sessionSummary": "everything is fine"
				}
			}
		]
	}`
	sidecarPath := filepath.Join(root, "conversations", id+".trajectory.json")
	mustWrite(t, sidecarPath, []byte(trajectoryJSON))

	sess, msgs, err := ParseAntigravityCLISession(pbPath, "", "test-machine")
	require.NoError(t, err)

	assert.Equal(t, "antigravity-cli:"+id, sess.ID)
	assert.Equal(t, "check files please", sess.FirstMessage)

	// Expected messages:
	// 1. User: check files please
	// 2. Assistant: running command now (with tool call)
	// 3. User: synthetic message with tool results
	// 4. User (IsSystem): Low memory warning
	// 5. User (IsSystem): Checkpoint info
	require.Len(t, msgs, 5)

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "check files please", msgs[0].Content)

	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "running command now", msgs[1].Content)
	assert.True(t, msgs[1].HasThinking)
	assert.Equal(t, "I should run a command", msgs[1].ThinkingText)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "tc-1", msgs[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "run_command", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Bash", msgs[1].ToolCalls[0].Category)

	assert.Equal(t, RoleUser, msgs[2].Role)
	assert.Equal(t, "", msgs[2].Content)
	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "tc-1", msgs[2].ToolResults[0].ToolUseID)
	assert.Contains(t, msgs[2].ToolResults[0].ContentRaw, "file1.txt")

	assert.Equal(t, RoleUser, msgs[3].Role)
	assert.True(t, msgs[3].IsSystem)
	assert.Equal(t, "system warning: low memory", msgs[3].Content)

	assert.Equal(t, RoleUser, msgs[4].Role)
	assert.True(t, msgs[4].IsSystem)
	assert.Contains(t, msgs[4].Content, "everything is fine")

	// Verify FileInfo size and mtime are effective (sum of sizes, max of mtimes)
	pbStat, _ := os.Stat(pbPath)
	sidecarStat, _ := os.Stat(sidecarPath)
	expectedSize := pbStat.Size() + sidecarStat.Size()
	assert.Equal(t, expectedSize, sess.File.Size)
}

func TestAntigravityCLITrajectoryWithoutSupportedMessagesFallsBack(t *testing.T) {
	tcs := []struct {
		name    string
		sidecar string
	}{
		{
			name:    "empty object",
			sidecar: `{}`,
		},
		{
			name: "unknown step only",
			sidecar: `{
				"steps": [
					{
						"type": "CORTEX_STEP_TYPE_FUTURE_ONLY",
						"metadata": {
							"createdAt": "2026-05-20T22:40:00Z"
						},
						"futurePayload": {
							"text": "not supported yet"
						}
					}
				]
			}`,
		},
		{
			name: "tool result only",
			sidecar: `{
				"steps": [
					{
						"type": "CORTEX_STEP_TYPE_RUN_COMMAND",
						"metadata": {
							"createdAt": "2026-05-20T22:40:00Z",
							"executionId": "tc-1"
						},
						"runCommand": {
							"commandLine": "ls",
							"combinedOutput": "\"file1.txt\""
						}
					}
				]
			}`,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			id := "33333333-4444-5555-6666-777777777777"

			mustMkdir(t, filepath.Join(root, "conversations"))

			pbPath := filepath.Join(root, "conversations", id+".pb")
			mustWrite(t, pbPath, []byte("pb-stub"))
			mustWrite(t, filepath.Join(root, "conversations", id+".trajectory.json"), []byte(tc.sidecar))
			mustWrite(t, filepath.Join(root, "history.jsonl"),
				[]byte(`{"display":"history fallback","timestamp":1779000000000,`+
					`"workspace":"/tmp/proj","conversationId":"`+id+`"}`))

			sess, msgs, err := ParseAntigravityCLISession(pbPath, "", "test-machine")
			require.NoError(t, err)

			require.Len(t, msgs, 1)
			assert.Equal(t, RoleUser, msgs[0].Role)
			assert.Equal(t, "history fallback", msgs[0].Content)
			assert.Equal(t, 1, sess.MessageCount)
			assert.Equal(t, "history fallback", sess.FirstMessage)
		})
	}
}

// writeAntigravityTestSidecar writes a trajectory sidecar with the first
// numSteps of a fixed user-input / planner-response / run-command sequence.
func writeAntigravityTestSidecar(
	t *testing.T, root, id string, numSteps int,
) string {
	t.Helper()
	return writeAntigravityTestSidecarWithGenMetadata(
		t, root, id, numSteps, "",
	)
}

// writeAntigravityTestSidecarWithGenMetadata writes the same fixed
// step sequence as writeAntigravityTestSidecar plus an optional
// generatorMetadata JSON array (steps: 0=USER_INPUT,
// 1=PLANNER_RESPONSE, 2=RUN_COMMAND; realistic generations use
// stepIndices [1,2]).
func writeAntigravityTestSidecarWithGenMetadata(
	t *testing.T, root, id string, numSteps int, genMetadataJSON string,
) string {
	t.Helper()
	allSteps := []string{
		`{
			"type": "CORTEX_STEP_TYPE_USER_INPUT",
			"status": "STATUS_COMPLETED",
			"metadata": {"createdAt": "2026-06-10T20:40:00Z"},
			"userInput": {"userResponse": "sidecar prompt"}
		}`,
		`{
			"type": "CORTEX_STEP_TYPE_PLANNER_RESPONSE",
			"status": "STATUS_COMPLETED",
			"metadata": {"createdAt": "2026-06-10T20:41:00Z"},
			"plannerResponse": {
				"thinking": "sidecar thinking",
				"response": "sidecar assistant reply",
				"toolCalls": [{
					"name": "run_command",
					"argumentsJson": "{\"command\":\"ls\"}",
					"id": "tc-1"
				}]
			}
		}`,
		`{
			"type": "CORTEX_STEP_TYPE_RUN_COMMAND",
			"status": "STATUS_COMPLETED",
			"metadata": {"createdAt": "2026-06-10T20:42:00Z", "executionId": "tc-1"},
			"runCommand": {
				"commandLine": "ls",
				"cwd": "/tmp",
				"combinedOutput": "\"out.txt\""
			}
		}`,
	}
	require.LessOrEqual(t, numSteps, len(allSteps))
	body := `{"trajectoryId":"traj","cascadeId":"` + id + `","steps":[` +
		strings.Join(allSteps[:numSteps], ",") + `]`
	if genMetadataJSON != "" {
		body += `,"generatorMetadata":` + genMetadataJSON
	}
	body += `}`
	p := filepath.Join(root, "conversations", id+".trajectory.json")
	mustWrite(t, p, []byte(body))
	return p
}

func TestAntigravityCLIDBPrefersSidecarWithEqualCoverage(t *testing.T) {
	root := t.TempDir()
	id := "66666666-7777-8888-9999-aaaaaaaaaaaa"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath) // 2 raw steps
	writeAntigravityTestSidecar(t, root, id, 2)
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"history prompt","timestamp":1779000000000,`+
			`"workspace":"/tmp/db-proj","conversationId":"`+id+`"}`))

	sess, msgs, _, status, err := ParseAntigravityCLISessionWithStatus(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.False(t, status.NeedsRetry)

	// Sidecar messages win: structured tool call, thinking, and the
	// sidecar's own user prompt (no history.jsonl merge).
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "sidecar prompt", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "sidecar assistant reply", msgs[1].Content)
	assert.True(t, msgs[1].HasThinking)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "tc-1", msgs[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "sidecar prompt", sess.FirstMessage)
}

func TestAntigravityCLIDBKeepsDBDecodeWhenSidecarLags(t *testing.T) {
	root := t.TempDir()
	id := "77777777-8888-9999-aaaa-bbbbbbbbbbbb"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath) // 2 raw steps
	// Sidecar covers only 1 of 2 steps -- a live session agy-reader has
	// not caught up with yet. The fuller DB decode must win.
	writeAntigravityTestSidecar(t, root, id, 1)
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"history prompt","timestamp":1779000000000,`+
			`"workspace":"/tmp/db-proj","conversationId":"`+id+`"}`))

	_, msgs, _, status, err := ParseAntigravityCLISessionWithStatus(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.False(t, status.NeedsRetry)
	require.Len(t, msgs, 2)
	assert.Equal(t, "history prompt", msgs[0].Content, "history merge applies")
	assert.Contains(t, msgs[1].Content, "assistant reply content body")
}

func TestAntigravityCLIDBSidecarUsedWhenDBDecodeEmpty(t *testing.T) {
	root := t.TempDir()
	id := "88888888-9999-aaaa-bbbb-cccccccccccc"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	createAntigravityStepTables(t, db)
	require.NoError(t, db.Close())
	writeAntigravityTestSidecar(t, root, id, 2)

	_, msgs, _, status, err := ParseAntigravityCLISessionWithStatus(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.False(t, status.NeedsRetry,
		"sidecar provided full-resolution data; no retry needed")
	require.Len(t, msgs, 2)
	assert.Equal(t, "sidecar prompt", msgs[0].Content)
	require.Len(t, msgs[1].ToolCalls, 1)
}

func TestAntigravityCLIDBFileInfoIncludesTrajectorySidecar(t *testing.T) {
	root := t.TempDir()
	id := "99999999-aaaa-bbbb-cccc-dddddddddddd"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	mustWrite(t, dbPath, []byte("db"))
	sidecarPath := filepath.Join(
		root, "conversations", id+".trajectory.json",
	)
	mustWrite(t, sidecarPath, []byte("sidecar"))

	early := time.Unix(1779000000, 0)
	late := time.Unix(1779000300, 0)
	require.NoError(t, os.Chtimes(dbPath, early, early))
	require.NoError(t, os.Chtimes(sidecarPath, late, late))

	info, err := AntigravityCLIFileInfo(dbPath)
	require.NoError(t, err)
	assert.Equal(t, int64(len("db")+len("sidecar")), info.Size())
	assert.Equal(t, late.UnixNano(), info.ModTime().UnixNano(),
		"agy-reader sidecar update must change the .db fingerprint")
}

func TestAntigravityCLIPBUsesSidecarDespiteOlderMtime(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	mustMkdir(t, filepath.Join(root, "conversations"))

	pbPath := filepath.Join(root, "conversations", id+".pb")
	mustWrite(t, pbPath, []byte("pb-stub"))
	sidecarPath := writeAntigravityTestSidecar(t, root, id, 2)

	// Sidecar predates the .pb -- e.g. the encrypted file was touched
	// after the final agy-reader sync. The old mtime gate rejected the
	// sidecar here and fell back to low-fidelity history rows; the
	// sidecar must win regardless because .pb has no richer decode.
	early := time.Unix(1779000000, 0)
	late := time.Unix(1779000300, 0)
	require.NoError(t, os.Chtimes(sidecarPath, early, early))
	require.NoError(t, os.Chtimes(pbPath, late, late))
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"history prompt","timestamp":1779000000000,`+
			`"workspace":"/tmp/pb-proj","conversationId":"`+id+`"}`))

	sess, msgs, err := ParseAntigravityCLISession(pbPath, "", "test-machine")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "sidecar prompt", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "sidecar prompt", sess.FirstMessage)
}

// createAntigravityUndecodableDB writes a .db whose steps rows carry
// payloads the heuristic decoder cannot turn into displayable messages.
func createAntigravityUndecodableDB(t *testing.T, path string, rows int) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	createAntigravityStepTables(t, db)
	for i := range rows {
		mustExec(t, db,
			`INSERT INTO steps (idx, step_type, step_payload) `+
				`VALUES (?, ?, ?)`,
			i, 99, []byte{0xff, 0xff, 0xff})
	}
}

func TestAntigravityCLIDBPartialSidecarNotPersistedAsCurrent(t *testing.T) {
	root := t.TempDir()
	id := "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityUndecodableDB(t, dbPath, 3)
	// Sidecar lags the DB (2 of 3 steps): best available transcript, but
	// the row must stay retryable rather than persist as current.
	writeAntigravityTestSidecar(t, root, id, 2)

	_, msgs, _, status, err := ParseAntigravityCLISessionWithStatus(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.True(t, status.NeedsRetry,
		"partial sidecar with undecodable DB rows must leave the row stale")
	require.Len(t, msgs, 2)
	assert.Equal(t, "sidecar prompt", msgs[0].Content)
}

func TestAntigravityCLIDBCoveringSidecarRescuesUndecodableRows(t *testing.T) {
	root := t.TempDir()
	id := "cccccccc-dddd-eeee-ffff-000000000000"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityUndecodableDB(t, dbPath, 3)
	writeAntigravityTestSidecar(t, root, id, 3)

	_, msgs, _, status, err := ParseAntigravityCLISessionWithStatus(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.False(t, status.NeedsRetry,
		"covering sidecar is full-resolution data; no retry needed")
	require.Len(t, msgs, 3)
	require.Len(t, msgs[1].ToolCalls, 1)
}

func TestAntigravityCLISidecarWinsKeepsTokenUsage(t *testing.T) {
	root := t.TempDir()
	id := "dddddddd-eeee-ffff-0000-111111111111"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	createAntigravityStepTables(t, db)
	mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)

	tsEarly := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000000}})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsEarly},
		{num: 17, wire: pbWireBytes, bytes: []byte("user question text goes here and is long")},
	})
	tsLate := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000100}})
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsLate},
		{num: 17, wire: pbWireBytes, bytes: []byte("assistant response body goes here and is long")},
	})
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, ?)`, userPayload)
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 17, ?)`, asstPayload)

	genData := createAntigravityMockGenMetadata(t, 2400, 180, 0, "Test Gemini 3.5")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (1, ?, ?)`, genData, len(genData))
	require.NoError(t, db.Close())

	// Sidecar covers both raw steps, so its transcript wins over the
	// DB decode. This sidecar has no generatorMetadata, so the selected
	// messages carry no token fields and usage flows only from the DB
	// gen_metadata events.
	writeAntigravityTestSidecar(t, root, id, 2)

	sess, msgs, usageEvents, status, err := ParseAntigravityCLISessionWithStatus(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.False(t, status.NeedsRetry)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "sidecar prompt", msgs[0].Content, "sidecar transcript should win")

	require.Len(t, usageEvents, 1)
	assert.Equal(t, 2400, usageEvents[0].InputTokens)
	assert.Equal(t, 180, usageEvents[0].OutputTokens)

	// Session totals must come from gen_metadata usage even though
	// the sidecar transcript has no per-message token metadata.
	assert.Equal(t, 180, sess.TotalOutputTokens)
	assert.Equal(t, 2400, sess.PeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
}

func TestAntigravityCLISidecarRescueKeepsGenMetadataUsage(t *testing.T) {
	root := t.TempDir()
	id := "eeeeeeee-ffff-0000-1111-222222222222"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	createAntigravityStepTables(t, db)
	mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)
	// Two raw steps the heuristic cannot decode.
	for i := range 2 {
		mustExec(t, db,
			`INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
			i, 99, []byte{0xff, 0xff, 0xff})
	}
	genData := createAntigravityMockGenMetadata(t, 2400, 180, 0, "Test Gemini 3.5")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (1, ?, ?)`, genData, len(genData))
	require.NoError(t, db.Close())

	// Covering sidecar rescues the undecodable rows.
	writeAntigravityTestSidecar(t, root, id, 2)

	sess, msgs, usageEvents, status, err := ParseAntigravityCLISessionWithStatus(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.False(t, status.NeedsRetry, "covering sidecar is full-resolution data")
	require.NotEmpty(t, msgs)
	assert.Equal(t, "sidecar prompt", msgs[0].Content, "sidecar transcript should win")

	// gen_metadata usage must survive even though no DB step decoded.
	require.Len(t, usageEvents, 1)
	assert.Equal(t, "Test Gemini 3.5", usageEvents[0].Model)
	assert.Equal(t, 2400, usageEvents[0].InputTokens)
	assert.Equal(t, 180, usageEvents[0].OutputTokens)
	assert.Equal(t, 0, usageEvents[0].ReasoningTokens, "reasoning not available in gen_metadata")
	assert.Equal(t, 180, sess.TotalOutputTokens)
	assert.Equal(t, 2400, sess.PeakContextTokens)
}

func TestAntigravityCLIPBSidecarEmitsUsageEvents(t *testing.T) {
	root := t.TempDir()
	id := "ffffffff-0000-1111-2222-333333333333"
	mustMkdir(t, filepath.Join(root, "conversations"))

	pbPath := filepath.Join(root, "conversations", id+".pb")
	mustWrite(t, pbPath, []byte("pb-junk-bytes"))

	// Gen A claims the planner step (1) and its tool step (2). Gen B
	// only claims a non-planner step. Gen C is the failed/retried
	// usage:{} case and must be skipped.
	genJSON := `[
		{
			"stepIndices": [1, 2],
			"chatModel": {
				"model": "MODEL_PLACEHOLDER_M20",
				"chatStartMetadata": {"createdAt": "2026-06-10T20:40:30Z"},
				"usage": {
					"inputTokens": "19733",
					"outputTokens": "253",
					"thinkingOutputTokens": "208"
				}
			}
		},
		{
			"stepIndices": [2],
			"chatModel": {
				"model": "MODEL_PLACEHOLDER_M20",
				"usage": {
					"inputTokens": "1820",
					"outputTokens": "97",
					"cacheReadTokens": "18661"
				}
			}
		},
		{
			"stepIndices": [2],
			"chatModel": {
				"model": "MODEL_PLACEHOLDER_M20",
				"usage": {}
			}
		}
	]`
	writeAntigravityTestSidecarWithGenMetadata(t, root, id, 3, genJSON)

	sess, msgs, usageEvents, status, err := ParseAntigravityCLISessionWithStatus(
		pbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.False(t, status.NeedsRetry)

	require.Len(t, usageEvents, 2, "usage:{} generation must be skipped")

	evA := usageEvents[0]
	assert.Equal(t, "sidecar", evA.Source)
	assert.Equal(t, "MODEL_PLACEHOLDER_M20", evA.Model,
		"placeholder model names are kept verbatim")
	assert.Equal(t, 19733, evA.InputTokens)
	assert.Equal(t, 253, evA.OutputTokens,
		"outputTokens already includes thinking; no re-fold")
	assert.Equal(t, 208, evA.ReasoningTokens)
	assert.Equal(t, 0, evA.CacheReadInputTokens)
	assert.Equal(t, "2026-06-10T20:40:30Z", evA.OccurredAt,
		"chatStartMetadata.createdAt formatted RFC3339Nano")
	assert.Empty(t, evA.DedupKey)
	assert.Nil(t, evA.MessageOrdinal)
	assert.Equal(t, sess.ID, evA.SessionID)

	evB := usageEvents[1]
	assert.Equal(t, "sidecar", evB.Source)
	assert.Equal(t, 1820, evB.InputTokens)
	assert.Equal(t, 97, evB.OutputTokens)
	assert.Equal(t, 0, evB.ReasoningTokens)
	assert.Equal(t, 18661, evB.CacheReadInputTokens)
	assert.Empty(t, evB.OccurredAt,
		"no chatStartMetadata and no mapped planner step")
	assert.Equal(t, sess.ID, evB.SessionID)

	// Gen A attributes its tokens to the planner message.
	require.Len(t, msgs, 3)
	planner := msgs[1]
	assert.Equal(t, RoleAssistant, planner.Role)
	assert.Equal(t, "MODEL_PLACEHOLDER_M20", planner.Model)
	assert.Equal(t, 19733, planner.ContextTokens,
		"context = input + cacheRead of the first attributing gen")
	assert.Equal(t, 253, planner.OutputTokens)
	assert.True(t, planner.HasContextTokens)
	assert.True(t, planner.HasOutputTokens)
	assert.Empty(t, planner.TokenUsage,
		"raw token_usage must stay empty or analytics double-count")

	// Session totals come from the events: output sums, peak context
	// is max(input + cacheRead).
	assert.Equal(t, 253+97, sess.TotalOutputTokens)
	assert.Equal(t, 1820+18661, sess.PeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
}

func TestAntigravityCLIDBGenMetadataWinsOverSidecarUsage(t *testing.T) {
	root := t.TempDir()
	id := "abababab-cdcd-efef-0101-232323232323"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	createAntigravityStepTables(t, db)
	mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)

	tsEarly := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000000}})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsEarly},
		{num: 17, wire: pbWireBytes, bytes: []byte("user question text goes here and is long")},
	})
	tsLate := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000100}})
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsLate},
		{num: 17, wire: pbWireBytes, bytes: []byte("assistant response body goes here and is long")},
	})
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, ?)`, userPayload)
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 17, ?)`, asstPayload)

	genData := createAntigravityMockGenMetadata(t, 2400, 180, 0, "Test Gemini 3.5")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (1, ?, ?)`, genData, len(genData))
	require.NoError(t, db.Close())

	// Covering sidecar whose generatorMetadata carries DIFFERENT
	// numbers than the DB gen_metadata: the DB events must win so the
	// generation is not double counted, while the sidecar transcript
	// still carries per-message attribution from the sidecar numbers.
	genJSON := `[{
		"stepIndices": [1],
		"chatModel": {
			"model": "MODEL_PLACEHOLDER_M132",
			"usage": {
				"inputTokens": "5000",
				"outputTokens": "111",
				"thinkingOutputTokens": "60"
			}
		}
	}]`
	writeAntigravityTestSidecarWithGenMetadata(t, root, id, 2, genJSON)

	sess, msgs, usageEvents, status, err := ParseAntigravityCLISessionWithStatus(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.False(t, status.NeedsRetry)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "sidecar prompt", msgs[0].Content, "sidecar transcript should win")

	// Exactly one event: gen_metadata wins, sidecar events fill gaps only.
	require.Len(t, usageEvents, 1)
	assert.Equal(t, "generation", usageEvents[0].Source)
	assert.Equal(t, 2400, usageEvents[0].InputTokens)
	assert.Equal(t, 180, usageEvents[0].OutputTokens)

	// Session totals follow the winning gen_metadata events.
	assert.Equal(t, 180, sess.TotalOutputTokens)
	assert.Equal(t, 2400, sess.PeakContextTokens)

	// Per-message attribution comes from the sidecar generatorMetadata.
	require.Len(t, msgs, 2)
	planner := msgs[1]
	assert.Equal(t, RoleAssistant, planner.Role)
	assert.Equal(t, "MODEL_PLACEHOLDER_M132", planner.Model)
	assert.Equal(t, 5000, planner.ContextTokens)
	assert.Equal(t, 111, planner.OutputTokens)
	assert.True(t, planner.HasContextTokens)
	assert.True(t, planner.HasOutputTokens)
}

func TestAntigravityCLIDBWithoutGenMetadataGetsSidecarUsage(t *testing.T) {
	root := t.TempDir()
	id := "acacacac-bdbd-cece-dfdf-454545454545"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath) // 2 raw steps, no gen_metadata table

	genJSON := `[{
		"stepIndices": [1],
		"chatModel": {
			"model": "MODEL_PLACEHOLDER_M20",
			"usage": {
				"inputTokens": "1500",
				"outputTokens": "77",
				"thinkingOutputTokens": "20",
				"cacheReadTokens": "300"
			}
		}
	}]`
	writeAntigravityTestSidecarWithGenMetadata(t, root, id, 2, genJSON)

	sess, msgs, usageEvents, status, err := ParseAntigravityCLISessionWithStatus(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.False(t, status.NeedsRetry)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "sidecar prompt", msgs[0].Content)

	// No gen_metadata table: sidecar generatorMetadata fills the gap.
	require.Len(t, usageEvents, 1)
	assert.Equal(t, "sidecar", usageEvents[0].Source)
	assert.Equal(t, "MODEL_PLACEHOLDER_M20", usageEvents[0].Model)
	assert.Equal(t, 1500, usageEvents[0].InputTokens)
	assert.Equal(t, 77, usageEvents[0].OutputTokens)
	assert.Equal(t, 20, usageEvents[0].ReasoningTokens)
	assert.Equal(t, 300, usageEvents[0].CacheReadInputTokens)
	assert.Equal(t, sess.ID, usageEvents[0].SessionID)

	assert.Equal(t, 77, sess.TotalOutputTokens)
	assert.Equal(t, 1800, sess.PeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
}

func TestAntigravityCLINonCoveringSidecarUsageRejected(t *testing.T) {
	root := t.TempDir()
	id := "acacacac-bdbd-cece-dfdf-565656565656"
	mustMkdir(t, filepath.Join(root, "conversations"))

	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath) // 2 raw steps, no gen_metadata table

	// Sidecar lags the DB: 1 step vs 2 raw rows. Its generatorMetadata
	// would decode to a usage event, but a lagging sidecar has only seen
	// part of the session, so persisting it would underreport totals on
	// a row that looks current.
	genJSON := `[{
		"stepIndices": [0],
		"chatModel": {
			"model": "MODEL_PLACEHOLDER_M20",
			"usage": {
				"inputTokens": "1500",
				"outputTokens": "77"
			}
		}
	}]`
	writeAntigravityTestSidecarWithGenMetadata(t, root, id, 1, genJSON)

	sess, msgs, usageEvents, status, err := ParseAntigravityCLISessionWithStatus(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.False(t, status.NeedsRetry)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "user prompt text goes here", msgs[0].Content,
		"non-covering sidecar must lose the transcript to the DB decode")

	assert.Empty(t, usageEvents,
		"non-covering sidecar usage must be rejected like its transcript")
	assert.False(t, sess.HasTotalOutputTokens)
	assert.False(t, sess.HasPeakContextTokens)
}

func TestAgyTokenCountUnmarshal(t *testing.T) {
	tcs := []struct {
		name string
		json string
		want int
	}{
		{"quoted number", `"21186"`, 21186},
		{"bare number", `21186`, 21186},
		{"empty string", `""`, 0},
		{"null", `null`, 0},
		{"garbage string", `"abc"`, 0},
		{"object garbage", `{"bogus":true}`, 0},
		{"negative quoted", `"-5"`, 0},
		{"negative bare", `-5`, 0},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			var c agyTokenCount
			require.NoError(t, json.Unmarshal([]byte(tc.json), &c),
				"garbage must decode to 0, never error")
			assert.Equal(t, tc.want, int(c))
		})
	}

	t.Run("surrounding object survives garbage", func(t *testing.T) {
		var u agyChatModelUsage
		require.NoError(t, json.Unmarshal([]byte(
			`{"inputTokens":{"bogus":true},"outputTokens":"5"}`,
		), &u))
		assert.Equal(t, 0, int(u.InputTokens))
		assert.Equal(t, 5, int(u.OutputTokens))
	})
}

func TestAntigravityCLISidecarUsageStepIndexEdgeCases(t *testing.T) {
	const usageJSON = `"usage":{"inputTokens":"100","outputTokens":"10"}`
	tcs := []struct {
		name        string
		gens        string
		wantEvents  int
		wantAttrib  bool
		wantContext int
		wantOutput  int
	}{
		{
			name: "non-planner step indices emit event without attribution",
			gens: `[{"stepIndices":[2],"chatModel":{` +
				`"model":"MODEL_PLACEHOLDER_M20",` + usageJSON + `}}]`,
			wantEvents: 1,
		},
		{
			name: "empty step indices",
			gens: `[{"stepIndices":[],"chatModel":{` +
				`"model":"MODEL_PLACEHOLDER_M20",` + usageJSON + `}}]`,
			wantEvents: 1,
		},
		{
			name: "out of range step index does not panic",
			gens: `[{"stepIndices":[99],"chatModel":{` +
				`"model":"MODEL_PLACEHOLDER_M20",` + usageJSON + `}}]`,
			wantEvents: 1,
		},
		{
			name: "two gens claiming one planner: first attributes",
			gens: `[{"stepIndices":[1],"chatModel":{` +
				`"model":"MODEL_PLACEHOLDER_M20",` + usageJSON + `}},` +
				`{"stepIndices":[1],"chatModel":{` +
				`"model":"MODEL_PLACEHOLDER_M132",` +
				`"usage":{"inputTokens":"999","outputTokens":"99"}}}]`,
			wantEvents:  2,
			wantAttrib:  true,
			wantContext: 100,
			wantOutput:  10,
		},
		{
			name:       "missing chatModel skipped",
			gens:       `[{"stepIndices":[1]}]`,
			wantEvents: 0,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			id := "edededed-0101-2323-4545-676767676767"
			mustMkdir(t, filepath.Join(root, "conversations"))
			pbPath := filepath.Join(root, "conversations", id+".pb")
			mustWrite(t, pbPath, []byte("pb-junk-bytes"))
			writeAntigravityTestSidecarWithGenMetadata(
				t, root, id, 3, tc.gens,
			)

			_, msgs, usageEvents, _, err := ParseAntigravityCLISessionWithStatus(
				pbPath, "", "test-machine",
			)
			require.NoError(t, err)
			assert.Len(t, usageEvents, tc.wantEvents)

			var planner *ParsedMessage
			for i := range msgs {
				if msgs[i].Role == RoleAssistant {
					planner = &msgs[i]
					break
				}
			}
			require.NotNil(t, planner)
			if tc.wantAttrib {
				assert.True(t, planner.HasContextTokens)
				assert.True(t, planner.HasOutputTokens)
				assert.Equal(t, tc.wantContext, planner.ContextTokens)
				assert.Equal(t, tc.wantOutput, planner.OutputTokens)
				assert.Equal(t, "MODEL_PLACEHOLDER_M20", planner.Model,
					"second gen claiming the planner stays event-only")
			} else {
				assert.False(t, planner.HasContextTokens)
				assert.False(t, planner.HasOutputTokens)
				assert.Zero(t, planner.ContextTokens)
				assert.Zero(t, planner.OutputTokens)
			}
		})
	}
}

// TestAntigravitySessionFileMetadataIncludesWAL verifies the persisted
// file fingerprint covers WAL sidecars: WAL-only commits do not touch
// the main .db, so size/mtime must combine the sidecar set or the sync
// skip check will never reparse a live session.
func TestAntigravitySessionFileMetadataIncludesWAL(t *testing.T) {
	root := t.TempDir()
	id := "12121212-3434-5656-7878-909090909090"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath)

	mainInfo, err := os.Stat(dbPath)
	require.NoError(t, err)

	walPath := dbPath + "-wal"
	mustWrite(t, walPath, []byte("wal bytes"))
	walTime := mainInfo.ModTime().Add(5 * time.Second)
	require.NoError(t, os.Chtimes(walPath, walTime, walTime))

	sess, _, _, err := ParseAntigravitySession(dbPath, "p", "m")
	require.NoError(t, err)

	// The parse's own read-only open can create or touch -shm/-wal
	// siblings, so the expected composite comes from post-parse disk
	// state - the same state the next sync's skip check will stat.
	var wantSize int64
	for _, p := range []string{dbPath, walPath, dbPath + "-shm"} {
		fi, statErr := os.Stat(p)
		if statErr != nil {
			continue
		}
		wantSize += fi.Size()
	}
	require.Greater(t, wantSize, mainInfo.Size(),
		"setup: WAL sidecar must contribute to the composite")
	assert.Equal(t, wantSize, sess.File.Size,
		"file size must include WAL/SHM sidecars")
	assert.Equal(t, walTime.UnixNano(), sess.File.Mtime,
		"file mtime must reflect the newest sidecar")
}

// TestAntigravityFileInfoIncludesBrainArtifacts pins brain artifacts
// into the IDE composite fingerprint: ParseAntigravitySession renders
// brain/<id>/*.md (+ .metadata.json) as messages, so a brain-only
// add/edit/delete must change the effective file info or skip checks
// keep stale brain messages.
func TestAntigravityFileInfoIncludesBrainArtifacts(t *testing.T) {
	root := t.TempDir()
	id := "13131313-2424-3535-4646-575757575757"

	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")
	mustWrite(t, dbPath, []byte("db"))

	brainDir := filepath.Join(root, "brain", id)
	mustMkdir(t, brainDir)
	mdPath := filepath.Join(brainDir, "task.md")
	metaPath := filepath.Join(brainDir, "task.md.metadata.json")
	strayPath := filepath.Join(brainDir, "scratch.txt")
	mustWrite(t, mdPath, []byte("plan body"))
	mustWrite(t, metaPath, []byte(`{"summary":"s"}`))
	// Files the parser never reads must not affect the fingerprint.
	mustWrite(t, strayPath, []byte("xxxxxxxx"))

	early := time.Unix(1779000000, 0)
	late := time.Unix(1779000300, 0)
	require.NoError(t, os.Chtimes(dbPath, early, early))
	require.NoError(t, os.Chtimes(mdPath, early, early))
	require.NoError(t, os.Chtimes(metaPath, late, late))
	require.NoError(t, os.Chtimes(strayPath, early, early))

	info, err := AntigravityFileInfo(dbPath)
	require.NoError(t, err)
	wantSize := int64(
		len("db") + len("plan body") + len(`{"summary":"s"}`),
	)
	assert.Equal(t, wantSize, info.Size(),
		"brain artifacts must contribute to the composite size")
	assert.Equal(t, late.UnixNano(), info.ModTime().UnixNano(),
		"newest brain file must drive the composite mtime")
}

func TestAntigravityTokenUsage(t *testing.T) {
	root := t.TempDir()
	id := "55555555-6666-7777-8888-999999999999"

	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()

	createAntigravityStepTables(t, db)

	// Create gen_metadata table
	mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)

	// Create user prompt step (idx=0) and assistant reply step (idx=1)
	tsEarly := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000000}})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsEarly},
		{num: 17, wire: pbWireBytes, bytes: []byte("user question text goes here and is long")},
	})
	tsLate := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000100}})
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsLate},
		{num: 17, wire: pbWireBytes, bytes: []byte("assistant response body goes here and is long")},
	})

	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, ?)`, userPayload)
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 17, ?)`, asstPayload)

	// Create gen_metadata entry for the assistant step (idx=1)
	genData := createAntigravityMockGenMetadata(t, 2400, 180, 0, "Test Gemini 3.5")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (1, ?, ?)`, genData, len(genData))

	sess, msgs, usageEvents, err := ParseAntigravitySession(dbPath, "test-project", "test-machine")
	require.NoError(t, err)

	// 1. Verify model and message token counts
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "Test Gemini 3.5", msgs[1].Model)
	assert.Equal(t, 2400, msgs[1].ContextTokens)
	assert.Equal(t, 180, msgs[1].OutputTokens)
	assert.True(t, msgs[1].HasContextTokens)
	assert.True(t, msgs[1].HasOutputTokens)

	// 2. Verify usage events
	require.Len(t, usageEvents, 1)
	assert.Equal(t, "generation", usageEvents[0].Source)
	assert.Equal(t, "Test Gemini 3.5", usageEvents[0].Model)
	assert.Equal(t, 2400, usageEvents[0].InputTokens)
	assert.Equal(t, 180, usageEvents[0].OutputTokens)
	assert.Equal(t, 0, usageEvents[0].ReasoningTokens, "reasoning not available in gen_metadata")

	// 3. Verify session rollup
	assert.Equal(t, 180, sess.TotalOutputTokens)
	assert.Equal(t, 2400, sess.PeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
}

// TestAntigravityTokenUsageCachedTokens verifies the gen_metadata path
// when cache-read tokens are present. ContextTokens is the full context
// window (uncached input + cache-read), InputTokens carries just the
// uncached portion, and CacheReadInputTokens carries the cache-read
// count. HasContextTokens must be true when context > 0.
func TestAntigravityTokenUsageCachedTokens(t *testing.T) {
	root := t.TempDir()
	id := "55555555-6666-7777-8888-aaaaaaaaaaaa"

	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()

	createAntigravityStepTables(t, db)
	mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)

	tsEarly := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000000}})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsEarly},
		{num: 17, wire: pbWireBytes, bytes: []byte("user question text goes here and is long")},
	})
	tsLate := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000100}})
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsLate},
		{num: 17, wire: pbWireBytes, bytes: []byte("assistant response body goes here and is long")},
	})
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, ?)`, userPayload)
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 17, ?)`, asstPayload)

	// uncachedInput=2200, totalOutput=180, cacheRead=200
	genData := createAntigravityMockGenMetadata(t, 2200, 180, 200, "Test Gemini 3.5")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (1, ?, ?)`, genData, len(genData))

	sess, msgs, usageEvents, err := ParseAntigravitySession(dbPath, "test-project", "test-machine")
	require.NoError(t, err)

	// Message token counts: context = uncached + cache-read.
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, 2400, msgs[1].ContextTokens, "ContextTokens = uncached + cache-read = 2200 + 200")
	assert.Equal(t, 180, msgs[1].OutputTokens)
	assert.True(t, msgs[1].HasContextTokens, "HasContextTokens true when context > 0")
	assert.True(t, msgs[1].HasOutputTokens)

	// Usage event: InputTokens is the uncached portion, CacheReadInputTokens the rest.
	require.Len(t, usageEvents, 1)
	assert.Equal(t, "generation", usageEvents[0].Source)
	assert.Equal(t, 2200, usageEvents[0].InputTokens, "InputTokens = uncached input")
	assert.Equal(t, 200, usageEvents[0].CacheReadInputTokens)
	assert.Equal(t, 180, usageEvents[0].OutputTokens)
	assert.Equal(t, 0, usageEvents[0].ReasoningTokens, "reasoning not available in gen_metadata")

	// Session rollup: PeakContextTokens is the peak context (uncached + cache-read).
	assert.Equal(t, 180, sess.TotalOutputTokens)
	assert.Equal(t, 2400, sess.PeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
}

// TestAntigravityTokenUsageCachedTokensAllInputs verifies the edge case
// where input == cached (so uncached = 0). HasContextTokens must still be
// true because cached > 0, and InputTokens (uncached) should be 0.
func TestAntigravityTokenUsageCachedTokensAllInputs(t *testing.T) {
	root := t.TempDir()
	id := "55555555-6666-7777-8888-bbbbbbbbbbbb"

	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()

	createAntigravityStepTables(t, db)
	mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)

	tsEarly := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000000}})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsEarly},
		{num: 17, wire: pbWireBytes, bytes: []byte("user question text goes here and is long")},
	})
	tsLate := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000100}})
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsLate},
		{num: 17, wire: pbWireBytes, bytes: []byte("assistant response body goes here and is long")},
	})
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, ?)`, userPayload)
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 17, ?)`, asstPayload)

	// uncachedInput=50, totalOutput=100, cacheRead=200
	// Nearly all input tokens served from cache (50 uncached out of 250 total context).
	genData := createAntigravityMockGenMetadata(t, 50, 100, 200, "Test Gemini 3.5")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (1, ?, ?)`, genData, len(genData))

	sess, msgs, usageEvents, err := ParseAntigravitySession(dbPath, "test-project", "test-machine")
	require.NoError(t, err)

	require.Len(t, msgs, 2)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, 250, msgs[1].ContextTokens, "ContextTokens = uncached + cache-read = 50 + 200")
	assert.Equal(t, 100, msgs[1].OutputTokens)
	assert.True(t, msgs[1].HasContextTokens, "HasContextTokens true when context > 0")
	assert.True(t, msgs[1].HasOutputTokens)

	require.Len(t, usageEvents, 1)
	assert.Equal(t, 50, usageEvents[0].InputTokens, "InputTokens = uncached input = 50")
	assert.Equal(t, 200, usageEvents[0].CacheReadInputTokens)
	assert.Equal(t, 100, usageEvents[0].OutputTokens)
	assert.Equal(t, 0, usageEvents[0].ReasoningTokens)

	assert.Equal(t, 100, sess.TotalOutputTokens)
	assert.Equal(t, 250, sess.PeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
}

// TestAntigravityTokenUsageMixedDecode covers a session where one
// gen_metadata row belongs to a decoded step and another to a step the
// heuristic cannot render. Session totals must include both, not just
// the tokens reachable through decoded messages.
func TestAntigravityTokenUsageMixedDecode(t *testing.T) {
	root := t.TempDir()
	id := "55555555-6666-7777-8888-999999999997"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()
	createAntigravityStepTables(t, db)
	mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)

	tsEarly := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000000}})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsEarly},
		{num: 17, wire: pbWireBytes, bytes: []byte("user question text goes here and is long")},
	})
	tsLate := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000100}})
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsLate},
		{num: 17, wire: pbWireBytes, bytes: []byte("assistant response body goes here and is long")},
	})
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, ?)`, userPayload)
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 17, ?)`, asstPayload)
	// Undecodable step with its own gen_metadata usage.
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (2, 99, ?)`, []byte{0xff, 0xff, 0xff})

	genDecoded := createAntigravityMockGenMetadata(t, 2400, 180, 0, "Test Gemini 3.5")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (1, ?, ?)`, genDecoded, len(genDecoded))
	genUndecoded := createAntigravityMockGenMetadata(t, 3000, 220, 0, "Test Gemini 3.5")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (2, ?, ?)`, genUndecoded, len(genUndecoded))

	sess, msgs, usageEvents, err := ParseAntigravitySession(dbPath, "test-project", "test-machine")
	require.NoError(t, err)
	require.Len(t, msgs, 2, "undecodable step contributes no message")
	require.Len(t, usageEvents, 2, "both gen rows emit usage events")

	assert.Equal(t, 400, sess.TotalOutputTokens, "totals must include undecoded gen usage")
	assert.Equal(t, 3000, sess.PeakContextTokens, "peak must include undecoded gen usage")
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
}

// TestAntigravityCLITokenUsageMixedDecode is the CLI-path variant: the
// DB decode wins (no sidecar), one gen row maps to a decoded step and
// one to an undecodable step.
func TestAntigravityCLITokenUsageMixedDecode(t *testing.T) {
	root := t.TempDir()
	id := "ffffffff-0000-1111-2222-333333333333"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	createAntigravityStepTables(t, db)
	mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)

	tsLate := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000100}})
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsLate},
		{num: 17, wire: pbWireBytes, bytes: []byte("assistant response body goes here and is long")},
	})
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 17, ?)`, asstPayload)
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 99, ?)`, []byte{0xff, 0xff, 0xff})

	genDecoded := createAntigravityMockGenMetadata(t, 2400, 180, 0, "Test Gemini 3.5")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (0, ?, ?)`, genDecoded, len(genDecoded))
	genUndecoded := createAntigravityMockGenMetadata(t, 3000, 220, 0, "Test Gemini 3.5")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (1, ?, ?)`, genUndecoded, len(genUndecoded))
	require.NoError(t, db.Close())

	sess, msgs, usageEvents, _, err := ParseAntigravityCLISessionWithStatus(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	require.Len(t, usageEvents, 2, "both gen rows emit usage events")

	assert.Equal(t, 400, sess.TotalOutputTokens, "totals must include undecoded gen usage")
	assert.Equal(t, 3000, sess.PeakContextTokens, "peak must include undecoded gen usage")
}

// TestAntigravityZeroMessageKeepsUsageEvents covers a DB whose only
// step is undecodable but carries gen_metadata usage: the parse yields
// no messages, yet the usage events (and the totals derived from
// them) must still be returned so daily usage analytics see them.
func TestAntigravityZeroMessageKeepsUsageEvents(t *testing.T) {
	root := t.TempDir()
	id := "55555555-6666-7777-8888-999999999996"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()
	createAntigravityStepTables(t, db)
	mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 99, ?)`, []byte{0xff, 0xff, 0xff})
	genData := createAntigravityMockGenMetadata(t, 2400, 180, 0, "Test Gemini 3.5")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (0, ?, ?)`, genData, len(genData))

	sess, msgs, usageEvents, err := ParseAntigravitySession(dbPath, "test-project", "test-machine")
	require.NoError(t, err)
	assert.Empty(t, msgs)
	require.Len(t, usageEvents, 1, "usage events must survive zero-message parses")
	assert.Equal(t, "Test Gemini 3.5", usageEvents[0].Model)
	assert.Equal(t, 180, sess.TotalOutputTokens)
	assert.Equal(t, 2400, sess.PeakContextTokens)
}

// TestAntigravityCLIZeroMessageKeepsUsageEvents is the CLI variant: an
// undecodable .db with gen_metadata, no sidecar, and no history still
// returns its usage events alongside the retry-flagged session.
func TestAntigravityCLIZeroMessageKeepsUsageEvents(t *testing.T) {
	root := t.TempDir()
	id := "00000000-1111-2222-3333-444444444444"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	createAntigravityStepTables(t, db)
	mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 99, ?)`, []byte{0xff, 0xff, 0xff})
	genData := createAntigravityMockGenMetadata(t, 3000, 220, 0, "Test Gemini 3.5")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (0, ?, ?)`, genData, len(genData))
	require.NoError(t, db.Close())

	sess, msgs, usageEvents, status, err := ParseAntigravityCLISessionWithStatus(
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.True(t, status.NeedsRetry, "undecodable rows stay retryable")
	assert.Empty(t, msgs)
	require.Len(t, usageEvents, 1, "usage events must survive zero-message parses")
	assert.Equal(t, 220, usageEvents[0].OutputTokens)
	assert.Equal(t, 220, sess.TotalOutputTokens)
	assert.Equal(t, 3000, sess.PeakContextTokens)
}

func TestAntigravityTokenUsageDynamicField(t *testing.T) {
	root := t.TempDir()
	id := "55555555-6666-7777-8888-999999999998"

	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()

	createAntigravityStepTables(t, db)

	// Create gen_metadata table
	mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)

	// Create user prompt step (idx=0) and assistant reply step (idx=1)
	tsEarly := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000000}})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsEarly},
		{num: 17, wire: pbWireBytes, bytes: []byte("user question text goes here and is long")},
	})
	tsLate := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: 1779000100}})
	asstPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: tsLate},
		{num: 17, wire: pbWireBytes, bytes: []byte("assistant response body goes here")},
	})

	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, ?)`, userPayload)
	mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 17, ?)`, asstPayload)

	// Create gen_metadata entry with a dynamic model field number: 1187 (for MODEL_PLACEHOLDER_M187)
	genData := createAntigravityMockGenMetadataWithField(t, 1187, 5000, 400, 0, "Test Gemini 3.5 Flash")
	mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (1, ?, ?)`, genData, len(genData))

	sess, msgs, usageEvents, err := ParseAntigravitySession(dbPath, "test-project", "test-machine")
	require.NoError(t, err)

	require.Len(t, msgs, 2)
	assert.Equal(t, "Test Gemini 3.5 Flash", msgs[1].Model)
	assert.Equal(t, 5000, msgs[1].ContextTokens)
	assert.Equal(t, 400, msgs[1].OutputTokens)

	require.Len(t, usageEvents, 1)
	assert.Equal(t, "Test Gemini 3.5 Flash", usageEvents[0].Model)
	assert.Equal(t, 5000, usageEvents[0].InputTokens)
	assert.Equal(t, 400, usageEvents[0].OutputTokens)
	assert.Equal(t, 0, usageEvents[0].ReasoningTokens, "reasoning not available in gen_metadata")

	assert.Equal(t, 400, sess.TotalOutputTokens)
	assert.Equal(t, 5000, sess.PeakContextTokens)
}

func createAntigravityMockGenMetadata(t *testing.T, uncachedInput, totalOutput, cacheRead int, model string) []byte {
	return createAntigravityMockGenMetadataWithField(t, 1020, uncachedInput, totalOutput, cacheRead, model)
}

func createAntigravityMockGenMetadataWithField(t *testing.T, fieldNum int, uncachedInput, totalOutput, cacheRead int, model string) []byte {
	// Build token usage inner block with remapped field semantics
	// cross-validated against sidecar ground truth:
	//   f2 = uncached input (inputTokens)
	//   f3 = total output including thinking (outputTokens)
	//   f4 = absent (never carries semantics)
	//   f5 = cache-read (cacheReadTokens, present when > 0)
	usageInner := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: uint64(fieldNum)},
		{num: 2, wire: pbWireVarint, varint: uint64(uncachedInput)},
		{num: 3, wire: pbWireVarint, varint: uint64(totalOutput)},
	})
	if cacheRead > 0 {
		usageInner = encodePB([]pbField{
			{num: 1, wire: pbWireVarint, varint: uint64(fieldNum)},
			{num: 2, wire: pbWireVarint, varint: uint64(uncachedInput)},
			{num: 3, wire: pbWireVarint, varint: uint64(totalOutput)},
			{num: 5, wire: pbWireVarint, varint: uint64(cacheRead)},
		})
	}

	// Build Field 2 (Nested message) of Field 17
	f17Inner := encodePB([]pbField{
		{num: 2, wire: pbWireBytes, bytes: usageInner},
	})

	// Build top-level fields: Field 17 (Nested bytes), Field 21 (String bytes)
	topFields := []pbField{
		{num: 17, wire: pbWireBytes, bytes: f17Inner},
	}
	if model != "" {
		topFields = append(topFields, pbField{num: 21, wire: pbWireBytes, bytes: []byte(model)})
	}

	return encodePB(topFields)
}

// TestExtractTokenUsageFalsePositiveGuards verifies that decoy blocks
// satisfying the field1 model-kind range are rejected when the expected
// token fields are missing or implausible, so the walk continues to the
// real token block instead of stopping early with junk values.
func TestExtractTokenUsageFalsePositiveGuards(t *testing.T) {
	realToken := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1187},
		{num: 2, wire: pbWireVarint, varint: 5529},  // uncached input
		{num: 3, wire: pbWireVarint, varint: 72},    // total output
		{num: 5, wire: pbWireVarint, varint: 38885}, // cache-read
	})
	tests := []struct {
		name  string
		decoy []pbField
	}{
		{
			name: "decoy without output field",
			decoy: []pbField{
				{num: 1, wire: pbWireVarint, varint: 1371},
				{num: 2, wire: pbWireVarint, varint: 1234},
			},
		},
		{
			name: "decoy with implausible input",
			decoy: []pbField{
				{num: 1, wire: pbWireVarint, varint: 1371},
				{num: 2, wire: pbWireVarint, varint: 679261000},
				{num: 3, wire: pbWireVarint, varint: 100},
			},
		},
		{
			name: "decoy with implausible output",
			decoy: []pbField{
				{num: 1, wire: pbWireVarint, varint: 1371},
				{num: 2, wire: pbWireVarint, varint: 1234},
				{num: 3, wire: pbWireVarint, varint: 679261000},
			},
		},
		{
			name: "decoy with wrong-typed output",
			decoy: []pbField{
				{num: 1, wire: pbWireVarint, varint: 1371},
				{num: 2, wire: pbWireVarint, varint: 1234},
				{num: 3, wire: pbWireBytes, bytes: []byte("not a varint")},
			},
		},
		{
			name: "decoy with implausible cache-read",
			decoy: []pbField{
				{num: 1, wire: pbWireVarint, varint: 1371},
				{num: 2, wire: pbWireVarint, varint: 1234},
				{num: 3, wire: pbWireVarint, varint: 100},
				{num: 5, wire: pbWireVarint, varint: 679261000},
			},
		},
		{
			// Input and output are each within the cap, but their
			// combined footprint exceeds the plausibility threshold,
			// rejecting blocks that individually pass per-field checks.
			name: "decoy with input+output above cap",
			decoy: []pbField{
				{num: 1, wire: pbWireVarint, varint: 1371},
				{num: 2, wire: pbWireVarint, varint: 1_999_999},
				{num: 3, wire: pbWireVarint, varint: 1_999_999},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Decoy first in DFS order so acceptance would
			// shadow the real block.
			data := encodePB([]pbField{
				{num: 1, wire: pbWireBytes, bytes: encodePB(tt.decoy)},
				{num: 2, wire: pbWireBytes, bytes: realToken},
			})
			block, ok := extractTokenUsage(data)
			require.True(t, ok, "real token block should be found")
			assert.Equal(t, 5529, block.UncachedInput, "uncached input tokens")
			assert.Equal(t, 72, block.TotalOutput, "total output tokens")
			assert.Equal(t, 38885, block.CacheRead, "cache-read tokens")
		})
	}
}

// TestExtractModelNameRejectsNonPrintable verifies that field 21/19
// payloads that are valid UTF-8 but not human-readable text (nested
// protobuf messages whose low bytes pass utf8.Valid) are rejected
// instead of being persisted as the model name. The fragment bytes are
// the exact gen_metadata field-21 payload observed in production
// sessions, which leaked into messages.model with an embedded NUL byte
// and broke `pg push` (PG rejects NUL with SQLSTATE 22021).
func TestExtractModelNameRejectsNonPrintable(t *testing.T) {
	// hex 080020022A0201024001: a nested message (field 1 varint,
	// field 4 varint, field 5 bytes, field 8 varint), all bytes
	// < 0x80 so utf8.Valid accepts it.
	protoFragment := []byte{
		0x08, 0x00, 0x20, 0x02, 0x2A, 0x02,
		0x01, 0x02, 0x40, 0x01,
	}
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "real protobuf fragment in field 21",
			data: encodePB([]pbField{
				{num: 21, wire: pbWireBytes, bytes: protoFragment},
			}),
			want: "",
		},
		{
			name: "fragment in field 21 falls back to field 19",
			data: encodePB([]pbField{
				{num: 21, wire: pbWireBytes, bytes: protoFragment},
				{num: 19, wire: pbWireBytes, bytes: []byte("gemini-3-pro")},
			}),
			want: "gemini-3-pro",
		},
		{
			name: "fragment at top level falls through to nested model",
			data: encodePB([]pbField{
				{num: 21, wire: pbWireBytes, bytes: protoFragment},
				{num: 2, wire: pbWireBytes, bytes: encodePB([]pbField{
					{num: 21, wire: pbWireBytes, bytes: []byte("gemini-3-flash")},
				})},
			}),
			want: "gemini-3-flash",
		},
		{
			name: "digits-only value is rejected",
			data: encodePB([]pbField{
				{num: 21, wire: pbWireBytes, bytes: []byte("12345678")},
			}),
			want: "",
		},
		{
			name: "plain model name still accepted",
			data: encodePB([]pbField{
				{num: 21, wire: pbWireBytes, bytes: []byte("Test Gemini 3.5")},
			}),
			want: "Test Gemini 3.5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractModelName(tt.data))
		})
	}
}

// TestExtractTokenUsageNoCacheReadField verifies a real token block is
// accepted when field 5 (cache-read) is absent: proto3 omits zero-valued
// fields, and a fresh session with no cache hits legitimately has
// f5=0, which is omitted from the wire.
func TestExtractTokenUsageNoCacheReadField(t *testing.T) {
	block := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1187},
		{num: 2, wire: pbWireVarint, varint: 5529}, // uncached input
		{num: 3, wire: pbWireVarint, varint: 72},   // total output
	})
	data := encodePB([]pbField{
		{num: 1, wire: pbWireBytes, bytes: block},
	})
	result, ok := extractTokenUsage(data)
	require.True(t, ok, "token block without cache-read should be accepted")
	assert.Equal(t, 5529, result.UncachedInput, "uncached input tokens")
	assert.Equal(t, 72, result.TotalOutput, "total output tokens")
	assert.Equal(t, 0, result.CacheRead, "cache-read defaults to 0 when absent")
}

// TestTokenBlockFromIgnoresField4 verifies that a protobuf block
// where field 4 is present (always 0/absent in real blocks) is still
// accepted as long as all required fields are valid. Field 4 carries
// no semantics and is tolerated but ignored.
func TestTokenBlockFromIgnoresField4(t *testing.T) {
	block := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1371},
		{num: 2, wire: pbWireVarint, varint: 500},
		{num: 3, wire: pbWireVarint, varint: 100},
		{num: 4, wire: pbWireVarint, varint: 0}, // f4 is tolerated
	})
	data := encodePB([]pbField{
		{num: 1, wire: pbWireBytes, bytes: block},
	})
	result, ok := extractTokenUsage(data)
	assert.True(t, ok, "block with f4=0 should be accepted")
	assert.Equal(t, 500, result.UncachedInput)
	assert.Equal(t, 100, result.TotalOutput)
	assert.Equal(t, 0, result.CacheRead)
}

// TestExtractTokenUsageLargeValueFalsePositive is a regression test for the
// case where a nested message has field1 ∈ [1000, 5000) but field2 holds an
// implausibly large value (e.g. a nanosecond-latency counter). The real token
// block should be returned instead of the decoy.
func TestExtractTokenUsageLargeValueFalsePositive(t *testing.T) {
	// Decoy block: field1=1371 (in range), field2=679261000 (implausibly large)
	decoy := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1371},
		{num: 2, wire: pbWireVarint, varint: 679261000},
	})
	// A wrapper that embeds the decoy nested under field 8, matching the real
	// session layout where the false-positive lives at [1].[9].[8].
	innerWrapper := encodePB([]pbField{
		{num: 8, wire: pbWireBytes, bytes: decoy},
	})
	outerWrapper := encodePB([]pbField{
		{num: 9, wire: pbWireBytes, bytes: innerWrapper},
	})

	// Real token block with remapped field semantics:
	// f2=5529 (uncached input), f3=72 (total output), f5=38885 (cache-read).
	realToken := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1187},
		{num: 2, wire: pbWireVarint, varint: 5529},
		{num: 3, wire: pbWireVarint, varint: 72},
		{num: 5, wire: pbWireVarint, varint: 38885},
	})
	realWrapper := encodePB([]pbField{
		{num: 4, wire: pbWireBytes, bytes: realToken},
	})

	// Top-level: field1 contains the real token block, field1 also has
	// the decoy-containing wrapper. The decoy appears first in DFS order
	// to validate that it is correctly skipped.
	data := encodePB([]pbField{
		{num: 1, wire: pbWireBytes, bytes: outerWrapper}, // decoy path first
		{num: 2, wire: pbWireBytes, bytes: realWrapper},  // real token block second
	})

	block, ok := extractTokenUsage(data)
	require.True(t, ok, "extractTokenUsage should find a token block")
	assert.Equal(t, 5529, block.UncachedInput, "uncached input tokens")
	assert.Equal(t, 72, block.TotalOutput, "total output tokens")
	assert.Equal(t, 38885, block.CacheRead, "cache-read tokens")
}

func TestAntigravityAdjacentToolCalls(t *testing.T) {
	// A flat sequence of strings simulating two consecutive tool calls, e.g. run_command.
	// The new logic should prefer the following UUID and not deduplicate distinct calls.
	fields := []agProtoField{
		{Number: 1, Wire: pbWireBytes, Bytes: []byte("run_command")},
		{Number: 1, Wire: pbWireBytes, Bytes: []byte("11111111-1111-1111-1111-111111111111")},
		{Number: 1, Wire: pbWireBytes, Bytes: []byte(`{"command": "ls"}`)},
		{Number: 1, Wire: pbWireBytes, Bytes: []byte("run_command")},
		{Number: 1, Wire: pbWireBytes, Bytes: []byte("22222222-2222-2222-2222-222222222222")},
		{Number: 1, Wire: pbWireBytes, Bytes: []byte(`{"command": "pwd"}`)},
	}

	calls := extractAntigravityToolCalls(0, fields)

	require.Len(t, calls, 2)
	assert.Equal(t, "run_command", calls[0].ToolName)
	assert.Equal(t, "Bash", calls[0].Category)
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", calls[0].ToolUseID)
	assert.Equal(t, `{"command": "ls"}`, calls[0].InputJSON)

	assert.Equal(t, "run_command", calls[1].ToolName)
	assert.Equal(t, "Bash", calls[1].Category)
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", calls[1].ToolUseID)
	assert.Equal(t, `{"command": "pwd"}`, calls[1].InputJSON)
}

func TestAntigravityAdjacentToolCallsNoUUIDs(t *testing.T) {
	// Two identical tool names with no adjacent UUIDs or JSON.
	// Both must be emitted as separate calls with synthetic IDs.
	fields := []agProtoField{
		{Number: 1, Wire: pbWireBytes, Bytes: []byte("run_command")},
		{Number: 1, Wire: pbWireBytes, Bytes: []byte("run_command")},
	}

	calls := extractAntigravityToolCalls(0, fields)

	require.Len(t, calls, 2)
	assert.Equal(t, "run_command", calls[0].ToolName)
	assert.Equal(t, "run_command", calls[1].ToolName)
	// Synthetic IDs should differ because they encode the string index.
	assert.NotEqual(t, calls[0].ToolUseID, calls[1].ToolUseID)
	assert.Contains(t, calls[0].ToolUseID, "ag-step-")
	assert.Contains(t, calls[1].ToolUseID, "ag-step-")
}

func TestAntigravityToolCallUUIDBackwardFallback(t *testing.T) {
	// UUID appears before the tool name (e.g. tool at end of string list).
	// The backward scan offsets (-1, -2) should still pick it up.
	fields := []agProtoField{
		{Number: 1, Wire: pbWireBytes, Bytes: []byte("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")},
		{Number: 1, Wire: pbWireBytes, Bytes: []byte("view_file")},
	}

	calls := extractAntigravityToolCalls(0, fields)

	require.Len(t, calls, 1)
	assert.Equal(t, "view_file", calls[0].ToolName)
	assert.Equal(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", calls[0].ToolUseID)
}

// TestAntigravityToolCallsRejectsGenericStrings verifies that standalone
// generic strings like "read", "write", "message", "process" that happen
// to match the global taxonomy but are NOT known Antigravity tool names
// are not fabricated into tool calls without corroborating evidence.
func TestAntigravityToolCallsRejectsGenericStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fields  []agProtoField
		wantLen int
	}{
		{
			name: "standalone read with no evidence",
			fields: []agProtoField{
				{Number: 1, Wire: pbWireBytes, Bytes: []byte("read")},
			},
			wantLen: 0,
		},
		{
			name: "standalone write with no evidence",
			fields: []agProtoField{
				{Number: 1, Wire: pbWireBytes, Bytes: []byte("write")},
			},
			wantLen: 0,
		},
		{
			name: "standalone message with no evidence",
			fields: []agProtoField{
				{Number: 1, Wire: pbWireBytes, Bytes: []byte("message")},
			},
			wantLen: 0,
		},
		{
			name: "standalone process with no evidence",
			fields: []agProtoField{
				{Number: 1, Wire: pbWireBytes, Bytes: []byte("process")},
			},
			wantLen: 0,
		},
		{
			name: "string containing subagent is not a tool name",
			fields: []agProtoField{
				{Number: 1, Wire: pbWireBytes, Bytes: []byte("some_subagent_thing")},
			},
			wantLen: 0,
		},
		{
			name: "read with adjacent UUID is rejected",
			fields: []agProtoField{
				{Number: 1, Wire: pbWireBytes, Bytes: []byte("read")},
				{Number: 2, Wire: pbWireBytes, Bytes: []byte("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")},
			},
			wantLen: 0,
		},
		{
			name: "read with adjacent JSON input is rejected",
			fields: []agProtoField{
				{Number: 1, Wire: pbWireBytes, Bytes: []byte("read")},
				{Number: 2, Wire: pbWireBytes, Bytes: []byte(`{"path": "/tmp/x"}`)},
			},
			wantLen: 0,
		},
		{
			name: "known Antigravity tool view_file always accepted",
			fields: []agProtoField{
				{Number: 1, Wire: pbWireBytes, Bytes: []byte("view_file")},
			},
			wantLen: 1,
		},
		{
			name: "known Antigravity tool invoke_subagent always accepted",
			fields: []agProtoField{
				{Number: 1, Wire: pbWireBytes, Bytes: []byte("invoke_subagent")},
			},
			wantLen: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := extractAntigravityToolCalls(0, tc.fields)
			assert.Len(t, calls, tc.wantLen)
		})
	}
}

func TestAntigravityAdjacentToolCallsIntervening(t *testing.T) {
	// Tool call N has no UUID. Tool call N+1 has a UUID.
	// Call N should not steal Call N+1's UUID or JSON input across the intervening tool name.
	fields := []agProtoField{
		{Number: 1, Wire: pbWireBytes, Bytes: []byte("run_command")},
		{Number: 2, Wire: pbWireBytes, Bytes: []byte("run_command")},
		{Number: 3, Wire: pbWireBytes, Bytes: []byte("11111111-1111-1111-1111-111111111111")},
		{Number: 4, Wire: pbWireBytes, Bytes: []byte(`{"command": "pwd"}`)},
	}

	calls := extractAntigravityToolCalls(0, fields)
	require.Len(t, calls, 2)

	// First call should have synthetic ID and NO JSON input (it is blocked by the second tool name)
	assert.Equal(t, "run_command", calls[0].ToolName)
	assert.Contains(t, calls[0].ToolUseID, "ag-step-")
	assert.Empty(t, calls[0].InputJSON)

	// Second call should have the UUID and JSON input
	assert.Equal(t, "run_command", calls[1].ToolName)
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", calls[1].ToolUseID)
	assert.Equal(t, `{"command": "pwd"}`, calls[1].InputJSON)
}

// TestDecodeAntigravityStepToolOnlyAssistantStep verifies that an
// assistant step containing only tool calls (no displayable prose)
// is still emitted as a ParsedMessage with HasToolUse=true rather
// than being silently dropped.
func TestDecodeAntigravityStepToolOnlyAssistantStep(t *testing.T) {
	// An assistant step (stepType=15) with only a tool name and a UUID.
	// agProtoCollectStrings with minLen=20 would miss the short strings,
	// and the UUID would be filtered by cleanAntigravityStepStrings,
	// leaving no displayable content. Previously this caused the step
	// to be dropped entirely; now it should still be emitted with the
	// tool call intact.
	inner := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779326586},
	})
	payload := encodePB([]pbField{
		{num: 17, wire: pbWireBytes, bytes: []byte("view_file")},
		{num: 18, wire: pbWireBytes, bytes: []byte("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")},
		{num: 5, wire: pbWireBytes, bytes: inner},
	})

	msg, ok := decodeAntigravityStep(0, 15, payload)
	require.True(t, ok, "tool-only assistant step should not be dropped")
	assert.Equal(t, RoleAssistant, msg.Role)
	assert.True(t, msg.HasToolUse, "HasToolUse should be true")
	require.Len(t, msg.ToolCalls, 1)
	assert.Equal(t, "view_file", msg.ToolCalls[0].ToolName)
}
