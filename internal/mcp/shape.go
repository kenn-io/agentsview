// ABOUTME: Response-shaping helpers shared by the MCP tools: limit
// ABOUTME: clamping, rune-safe truncation, the
// ABOUTME: self-reference exclusion window, and the role filter.
package mcp

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/stringutil"
)

const (
	responseBudgetBytes = 12_000

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
	// maxContentSearchLimit is higher than maxSearchLimit: search_content
	// matches are short snippets, and recall callers page fewer times when
	// a filtered search can return more of them at once.
	maxContentSearchLimit = 50
	contentTotalCap       = 500
	defaultListLimit      = 20
	maxListLimit          = 100

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
func truncate(s string, maximum int) (string, bool) {
	if maximum <= 0 {
		return s, false
	}
	prefix := stringutil.TruncateRunes(s, maximum, "")
	return prefix, len(prefix) < len(s)
}

// clampLimit normalizes a requested page size into [1, max], using
// def when the request is unset or out of range.
func clampLimit(requested, def, maximum int) int {
	if requested <= 0 || requested > maximum {
		return def
	}
	return requested
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

// responseSize matches the SDK's escaping and nil collection format.
func responseSize(out any) int {
	data, _ := json.Marshal(out, jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true), jsontext.AllowInvalidUTF8(true), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
	return len(data)
}

// fitsResponse reports whether the encoded response fits the byte budget.
func fitsResponse(out any) bool {
	return responseSize(out) <= responseBudgetBytes
}

// fitPrefix selects the largest fitting prefix and leaves it built.
func fitPrefix(n int, build func(int) bool) int {
	k := sort.Search(n+1, func(i int) bool { return !build(i) })
	k = max(k-1, 0)
	build(k)
	return k
}

// fitText returns the longest rune prefix whose encoded item fits its budget.
func fitText(text string, budget int, encode func(string) any) string {
	runes := []rune(text)
	n := sort.Search(len(runes), func(i int) bool {
		return responseSize(encode(string(runes[:i+1]))) > budget
	})
	return string(runes[:n])
}
