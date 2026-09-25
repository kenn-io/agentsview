package serdejson

import (
	"fmt"
	"time"
)

// FormatTimestamp renders t in UTC like chrono's
// to_rfc3339_opts(SecondsFormat::AutoSi, true): no fraction when the
// nanoseconds are zero, otherwise 3, 6 or 9 digits, whichever is exact.
func FormatTimestamp(t time.Time) string {
	t = t.UTC()
	base := t.Format("2006-01-02T15:04:05")
	ns := t.Nanosecond()
	switch {
	case ns == 0:
		return base + "Z"
	case ns%1_000_000 == 0:
		return fmt.Sprintf("%s.%03dZ", base, ns/1_000_000)
	case ns%1_000 == 0:
		return fmt.Sprintf("%s.%06dZ", base, ns/1_000)
	default:
		return fmt.Sprintf("%s.%09dZ", base, ns)
	}
}
