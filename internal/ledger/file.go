package ledger

import (
	"errors"
	"fmt"
	"strconv"

	"go.kenn.io/agentsview/internal/serdejson"
)

// MarshalSegmentFile returns serde_json::to_string_pretty(&segment)
// (segment.rs:221): 2-space indent, "key": value, empty [] and {}, and no
// trailing newline.
func MarshalSegmentFile(s Segment) ([]byte, error) {
	events, err := s.EventsJSON()
	if err != nil {
		return nil, err
	}
	b := []byte(`{"source":`)
	b = appendString(b, s.Source)
	b = append(b, `,"source_seq":`...)
	b = strconv.AppendUint(b, s.SourceSeq, 10)
	b = append(b, `,"checksum":`...)
	b = strconv.AppendUint(b, uint64(s.Checksum), 10)
	b = append(b, `,"created_at":`...)
	b = appendString(b, s.CreatedAt)
	b = append(b, `,"events":`...)
	b = append(b, events...)
	b = append(b, '}')
	return prettyFromCompact(b), nil
}

// ParseSegmentFile parses a segment file the way serde's derived
// Deserialize for Segment does (unknown fields ignored, missing required
// fields rejected). It does NOT verify the checksum, like
// read_from_file (segment.rs:409-414).
func ParseSegmentFile(b []byte) (Segment, error) {
	v, err := serdejson.Decode(b)
	if err != nil {
		return Segment{}, fmt.Errorf("serialization error: %w", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return Segment{}, errors.New("serialization error: segment is not an object")
	}
	var s Segment
	if s.Source, err = requiredString(obj, "source"); err != nil {
		return Segment{}, fmt.Errorf("serialization error: %w", err)
	}
	if s.SourceSeq, err = requiredUint(obj, "source_seq", 64); err != nil {
		return Segment{}, fmt.Errorf("serialization error: %w", err)
	}
	sum, err := requiredUint(obj, "checksum", 32)
	if err != nil {
		return Segment{}, fmt.Errorf("serialization error: %w", err)
	}
	s.Checksum = uint32(sum)
	created, err := requiredString(obj, "created_at")
	if err != nil {
		return Segment{}, fmt.Errorf("serialization error: %w", err)
	}
	t, err := ParseTimestamp(created)
	if err != nil {
		return Segment{}, fmt.Errorf("serialization error: created_at: %w", err)
	}
	s.CreatedAt = serdejson.FormatTimestamp(t)
	raw, ok := obj["events"]
	if !ok {
		return Segment{}, errors.New("serialization error: missing field `events`")
	}
	if s.Events, err = eventsFromValue(raw); err != nil {
		return Segment{}, err
	}
	return s, nil
}

// prettyFromCompact re-indents compact JSON exactly like serde_json's
// PrettyFormatter. Input must be compact (no insignificant whitespace),
// which everything this package produces is.
func prettyFromCompact(in []byte) []byte {
	out := make([]byte, 0, len(in)*2)
	depth := 0
	inString, escaped := false, false
	newline := func() {
		out = append(out, '\n')
		for range depth {
			out = append(out, ' ', ' ')
		}
	}
	for i := 0; i < len(in); i++ {
		c := in[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			out = append(out, c)
		case '{', '[':
			if i+1 < len(in) && (in[i+1] == '}' || in[i+1] == ']') {
				out = append(out, c, in[i+1])
				i++
				continue
			}
			out = append(out, c)
			depth++
			newline()
		case '}', ']':
			depth--
			newline()
			out = append(out, c)
		case ',':
			out = append(out, c)
			newline()
		case ':':
			out = append(out, ':', ' ')
		default:
			out = append(out, c)
		}
	}
	return out
}
