package db

import (
	"context"
	"database/sql"
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
	Date, DateFrom, DateTo, ActiveSince               string
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
	// every mode (enrichSemanticHits for semantic/hybrid,
	// deriveLexicalUnits for substring/regex/fts).
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
		ActiveSince:      f.ActiveSince,
		ExcludeOneShot:   !f.IncludeOneShot,
		ExcludeAutomated: !f.IncludeAutomated,
		IncludeChildren:  f.IncludeChildren,
	}
}

// sessionScopeSubquery returns "session_id IN (SELECT id FROM sessions
// WHERE <buildSessionFilter where>)" plus its args, reusing the session
// filter machinery. The Limit/Cursor on the inner filter are irrelevant
// (no LIMIT in a SELECT id subquery), so they are left unset.
func sessionScopeSubquery(f ContentSearchFilter) (string, []any) {
	where, args := buildSessionFilter(contentSessionFilter(f))
	return "session_id IN (SELECT id FROM sessions WHERE " + where + ")", args
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

// semanticSessionScopeSubquery is sessionScopeSubquery minus the
// sidebar-child exclusion: semantic/hybrid unit visibility is governed by
// Scope (which supersedes IncludeChildren), so the hybrid FTS leg must see
// the same universe the vector leg does — every other predicate (project,
// agent, dates, automated, one-shot for top-level sessions) still applies
// to each session's own row.
func semanticSessionScopeSubquery(f ContentSearchFilter) (string, []any) {
	where, args := buildSessionBaseFilter(semanticContentSessionFilter(f))
	return "session_id IN (SELECT id FROM sessions WHERE " + where + ")", args
}

// SearchContent runs a content search and returns a page of matches.
func (db *DB) SearchContent(
	ctx context.Context, f ContentSearchFilter,
) (ContentSearchPage, error) {
	if f.Limit <= 0 || f.Limit > MaxContentSearchLimit {
		f.Limit = DefaultContentSearchLimit
	}
	if f.Pattern == "" {
		return ContentSearchPage{}, nil
	}

	// Semantic and hybrid validate and default Sources themselves (messages
	// only) ahead of the substring/regex/fts source-set default just below,
	// which fills in tool_input/tool_result that neither mode supports.
	switch f.Mode {
	case "semantic":
		return db.searchContentSemantic(ctx, f)
	case "hybrid":
		return db.searchContentHybrid(ctx, f)
	}

	if len(f.Sources) == 0 {
		f.Sources = []string{"messages", "tool_input", "tool_result"}
	}
	for _, s := range f.Sources {
		if s != "messages" && s != "tool_input" && s != "tool_result" {
			return ContentSearchPage{}, searchInputErrorf("search: unknown source %q", s)
		}
	}
	switch f.Mode {
	case "", "substring":
		return db.searchContentSubstring(ctx, f)
	case "regex":
		return db.searchContentRegex(ctx, f)
	case "fts":
		return db.searchContentFTS(ctx, f)
	default:
		return ContentSearchPage{}, searchInputErrorf(
			"search: invalid mode %q", f.Mode)
	}
}

func hasSource(f ContentSearchFilter, name string) bool {
	return slices.Contains(f.Sources, name)
}

// searchContentSubstring builds a UNION ALL across the selected sources
// with a case-insensitive LIKE, scoped to qualifying sessions, ordered by
// recency then ordinal, fetching Limit+1 rows for cursor detection.
func (db *DB) searchContentSubstring(
	ctx context.Context, f ContentSearchFilter,
) (ContentSearchPage, error) {
	scope, scopeArgs := sessionScopeSubquery(f)
	like := "%" + escapeLike(f.Pattern) + "%"

	var branches []string
	var args []any

	// The snippet column carries the full source field; the snippet is built in
	// Go (substringSnippet) so secret redaction sees whole secrets, not a window
	// pre-truncated in SQL that could split a secret and leak a fragment. Only
	// the Limit+1 returned rows ship a full body.
	snippetExpr := func(col string) string { return col }

	if hasSource(f, "messages") {
		sysPred := "1=1"
		if f.ExcludeSystem {
			sysPred = "m.is_system = 0 AND " +
				SystemPrefixSQL("m.content", "m.role")
		}
		branches = append(branches, fmt.Sprintf(`
			SELECT m.session_id, s.project, s.agent, 'message' AS location,
				m.role AS role, '' AS tool_name, m.ordinal,
				COALESCE(m.timestamp,'') AS ts, %s AS snippet,
				COALESCE(s.ended_at, s.started_at, '') AS sort_ts,
				0 AS src, m.id AS row_id
			FROM messages m JOIN sessions s ON s.id = m.session_id
			WHERE m.content LIKE ? ESCAPE '\' AND %s AND m.%s`,
			snippetExpr("m.content"), sysPred, scope))
		args = append(args, like)
		args = append(args, scopeArgs...)
	}
	if hasSource(f, "tool_input") {
		branches = append(branches, fmt.Sprintf(`
			SELECT tc.session_id, s.project, s.agent, 'tool_input' AS location,
				'assistant' AS role, tc.tool_name, mm.ordinal,
				COALESCE(mm.timestamp,'') AS ts, %s AS snippet,
				COALESCE(s.ended_at, s.started_at, '') AS sort_ts,
				1 AS src, tc.id AS row_id
			FROM tool_calls tc
			JOIN messages mm ON mm.id = tc.message_id
			JOIN sessions s ON s.id = tc.session_id
			WHERE tc.input_json LIKE ? ESCAPE '\' AND tc.%s`,
			snippetExpr("tc.input_json"), scope))
		args = append(args, like)
		args = append(args, scopeArgs...)
	}
	if hasSource(f, "tool_result") {
		// Canonical output: result_content only when the call has no result
		// events (those are matched in the events branch below). The dedup is
		// keyed on tool_use_id and applied only when that ID is non-empty: many
		// agents leave tool_use_id blank, and matching '' = '' would let one
		// empty-ID result event suppress the result_content of every other
		// empty-ID call in the session. Empty-ID calls therefore skip the dedup
		// -- they may surface in both branches (a harmless duplicate) but are
		// never missed. A precise per-call key would need a call_index on
		// tool_calls, which SQLite does not store.
		branches = append(branches, fmt.Sprintf(`
			SELECT tc.session_id, s.project, s.agent, 'tool_result' AS location,
				'assistant' AS role, tc.tool_name, mm.ordinal,
				COALESCE(mm.timestamp,'') AS ts, %s AS snippet,
				COALESCE(s.ended_at, s.started_at, '') AS sort_ts,
				2 AS src, tc.id AS row_id
			FROM tool_calls tc
			JOIN messages mm ON mm.id = tc.message_id
			JOIN sessions s ON s.id = tc.session_id
			WHERE tc.result_content LIKE ? ESCAPE '\'
			  AND NOT EXISTS (SELECT 1 FROM tool_result_events tre
			    WHERE tre.session_id = tc.session_id
			      AND tre.tool_use_id = tc.tool_use_id
			      AND tc.tool_use_id <> '')
			  AND tc.%s`,
			snippetExpr("tc.result_content"), scope))
		args = append(args, like)
		args = append(args, scopeArgs...)
		branches = append(branches, fmt.Sprintf(`
			SELECT tre.session_id, s.project, s.agent, 'tool_result' AS location,
				'assistant' AS role, '' AS tool_name,
				tre.tool_call_message_ordinal AS ordinal,
				COALESCE(tre.timestamp,'') AS ts, %s AS snippet,
				COALESCE(s.ended_at, s.started_at, '') AS sort_ts,
				3 AS src, tre.id AS row_id
			FROM tool_result_events tre
			JOIN sessions s ON s.id = tre.session_id
			WHERE tre.content LIKE ? ESCAPE '\' AND tre.%s`,
			snippetExpr("tre.content"), scope))
		args = append(args, like)
		args = append(args, scopeArgs...)
	}
	if len(branches) == 0 {
		return ContentSearchPage{}, nil
	}

	query := "SELECT session_id, project, agent, location, role, tool_name, " +
		"ordinal, ts, snippet FROM (" +
		strings.Join(branches, " UNION ALL ") +
		") ORDER BY julianday(sort_ts) DESC, session_id ASC, ordinal ASC, src ASC, row_id ASC " +
		"LIMIT ? OFFSET ?"
	args = append(args, f.Limit+1, f.Cursor)

	return db.scanContentMatches(ctx, query, args, f.Limit, f.Cursor, f.substringSnippet)
}

// scanContentMatches runs query and assembles a ContentSearchPage, treating
// the (Limit+1)-th row as the cursor sentinel. The body column is the full
// source field; makeSnippet derives the (windowed, redacted) snippet from it
// so redaction sees whole secrets rather than a pre-truncated window. The
// returned page then gets its derived unit ranges and lineage assigned by
// the shared deriveLexicalUnits pass (post-truncation, O(page)).
func (db *DB) scanContentMatches(
	ctx context.Context, query string, args []any, limit, cursor int,
	makeSnippet func(body string) string,
) (ContentSearchPage, error) {
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return ContentSearchPage{}, fmt.Errorf("content search: %w", err)
	}
	defer rows.Close()
	out := make([]ContentMatch, 0)
	for rows.Next() {
		var m ContentMatch
		var body string
		if err := rows.Scan(&m.SessionID, &m.Project, &m.Agent,
			&m.Location, &m.Role, &m.ToolName, &m.Ordinal,
			&m.Timestamp, &body); err != nil {
			return ContentSearchPage{}, fmt.Errorf("scan match: %w", err)
		}
		m.Snippet = makeSnippet(body)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return ContentSearchPage{}, err
	}
	// Close the cursor before deriving units (exhausting Next already
	// auto-closed it; this keeps the release explicit): deriveLexicalUnits
	// issues new queries, which must never wait on a connection this cursor
	// would otherwise still pin.
	if err := rows.Close(); err != nil {
		return ContentSearchPage{}, fmt.Errorf("closing content matches: %w", err)
	}
	page := ContentSearchPage{Matches: out}
	if len(out) > limit {
		page.Matches = out[:limit]
		page.NextCursor = cursor + limit
	}
	if err := db.deriveLexicalUnits(ctx, page.Matches); err != nil {
		return ContentSearchPage{}, err
	}
	return page, nil
}

// searchContentRegex compiles the pattern, narrows candidate rows with a
// LIKE prefilter on any required literal substring (full scan when none),
// streams candidates, and keeps RE2 matches. Snippets are built in Go.
func (db *DB) searchContentRegex(
	ctx context.Context, f ContentSearchFilter,
) (ContentSearchPage, error) {
	re, err := regexp.Compile(f.Pattern)
	if err != nil {
		return ContentSearchPage{}, searchInputErrorf("search: invalid regex: %v", err)
	}
	lit := literalPrefix(f.Pattern)

	rows, err := db.regexCandidateRows(ctx, f, lit)
	if err != nil {
		return ContentSearchPage{}, err
	}
	defer rows.Close()

	out := make([]ContentMatch, 0)
	// Regex paging has no SQL OFFSET: each page re-fetches and re-matches
	// candidates from the start, discarding the first f.Cursor confirmed
	// matches. Ordering is deterministic so paging stays correct; deep pages
	// cost O(cursor) extra RE2 work, acceptable for interactive use.
	seen := 0
	for rows.Next() {
		var m ContentMatch
		var body string
		if err := rows.Scan(&m.SessionID, &m.Project, &m.Agent,
			&m.Location, &m.Role, &m.ToolName, &m.Ordinal,
			&m.Timestamp, &body); err != nil {
			return ContentSearchPage{}, fmt.Errorf("scan candidate: %w", err)
		}
		loc := re.FindStringIndex(body)
		if loc == nil {
			continue
		}
		if seen < f.Cursor {
			seen++
			continue
		}
		m.Snippet = f.buildSnippet(body, loc[0], loc[1])
		out = append(out, m)
		if len(out) > f.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return ContentSearchPage{}, err
	}
	// Close the candidate cursor before deriving units: the loop breaks out
	// with rows still open once Limit+1 matches are collected, and
	// deriveLexicalUnits issues new queries that could otherwise block on a
	// constrained connection pool while this cursor pins a connection.
	if err := rows.Close(); err != nil {
		return ContentSearchPage{}, fmt.Errorf("closing regex candidates: %w", err)
	}
	page := ContentSearchPage{Matches: out}
	if len(out) > f.Limit {
		page.Matches = out[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	if err := db.deriveLexicalUnits(ctx, page.Matches); err != nil {
		return ContentSearchPage{}, err
	}
	return page, nil
}

// regexCandidateRows returns full-body rows for the selected sources,
// LIKE-prefiltered by lit when non-empty, ordered for stable paging.
// Each branch selects: session_id, project, agent, location, role,
// tool_name, ordinal, ts AS ts, body, sort_ts, src, row_id. The outer
// query projects the first 9 columns by name.
func (db *DB) regexCandidateRows(
	ctx context.Context, f ContentSearchFilter, lit string,
) (*sql.Rows, error) {
	scope, scopeArgs := sessionScopeSubquery(f)
	var branches []string
	var args []any

	addLike := func() { args = append(args, "%"+escapeLike(lit)+"%") }

	prefilterClause := func(col string) string {
		if lit == "" {
			return col + " IS NOT NULL"
		}
		addLike()
		return col + " LIKE ? ESCAPE '\\'"
	}

	if hasSource(f, "messages") {
		sysPred := "1=1"
		if f.ExcludeSystem {
			sysPred = "m.is_system = 0 AND " +
				SystemPrefixSQL("m.content", "m.role")
		}
		w := prefilterClause("m.content")
		branches = append(branches, fmt.Sprintf(`
			SELECT m.session_id AS session_id, s.project AS project,
				s.agent AS agent, 'message' AS location,
				m.role AS role, '' AS tool_name,
				m.ordinal AS ordinal, COALESCE(m.timestamp,'') AS ts,
				m.content AS body,
				COALESCE(s.ended_at, s.started_at, '') AS sort_ts,
				0 AS src, m.id AS row_id
			FROM messages m JOIN sessions s ON s.id = m.session_id
			WHERE %s AND %s AND m.%s`, w, sysPred, scope))
		args = append(args, scopeArgs...)
	}
	if hasSource(f, "tool_input") {
		w := prefilterClause("tc.input_json")
		branches = append(branches, fmt.Sprintf(`
			SELECT tc.session_id AS session_id, s.project AS project,
				s.agent AS agent, 'tool_input' AS location,
				'assistant' AS role, tc.tool_name AS tool_name,
				mm.ordinal AS ordinal, COALESCE(mm.timestamp,'') AS ts,
				tc.input_json AS body,
				COALESCE(s.ended_at, s.started_at, '') AS sort_ts,
				1 AS src, tc.id AS row_id
			FROM tool_calls tc JOIN messages mm ON mm.id = tc.message_id
			JOIN sessions s ON s.id = tc.session_id
			WHERE %s AND tc.%s`, w, scope))
		args = append(args, scopeArgs...)
	}
	if hasSource(f, "tool_result") {
		w := prefilterClause("tc.result_content")
		branches = append(branches, fmt.Sprintf(`
			SELECT tc.session_id AS session_id, s.project AS project,
				s.agent AS agent, 'tool_result' AS location,
				'assistant' AS role, tc.tool_name AS tool_name,
				mm.ordinal AS ordinal, COALESCE(mm.timestamp,'') AS ts,
				tc.result_content AS body,
				COALESCE(s.ended_at, s.started_at, '') AS sort_ts,
				2 AS src, tc.id AS row_id
			FROM tool_calls tc JOIN messages mm ON mm.id = tc.message_id
			JOIN sessions s ON s.id = tc.session_id
			WHERE %s AND NOT EXISTS (SELECT 1 FROM tool_result_events tre
			    WHERE tre.session_id = tc.session_id AND tre.tool_use_id = tc.tool_use_id
			      AND tc.tool_use_id <> '')
			  AND tc.%s`, w, scope))
		args = append(args, scopeArgs...)
		wEv := prefilterClause("tre.content")
		branches = append(branches, fmt.Sprintf(`
			SELECT tre.session_id AS session_id, s.project AS project,
				s.agent AS agent, 'tool_result' AS location,
				'assistant' AS role, '' AS tool_name,
				tre.tool_call_message_ordinal AS ordinal,
				COALESCE(tre.timestamp,'') AS ts,
				tre.content AS body,
				COALESCE(s.ended_at, s.started_at, '') AS sort_ts,
				3 AS src, tre.id AS row_id
			FROM tool_result_events tre JOIN sessions s ON s.id = tre.session_id
			WHERE %s AND tre.%s`, wEv, scope))
		args = append(args, scopeArgs...)
	}
	if len(branches) == 0 {
		// Return an empty result set.
		q := "SELECT '' AS session_id, '' AS project, '' AS agent, '' AS location, " +
			"'' AS role, '' AS tool_name, 0 AS ordinal, '' AS ts, '' AS body " +
			"WHERE 0"
		return db.getReader().QueryContext(ctx, q)
	}

	query := "SELECT session_id, project, agent, location, role, tool_name, " +
		"ordinal, ts, body FROM (" +
		strings.Join(branches, " UNION ALL ") +
		") ORDER BY julianday(sort_ts) DESC, session_id ASC, ordinal ASC, src ASC, row_id ASC"
	return db.getReader().QueryContext(ctx, query, args...)
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

// searchContentFTS uses messages_fts for fast tokenized matching over
// message content only. The caller (service/CLI) guarantees Sources is
// messages-only for fts mode.
func (db *DB) searchContentFTS(
	ctx context.Context, f ContentSearchFilter,
) (ContentSearchPage, error) {
	// Guard FTS availability up front: a missing messages_fts table would
	// otherwise raise a generic SQLITE_ERROR that classifyFTSError would misread
	// as invalid user input (400). With FTS present, the only SQLITE_ERROR the
	// MATCH query can raise comes from a malformed pattern.
	if !db.HasFTS() {
		return ContentSearchPage{}, errFTSUnavailable
	}
	scope, scopeArgs := sessionScopeSubquery(f)
	sysPred := "1=1"
	if f.ExcludeSystem {
		sysPred = "m.is_system = 0 AND " + SystemPrefixSQL("m.content", "m.role")
	}
	// Select the full content (not FTS snippet()) so the snippet is built in Go
	// and secret redaction sees whole secrets rather than a pre-truncated window.
	query := fmt.Sprintf(`
		SELECT m.session_id, s.project, s.agent, 'message', m.role, '',
			m.ordinal, COALESCE(m.timestamp,'') AS ts, m.content AS snippet
		FROM messages_fts
		JOIN messages m ON m.id = messages_fts.rowid
		JOIN sessions s ON s.id = m.session_id
		WHERE messages_fts MATCH ? AND %s AND m.%s
		ORDER BY rank ASC, m.ordinal ASC, m.id ASC
		LIMIT ? OFFSET ?`, sysPred, scope)
	args := []any{PrepareFTSQuery(f.Pattern)}
	args = append(args, scopeArgs...)
	args = append(args, f.Limit+1, f.Cursor)
	page, err := db.scanContentMatches(ctx, query, args, f.Limit, f.Cursor, f.ftsSnippet)
	if err != nil {
		return ContentSearchPage{}, classifyFTSError(err)
	}
	return page, nil
}

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

// searchContentSemantic runs mode "semantic": it over-fetches ranked hits
// from the wired VectorSearcher, keeps hits whose session passes the
// filter's metadata scope (loaded with one query over the hit session IDs;
// the sidebar-child exclusion is lifted — f.Scope governs subordinate-unit
// visibility instead, dropping hits ScopeExcludes rules out),
// routes the surviving ranking through the same RRF merge hybrid uses as a
// one-leg fusion (so subordinate units are penalized identically; matches
// still carry the searcher's own scores), enriches surviving (session_id,
// ordinal) pairs with session/message metadata in one query, and returns
// them in the fused order, truncated to f.Limit.
func (db *DB) searchContentSemantic(
	ctx context.Context, f ContentSearchFilter,
) (ContentSearchPage, error) {
	if err := ValidateSemanticFilter(f); err != nil {
		return ContentSearchPage{}, err
	}
	searcher := db.getVectorSearcher()
	if searcher == nil {
		return ContentSearchPage{}, ErrSemanticUnavailable
	}

	k := max(f.Limit*4, SemanticOverfetchMin)
	hits, err := searcher.SemanticSearch(ctx, f.Pattern, k)
	if err != nil {
		return ContentSearchPage{}, err
	}
	if len(hits) == 0 {
		return ContentSearchPage{}, nil
	}

	allowed, err := db.semanticAllowedSessionIDs(ctx, f, uniqueSessionIDs(hits))
	if err != nil {
		return ContentSearchPage{}, err
	}
	surviving := make([]VectorHit, 0, len(hits))
	for _, h := range hits {
		if allowed[h.SessionID] && !ScopeExcludes(f.Scope, h.Subordinate) {
			surviving = append(surviving, h)
		}
	}
	if len(surviving) == 0 {
		return ContentSearchPage{}, nil
	}
	surviving = ApplySubordinatePenalty(surviving)

	meta, err := db.enrichSemanticHits(ctx, surviving)
	if err != nil {
		return ContentSearchPage{}, err
	}

	out := make([]ContentMatch, 0, min(len(surviving), f.Limit))
	for _, h := range surviving {
		info, ok := meta[semanticHitKey{h.SessionID, h.Ordinal}]
		if !ok {
			continue
		}
		score := float64(h.Score)
		out = append(out, ContentMatch{
			SessionID:       h.SessionID,
			Project:         info.project,
			Agent:           info.agent,
			Location:        "message",
			Role:            info.role,
			Ordinal:         h.Ordinal,
			OrdinalRange:    [2]int{h.OrdinalStart, h.OrdinalEnd},
			Subordinate:     h.Subordinate,
			Relationship:    info.relationshipType,
			ParentSessionID: info.parentSessionID,
			Sidechain:       info.isSidechain,
			Timestamp:       info.timestamp,
			Snippet:         f.SemanticSnippet(info.content, h.Snippet),
			Score:           &score,
		})
		if len(out) >= f.Limit {
			break
		}
	}
	return ContentSearchPage{Matches: out}, nil
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

// ApplySubordinatePenalty reorders rank-ordered semantic hits through
// RRFMerge as a one-leg fusion, so mode "semantic" penalizes subordinate
// units exactly like mode "hybrid" (one implementation, no hybrid-only
// special case). Hits keep their own scores; only the order changes. A
// duplicate fusion key (two hits on the same unit) keeps its best-ranked
// hit.
func ApplySubordinatePenalty(hits []VectorHit) []VectorHit {
	leg := make([]RankedUnit, 0, len(hits))
	byKey := make(map[string]VectorHit, len(hits))
	for _, h := range hits {
		key := UnitFusionKey(h.SessionID, h.OrdinalStart)
		if _, dup := byKey[key]; dup {
			continue
		}
		leg = append(leg, RankedUnit{Key: key, Subordinate: h.Subordinate})
		byKey[key] = h
	}
	merged := RRFMerge([][]RankedUnit{leg}, 0)
	out := make([]VectorHit, 0, len(merged))
	for _, m := range merged {
		out = append(out, byKey[m.Unit.Key])
	}
	return out
}

// hybridDisplay carries what one fused unit needs for presentation: the
// anchor (session, ordinal) the match reports and enriches by, the unit's
// ordinal span and subordinate flag (structurally derived for a unit-less
// message-granularity FTS hit, see classifyUnitlessHybridHits), plus the
// leg's raw (unredacted) approximate snippet text used only to center the
// redacted window.
type hybridDisplay struct {
	sessionID    string
	ordinal      int
	ordinalStart int
	ordinalEnd   int
	subordinate  bool
	snippet      string
}

// hybridLeg is one rank-ordered fusion leg: entries for RRFMerge plus each
// key's display info.
type hybridLeg struct {
	ranked  []RankedUnit
	display map[string]hybridDisplay
}

// searchContentHybrid runs mode "hybrid": lexical (FTS) and semantic (vector)
// rankings are each over-fetched to k, the vector leg is filtered down to
// sessions passing the filter's metadata scope (the FTS leg filters in SQL),
// FTS message hits are resolved to their containing units, and the two
// rank-ordered leg lists are fused at unit granularity with RRFMerge.
// Returned matches are enriched with session/message metadata via the same
// lookup semantic search uses, ordered by fused score descending, truncated
// to f.Limit.
func (db *DB) searchContentHybrid(
	ctx context.Context, f ContentSearchFilter,
) (ContentSearchPage, error) {
	if err := ValidateSemanticFilter(f); err != nil {
		return ContentSearchPage{}, err
	}
	searcher := db.getVectorSearcher()
	if searcher == nil {
		return ContentSearchPage{}, ErrSemanticUnavailable
	}
	if !db.HasFTS() {
		return ContentSearchPage{}, errFTSUnavailable
	}

	k := max(f.Limit*4, SemanticOverfetchMin)
	vecLeg, err := db.hybridVectorLeg(ctx, f, searcher, k)
	if err != nil {
		return ContentSearchPage{}, err
	}
	ftsLeg, err := db.hybridFTSLeg(ctx, f, searcher, k)
	if err != nil {
		return ContentSearchPage{}, err
	}
	if len(vecLeg.ranked) == 0 && len(ftsLeg.ranked) == 0 {
		return ContentSearchPage{}, nil
	}

	merged := RRFMerge([][]RankedUnit{vecLeg.ranked, ftsLeg.ranked}, f.Limit)
	return db.enrichHybridMatches(ctx, f, merged, vecLeg.display, ftsLeg.display)
}

// hybridVectorLeg over-fetches k semantic unit hits, drops any whose session
// fails the filter's metadata scope (the same child-exclusion-lifted lookup
// searchContentSemantic uses) or whose subordinate flag falls outside
// f.Scope, and returns the survivors as a rank-ordered fusion leg keyed by
// unit. Both filters run before the merge so reciprocal-rank fusion only
// ranks eligible units and a scoped search can still fill the limit.
func (db *DB) hybridVectorLeg(
	ctx context.Context, f ContentSearchFilter, searcher VectorSearcher, k int,
) (hybridLeg, error) {
	leg := hybridLeg{display: make(map[string]hybridDisplay)}
	hits, err := searcher.SemanticSearch(ctx, f.Pattern, k)
	if err != nil {
		return hybridLeg{}, err
	}
	if len(hits) == 0 {
		return leg, nil
	}
	allowed, err := db.semanticAllowedSessionIDs(ctx, f, uniqueSessionIDs(hits))
	if err != nil {
		return hybridLeg{}, err
	}
	for _, h := range hits {
		if !allowed[h.SessionID] || ScopeExcludes(f.Scope, h.Subordinate) {
			continue
		}
		key := UnitFusionKey(h.SessionID, h.OrdinalStart)
		if _, seen := leg.display[key]; seen {
			continue
		}
		leg.ranked = append(leg.ranked, RankedUnit{Key: key, Subordinate: h.Subordinate})
		leg.display[key] = hybridDisplay{
			sessionID: h.SessionID, ordinal: h.Ordinal, snippet: h.Snippet,
			ordinalStart: h.OrdinalStart, ordinalEnd: h.OrdinalEnd,
			subordinate: h.Subordinate,
		}
	}
	return leg, nil
}

// maxHybridFTSBatches caps how many k-row FTS batches hybridFTSLeg fetches.
// It bounds the worst-case work when discard dominates — many rows collapsing
// into one unit, or a narrow f.Scope dropping most rows — while letting the
// leg keep paging past discarded rows instead of under-filling after the
// first batch. The residual is documented: a leg needing survivors deeper
// than maxHybridFTSBatches x k rows can still under-fill.
const maxHybridFTSBatches = 4

// hybridFTSLeg runs a rank-ordered FTS query over the embedded universe
// (role user/assistant, is_system = 0, system-prefix excluded -- the same
// predicate ScanEmbeddableUnits uses), scoped in SQL to sessions passing
// the child-exclusion-lifted filter (semanticSessionScopeSubquery, so both
// hybrid legs see the same universe), resolves each message hit to its
// containing unit, drops units outside f.Scope, and returns up to k hits
// as a rank-ordered fusion leg. A hit inside a unit adopts the unit's fusion
// key and subordinate flag while keeping its own message ordinal as the
// anchor and its FTS snippet for display (the FTS-anchor override); several
// hits in one unit collapse to the best-ranked one. A hit with no containing
// unit keeps a message-granularity key and survives fusion on its own, with
// its range and subordinate flag structurally derived before the scope
// filter and the merge (classifyUnitlessHybridHits), so it is excluded and
// penalized exactly like lexical mode classifies the same anchor.
//
// Rows are fetched in rank-ordered batches of k with OFFSET continuation:
// collapse and scope filtering can discard most of a batch, so the leg keeps
// fetching until it holds k entries, the stream is exhausted, or
// maxHybridFTSBatches is hit. The display seen-check dedups across batches;
// earlier batches rank better, so the best-ranked hit per unit always wins.
func (db *DB) hybridFTSLeg(
	ctx context.Context, f ContentSearchFilter, searcher VectorSearcher, k int,
) (hybridLeg, error) {
	leg := hybridLeg{display: make(map[string]hybridDisplay, k)}
	for batch := range maxHybridFTSBatches {
		hits, err := db.fetchHybridFTSBatch(ctx, f, k, batch*k)
		if err != nil {
			return hybridLeg{}, err
		}
		if err := db.appendHybridFTSHits(ctx, searcher, f.Scope, hits, &leg); err != nil {
			return hybridLeg{}, err
		}
		if len(hits) < k || len(leg.ranked) >= k {
			break
		}
	}
	return leg, nil
}

// fetchHybridFTSBatch fetches one rank-ordered batch of at most k FTS message
// rows for hybridFTSLeg, starting at offset. The ORDER BY carries m.id as a
// deterministic tiebreak so OFFSET continuation is stable across batches when
// ranks tie.
func (db *DB) fetchHybridFTSBatch(
	ctx context.Context, f ContentSearchFilter, k, offset int,
) ([]hybridDisplay, error) {
	scope, scopeArgs := semanticSessionScopeSubquery(f)
	query := fmt.Sprintf(`
		SELECT m.session_id, m.ordinal,
		       snippet(messages_fts, 0, '', '', '...', 32) AS snip
		FROM messages_fts f JOIN messages m ON m.id = f.rowid
		WHERE messages_fts MATCH ? AND m.role IN ('user','assistant')
		  AND m.is_system = 0 AND %s
		  AND m.%s
		ORDER BY f.rank, m.id LIMIT ? OFFSET ?`,
		SystemPrefixSQL("m.content", "m.role"), scope)

	args := []any{PrepareFTSQuery(f.Pattern)}
	args = append(args, scopeArgs...)
	args = append(args, k, offset)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classifyFTSError(fmt.Errorf("hybrid search fts leg: %w", err))
	}
	defer rows.Close()

	var hits []hybridDisplay
	for rows.Next() {
		var hit hybridDisplay
		if err := rows.Scan(&hit.sessionID, &hit.ordinal, &hit.snippet); err != nil {
			return nil, fmt.Errorf("scan hybrid fts hit: %w", err)
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}

// appendHybridFTSHits resolves one batch of FTS message hits to their
// containing units, classifies unit-less hits structurally (range and
// subordinate flag, so the scope filter and the fusion penalty treat them
// exactly like lexical mode), and accumulates the survivors into leg: hits
// outside scope are dropped, and a unit already seen (within or across
// batches) keeps its earlier, better-ranked entry.
func (db *DB) appendHybridFTSHits(
	ctx context.Context, searcher VectorSearcher, scope string,
	hits []hybridDisplay, leg *hybridLeg,
) error {
	if len(hits) == 0 {
		return nil
	}
	refs := make([]MessageRef, len(hits))
	for i, hit := range hits {
		refs[i] = MessageRef{SessionID: hit.sessionID, Ordinal: hit.ordinal}
	}
	units, err := searcher.ResolveMessageUnits(ctx, refs)
	if err != nil {
		return fmt.Errorf("resolving fts hits to units: %w", err)
	}
	if len(units) != len(refs) {
		return fmt.Errorf(
			"resolving fts hits to units: got %d units for %d refs", len(units), len(refs))
	}

	keys := make([]string, len(hits))
	var unitless []int
	for i := range hits {
		hit := &hits[i]
		keys[i] = MessageFusionKey(hit.sessionID, hit.ordinal)
		hit.ordinalStart, hit.ordinalEnd = hit.ordinal, hit.ordinal
		if units[i].DocKey == "" {
			unitless = append(unitless, i)
			continue
		}
		keys[i] = UnitFusionKey(units[i].SessionID, units[i].OrdinalStart)
		hit.ordinalStart = units[i].OrdinalStart
		hit.ordinalEnd = units[i].OrdinalEnd
		hit.subordinate = units[i].Subordinate
	}
	if err := db.classifyUnitlessHybridHits(ctx, hits, unitless); err != nil {
		return err
	}

	for i, hit := range hits {
		if ScopeExcludes(scope, hit.subordinate) {
			continue
		}
		if _, seen := leg.display[keys[i]]; seen {
			continue
		}
		leg.ranked = append(leg.ranked, RankedUnit{Key: keys[i], Subordinate: hit.subordinate})
		leg.display[keys[i]] = hit
	}
	return nil
}

// enrichHybridMatches looks up session/message metadata for the fused units
// (reusing enrichSemanticHits' CTE join) and assembles the final page in
// fused-score order. When the FTS leg contributed to a unit, its display
// wins: the match anchors on the FTS-matched message's ordinal and centers
// on the FTS snippet (the vector leg's chunk anchor may be a different run
// member). Either way the returned snippet itself is built (and redacted)
// from the anchor message's full content via SemanticSnippet, the same
// guarantee mode "semantic" gives. Unit-less FTS rows arrive with their
// derived range and subordinate flag already assigned pre-merge
// (classifyUnitlessHybridHits), so no derivation runs here.
func (db *DB) enrichHybridMatches(
	ctx context.Context, f ContentSearchFilter, merged []FusedUnit,
	vecDisplay, ftsDisplay map[string]hybridDisplay,
) (ContentSearchPage, error) {
	displays := make([]hybridDisplay, len(merged))
	asHits := make([]VectorHit, len(merged))
	for i, m := range merged {
		d, ok := ftsDisplay[m.Unit.Key]
		if !ok {
			d = vecDisplay[m.Unit.Key]
		}
		displays[i] = d
		asHits[i] = VectorHit{SessionID: d.sessionID, Ordinal: d.ordinal}
	}
	meta, err := db.enrichSemanticHits(ctx, asHits)
	if err != nil {
		return ContentSearchPage{}, err
	}

	out := make([]ContentMatch, 0, len(merged))
	for i, m := range merged {
		d := displays[i]
		info, ok := meta[semanticHitKey{d.sessionID, d.ordinal}]
		if !ok {
			continue
		}
		score := m.Score
		out = append(out, ContentMatch{
			SessionID:       d.sessionID,
			Project:         info.project,
			Agent:           info.agent,
			Location:        "message",
			Role:            info.role,
			Ordinal:         d.ordinal,
			OrdinalRange:    [2]int{d.ordinalStart, d.ordinalEnd},
			Subordinate:     d.subordinate,
			Relationship:    info.relationshipType,
			ParentSessionID: info.parentSessionID,
			Sidechain:       info.isSidechain,
			Timestamp:       info.timestamp,
			Snippet:         f.SemanticSnippet(info.content, d.snippet),
			Score:           &score,
		})
	}
	return ContentSearchPage{Matches: out}, nil
}

// uniqueSessionIDs returns the distinct session IDs referenced by hits.
// Order is irrelevant: the result only feeds an IN (...) clause.
func uniqueSessionIDs(hits []VectorHit) []string {
	seen := make(map[string]bool, len(hits))
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		if !seen[h.SessionID] {
			seen[h.SessionID] = true
			ids = append(ids, h.SessionID)
		}
	}
	return ids
}

// semanticAllowedSessionIDs runs one query per maxSQLVars-sized chunk of ids
// returning the subset that pass the ContentSearchFilter's metadata scope
// (project, agent, date range, one-shot/automated, ...), reusing the same
// SessionFilter mapping sessionScopeSubquery uses so the two paths cannot
// drift apart. Like semanticSessionScopeSubquery it deliberately omits the
// sidebar-child exclusion and exempts child sessions from the one-shot
// gate (semanticContentSessionFilter): in semantic/hybrid modes Scope
// supersedes IncludeChildren, so subordinate units stay visible to the
// vector leg. Chunking keeps each query's bind count under SQLite's
// 999-variable limit: a semantic overfetch can surface hits from thousands
// of distinct sessions, well past a single IN clause's budget.
func (db *DB) semanticAllowedSessionIDs(
	ctx context.Context, f ContentSearchFilter, ids []string,
) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	where, filterArgs := buildSessionBaseFilter(semanticContentSessionFilter(f))
	query := "SELECT id FROM sessions WHERE " + where + " AND id IN "

	allowed := make(map[string]bool, len(ids))
	err := queryChunked(ids, func(chunk []string) error {
		placeholders, chunkArgs := inPlaceholders(chunk)
		args := make([]any, 0, len(filterArgs)+len(chunkArgs))
		args = append(args, filterArgs...)
		args = append(args, chunkArgs...)

		rows, err := db.getReader().QueryContext(ctx, query+placeholders, args...)
		if err != nil {
			return fmt.Errorf("semantic search session scope: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scan semantic session id: %w", err)
			}
			allowed[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		return rows.Close()
	})
	if err != nil {
		return nil, err
	}
	return allowed, nil
}

// semanticHitKey identifies a (session_id, ordinal) pair for enrichment
// lookup.
type semanticHitKey struct {
	sessionID string
	ordinal   int
}

// semanticHitInfo is the session/message metadata enrichSemanticHits attaches
// to a surviving hit. content is the message's full, un-truncated content:
// semantic/hybrid snippets are built from it (see SemanticSnippet) rather
// than from the searcher's pre-truncated chunk/snippet text, so secret
// redaction sees the same whole-body context the substring/regex/fts paths
// give it instead of a fragment that can split a secret at the truncation
// boundary. relationshipType, parentSessionID, and isSidechain carry the
// hit's lineage — joined here from sessions.db (the vector mirror does not
// store lineage per hit); isSidechain is the ANCHOR ordinal's message flag.
type semanticHitInfo struct {
	project, agent, role, timestamp, content string
	relationshipType, parentSessionID        string
	isSidechain                              bool
}

// enrichHitsChunk is the max hits enrichSemanticHits binds per VALUES CTE
// query. Each hit binds 2 params (session_id, ordinal), so this halves the
// shared maxSQLVars chunk to keep 2*chunk within SQLite's 999-variable limit.
const enrichHitsChunk = maxSQLVars / 2

// enrichSemanticHits looks up session/message metadata for hits' (session_id,
// ordinal) pairs via a "WITH hits(session_id, ordinal) AS (VALUES ...)" CTE
// joined to messages/sessions, one query per enrichHitsChunk-sized slice of
// hits (a semantic overfetch can carry thousands of hits, well past what one
// VALUES clause can bind). SQLite versions without row-value IN support over
// VALUES rule out "(session_id, ordinal) IN (VALUES ...)"; the CTE join form
// works everywhere.
func (db *DB) enrichSemanticHits(
	ctx context.Context, hits []VectorHit,
) (map[semanticHitKey]semanticHitInfo, error) {
	out := make(map[semanticHitKey]semanticHitInfo, len(hits))
	for start := 0; start < len(hits); start += enrichHitsChunk {
		chunk := hits[start:min(start+enrichHitsChunk, len(hits))]

		values := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*2)
		for i, h := range chunk {
			values[i] = "(?, ?)"
			args = append(args, h.SessionID, h.Ordinal)
		}
		query := "WITH hits(session_id, ordinal) AS (VALUES " +
			strings.Join(values, ", ") + ") " +
			"SELECT m.session_id, s.project, s.agent, m.role, m.ordinal, " +
			"COALESCE(m.timestamp, ''), m.content, " +
			"COALESCE(s.relationship_type, ''), " +
			"COALESCE(s.parent_session_id, ''), m.is_sidechain " +
			"FROM hits h " +
			"JOIN messages m ON m.session_id = h.session_id AND m.ordinal = h.ordinal " +
			"JOIN sessions s ON s.id = m.session_id"

		rows, err := db.getReader().QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("semantic search enrich: %w", err)
		}
		for rows.Next() {
			var key semanticHitKey
			var info semanticHitInfo
			if err := rows.Scan(&key.sessionID, &info.project, &info.agent,
				&info.role, &key.ordinal, &info.timestamp, &info.content,
				&info.relationshipType, &info.parentSessionID,
				&info.isSidechain); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan semantic hit: %w", err)
			}
			out[key] = info
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// snippetTruncationMarkers are the elision markers left by the two sources of
// approximate snippet text semantic/hybrid modes locate within a message's
// full content: the vector index's trailing unicode ellipsis
// (internal/vector's truncateRunes) and FTS5 snippet()'s literal "..." marker
// (used at both ends), configured as fetchHybridFTSBatch's 5th snippet()
// argument.
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
