package parser

import (
	"log"
	"time"
)

// timestampLayouts lists accepted timestamp formats in priority
// order.
var timestampLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.000000",
	"2006-01-02T15:04:05.000Z",
	"2006-01-02T15:04:05Z",
	"2006-01-02T15:04:05.000-07:00",
	"2006-01-02T15:04:05-07:00",
	"2006-01-02 15:04:05",
}

// parseTimestamp parses a raw timestamp string into a time.Time.
// Returns zero time if the string is empty or unparseable.
func parseTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	for _, layout := range timestampLayouts {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func logParseError(ts string) {
	const maxLen = 100
	if len(ts) > maxLen {
		ts = ts[:maxLen] + "..."
	}
	log.Printf(
		"unparseable timestamp %q: no matching layout", ts,
	)
}
