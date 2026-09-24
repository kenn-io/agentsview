package ledger

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
)

// WriterStore is the storage a Writer appends through. *db.DB and
// *postgres.Store implement it.
type WriterStore interface {
	LatestLedgerSeq(ctx context.Context, zone, source string) (uint64, error)
	AppendLedgerSegment(ctx context.Context, zone string, seg Segment, origin string) (PublishOutcome, error)
}

// Sink receives events from producers. *ZoneWriters implements it; callers
// hold a nil Sink when the ledger is disabled and skip the call.
type Sink interface {
	Append(ctx context.Context, zone string, events []Event) error
}

// ErrNoEvents is returned for an empty append.
var ErrNoEvents = errors.New("ledger: no events to append")

// maxAppendAttempts bounds retries when another writer took the next seq.
const maxAppendAttempts = 3

// Writer appends batches for one (zone, local source) as sealed segments.
// jilog has no writer (store.rs:113-171 is the caller's job); this is the
// agentsview single-writer: seq = latest+1 chosen under exclusive, and a
// lost race on the primary key is retried with a fresh seq, never
// overwritten.
type Writer struct {
	zone      string
	source    string
	store     WriterStore
	exclusive func(func() error) error
	now       func() time.Time
	mu        sync.Mutex
}

// NewWriter returns a Writer. exclusive serializes seq selection with the
// rest of the archive's writers (engine.RunExclusive on SQLite); nil runs
// the work directly.
func NewWriter(store WriterStore, zone, source string, exclusive func(func() error) error) *Writer {
	if exclusive == nil {
		exclusive = func(work func() error) error { return work() }
	}
	return &Writer{zone: zone, source: source, store: store, exclusive: exclusive, now: time.Now}
}

// Append writes events as one sealed segment and returns it. Zero
// EventID, Timestamp and SourceSeq are filled (SourceSeq with the
// segment's seq); an empty Zone or Source is filled, a different one is an
// error, so only the local source is ever written.
func (w *Writer) Append(ctx context.Context, events []Event) (Segment, error) {
	if len(events) == 0 {
		return Segment{}, ErrNoEvents
	}
	if len(events) > MaxEventsPerSegment {
		return Segment{}, fmt.Errorf("ledger: %d events exceed the %d-event segment limit", len(events), MaxEventsPerSegment)
	}
	now := w.now().UTC()
	filled := make([]Event, len(events))
	keepSeq := make([]bool, len(events))
	for i, e := range events {
		switch e.Zone {
		case "":
			e.Zone = w.zone
		case w.zone:
		default:
			return Segment{}, fmt.Errorf("ledger: event zone %q does not match writer zone %q", e.Zone, w.zone)
		}
		switch e.Source {
		case "":
			e.Source = w.source
		case w.source:
		default:
			return Segment{}, fmt.Errorf("ledger: event source %q is not the local source %q", e.Source, w.source)
		}
		if e.EventID == uuid.Nil {
			e.EventID = NewEventID()
		}
		if e.Timestamp.IsZero() {
			e.Timestamp = now
		}
		keepSeq[i] = e.SourceSeq != 0
		filled[i] = e
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	var lastErr error
	for range maxAppendAttempts {
		var seg Segment
		err := w.exclusive(func() error {
			latest, err := w.store.LatestLedgerSeq(ctx, w.zone, w.source)
			if err != nil {
				return err
			}
			if latest >= math.MaxInt64 {
				return fmt.Errorf("ledger: source %s has exhausted its sequence numbers", w.source)
			}
			seg = NewSegment(w.source, latest+1, now)
			for i, e := range filled {
				if !keepSeq[i] {
					e.SourceSeq = seg.SourceSeq
				}
				seg.Append(e)
			}
			if err := seg.Seal(); err != nil {
				return err
			}
			_, err = w.store.AppendLedgerSegment(ctx, w.zone, seg, OriginLocal)
			return err
		})
		if err == nil {
			return seg, nil
		}
		if conflict, ok := errors.AsType[*ConflictError](err); ok && conflict.Source == w.source {
			lastErr = err
			continue
		}
		return Segment{}, err
	}
	return Segment{}, fmt.Errorf("ledger: append lost %d races for the next seq: %w", maxAppendAttempts, lastErr)
}

// ZoneWriters routes appends to one Writer per configured zone for the
// local source. zones[0] is the default zone used for "".
type ZoneWriters struct {
	source      string
	defaultZone string
	writers     map[string]*Writer
}

// NewZoneWriters builds a Writer per zone; zones[0] is the default zone.
func NewZoneWriters(store WriterStore, source string, zones []string, exclusive func(func() error) error) *ZoneWriters {
	z := &ZoneWriters{source: source, writers: make(map[string]*Writer, len(zones))}
	for i, id := range zones {
		if i == 0 {
			z.defaultZone = id
		}
		z.writers[id] = NewWriter(store, id, source, exclusive)
	}
	return z
}

// Source is the local source these writers append as.
func (z *ZoneWriters) Source() string { return z.source }

// Append implements Sink; "" is the default zone.
func (z *ZoneWriters) Append(ctx context.Context, zone string, events []Event) error {
	_, err := z.AppendSegment(ctx, zone, events)
	return err
}

// AppendSegment is Append returning the written segment.
func (z *ZoneWriters) AppendSegment(ctx context.Context, zone string, events []Event) (Segment, error) {
	if zone == "" {
		zone = z.defaultZone
	}
	w, ok := z.writers[zone]
	if !ok {
		return Segment{}, fmt.Errorf("ledger: zone %q is not configured", zone)
	}
	return w.Append(ctx, events)
}
