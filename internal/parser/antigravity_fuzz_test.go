package parser

// Fuzz targets for the Antigravity wire-walk (GitHub #648).
//
// The Antigravity format is an undocumented, unversioned protobuf
// schema that has changed across dot releases; fixtures only cover
// payload shapes we have happened to observe. These targets assert
// output INVARIANTS instead of expected values, so corner cases we
// cannot enumerate still have pinned properties:
//
//   - agProtoParse: never panics, returns a structurally sound
//     field tree bounded by the input, and is deterministic. The
//     total-fields allocation budget (each two-byte wire field
//     decodes into a ~100-byte struct, so unbudgeted parses
//     amplify memory two orders of magnitude) is asserted here
//     but pinned deterministically by TestAgProtoFieldBudget:
//     mutated inputs stay far too small to probe it.
//   - decodeAntigravityStep: content is non-empty valid UTF-8 with
//     no NUL bytes, roles are in the enum, timestamps are zero or
//     inside the 2000..2100 plausibility window.
//   - extractModelName: empty or printable with at least one letter
//     (the 2026-06-11 incident: a nested protobuf fragment whose
//     bytes are all < 0x80 is valid UTF-8, leaked into
//     messages.model, and broke pg push with SQLSTATE 22021).
//   - extractTokenUsage: counts are non-negative and bounded by
//     maxPlausibleTokens, including the input+output sum used as a
//     combined plausibility guard. Reasoning is always 0 (no per-field
//     breakdown available in gen_metadata).
//
// Plain `go test` runs the f.Add seeds plus any regression inputs
// committed under testdata/fuzz/<Target>/, so the corpus is enforced
// on every CI run; `go test -fuzz=<Target>` explores from there.

import (
	"bytes"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agIncidentFragment is the real gen_metadata fragment (hex
// 080020022A0201024001) that extractModelName persisted verbatim as
// a model name before isPlausibleModelName existed: every byte is
// < 0x80, so it passes utf8.Valid while containing a NUL.
var agIncidentFragment = []byte{
	0x08, 0x00, 0x20, 0x02, 0x2A, 0x02, 0x01, 0x02, 0x40, 0x01,
}

// fuzzAgTimestamp encodes a google.protobuf.Timestamp-shaped message.
func fuzzAgTimestamp(sec, nanos uint64) []byte {
	return encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: sec},
		{num: 2, wire: pbWireVarint, varint: nanos},
	})
}

// fuzzAgWireSeeds returns payloads exercising the walker: realistic
// step and gen_metadata shapes, the incident fragment, and malformed
// edges (truncated varints, oversized lengths, deep nesting, group
// wire types, window-boundary timestamps).
func fuzzAgWireSeeds() [][]byte {
	userStep := encodePB([]pbField{
		{num: 3, wire: pbWireBytes, bytes: fuzzAgTimestamp(1_779_326_586, 959_000_000)},
		{num: 17, wire: pbWireBytes, bytes: []byte(
			"Please refactor the sync engine to retry failed pushes.",
		)},
	})
	nulStep := encodePB([]pbField{
		{num: 17, wire: pbWireBytes, bytes: []byte(
			"abcdefghij\x00klmnopqrstuvwxyz now twenty-plus runes",
		)},
	})
	// f2=5529 (uncached input), f3=72 (total output), f5=38885 (cache-read).
	// Remapped field semantics cross-validated against sidecar ground truth.
	usageBlock := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1020},
		{num: 2, wire: pbWireVarint, varint: 5529},
		{num: 3, wire: pbWireVarint, varint: 72},
		{num: 5, wire: pbWireVarint, varint: 38885},
	})
	genMeta := encodePB([]pbField{
		{num: 17, wire: pbWireBytes, bytes: usageBlock},
		{num: 21, wire: pbWireBytes, bytes: []byte("gemini-3.5-flash")},
	})
	nestedFragmentModel := encodePB([]pbField{
		{num: 2, wire: pbWireBytes, bytes: encodePB([]pbField{
			{num: 21, wire: pbWireBytes, bytes: agIncidentFragment},
		})},
	})
	// Decoy: f2 input above cap.
	usageDecoyLatency := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1020},
		{num: 2, wire: pbWireVarint, varint: 2_000_001},
		{num: 3, wire: pbWireVarint, varint: 1},
	})
	// Decoy: f3 output with wrong wire type.
	usageDecoyWire := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1020},
		{num: 2, wire: pbWireVarint, varint: 5},
		{num: 3, wire: pbWireBytes, bytes: []byte("xx")},
	})
	// Decoy: f2+f3 sum above cap (each individually plausible).
	usageDecoyFoldedCap := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1020},
		{num: 2, wire: pbWireVarint, varint: 2_000_000},
		{num: 3, wire: pbWireVarint, varint: 2_000_000},
	})
	onion := []byte("core")
	for range agProtoMaxDepth + 4 {
		onion = encodePB([]pbField{
			{num: 1, wire: pbWireBytes, bytes: onion},
		})
	}
	// Step-shaped payloads (nested timestamp + a collectable
	// string) so the window-boundary values actually reach the
	// timestamp assertions in FuzzDecodeAntigravityStep: bare
	// timestamp messages decode to no content and bail before
	// any invariant is checked.
	stepWithTS := func(sec, nanos uint64) []byte {
		return encodePB([]pbField{
			{num: 3, wire: pbWireBytes, bytes: fuzzAgTimestamp(sec, nanos)},
			{num: 17, wire: pbWireBytes, bytes: []byte(
				"Boundary probe prompt with enough runes to keep.",
			)},
		})
	}
	return [][]byte{
		nil,
		{0x00},                               // zero field number
		{0x80},                               // truncated tag varint
		{0x08},                               // varint field, missing value
		{0x0A, 0xFF, 0xFF, 0xFF, 0xFF, 0x0F}, // length far past input
		{0x09, 0x01},                         // short fixed64
		{0x0D, 0x01, 0x02},                   // short fixed32
		{0x0B, 0x0C},                         // start/end group pair
		{0x08, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01}, // 10-byte varint
		agIncidentFragment,
		userStep,
		nulStep,
		genMeta,
		nestedFragmentModel,
		usageDecoyLatency,
		usageDecoyWire,
		usageDecoyFoldedCap,
		onion,
		bytes.Repeat([]byte{0x08, 0x00}, 64), // dense minimal fields
		fuzzAgTimestamp(1_779_326_586, 4_000_000_000), // nanos out of range
		// Out-of-range nanos behind collectable content: the
		// timestamp is rejected, so the decoded message keeps a
		// zero Timestamp instead of a shifted one.
		stepWithTS(1_779_326_586, 4_000_000_000),
		stepWithTS(946_684_800, 0),             // window lower bound (excluded)
		stepWithTS(946_684_801, 999_999_999),   // first inside window
		stepWithTS(4_102_444_799, 999_999_999), // last inside window
		stepWithTS(4_102_444_800, 0),           // window upper bound (excluded)
		encodePB([]pbField{
			{num: 21, wire: pbWireBytes, bytes: []byte("   ")},
		}), // whitespace-only model candidate
		encodePB([]pbField{
			{num: 19, wire: pbWireBytes, bytes: []byte("123-456_789")},
		}), // letterless model candidate
	}
}

// checkAgProtoFieldTree asserts the structural invariants of a
// decoded field tree against the buffer it was parsed from: positive
// field numbers, wire types from the wire-format enum, fixed payloads
// of exactly 4/8 bytes, length-delimited payloads no longer than the
// enclosing buffer, and nesting bounded by the recursion cap. Returns
// the total number of fields in the tree so callers can assert the
// allocation budget.
func checkAgProtoFieldTree(
	t *testing.T, fields []agProtoField, buf []byte, depth int,
) int {
	t.Helper()
	require.LessOrEqual(t, depth, agProtoMaxDepth,
		"nested past the recursion cap")
	total := len(fields)
	for _, f := range fields {
		assert.GreaterOrEqual(t, f.Number, 1, "field number")
		switch f.Wire {
		case pbWireVarint:
			assert.Nil(t, f.Fixed, "varint field carries fixed bytes")
			assert.Nil(t, f.Bytes, "varint field carries payload")
		case pbWireFixed64:
			assert.Len(t, f.Fixed, 8, "fixed64 payload")
		case pbWireFixed32:
			assert.Len(t, f.Fixed, 4, "fixed32 payload")
		case pbWireBytes:
			assert.LessOrEqual(t, len(f.Bytes), len(buf),
				"payload longer than enclosing buffer")
			if f.Nested != nil {
				total += checkAgProtoFieldTree(
					t, f.Nested, f.Bytes, depth+1,
				)
			}
		case pbWireStartGroup, pbWireEndGroup:
			assert.Nil(t, f.Fixed, "group field carries fixed bytes")
			assert.Nil(t, f.Bytes, "group field carries payload")
		default:
			assert.Failf(t, "unknown wire type",
				"field %d wire %d", f.Number, f.Wire)
		}
	}
	return total
}

func FuzzAgProtoParse(f *testing.F) {
	for _, seed := range fuzzAgWireSeeds() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		fields, err := agProtoParse(data)
		if err != nil {
			return
		}
		total := checkAgProtoFieldTree(t, fields, data, 0)
		assert.LessOrEqual(t, total, agProtoMaxFields,
			"decoded fields exceed the allocation budget")
		again, err := agProtoParse(data)
		require.NoError(t, err, "second parse of accepted input")
		assert.Equal(t, fields, again, "parse is not deterministic")
	})
}

func FuzzAgProtoLooksLikePrefix(f *testing.F) {
	for _, seed := range fuzzAgWireSeeds() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		// Panic safety only: the prefix sniff runs on untrusted
		// decryption candidates, but shares no output contract
		// with the full parse (it bounds field numbers, the
		// parser does not).
		first := agProtoLooksLikePrefix(data)
		assert.Equal(t, first, agProtoLooksLikePrefix(data),
			"prefix sniff is not deterministic")
	})
}

func FuzzDecodeAntigravityStep(f *testing.F) {
	for i, seed := range fuzzAgWireSeeds() {
		f.Add(i, 14, seed)
		f.Add(i, 2, seed)
	}
	windowMin := time.Unix(946_684_801, 0)
	windowMax := time.Unix(4_102_444_800, 0)
	f.Fuzz(func(t *testing.T, idx, stepType int, payload []byte) {
		msg, ok := decodeAntigravityStep(idx, stepType, payload)
		if !ok {
			return
		}
		require.NotEmpty(t, msg.Content, "decoded message without content")
		assert.True(t, utf8.ValidString(msg.Content),
			"content is not valid UTF-8")
		assert.NotContains(t, msg.Content, "\x00",
			"content contains a NUL byte")
		assert.Equal(t, len(msg.Content), msg.ContentLength)
		wantRole := RoleAssistant
		if stepType == 14 {
			wantRole = RoleUser
		}
		assert.Equal(t, wantRole, msg.Role)
		if !msg.Timestamp.IsZero() {
			assert.False(t, msg.Timestamp.Before(windowMin),
				"timestamp %v before plausibility window", msg.Timestamp)
			assert.False(t, !msg.Timestamp.Before(windowMax),
				"timestamp %v after plausibility window", msg.Timestamp)
		}
	})
}

func FuzzExtractModelName(f *testing.F) {
	for _, seed := range fuzzAgWireSeeds() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		model := extractModelName(data)
		if model == "" {
			return
		}
		hasLetter := false
		for _, r := range model {
			assert.True(t, unicode.IsPrint(r),
				"model %q contains non-printable rune %q", model, r)
			if unicode.IsLetter(r) {
				hasLetter = true
			}
		}
		assert.True(t, hasLetter, "model %q has no letters", model)
	})
}

func FuzzExtractTokenUsage(f *testing.F) {
	for _, seed := range fuzzAgWireSeeds() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		block, ok := extractTokenUsage(data)
		if !ok {
			assert.Zero(t, block.UncachedInput)
			assert.Zero(t, block.TotalOutput)
			assert.Zero(t, block.CacheRead)
			return
		}
		for name, v := range map[string]int{
			"uncachedInput": block.UncachedInput, "totalOutput": block.TotalOutput, "cacheRead": block.CacheRead,
		} {
			assert.GreaterOrEqual(t, v, 0, "%s tokens negative", name)
			assert.LessOrEqual(t, v, maxPlausibleTokens,
				"%s tokens above plausibility cap", name)
		}
		// The caller uses input+output as a combined plausibility guard
		// (each independently bounded, but decoys with both near the cap
		// are implausible for a single generation).
		assert.LessOrEqual(t, block.UncachedInput+block.TotalOutput, maxPlausibleTokens,
			"input+output above plausibility cap")
	})
}
