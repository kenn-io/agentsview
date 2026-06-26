package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	Mode          string   // "substring" (default) | "regex" | "fts"
	Sources       []string // subset of {"messages","tool_input","tool_result"}
	ExcludeSystem bool

	Project, ExcludeProject, Machine, Agent           string
	Date, DateFrom, DateTo, ActiveSince               string
	IncludeChildren, IncludeAutomated, IncludeOneShot bool

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

// sessionScopeSubquery returns "session_id IN (SELECT id FROM sessions
// WHERE <buildSessionFilter where>)" plus its args, reusing the session
// filter machinery. The Limit/Cursor on the inner filter are irrelevant
// (no LIMIT in a SELECT id subquery), so they are left unset.
func sessionScopeSubquery(f ContentSearchFilter) (string, []any) {
	// Mirror session list: one-shot and automated sessions are excluded by
	// default, and IncludeOneShot/IncludeAutomated opt them back in.
	// Comprehensive secret coverage comes from the secrets subsystem
	// (scanned over every session at sync), not from search defaults.
	sf := SessionFilter{
		Project: f.Project, ExcludeProject: f.ExcludeProject,
		Machine: f.Machine, Agent: f.Agent,
		Date: f.Date, DateFrom: f.DateFrom, DateTo: f.DateTo,
		ActiveSince:      f.ActiveSince,
		ExcludeOneShot:   !f.IncludeOneShot,
		ExcludeAutomated: !f.IncludeAutomated,
		IncludeChildren:  f.IncludeChildren,
	}
	where, args := buildSessionFilter(sf)
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
// the (Limit+1)-th row as the cursor sentinel. The query's final column is the
// full source field; makeSnippet derives the (windowed, redacted) snippet from
// it so redaction sees whole secrets rather than a pre-truncated window.
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
	page := ContentSearchPage{Matches: out}
	if len(out) > limit {
		page.Matches = out[:limit]
		page.NextCursor = cursor + limit
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
	page := ContentSearchPage{Matches: out}
	if len(out) > f.Limit {
		page.Matches = out[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	return page, nil
}

// regexCandidateRows returns full-body rows for the selected sources,
// LIKE-prefiltered by lit when non-empty, ordered for stable paging.
// Each branch selects: session_id, project, agent, location, role,
// tool_name, ordinal, ts AS ts, body, sort_ts.
// The outer query projects the first 9 columns by name.
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
		return ContentSearchPage{}, errors.New("search: full-text search is unavailable")
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
