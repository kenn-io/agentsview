package ledger

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var segNow = time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)

// withID returns ev with a distinct event_id so segments built from
// several test events differ, like jilog's Uuid::now_v7() helpers.
func withID(ev Event, n byte) Event {
	ev.EventID = uuid.UUID{0x01, 0x90, 0xf5, 0xa2, 0x7c, 0x3e, 0x70, 0, 0x80, 0, 0, 0, 0, 0, 0xbb, n}
	return ev
}

// Ports jilog ledger-core segment.rs:473-757 (the pure tests; file I/O
// tests move to internal/ledger/segfile).
func TestSegment(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"test_new_segment_is_empty", func(t *testing.T) {
			t.Helper()
			seg := NewSegment("host-a", 1, segNow)
			assert.Empty(t, seg.Events)
			assert.Equal(t, "host-a", seg.Source)
			assert.Equal(t, uint64(1), seg.SourceSeq)
			assert.Equal(t, uint32(0), seg.Checksum)
			assert.False(t, seg.Sealed())
			assert.Equal(t, "2026-01-02T03:05:00Z", seg.CreatedAt)
			local := NewSegment("host-a", 1, time.Date(2026, 1, 2, 4, 5, 0, 0, time.FixedZone("plus1", 3600)))
			assert.Equal(t, "2026-01-02T03:05:00Z", local.CreatedAt, "created_at is always UTC")
		}},
		{"test_append_and_len", func(t *testing.T) {
			t.Helper()
			seg := NewSegment("host-a", 1, segNow)
			seg.Append(withID(testEvent("zone-a", 1), 1))
			seg.Append(withID(testEvent("zone-a", 2), 2))
			assert.Len(t, seg.Events, 2)
		}},
		{"test_seal_and_verify", func(t *testing.T) {
			t.Helper()
			seg := NewSegment("host-a", 1, segNow)
			seg.Append(withID(testEvent("zone-a", 1), 1))
			require.NoError(t, seg.Seal())
			assert.NotEqual(t, uint32(0), seg.Checksum)
			ok, err := seg.Verify()
			require.NoError(t, err)
			assert.True(t, ok)
		}},
		{"test_verify_detects_tampering", func(t *testing.T) {
			t.Helper()
			seg := NewSegment("host-a", 1, segNow)
			seg.Append(withID(testEvent("zone-a", 1), 1))
			require.NoError(t, seg.Seal())
			seg.Events = append(seg.Events, withID(testEvent("zone-a", 2), 2))
			ok, err := seg.Verify()
			require.NoError(t, err)
			assert.False(t, ok)
		}},
		{"test_filename_format", func(t *testing.T) {
			t.Helper()
			assert.Equal(t, "host-a-000001.json", NewSegment("host-a", 1, segNow).Filename())
			assert.Equal(t, "host-b-000042.json", NewSegment("host-b", 42, segNow).Filename())
			assert.Equal(t, "a-1234567.json", NewSegment("a", 1234567, segNow).Filename(),
				"{:06} is a minimum width")
		}},
		{"test_content_matches_detects_differing_events", func(t *testing.T) {
			t.Helper()
			a := NewSegment("host-a", 1, segNow)
			a.Append(withID(testEvent("zone-a", 1), 1))
			require.NoError(t, a.Seal())
			b := a
			assert.True(t, a.ContentMatches(b))
			c := NewSegment("host-a", 1, segNow)
			c.Append(withID(testEvent("zone-a", 2), 2))
			require.NoError(t, c.Seal())
			c.Checksum = a.Checksum // forged
			assert.False(t, a.ContentMatches(c), "event content must be compared, not just CRC")
		}},
		{"test_content_matches_detects_differing_created_at", func(t *testing.T) {
			t.Helper()
			a := NewSegment("host-a", 1, segNow)
			a.Append(withID(testEvent("zone-a", 1), 1))
			require.NoError(t, a.Seal())
			b := a
			b.CreatedAt = "2026-01-02T03:05:01Z"
			assert.False(t, a.ContentMatches(b))
		}},
		{"empty_sealed_segment_crc_is_crc_of_brackets", func(t *testing.T) {
			t.Helper()
			seg := NewSegment("fixture-empty", 1, segNow)
			require.NoError(t, seg.Seal())
			assert.Equal(t, uint32(223132457), seg.Checksum)
			unsealed := NewSegment("fixture-empty", 1, segNow)
			ok, err := unsealed.Verify()
			require.NoError(t, err)
			assert.False(t, ok, "an unsealed empty segment fails verify, as in jilog")
		}},
		{"unknown_class_is_a_serialization_error", func(t *testing.T) {
			t.Helper()
			seg := NewSegment("x", 1, segNow)
			ev := testEvent("z", 1)
			ev.EventClass = "review"
			seg.Append(ev)
			require.ErrorContains(t, seg.Seal(), "serialization error")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestParseFilename(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		source string
		seq    uint64
		ok     bool
	}{
		{name: "padded", in: "host-a-000001.json", source: "host-a", seq: 1, ok: true},
		{name: "dashes_in_source_split_at_last", in: "host-with-dash-000007.json", source: "host-with-dash", seq: 7, ok: true},
		{name: "unpadded", in: "a-1.json", source: "a", seq: 1, ok: true},
		{name: "plus_sign_like_rust_u64_parse", in: "a-+1.json", source: "a", seq: 1, ok: true},
		{name: "wide", in: "a-18446744073709551615.json", source: "a", seq: 18446744073709551615, ok: true},
		{name: "overflow", in: "a-18446744073709551616.json"},
		{name: "no_dash", in: "garbage.json"},
		{name: "not_json", in: "a-1.txt"},
		{name: "extension_is_case_sensitive", in: "a-1.JSON"},
		{name: "empty_seq", in: "a-.json"},
		{name: "double_dash_keeps_dash_in_source", in: "a--1.json", source: "a-", seq: 1, ok: true},
		{name: "sync_conflict_never_parses", in: "host-b-000002.sync-conflict-20260819-123456-ABCDEF.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, seq, ok := ParseFilename(tt.in)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.source, source)
			assert.Equal(t, tt.seq, seq)
		})
	}
}
