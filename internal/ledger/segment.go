package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/serdejson"
)

// ErrIntegrity marks integrity failures: unsealed writes, checksum
// mismatches and conflicting duplicate identities. Its text is jilog's
// LedgerError::IntegrityFailure prefix (error.rs:15).
var ErrIntegrity = errors.New("integrity check failed")

// ErrAppendOnly is returned when storage refuses an UPDATE or DELETE.
var ErrAppendOnly = errors.New("ledger is append-only")

// PublishOutcome is the result of a no-clobber publish or append
// (jilog segment.rs:429-436).
type PublishOutcome int

const (
	// Published means the segment was written and did not exist before.
	Published PublishOutcome = iota
	// AlreadyIdentical means a content-identical copy already existed.
	AlreadyIdentical
)

func (o PublishOutcome) String() string {
	if o == AlreadyIdentical {
		return "already_identical"
	}
	return "published"
}

// Segment is jilog's authoritative storage unit (segment.rs:47-65).
// CreatedAt holds the canonical chrono AutoSi RFC3339 string, so a
// re-marshal reproduces jilog's bytes.
//
//nolint:recvcheck // Append and Seal mutate the segment (spec §26.7); the rest only read it.
type Segment struct {
	Source    string
	SourceSeq uint64
	Checksum  uint32
	CreatedAt string
	Events    []Event
}

// NewSegment ports Segment::new: empty, unsealed, created now.
func NewSegment(source string, seq uint64, now time.Time) Segment {
	return Segment{
		Source:    source,
		SourceSeq: seq,
		CreatedAt: serdejson.FormatTimestamp(now),
		Events:    []Event{},
	}
}

// Append adds an event. Order is the caller's responsibility, as in jilog.
func (s *Segment) Append(e Event) { s.Events = append(s.Events, e) }

// EventsJSON is serde_json::to_vec(&self.events): the exact bytes the
// CRC covers.
func (s Segment) EventsJSON() ([]byte, error) {
	b := []byte{'['}
	for i, e := range s.Events {
		if i > 0 {
			b = append(b, ',')
		}
		var err error
		if b, err = e.appendSerde(b); err != nil {
			return nil, err
		}
	}
	return append(b, ']'), nil
}

// Seal computes the CRC-32 (IEEE) of EventsJSON (segment.rs:128-133).
func (s *Segment) Seal() error {
	b, err := s.EventsJSON()
	if err != nil {
		return err
	}
	s.Checksum = crc32.ChecksumIEEE(b)
	return nil
}

// Verify re-serializes the events and compares the CRC
// (segment.rs:139-143). It errors only when serialization fails.
func (s Segment) Verify() (bool, error) {
	b, err := s.EventsJSON()
	if err != nil {
		return false, err
	}
	return crc32.ChecksumIEEE(b) == s.Checksum, nil
}

// Sealed reports jilog's "sealed" meaning: checksum != 0.
func (s Segment) Sealed() bool { return s.Checksum != 0 }

// Filename is "{source}-{seq:06}.json" (segment.rs:423-425); the width is
// a minimum, larger seqs get more digits.
func (s Segment) Filename() string {
	return fmt.Sprintf("%s-%06d.json", s.Source, s.SourceSeq)
}

// ContentMatches ports content_matches (segment.rs:390-396): identity,
// stored checksum, created_at and every serialized event field.
func (s Segment) ContentMatches(o Segment) bool {
	if s.Source != o.Source || s.SourceSeq != o.SourceSeq || s.Checksum != o.Checksum {
		return false
	}
	if !sameInstant(s.CreatedAt, o.CreatedAt) {
		return false
	}
	a, errA := s.EventsJSON()
	b, errB := o.EventsJSON()
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func sameInstant(a, b string) bool {
	if a == b {
		return true
	}
	ta, errA := ParseTimestamp(a)
	tb, errB := ParseTimestamp(b)
	return errA == nil && errB == nil && ta.Equal(tb)
}

// ParseFilename ports the store listing parse (store.rs:253-262): only
// "*.json", split the stem at the LAST '-', seq via u64 parsing (so
// unpadded "a-1.json" parses).
func ParseFilename(name string) (source string, seq uint64, ok bool) {
	stem, found := strings.CutSuffix(name, ".json")
	if !found {
		return "", 0, false
	}
	dash := strings.LastIndexByte(stem, '-')
	if dash < 0 {
		return "", 0, false
	}
	// Rust's u64::from_str accepts one leading '+'; strconv does not.
	digits := strings.TrimPrefix(stem[dash+1:], "+")
	if digits == "" || digits[0] == '+' || digits[0] == '-' {
		return "", 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return stem[:dash], n, true
}
