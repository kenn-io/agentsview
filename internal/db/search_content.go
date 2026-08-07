package db

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-sqlite3"
	"go.kenn.io/agentsview/internal/secrets"
)

// DefaultContentSearchLimit and MaxContentSearchLimit bound result pages.
const (
	DefaultContentSearchLimit = 50
	MaxContentSearchLimit     = 500
	contentSnippetRadius      = 60 // chars of context on each side of a match
)

// ContentSearchFilter parameterises SearchContent. Session-scoping fields
// mirror SessionFilter; they are mapped through buildSessionFilter so the
// include-children / one-shot / orphan logic is shared, not reimplemented.
type ContentSearchFilter struct {
	Pattern       string
	Mode          string   // "substring" (default) | "regex" | "fts" | "semantic" | "hybrid"
	Sources       []string // subset of {"messages","tool_input","tool_result"}
	ExcludeSystem bool

	Project, ExcludeProject, Machine, Agent           string
	Date, DateFrom, DateTo, Timezone, ActiveSince     string
	IncludeChildren, IncludeAutomated, IncludeOneShot bool
	// GitBranch is a branchListSep-joined list of opaque (project, branch) tokens (EncodeBranchFilterToken).
	GitBranch string

	// Scope governs unit visibility for modes "semantic" and "hybrid":
	// "top" drops subordinate units (sidechain runs, subagent/fork
	// sessions), "subordinate" keeps only them, and "all" (or "") keeps
	// both. In those modes it supersedes IncludeChildren, which the other
	// modes keep honoring; validation happens at the API/CLI boundary and
	// an unknown value here is a SearchInputError.
	Scope string

	// RevealSecrets returns raw snippets. It defaults false so snippets are
	// secret-redacted unless a caller (the localhost-gated reveal path)
	// explicitly opts out; a forgotten flag fails safe.
	RevealSecrets bool

	Limit  int
	Cursor int
}

// ContentMatch is one matching message or tool call. Snippet is built from the
// full source field and, unless RevealSecrets is set, has any secret-shaped
// span overlapping the window masked (including secrets that extend past the
// window). The CLI sanitizes it for terminal display.
type ContentMatch struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project"`
	Agent     string `json:"agent"`
	Location  string `json:"location"` // message | tool_input | tool_result
	Role      string `json:"role"`
	ToolName  string `json:"tool_name,omitempty"`
	Ordinal   int    `json:"ordinal"`
	Timestamp string `json:"timestamp"`
	Snippet   string `json:"snippet"`
	// Score is the searcher's relevance score for "semantic"/"hybrid" modes,
	// nil for the other modes which have no comparable ranking signal.
	Score *float64 `json:"score,omitempty"`
	// OrdinalRange is always present: [start, end] of the conversation unit
	// containing the anchor; [ordinal, ordinal] when the anchor is its own
	// unit. Ordinal stays the anchor in every mode.
	OrdinalRange [2]int `json:"ordinal_range"`
	// Subordinate marks a match whose unit is classified subordinate
	// (sidechain run, or subagent/fork session), in every mode.
	Subordinate bool `json:"subordinate,omitempty"`
	// Relationship and ParentSessionID carry the matched session's lineage
	// and Sidechain the anchor message's is_sidechain flag, populated in
	// every mode by the shared Bun hydration or semantic enrichment path.
	Relationship    string `json:"relationship,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	Sidechain       bool   `json:"is_sidechain,omitempty"`
	// ContextBefore and ContextAfter hold the N messages immediately before
	// and after this match's ordinal when the caller requested inline
	// context (ContentSearchRequest.Context > 0). Populated by
	// directBackend.SearchContent, not by the store itself; nil when
	// context was not requested. The anchor message (this match's own
	// ordinal) is excluded from both slices.
	ContextBefore []Message `json:"context_before,omitempty"`
	ContextAfter  []Message `json:"context_after,omitempty"`
}

// ContentSearchPage is a page of matches with an optional next cursor.
type ContentSearchPage struct {
	Matches    []ContentMatch `json:"matches"`
	NextCursor int            `json:"next_cursor,omitempty"`
}

// SearchInputError marks a content-search failure caused by invalid user
// input (bad regex, unknown source, invalid mode) rather than an internal
// fault, so HTTP callers can map it to 400 instead of 500.
type SearchInputError struct{ Msg string }

func (e *SearchInputError) Error() string { return e.Msg }

func searchInputErrorf(format string, a ...any) error {
	return &SearchInputError{Msg: fmt.Sprintf(format, a...)}
}

// contentSessionFilter maps a ContentSearchFilter's session-scoping fields to
// a SessionFilter. Mirroring session list: one-shot and automated sessions
// are excluded by default, and IncludeOneShot/IncludeAutomated opt them back
// in. Comprehensive secret coverage comes from the secrets subsystem
// (scanned over every session at sync), not from search defaults. Shared by
// sessionScopeSubquery (substring/regex/fts) and the semantic-mode
// allowed-session-id lookup so the mapping cannot drift between them.
func contentSessionFilter(f ContentSearchFilter) SessionFilter {
	return SessionFilter{
		Project: f.Project, ExcludeProject: f.ExcludeProject,
		Machine: f.Machine, GitBranch: f.GitBranch, Agent: f.Agent,
		Date: f.Date, DateFrom: f.DateFrom, DateTo: f.DateTo,
		Timezone:         f.Timezone,
		ActiveSince:      f.ActiveSince,
		ExcludeOneShot:   !f.IncludeOneShot,
		ExcludeAutomated: !f.IncludeAutomated,
		IncludeChildren:  f.IncludeChildren,
	}
}

// semanticContentSessionFilter maps a ContentSearchFilter for the
// semantic/hybrid session scope: the shared contentSessionFilter mapping
// plus the child one-shot exemption (SessionFilter.ChildExemptOneShot) —
// child sessions must not be dropped by the one-shot gate in these modes,
// while top-level one-shots keep today's exclusion.
func semanticContentSessionFilter(f ContentSearchFilter) SessionFilter {
	sf := contentSessionFilter(f)
	sf.ChildExemptOneShot = true
	return sf
}

func hasSource(f ContentSearchFilter, name string) bool {
	return slices.Contains(f.Sources, name)
}

// snippetBounds returns the byte window [lo,hi) = [start-radius, end+radius)
// with the padding edges snapped to rune boundaries so a slice never splits a
// multibyte character (the matched span itself is already rune-aligned).
func snippetBounds(text string, start, end, radius int) (int, int) {
	lo := max(start-radius, 0)
	hi := min(end+radius, len(text))
	for lo < start && !utf8.RuneStart(text[lo]) {
		lo++
	}
	for hi > end && hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi--
	}
	return lo, hi
}

// buildSnippet windows body around [start,end) and, unless the filter opts into
// reveal, masks any secret overlapping the window via secrets.RedactWindow
// (which also catches secrets straddling the window edges).
func (f ContentSearchFilter) buildSnippet(body string, start, end int) string {
	lo, hi := snippetBounds(body, start, end, contentSnippetRadius)
	if f.RevealSecrets {
		return body[lo:hi]
	}
	return secrets.RedactWindow(body, lo, hi)
}

// substringSnippet builds the snippet for a substring match: it locates the
// case-insensitive pattern in body (the LIKE already matched, so it is present;
// fall back to the start if case-folding shifts the offset) and windows it.
func (f ContentSearchFilter) substringSnippet(body string) string {
	off := max(CaseInsensitiveIndex(body, f.Pattern), 0)
	return f.buildSnippet(body, off, min(off+len(f.Pattern), len(body)))
}

// CaseInsensitiveIndex returns the byte offset in s of the first
// case-insensitive occurrence of sub, or -1. The offset always indexes s
// directly: it walks s rune by rune instead of searching strings.ToLower(s),
// whose byte length can differ from s — the Kelvin sign U+212A lowercases from
// three bytes to one, U+023A lowercases from two bytes to three — which would
// shift the offset and, when ToLower grows the prefix, push it past len(s) so
// the caller's slice panics. Both backends use it to center snippets.
func CaseInsensitiveIndex(s, sub string) int {
	if sub == "" {
		return 0
	}
	for i := range s {
		if hasFoldPrefixAt(s, i, sub) {
			return i
		}
	}
	return -1
}

// hasFoldPrefixAt reports whether s[i:] begins with sub under simple Unicode
// lower-case folding, compared rune by rune so a case mapping that changes
// UTF-8 byte length cannot desynchronize the two cursors.
func hasFoldPrefixAt(s string, i int, sub string) bool {
	for _, want := range sub {
		if i >= len(s) {
			return false
		}
		got, size := utf8.DecodeRuneInString(s[i:])
		if got != want && unicode.ToLower(got) != unicode.ToLower(want) {
			return false
		}
		i += size
	}
	return true
}

// literalPrefix extracts a required literal prefix from a regex for use
// as a cheap SQL LIKE prefilter. Returns "" when no literal prefix exists.
func literalPrefix(pattern string) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return ""
	}
	prefix, _ := re.LiteralPrefix()
	return prefix
}

// errFTSUnavailable is returned by the "fts" and "hybrid" content-search
// modes when messages_fts is missing or unusable (e.g. the fts5 module
// failed to load), so both modes report the same capability gate.
var errFTSUnavailable = errors.New("search: full-text search is unavailable")

// ftsSnippet builds the snippet for an FTS match. FTS matching is tokenized, so
// there is no exact byte offset; it centers on the first case-insensitive
// occurrence of the de-quoted query phrase, falling back to the query's first
// token, then to the start. Trying the whole phrase first keeps a phrase query
// ("foo bar") centered on the phrase rather than on a stray earlier "foo". The
// approximation only affects snippet centering, not redaction, which scans the
// full body.
func (f ContentSearchFilter) ftsSnippet(body string) string {
	start, end := FTSSnippetRange(f.Pattern, body)
	return f.buildSnippet(body, start, end)
}

// FTSSnippetRange returns the byte range around which FTS-like snippets should
// be centered. It first tries the de-quoted raw phrase, then falls back to the
// first parsed prepared-FTS term, and finally to the start of the body.
func FTSSnippetRange(pattern, body string) (int, int) {
	if phrase := strings.Trim(pattern, "\""); phrase != "" {
		if off := CaseInsensitiveIndex(body, phrase); off >= 0 {
			return off, min(off+len(phrase), len(body))
		}
	}
	for _, term := range FTSTerms(PrepareFTSQuery(pattern)) {
		if term == "" {
			continue
		}
		if off := CaseInsensitiveIndex(body, term); off >= 0 {
			return off, min(off+len(term), len(body))
		}
		if fields := strings.Fields(term); len(fields) > 0 && fields[0] != term {
			first := fields[0]
			if off := CaseInsensitiveIndex(body, first); off >= 0 {
				return off, min(off+len(first), len(body))
			}
		}
		break
	}
	return 0, 0
}

// classifyFTSError maps a malformed FTS query into a SearchInputError so HTTP
// callers return 400 rather than 500. The FTS query's SQL is fixed and every
// argument except the MATCH pattern is parameterized, so a generic
// SQLITE_ERROR can only come from the user-supplied pattern (e.g. unbalanced
// quotes or stray operators). Operational failures (I/O, corruption, busy)
// carry distinct SQLite codes and pass through unchanged.
func classifyFTSError(err error) error {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code == sqlite3.ErrError {
		return &SearchInputError{
			Msg: fmt.Sprintf("search: invalid FTS query: %s", sqliteErr.Error()),
		}
	}
	return err
}

// SemanticOverfetchMin floors the candidate count requested from the
// VectorSearcher (k = max(f.Limit*4, SemanticOverfetchMin)): session-scope
// filtering may drop some of the searcher's top hits, so more are fetched
// than will ultimately be returned.
const SemanticOverfetchMin = 200

// validateSemanticSources returns a SearchInputError unless f.Sources is
// empty or exactly {"messages"}: semantic (and hybrid) search only indexes
// message content, mirroring the --fts messages-only restriction enforced
// upstream for fts mode.
func validateSemanticSources(f ContentSearchFilter) error {
	for _, s := range f.Sources {
		if s != "messages" {
			return searchInputErrorf(
				"search: semantic search only supports the messages source (got %q)", s)
		}
	}
	return nil
}

// ValidateSemanticFilter applies the input validation shared by modes
// "semantic" and "hybrid": sources must be empty or exactly {"messages"},
// and cursor pagination is rejected because both modes return a single
// ranked page rather than an offset-paged result set. It is exported so the
// PostgreSQL and DuckDB backends, which lack a VectorSearcher seam and
// always report ErrSemanticUnavailable for these modes, can run the same
// validation before that capability gate: an invalid request (bad cursor,
// wrong source) must return the same 400 SearchInputError on every backend
// rather than a 501 on backends that check capability first (backend parity,
// see AGENTS.md).
func ValidateSemanticFilter(f ContentSearchFilter) error {
	if err := validateSemanticSources(f); err != nil {
		return err
	}
	if f.Cursor != 0 {
		return searchInputErrorf(
			"semantic search returns a single ranked page; cursor pagination is not supported")
	}
	switch f.Scope {
	case "", "top", "all", "subordinate":
	default:
		return searchInputErrorf(
			"search: invalid scope %q (valid: top, all, subordinate)", f.Scope)
	}
	return nil
}

// ScopeExcludes reports whether a unit with the given subordinate flag
// falls outside the requested scope: "top" excludes subordinate units,
// "subordinate" excludes top-level ones, and ""/"all" exclude nothing.
// Scope filtering runs on each leg's hits before the RRF merge (and before
// the limit), so a scoped search still fills up to Limit from the
// over-fetched candidates instead of returning a post-truncation remnant.
func ScopeExcludes(scope string, subordinate bool) bool {
	switch scope {
	case "top":
		return subordinate
	case "subordinate":
		return !subordinate
	default:
		return false
	}
}

// UnitFusionKey identifies one embedding unit across the hybrid search's
// legs: the mirror's unique (session_id, ordinal_start) pair. The vector leg
// derives it from a VectorHit, the FTS leg from a resolved UnitRef, so hits
// on the same unit fuse.
func UnitFusionKey(sessionID string, ordinalStart int) string {
	return "u\x00" + sessionID + "\x00" + strconv.Itoa(ordinalStart)
}

// MessageFusionKey identifies an FTS hit with no containing unit at message
// granularity, so an exact-string hit outside the embeddable universe never
// vanishes from the fused result. The "m" prefix keeps it disjoint from
// UnitFusionKey's space.
func MessageFusionKey(sessionID string, ordinal int) string {
	return "m\x00" + sessionID + "\x00" + strconv.Itoa(ordinal)
}

// RankedUnit is one leg entry for RRFMerge: a fusion key plus the unit's
// subordinate flag.
type RankedUnit struct {
	Key         string
	Subordinate bool
}

// FusedUnit is one fused RRFMerge result.
type FusedUnit struct {
	Unit  RankedUnit
	Score float64
}

// RRFMerge fuses per-leg unit rankings (best first) with reciprocal-rank
// fusion, penalizing subordinate units by shifting their effective rank
// (rank+5 against a rank constant of 60). Semantic-only search routes its
// single ranked list through this same merge as a one-leg fusion, so the
// penalty applies identically in both modes. Ties break deterministically by
// ascending key; limit > 0 truncates the fused list. Each leg's entries must
// already be deduplicated by Key — both callers dedup via their display-map
// seen-checks — since a repeated key within one leg would accumulate score
// twice. This is a local merge rather than kitvec.Merge because kit's Merge
// has no per-hit rank-offset hook for the subordinate penalty; upstreaming
// such a hook would let this collapse onto kit's implementation later.
func RRFMerge(legs [][]RankedUnit, limit int) []FusedUnit {
	const rankConstant = 60
	const subordinatePenalty = 5
	scores := make(map[string]float64)
	var units []RankedUnit
	for _, leg := range legs {
		for i, u := range leg {
			rank := i + 1
			if u.Subordinate {
				rank += subordinatePenalty
			}
			if _, seen := scores[u.Key]; !seen {
				units = append(units, u)
			}
			scores[u.Key] += 1.0 / float64(rankConstant+rank)
		}
	}
	merged := make([]FusedUnit, len(units))
	for i, u := range units {
		merged[i] = FusedUnit{Unit: u, Score: scores[u.Key]}
	}
	slices.SortFunc(merged, func(a, b FusedUnit) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Unit.Key, b.Unit.Key)
	})
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// snippetTruncationMarkers are the elision markers left by the two sources of
// approximate snippet text semantic/hybrid modes locate within a message's
// full content: the vector index's trailing unicode ellipsis
// (internal/vector's truncateRunes) and FTS5 snippet()'s literal "..." marker
// (used at both ends by the SQLite hybrid lexical capability).
var snippetTruncationMarkers = []string{"...", "…"}

// approxSnippetSpan locates approx (a searcher-provided chunk/snippet or
// FTS snippet() fragment, possibly elided at one or both ends) within the
// message's full content, returning the byte span to center a redacted
// window on. approx is trimmed of elision markers first since those markers
// are not literal substrings of content. Returns ok=false when approx cannot
// be located verbatim (e.g. content changed since the snippet was derived),
// leaving the caller to fall back to some other span -- content itself is
// always what gets redacted, so a miss here only affects centering, not the
// redaction guarantee.
func approxSnippetSpan(content, approx string) (start, end int, ok bool) {
	trimmed := strings.TrimSpace(approx)
	for _, marker := range snippetTruncationMarkers {
		trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, marker))
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
	}
	if trimmed == "" {
		return 0, 0, false
	}
	off := strings.Index(content, trimmed)
	if off < 0 {
		return 0, 0, false
	}
	return off, off + len(trimmed), true
}

// SemanticSnippet builds the returned snippet for "semantic" and "hybrid"
// matches from the message's full content, not from the searcher's
// pre-truncated approx (chunk or FTS snippet() text): redaction
// (buildSnippet -> secrets.RedactWindow) must see the whole message so a
// secret straddling approx's truncation boundary cannot leak a fragment that
// full-content redaction would otherwise catch. approx is used only to
// center the window; when it cannot be located in content, FTSSnippetRange
// centers on the query pattern instead, and failing that on the start of
// content -- content is still what gets redacted either way.
func (f ContentSearchFilter) SemanticSnippet(content, approx string) string {
	if start, end, ok := approxSnippetSpan(content, approx); ok {
		return f.buildSnippet(content, start, end)
	}
	start, end := FTSSnippetRange(f.Pattern, content)
	return f.buildSnippet(content, start, end)
}
