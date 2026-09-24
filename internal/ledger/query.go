package ledger

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/agentsview/internal/serdejson"
)

// Query selects events. Storage filters in SQL (D20): Since, Class and
// Until, Subsystems are applied before Limit, unlike jilog's newest-5xlimit
// window (query.rs:210).
type Query struct {
	Since time.Time
	// Until, when non-zero, keeps events strictly before it.
	Until      time.Time
	Subsystems []string
	Class      *EventClass
	Zone       string // "" = every stored zone (callers order via SortZoneEvents, PR 15)
	Limit      int    // per zone
}

// ZoneEvents is one zone's result, newest first.
type ZoneEvents struct {
	Zone   string
	Events []Event
}

// Subsystem ports extract_subsystem (query.rs:238-248): payload.subsystem
// when it is a string, else object_ref minus the "subsystem:" prefix,
// else "".
func Subsystem(e Event) string {
	if obj, ok := e.Payload.(map[string]any); ok {
		if s, ok := obj["subsystem"].(string); ok {
			return s
		}
	}
	if e.ObjectRef != nil {
		if s, ok := strings.CutPrefix(*e.ObjectRef, "subsystem:"); ok {
			return s
		}
	}
	return ""
}

// hasSubsystem distinguishes "no subsystem" from an empty-string one.
func hasSubsystem(e Event) (string, bool) {
	if obj, ok := e.Payload.(map[string]any); ok {
		if s, ok := obj["subsystem"].(string); ok {
			return s, true
		}
	}
	if e.ObjectRef != nil {
		if s, ok := strings.CutPrefix(*e.ObjectRef, "subsystem:"); ok {
			return s, true
		}
	}
	return "", false
}

// Summary is payload.summary when it is a string, else "" (query.rs:351-356).
func Summary(e Event) string {
	if obj, ok := e.Payload.(map[string]any); ok {
		if s, ok := obj["summary"].(string); ok {
			return s
		}
	}
	return ""
}

// GlobMatch ports glob_match (query.rs:251-257): a trailing '*' is a
// prefix match, anything else is exact.
func GlobMatch(pattern, value string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(value, prefix)
	}
	return pattern == value
}

// SubsystemMatches ports subsystem_matches (query.rs:227-235): no patterns
// match everything; with patterns, an event without a subsystem fails.
func SubsystemMatches(patterns []string, e Event) bool {
	if len(patterns) == 0 {
		return true
	}
	name, ok := hasSubsystem(e)
	if !ok {
		return false
	}
	for _, p := range patterns {
		if GlobMatch(p, name) {
			return true
		}
	}
	return false
}

// ParseSince ports parse_since (query.rs:280-303). Suffixes are tried in
// the order h, d, w with a signed count; otherwise YYYY-MM-DD is UTC
// midnight. Error texts are jilog's context strings.
func ParseSince(s string, now time.Time) (time.Time, error) {
	units := []struct {
		suffix string
		noun   string
		unit   time.Duration
	}{
		{"h", "hour", time.Hour},
		{"d", "day", 24 * time.Hour},
		{"w", "week", 7 * 24 * time.Hour},
	}
	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			count, err := strconv.ParseInt(n, 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("invalid %s count: %s", u.noun, s)
			}
			return now.UTC().Add(-time.Duration(count) * u.unit), nil
		}
	}
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date: %s (expected Nh, Nd, Nw, or YYYY-MM-DD)", s)
	}
	return d.UTC(), nil
}

// FormatText renders jilog query's text output (query.rs:327-362),
// including the trailing newline of every println!.
func FormatText(results []ZoneEvents, since string, patterns []string) string {
	var b strings.Builder
	total := 0
	for _, z := range results {
		total += len(z.Events)
	}
	if total == 0 {
		scope := ""
		if len(patterns) > 0 {
			scope = " matching " + rustDebugStrings(patterns)
		}
		fmt.Fprintf(&b, "No events found since %s%s.\n", since, scope)
		return b.String()
	}
	fmt.Fprintf(&b, "Found %d event(s) since %s\n", total, since)
	if len(patterns) > 0 {
		fmt.Fprintf(&b, "Subsystem filter: %s\n", rustDebugStrings(patterns))
	}
	b.WriteString("\n")
	for _, z := range results {
		fmt.Fprintf(&b, "Zone: %s (%d events)\n", z.Zone, len(z.Events))
		for _, e := range z.Events {
			sub, ok := hasSubsystem(e)
			if !ok {
				sub = "?"
			}
			fmt.Fprintf(&b, "  [%s] %s %s %s\n",
				e.Timestamp.UTC().Format("2006-01-02 15:04:05"),
				padRight(e.EventClass.DebugLower(), 14),
				padRight(sub, 30),
				Summary(e))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// padRight is Rust's `{:N}` for strings: a minimum width in chars,
// left-aligned, never truncating.
func padRight(s string, width int) string {
	n := len([]rune(s))
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// rustDebugStrings renders a Vec<String> with Rust's Debug: ["a", "b"].
func rustDebugStrings(items []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(rustDebugString(s))
	}
	b.WriteByte(']')
	return b.String()
}

// rustDebugString approximates str's Debug escaping: \t \r \n \\ \" \0,
// and \u{..} for non-printable characters.
func rustDebugString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case 0:
			b.WriteString(`\0`)
		default:
			if unicode.IsPrint(r) || r == ' ' {
				b.WriteRune(r)
			} else {
				fmt.Fprintf(&b, `\u{%x}`, r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// FormatJSON renders jilog query's JSON output (query.rs:309-325): a
// pretty array of {"event": <event with sorted keys>, "zone": <id>}, with
// no trailing newline (the CLI adds println!'s newline).
func FormatJSON(results []ZoneEvents) ([]byte, error) {
	b := []byte{'['}
	first := true
	for _, z := range results {
		for _, e := range z.Events {
			if !first {
				b = append(b, ',')
			}
			first = false
			var err error
			b = append(b, `{"event":`...)
			if b, err = appendSortedEvent(b, e); err != nil {
				return nil, err
			}
			b = append(b, `,"zone":`...)
			b = appendString(b, z.Zone)
			b = append(b, '}')
		}
	}
	b = append(b, ']')
	return prettyFromCompact(b), nil
}

// appendSortedEvent writes the event with alphabetical keys, which is
// what json!({"event": e}) produces once the event becomes a Value.
func appendSortedEvent(b []byte, e Event) ([]byte, error) {
	payload, err := serdejson.Compact(e.Payload)
	if err != nil {
		return nil, fmt.Errorf("serialization error: %w", err)
	}
	b = append(b, `{"actor_ref":`...)
	b = appendStringPtr(b, e.ActorRef)
	b = append(b, `,"causation_id":`...)
	b = appendUUIDPtr(b, e.CausationID)
	b = append(b, `,"correlation_id":`...)
	b = appendUUIDPtr(b, e.CorrelationID)
	b = append(b, `,"event_class":`...)
	b = appendString(b, string(e.EventClass))
	b = append(b, `,"event_id":`...)
	b = appendString(b, e.EventID.String())
	b = append(b, `,"object_ref":`...)
	b = appendStringPtr(b, e.ObjectRef)
	b = append(b, `,"payload":`...)
	b = append(b, payload...)
	b = append(b, `,"payload_tier":`...)
	b = appendString(b, string(e.PayloadTier))
	b = append(b, `,"source":`...)
	b = appendString(b, e.Source)
	b = append(b, `,"source_seq":`...)
	b = strconv.AppendUint(b, e.SourceSeq, 10)
	b = append(b, `,"timestamp":`...)
	b = appendString(b, serdejson.FormatTimestamp(e.Timestamp))
	b = append(b, `,"zone":`...)
	b = appendString(b, e.Zone)
	return append(b, '}'), nil
}
