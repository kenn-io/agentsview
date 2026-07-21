// ABOUTME: human-mode rendering helpers for `session list` — the
// ABOUTME: resume-oriented table (in-flight marker, ID, AGE, AGENT,
// ABOUTME: PROJECT, BRANCH, MSGS, NAME, ~-collapsed CWD) and the field
// ABOUTME: formatters it relies on.
package main

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/db"
)

// resumeActiveWindow flags a session as in-flight when its last activity
// is within this window of now. It mirrors agentsview-mcp's activeWindow:
// recent activity means the session is resumable right now, so it gets the
// in-flight marker and `--resume`/`--active` surface it.
const resumeActiveWindow = 15 * time.Minute

// activeMarker is the in-flight dot rendered for recently-active sessions.
const activeMarker = "●"

// sessionActivityTime returns a session's last-activity time: EndedAt if
// present and parseable, else StartedAt, else CreatedAt, else the zero time.
// Timestamps are stored as RFC3339/RFC3339Nano strings, so both layouts are
// accepted. CreatedAt is the final fallback so AGE and the in-flight marker
// agree with the backend active_since filter and recent sort, which fall back
// to COALESCE(ended_at, started_at, created_at).
func sessionActivityTime(s db.Session) time.Time {
	for _, ts := range []*string{s.EndedAt, s.StartedAt} {
		if ts == nil {
			continue
		}
		if t, ok := parseSessionTime(*ts); ok {
			return t
		}
	}
	if t, ok := parseSessionTime(s.CreatedAt); ok {
		return t
	}
	return time.Time{}
}

// parseSessionTime parses an RFC3339 or RFC3339Nano timestamp.
func parseSessionTime(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// isSessionRecentlyActive reports whether the session's last activity is
// within resumeActiveWindow of now. A timestamp slightly in the future
// (clock skew, or a live session whose last activity is momentarily ahead
// of our clock) counts as active, matching the "now" label humanizeAge
// gives it.
func isSessionRecentlyActive(s db.Session, now time.Time) bool {
	t := sessionActivityTime(s)
	if t.IsZero() {
		return false
	}
	return now.Sub(t) < resumeActiveWindow
}

// humanizeAgeRelative renders the age of t relative to now using the shared
// relative buckets: "now" for a future/clock-skewed timestamp, then
// seconds/minutes/hours/days. It returns ("", false) once the age reaches a
// week, leaving the absolute-format choice to each caller — session list keeps
// a year-less "Jan 02"; search disambiguates the year.
func humanizeAgeRelative(t, now time.Time) (string, bool) {
	d := now.Sub(t)
	switch {
	case d < 0:
		return "now", true
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s", true
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m", true
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h", true
	case d < 7*24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d", true
	default:
		return "", false
	}
}

// humanizeSessionAge renders a session's last-activity time relative to
// now: seconds/minutes/hours/days for the recent past, an absolute year-less
// month and day beyond a week, and an em dash when no timestamp is available.
func humanizeSessionAge(s db.Session, now time.Time) string {
	t := sessionActivityTime(s)
	if t.IsZero() {
		return emDash
	}
	if rel, ok := humanizeAgeRelative(t, now); ok {
		return rel
	}
	return t.Format("Jan 02")
}

// sessionDisplayName is the session's human label: DisplayName when set,
// otherwise the first message.
func sessionDisplayName(s db.Session) string {
	if s.DisplayName != nil && *s.DisplayName != "" {
		return *s.DisplayName
	}
	if s.FirstMessage != nil {
		return *s.FirstMessage
	}
	return ""
}

// emDash stands in for empty optional fields (codex sessions have no cwd
// or branch) so a column reads as intentionally-absent, not blank.
const emDash = "—"

// orEmDash returns s, or an em dash when s is blank.
func orEmDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return emDash
	}
	return s
}

// collapseHome rewrites a home-prefixed cwd to ~ form for a compact,
// relaunch-friendly path. A blank cwd becomes an em dash.
func collapseHome(cwd, home string) string {
	if strings.TrimSpace(cwd) == "" {
		return emDash
	}
	if home != "" {
		if cwd == home {
			return "~"
		}
		if strings.HasPrefix(cwd, home+"/") {
			return "~" + cwd[len(home):]
		}
	}
	return cwd
}

// truncName collapses internal whitespace and trims to max runes, adding
// an ellipsis when it cut anything.
func truncName(s string, max int) string {
	t, cut := truncateRunes(collapseWhitespace(s), max)
	if cut {
		return t + "…"
	}
	return t
}

// collapseWhitespace folds runs of whitespace (including newlines) into a
// single space so a multi-line first message stays on one table row.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes cuts s to at most max runes on a rune boundary, returning
// the (possibly shortened) string and whether truncation occurred.
func truncateRunes(s string, max int) (string, bool) {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s, false
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i], true
		}
		n++
	}
	return s, false
}
