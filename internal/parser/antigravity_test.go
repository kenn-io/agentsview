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
	// past the end of the buffer — agProtoParse rejects this, but
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

	sess, msgs, err := ParseAntigravitySession(
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

// silence unused warning on time import in case the file is
// trimmed in the future.
var _ = time.Time{}

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

func TestAntigravityCLIStaleTrajectoryFallsBack(t *testing.T) {
	root := t.TempDir()
	id := "44444444-5555-6666-7777-888888888888"

	mustMkdir(t, filepath.Join(root, "conversations"))

	pbPath := filepath.Join(root, "conversations", id+".pb")
	mustWrite(t, pbPath, []byte("newer-pb-stub"))
	sidecarPath := filepath.Join(root, "conversations", id+".trajectory.json")
	mustWrite(t, sidecarPath, []byte(`{
		"steps": [
			{
				"type": "CORTEX_STEP_TYPE_USER_INPUT",
				"metadata": {
					"createdAt": "2026-05-20T22:40:00Z"
				},
				"userInput": {
					"userResponse": "stale trajectory prompt"
				}
			}
		]
	}`))
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"new history prompt","timestamp":1779000000000,`+
			`"workspace":"/tmp/proj","conversationId":"`+id+`"}`))

	now := time.Now()
	require.NoError(t, os.Chtimes(sidecarPath, now.Add(-time.Hour), now.Add(-time.Hour)))
	require.NoError(t, os.Chtimes(pbPath, now, now))

	sess, msgs, err := ParseAntigravityCLISession(pbPath, "", "test-machine")
	require.NoError(t, err)

	require.Len(t, msgs, 1)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "new history prompt", msgs[0].Content)
	assert.Equal(t, "new history prompt", sess.FirstMessage)
}
