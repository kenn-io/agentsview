package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memStore is an in-memory WriterStore. stealNext makes the next append
// lose the race for its seq, as if another writer had taken it.
type memStore struct {
	segs      map[string][]Segment // zone/source -> segments
	stealNext int
	appends   int
}

func newMemStore() *memStore { return &memStore{segs: map[string][]Segment{}} }

func (m *memStore) LatestLedgerSeq(_ context.Context, zone, source string) (uint64, error) {
	var latest uint64
	for _, s := range m.segs[zone+"/"+source] {
		latest = max(latest, s.SourceSeq)
	}
	return latest, nil
}

func (m *memStore) AppendLedgerSegment(_ context.Context, zone string, seg Segment, origin string) (PublishOutcome, error) {
	m.appends++
	if _, err := PrepareAppend(zone, seg, origin); err != nil {
		return Published, err
	}
	key := zone + "/" + seg.Source
	if m.stealNext > 0 {
		m.stealNext--
		thief := NewSegment(seg.Source, seg.SourceSeq, seg.Events[0].Timestamp)
		m.segs[key] = append(m.segs[key], thief)
		return Published, &ConflictError{Zone: zone, Source: seg.Source, Seq: seg.SourceSeq}
	}
	m.segs[key] = append(m.segs[key], seg)
	return Published, nil
}

func TestWriterAppend(t *testing.T) {
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	newWriter := func(st *memStore) *Writer {
		w := NewWriter(st, "zone-a", "av-local", nil)
		w.now = func() time.Time { return now }
		return w
	}
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"fills_zero_fields_and_seals", func(t *testing.T) {
			t.Helper()
			st := newMemStore()
			seg, err := newWriter(st).Append(t.Context(), []Event{
				{EventClass: ClassHealth, PayloadTier: TierMetadataOnly},
				{EventClass: ClassDecision, PayloadTier: TierStructured, SourceSeq: 99},
			})
			require.NoError(t, err)
			assert.Equal(t, uint64(1), seg.SourceSeq)
			assert.Equal(t, "av-local", seg.Source)
			assert.True(t, seg.Sealed())
			ok, err := seg.Verify()
			require.NoError(t, err)
			assert.True(t, ok)
			e := seg.Events[0]
			assert.Equal(t, "zone-a", e.Zone)
			assert.Equal(t, "av-local", e.Source)
			assert.Equal(t, uint64(1), e.SourceSeq, "a zero event seq takes the segment seq")
			assert.Equal(t, uuid.Version(7), e.EventID.Version())
			assert.True(t, e.Timestamp.Equal(now))
			assert.Equal(t, uint64(99), seg.Events[1].SourceSeq, "an explicit event seq is kept")
		}},
		{"next_append_takes_next_seq", func(t *testing.T) {
			t.Helper()
			st := newMemStore()
			w := newWriter(st)
			_, err := w.Append(t.Context(), []Event{{EventClass: ClassHealth, PayloadTier: TierMetadataOnly}})
			require.NoError(t, err)
			seg, err := w.Append(t.Context(), []Event{{EventClass: ClassHealth, PayloadTier: TierMetadataOnly}})
			require.NoError(t, err)
			assert.Equal(t, uint64(2), seg.SourceSeq)
		}},
		{"lost_race_retries_with_fresh_seq", func(t *testing.T) {
			t.Helper()
			st := newMemStore()
			st.stealNext = 2
			seg, err := newWriter(st).Append(t.Context(), []Event{{EventClass: ClassHealth, PayloadTier: TierMetadataOnly}})
			require.NoError(t, err)
			assert.Equal(t, uint64(3), seg.SourceSeq)
			assert.Equal(t, 3, st.appends)
			assert.Equal(t, uint64(3), seg.Events[0].SourceSeq)
		}},
		{"gives_up_after_three_lost_races", func(t *testing.T) {
			t.Helper()
			st := newMemStore()
			st.stealNext = 3
			_, err := newWriter(st).Append(t.Context(), []Event{{EventClass: ClassHealth, PayloadTier: TierMetadataOnly}})
			require.ErrorIs(t, err, ErrIntegrity)
			assert.Contains(t, err.Error(), "lost 3 races")
		}},
		{"rejects_foreign_source_and_zone", func(t *testing.T) {
			t.Helper()
			w := newWriter(newMemStore())
			_, err := w.Append(t.Context(), []Event{{Source: "someone-else", EventClass: ClassHealth, PayloadTier: TierMetadataOnly}})
			require.ErrorContains(t, err, "is not the local source")
			_, err = w.Append(t.Context(), []Event{{Zone: "zone-b", EventClass: ClassHealth, PayloadTier: TierMetadataOnly}})
			require.ErrorContains(t, err, "does not match writer zone")
		}},
		{"rejects_empty_and_oversized_batches", func(t *testing.T) {
			t.Helper()
			w := newWriter(newMemStore())
			_, err := w.Append(t.Context(), nil)
			require.ErrorIs(t, err, ErrNoEvents)
			_, err = w.Append(t.Context(), make([]Event, MaxEventsPerSegment+1))
			require.ErrorContains(t, err, "1000-event segment limit")
		}},
		{"invalid_class_fails_before_storage", func(t *testing.T) {
			t.Helper()
			st := newMemStore()
			_, err := newWriter(st).Append(t.Context(), []Event{{EventClass: "review", PayloadTier: TierMetadataOnly}})
			require.ErrorContains(t, err, "serialization error")
			assert.Equal(t, 0, st.appends)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestZoneWriters(t *testing.T) {
	st := newMemStore()
	zw := NewZoneWriters(st, "av-local", []string{"default", "ops"}, nil)
	var sink Sink = zw
	require.NoError(t, sink.Append(t.Context(), "", []Event{{EventClass: ClassHealth, PayloadTier: TierMetadataOnly}}))
	require.NoError(t, sink.Append(t.Context(), "ops", []Event{{EventClass: ClassHealth, PayloadTier: TierMetadataOnly}}))
	assert.Len(t, st.segs["default/av-local"], 1, `"" is the first (default) zone`)
	assert.Len(t, st.segs["ops/av-local"], 1)
	err := sink.Append(t.Context(), "nope", []Event{{EventClass: ClassHealth, PayloadTier: TierMetadataOnly}})
	require.ErrorContains(t, err, `zone "nope" is not configured`)
	assert.Equal(t, "av-local", zw.Source())
	assert.NotErrorIs(t, err, ErrIntegrity)
}
