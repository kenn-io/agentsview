package parser

import (
	"encoding/binary"
	"errors"
	"strings"
	"unicode/utf8"
)

// Generic protobuf wire-format walker used by both Antigravity
// agents. We do not have the official .proto schema, so the
// walker extracts a permissive field tree and exposes helpers
// for pulling out strings and Timestamp-like (seconds, nanos)
// values by field number.

const (
	pbWireVarint     = 0
	pbWireFixed64    = 1
	pbWireBytes      = 2
	pbWireStartGroup = 3 // deprecated, ignored
	pbWireEndGroup   = 4 // deprecated, ignored
	pbWireFixed32    = 5

	// agProtoMaxDepth caps nested-message recursion to keep
	// malformed payloads from exploding the call stack.
	agProtoMaxDepth = 32

	// agProtoMaxFields caps the total number of fields decoded
	// across one parse, including speculative nested re-parses.
	// Each wire field can be as small as two bytes but decodes
	// into a ~100-byte agProtoField, so without a budget a flat
	// run of minimal varint fields amplifies memory by two
	// orders of magnitude (an 8 MiB blob allocates over 1 GiB).
	// Step payloads, gen_metadata, and decrypted .pb transcripts
	// are unbounded blobs, so the depth cap alone does not bound
	// allocation: the blowup is breadth, not nesting. On
	// exhaustion the walk truncates rather than fails (see
	// agProtoParse), so a payload past the budget degrades to a
	// decoded prefix instead of losing everything. Real payloads
	// sit well under the cap (the largest observed gen_metadata
	// blob, 186 KB, could hold at most ~95k minimal fields).
	agProtoMaxFields = 1 << 20
)

// errAgProtoBudget signals that the shared total-fields budget ran
// out mid-walk. It never escapes agProtoParse: the partial result is
// returned instead.
var errAgProtoBudget = errors.New("antigravity proto: too many fields")

// agProtoField is one wire-format field decoded from a payload.
// For length-delimited fields, Bytes holds the raw payload and
// Nested holds the recursively-decoded sub-fields when the
// payload looks like a well-formed message; Nested is nil when
// the payload is opaque (likely a string or binary blob).
type agProtoField struct {
	Number int
	Wire   int
	// Varint holds the value when Wire == pbWireVarint.
	Varint uint64
	// Fixed holds the raw 4 or 8 bytes when Wire is fixed32/64.
	Fixed []byte
	// Bytes holds the length-delimited payload as-is.
	Bytes []byte
	// Nested is set when Bytes was successfully reparsed as a
	// nested message. Nil if the payload is not a message.
	Nested []agProtoField
}

// agProtoParse decodes a protobuf payload into a flat slice of
// fields at the top level. Length-delimited sub-payloads are
// speculatively re-parsed as nested messages. A payload that runs
// past the total-fields budget yields the decoded prefix rather
// than an error: the memory bound holds either way, and a huge
// transcript should degrade to partial content, not disappear.
func agProtoParse(data []byte) ([]agProtoField, error) {
	budget := agProtoMaxFields
	fields, err := agProtoParseDepth(data, 0, &budget)
	if errors.Is(err, errAgProtoBudget) {
		return fields, nil
	}
	return fields, err
}

// agProtoParseDepth decodes one message level. budget is shared
// across the whole parse tree (failed speculative nested parses
// included, since their discarded fields were still allocated)
// and makes total allocation independent of how adversarially
// dense the input packs its fields. On exhaustion it returns the
// fields decoded so far alongside errAgProtoBudget; a nested
// caller drops the partial slice (the payload stays opaque) while
// the top-level caller keeps it.
func agProtoParseDepth(
	data []byte, depth int, budget *int,
) ([]agProtoField, error) {
	if depth > agProtoMaxDepth {
		return nil, errors.New("antigravity proto: max depth")
	}
	var out []agProtoField
	pos := 0
	for pos < len(data) {
		if *budget <= 0 {
			return out, errAgProtoBudget
		}
		*budget--
		tag, n := binary.Uvarint(data[pos:])
		if n <= 0 {
			return nil, errors.New("antigravity proto: bad tag")
		}
		pos += n
		number := int(tag >> 3)
		wire := int(tag & 0x7)
		if number <= 0 {
			return nil, errors.New("antigravity proto: zero field")
		}
		f := agProtoField{Number: number, Wire: wire}
		switch wire {
		case pbWireVarint:
			v, m := binary.Uvarint(data[pos:])
			if m <= 0 {
				return nil, errors.New(
					"antigravity proto: bad varint",
				)
			}
			f.Varint = v
			pos += m
		case pbWireFixed64:
			if pos+8 > len(data) {
				return nil, errors.New(
					"antigravity proto: short fixed64",
				)
			}
			f.Fixed = data[pos : pos+8]
			pos += 8
		case pbWireFixed32:
			if pos+4 > len(data) {
				return nil, errors.New(
					"antigravity proto: short fixed32",
				)
			}
			f.Fixed = data[pos : pos+4]
			pos += 4
		case pbWireBytes:
			ln, m := binary.Uvarint(data[pos:])
			if m <= 0 {
				return nil, errors.New(
					"antigravity proto: bad length",
				)
			}
			pos += m
			// Overflow-safe: pos <= len(data) is invariant (Uvarint
			// caps m), so len(data)-pos is non-negative. Comparing
			// against ln without addition avoids uint64 wrap on a
			// malformed huge length varint.
			if ln > uint64(len(data)-pos) {
				return nil, errors.New(
					"antigravity proto: short bytes",
				)
			}
			f.Bytes = data[pos : pos+int(ln)]
			pos += int(ln)
			if nested, err := agProtoParseDepth(
				f.Bytes, depth+1, budget,
			); err == nil && agProtoLooksLikeMessage(nested) {
				f.Nested = nested
			}
		case pbWireStartGroup, pbWireEndGroup:
			// Skip; deprecated and not used by Antigravity.
		default:
			return nil, errors.New(
				"antigravity proto: unknown wire type",
			)
		}
		out = append(out, f)
	}
	return out, nil
}

// agProtoLooksLikeMessage returns true when the decoded fields
// look more like a nested message than coincidentally-parseable
// random bytes (e.g. UTF-8 strings whose first byte happens to
// decode as a valid tag). The heuristic: at least one field and
// every field's number is in a reasonable range.
func agProtoLooksLikeMessage(fields []agProtoField) bool {
	if len(fields) == 0 {
		return false
	}
	for _, f := range fields {
		if f.Number < 1 || f.Number > 100000 {
			return false
		}
	}
	return true
}

// agProtoString returns the payload of f as a UTF-8 string when
// f is length-delimited and the bytes parse as valid UTF-8.
func agProtoString(f agProtoField) (string, bool) {
	if f.Wire != pbWireBytes {
		return "", false
	}
	if !utf8.Valid(f.Bytes) {
		return "", false
	}
	return string(f.Bytes), true
}

// agProtoFind returns the first sub-field with the given field
// number, descending into Nested messages.
func agProtoFind(
	fields []agProtoField, number int,
) (agProtoField, bool) {
	for _, f := range fields {
		if f.Number == number {
			return f, true
		}
	}
	return agProtoField{}, false
}

// agProtoCollectStrings walks the field tree and returns every
// UTF-8 string with at least minLen runes. Returned in encounter
// order. Duplicates are preserved (callers can dedupe). NUL runes
// are replaced with U+FFFD: they are valid UTF-8 (NUL-delimited
// tool output like `git ls-files -z` is realistic content), but a
// NUL that leaks into persisted content breaks `pg push`
// (PostgreSQL rejects NUL in text columns with SQLSTATE 22021),
// so the strings are sanitized rather than dropped.
func agProtoCollectStrings(
	fields []agProtoField, minLen int,
) []string {
	var out []string
	var walk func([]agProtoField)
	walk = func(fs []agProtoField) {
		for _, f := range fs {
			if f.Wire == pbWireBytes && f.Nested == nil {
				if s, ok := agProtoString(f); ok &&
					utf8.RuneCountInString(s) >= minLen {
					out = append(out,
						strings.ReplaceAll(s, "\x00", "\uFFFD"))
				}
			}
			if f.Nested != nil {
				walk(f.Nested)
			}
		}
	}
	walk(fields)
	return out
}

// agProtoLooksLikePrefix returns true when data starts with a
// well-formed protobuf field sequence — even if the final field
// runs off the end of data. Used to validate decryption
// candidates against a fixed-size sniff (e.g. the first 4KB):
// agProtoParse would reject a buffer truncated mid-field, which
// can throw away valid large transcripts. This validator accepts
// any clean prefix of fields plus a partially-read tail, as long
// as at least one full field decoded cleanly first.
func agProtoLooksLikePrefix(data []byte) bool {
	pos := 0
	parsed := 0
	for pos < len(data) {
		tag, n := binary.Uvarint(data[pos:])
		if n <= 0 {
			return parsed > 0
		}
		pos += n
		number := int(tag >> 3)
		wire := int(tag & 0x7)
		if number < 1 || number > 100000 {
			return false
		}
		switch wire {
		case pbWireVarint:
			_, m := binary.Uvarint(data[pos:])
			if m <= 0 {
				return parsed > 0
			}
			pos += m
		case pbWireFixed64:
			if pos+8 > len(data) {
				return parsed > 0
			}
			pos += 8
		case pbWireFixed32:
			if pos+4 > len(data) {
				return parsed > 0
			}
			pos += 4
		case pbWireBytes:
			ln, m := binary.Uvarint(data[pos:])
			if m <= 0 {
				return parsed > 0
			}
			pos += m
			if ln > uint64(len(data)-pos) {
				return parsed > 0
			}
			pos += int(ln)
		case pbWireStartGroup, pbWireEndGroup:
			// deprecated; skip
		default:
			return false
		}
		parsed++
	}
	return parsed > 0
}

// agProtoTimestamp returns (seconds, nanos, ok) when fields
// match the google.protobuf.Timestamp shape: field 1 varint
// seconds, optional field 2 varint nanos, and no other fields.
// A nanos value outside [0, 1e9) marks the message as not a
// Timestamp: the spec bounds nanos to that range, and a larger
// varint cast to int32 could go negative and shift time.Unix
// results outside the plausibility window callers check on
// seconds alone.
func agProtoTimestamp(
	fields []agProtoField,
) (int64, int32, bool) {
	var sec int64
	var nanos int32
	sawSec := false
	for _, f := range fields {
		if f.Wire != pbWireVarint {
			return 0, 0, false
		}
		switch f.Number {
		case 1:
			sec = int64(f.Varint)
			sawSec = true
		case 2:
			if f.Varint >= 1_000_000_000 {
				return 0, 0, false
			}
			nanos = int32(f.Varint)
		default:
			return 0, 0, false
		}
	}
	return sec, nanos, sawSec
}
