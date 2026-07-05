// ABOUTME: Response-shaping helpers shared by the MCP tools: limit
// ABOUTME: clamping, rune-safe truncation, FTS query building, the
// ABOUTME: self-reference exclusion window, and the role filter.
package mcp

import (
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// activeExclusionWindow is how recently a session must have been
	// active for search tools to exclude it by default. This keeps an
	// agent from retrieving its own in-progress conversation (which
	// agentsview syncs in near-real-time) and chasing its tail.
	activeExclusionWindow = 10 * time.Minute

	defaultMaxCharsPerMessage = 2000
	maxMaxCharsPerMessage     = 20000
	defaultMessageLimit       = 20
	maxMessageLimit           = 100
	defaultSearchLimit        = 10
	maxSearchLimit            = 30
	defaultListLimit          = 20
	maxListLimit              = 100

	// overviewTailFetch is how many trailing messages the overview
	// tool fetches to find the last few non-system, role-allowed ones.
	overviewTailFetch = 10
	// overviewLastMessages is how many surfaced messages the overview
	// returns.
	overviewLastMessages = 3
	// overviewMaxChars caps each surfaced overview message.
	overviewMaxChars = 500
	// nameMaxChars caps session display names in list/search results.
	nameMaxChars = 200
	// contextMessageMaxChars caps each search_content context_before/
	// context_after message.
	contextMessageMaxChars = 500
)

// truncate cuts s to at most max runes on a rune boundary, returning
// the (possibly shortened) string and whether truncation occurred.
func truncate(s string, max int) (string, bool) {
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

// clampLimit normalizes a requested page size into [1, max], using
// def when the request is unset or out of range.
func clampLimit(requested, def, max int) int {
	if requested <= 0 || requested > max {
		return def
	}
	return requested
}

// buildSearchQuery turns a user's search input into an FTS5 expression
// that is safe and useful to hand to the agentsview search layer. Two
// sharp edges bite a naive caller: bare punctuation in a token (the
// hyphen in "agentsview-mcp", a colon in "foo:bar", a stray paren) is
// parsed as query syntax and raises an error, and an unquoted multi-word
// query is matched as a single exact phrase, so natural-language input
// quietly returns nothing.
//
// We quote each whitespace-separated term, escaping any embedded quote by
// doubling it. Quoting makes punctuation literal inside the term, and
// space-separated quoted phrases combine under FTS5's implicit AND - so
// every term must appear without demanding an exact phrase. The leading
// quote also makes db.PrepareFTSQuery pass the query through unchanged
// instead of re-wrapping it as a phrase.
//
// A query the caller already opened with a double quote is treated as a
// deliberate FTS expression (including an explicit "exact phrase") and is
// passed through untouched.
func buildSearchQuery(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, `"`) {
		return raw
	}
	var b strings.Builder
	for i, term := range strings.Fields(raw) {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('"')
		b.WriteString(strings.ReplaceAll(term, `"`, `""`))
		b.WriteByte('"')
	}
	return b.String()
}

// isActiveSince reports whether an RFC3339 timestamp falls within the
// exclusion window ending at now. Unparseable or empty timestamps are
// treated as not active, so such results are kept.
func isActiveSince(ts string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return now.Sub(t) < activeExclusionWindow
}

// roleAllowed reports whether a message role passes the filter. An empty
// filter allows user and assistant messages only; tool dumps and system
// messages must be requested explicitly.
func roleAllowed(role string, roles []string) bool {
	if len(roles) == 0 {
		return role == "user" || role == "assistant"
	}
	return slices.Contains(roles, role)
}
