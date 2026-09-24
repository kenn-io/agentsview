package ledger

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/serdejson"
)

// Segment origins recorded in ledger_segments.origin.
const (
	OriginLocal  = "local"  // written by this host's Writer
	OriginImport = "import" // read from a jilog-format segments directory
	OriginSpool  = "spool"  // ingested from a jilog spool (PR 16)
	OriginPush   = "push"   // replicated into the PostgreSQL hub (PR 14)
)

// MaxEventsPerSegment bounds one append (and one API request).
const MaxEventsPerSegment = 1000

// sourceNameRule is jilog's InvalidSource text (ledger-spool error.rs).
const sourceNameRule = "^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$"

// ConflictError reports a segment identity that already exists with
// different content. It is an integrity failure and never an overwrite
// (jilog segment.rs:355-366).
type ConflictError struct {
	Zone   string
	Source string
	Seq    uint64
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf(
		"integrity check failed: duplicate identity %s:%d in zone %s with DIFFERENT content — not overwriting",
		e.Source, e.Seq, e.Zone)
}

// Unwrap makes errors.Is(err, ErrIntegrity) true.
func (e *ConflictError) Unwrap() error { return ErrIntegrity }

// EventRow is one ledger_events projection row, identical on SQLite and
// PostgreSQL except for how Timestamp is bound.
type EventRow struct {
	EventID       string
	Source        string
	SourceSeq     int64
	Timestamp     time.Time // UTC, truncated to microseconds
	CorrelationID *string
	CausationID   *string
	ActorRef      *string
	ObjectRef     *string
	EventClass    string
	PayloadTier   string
	Payload       *string // compact serde JSON; nil when the payload is null
	Subsystem     string
	Summary       string
	EventJSON     string // compact serde JSON of the whole event
}

// PreparedSegment is a validated segment plus its storage rows. Both
// backends build it with PrepareAppend so their checks cannot drift.
type PreparedSegment struct {
	Zone       string
	Source     string
	Seq        int64
	Checksum   int64
	CreatedAt  string
	EventsJSON string
	Origin     string
	Events     []EventRow
}

// PrepareAppend validates a segment for storage the way jilog's
// write_segment and refresh_from_store do (store.rs:113-119,
// db.rs:313-376) and projects its events. Unsealed non-empty segments and
// checksum mismatches are integrity failures.
func PrepareAppend(zone string, seg Segment, origin string) (PreparedSegment, error) {
	if !ValidSourceName(zone) {
		return PreparedSegment{}, fmt.Errorf("invalid ledger zone %q: must match %s", zone, sourceNameRule)
	}
	if !ValidSourceName(seg.Source) {
		return PreparedSegment{}, fmt.Errorf("invalid segment source %q: must match %s", seg.Source, sourceNameRule)
	}
	switch origin {
	case OriginLocal, OriginImport, OriginSpool, OriginPush:
	default:
		return PreparedSegment{}, fmt.Errorf("invalid ledger segment origin %q", origin)
	}
	if seg.SourceSeq == 0 {
		return PreparedSegment{}, errors.New("segment source_seq must be at least 1")
	}
	if seg.SourceSeq > math.MaxInt64 {
		return PreparedSegment{}, fmt.Errorf(
			"source_seq exceeds i64::MAX (%d) — not representable in the ledger tables", int64(math.MaxInt64))
	}
	if seg.Checksum == 0 && len(seg.Events) > 0 {
		return PreparedSegment{}, fmt.Errorf("%w: segment must be sealed before writing (checksum is 0)", ErrIntegrity)
	}
	if _, err := ParseTimestamp(seg.CreatedAt); err != nil {
		return PreparedSegment{}, fmt.Errorf("segment created_at %q: %w", seg.CreatedAt, err)
	}
	ok, err := seg.Verify()
	if err != nil {
		return PreparedSegment{}, err
	}
	if !ok {
		return PreparedSegment{}, fmt.Errorf("%w: checksum mismatch for %s", ErrIntegrity, seg.Filename())
	}
	eventsJSON, err := seg.EventsJSON()
	if err != nil {
		return PreparedSegment{}, err
	}
	if !utf8.Valid(eventsJSON) {
		return PreparedSegment{}, fmt.Errorf("serialization error: %s holds invalid UTF-8", seg.Filename())
	}
	rows := make([]EventRow, 0, len(seg.Events))
	for _, e := range seg.Events {
		if e.SourceSeq > math.MaxInt64 {
			return PreparedSegment{}, fmt.Errorf(
				"an event's source_seq exceeds i64::MAX (%d) — not representable in the ledger tables", int64(math.MaxInt64))
		}
		row, err := projectEvent(e)
		if err != nil {
			return PreparedSegment{}, err
		}
		rows = append(rows, row)
	}
	return PreparedSegment{
		Zone:       zone,
		Source:     seg.Source,
		Seq:        int64(seg.SourceSeq),
		Checksum:   int64(seg.Checksum),
		CreatedAt:  seg.CreatedAt,
		EventsJSON: string(eventsJSON),
		Origin:     origin,
		Events:     rows,
	}, nil
}

// SameContent reports whether a stored row is this segment: the same
// checksum, created_at instant and events bytes (content_matches).
func (p PreparedSegment) SameContent(checksum int64, createdAt, eventsJSON string) bool {
	return checksum == p.Checksum && sameInstant(createdAt, p.CreatedAt) && eventsJSON == p.EventsJSON
}

// Conflict returns the ConflictError for this segment's identity.
func (p PreparedSegment) Conflict() error {
	return &ConflictError{Zone: p.Zone, Source: p.Source, Seq: uint64(p.Seq)}
}

func projectEvent(e Event) (EventRow, error) {
	eventJSON, err := e.MarshalSerde()
	if err != nil {
		return EventRow{}, err
	}
	row := EventRow{
		EventID:     e.EventID.String(),
		Source:      e.Source,
		SourceSeq:   int64(e.SourceSeq),
		Timestamp:   e.Timestamp.UTC().Truncate(time.Microsecond),
		ActorRef:    e.ActorRef,
		ObjectRef:   e.ObjectRef,
		EventClass:  string(e.EventClass),
		PayloadTier: string(e.PayloadTier),
		Subsystem:   Subsystem(e),
		Summary:     Summary(e),
		EventJSON:   string(eventJSON),
	}
	if e.CorrelationID != nil {
		s := e.CorrelationID.String()
		row.CorrelationID = &s
	}
	if e.CausationID != nil {
		s := e.CausationID.String()
		row.CausationID = &s
	}
	if e.Payload != nil {
		b, err := serdejson.Compact(e.Payload)
		if err != nil {
			return EventRow{}, fmt.Errorf("serialization error: %w", err)
		}
		s := string(b)
		row.Payload = &s
	}
	return row, nil
}

// StorageTimestamp is the SQLite text form of a ledger timestamp: UTC,
// fixed six fractional digits, so lexical order is time order and matches
// PostgreSQL's microsecond TIMESTAMPTZ.
func StorageTimestamp(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
}

// EncodeCheckpoint returns the failures_json and missing_json column
// values for a checkpoint (JSON arrays, never null).
func EncodeCheckpoint(c VerifyCheckpoint) (failuresJSON, missingJSON string, err error) {
	failures := c.Failures
	if failures == nil {
		failures = [][3]string{}
	}
	missing := c.Missing
	if missing == nil {
		missing = [][2]string{}
	}
	f, err := json.Marshal(failures)
	if err != nil {
		return "", "", fmt.Errorf("encoding ledger verify state: %w", err)
	}
	m, err := json.Marshal(missing)
	if err != nil {
		return "", "", fmt.Errorf("encoding ledger verify state: %w", err)
	}
	return string(f), string(m), nil
}

// DecodeCheckpoint rebuilds a stored checkpoint. A row that no longer
// parses degrades to the zero checkpoint, so the next verify of that
// source is a full one (jilog store.rs:502-507).
func DecodeCheckpoint(verifiedSeq int64, failuresJSON, missingJSON string) *VerifyCheckpoint {
	c := &VerifyCheckpoint{VerifiedSeq: uint64(verifiedSeq)}
	if verifiedSeq < 0 ||
		json.Unmarshal([]byte(failuresJSON), &c.Failures) != nil ||
		json.Unmarshal([]byte(missingJSON), &c.Missing) != nil {
		return &VerifyCheckpoint{}
	}
	return c
}
