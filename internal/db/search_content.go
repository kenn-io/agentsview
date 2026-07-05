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
	kitvec "go.kenn.io/kit/vector"
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

// semanticOverfetchMin floors the candidate count requested from the
// VectorSearcher (k = max(f.Limit*4, semanticOverfetchMin)): session-scope
// filtering may drop some of the searcher's top hits, so more are fetched
// than will ultimately be returned.
const semanticOverfetchMin = 200

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
	return nil
}

// searchContentSemantic runs mode "semantic": it over-fetches ranked hits
// from the wired VectorSearcher, keeps hits whose session passes the
// filter's metadata scope (loaded with one query over the hit session IDs),
// enriches surviving (session_id, ordinal) pairs with session/message
// metadata in one query, and returns them in the searcher's rank order,
// truncated to f.Limit.
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

	k := max(f.Limit*4, semanticOverfetchMin)
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
		if allowed[h.SessionID] {
			surviving = append(surviving, h)
		}
	}
	if len(surviving) == 0 {
		return ContentSearchPage{}, nil
	}

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
			SessionID: h.SessionID,
			Project:   info.project,
			Agent:     info.agent,
			Location:  "message",
			Role:      info.role,
			Ordinal:   h.Ordinal,
			Timestamp: info.timestamp,
			Snippet:   f.semanticSnippet(info.content, h.Snippet),
			Score:     &score,
		})
		if len(out) >= f.Limit {
			break
		}
	}
	return ContentSearchPage{Matches: out}, nil
}

// matchKey identifies one message across the hybrid search's vector and FTS
// legs so kitvec.Merge can fuse them by document identity.
type matchKey struct {
	SessionID string
	Ordinal   int
}

// searchContentHybrid runs mode "hybrid": lexical (FTS) and semantic (vector)
// rankings are each over-fetched to k, the vector leg is filtered down to
// sessions passing the filter's metadata scope (the FTS leg filters in SQL),
// and the two rank-ordered leg lists are fused with reciprocal-rank fusion
// (kitvec.Merge, RankConstant 60). The vector leg is passed first so its
// snippet wins when a document appears in both legs. Returned matches are
// enriched with session/message metadata via the same lookup semantic search
// uses, ordered by fused score descending, truncated to f.Limit.
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

	k := max(f.Limit*4, semanticOverfetchMin)
	vecLeg, vecSnippets, err := db.hybridVectorLeg(ctx, f, searcher, k)
	if err != nil {
		return ContentSearchPage{}, err
	}
	ftsLeg, ftsSnippets, err := db.hybridFTSLeg(ctx, f, k)
	if err != nil {
		return ContentSearchPage{}, err
	}
	if len(vecLeg) == 0 && len(ftsLeg) == 0 {
		return ContentSearchPage{}, nil
	}

	merged := kitvec.Merge([][]kitvec.Hit[matchKey]{vecLeg, ftsLeg}, kitvec.MergeOptions{
		Strategy:     kitvec.MergeReciprocalRank,
		RankConstant: 60,
		Limit:        f.Limit,
	})
	if len(merged) == 0 {
		return ContentSearchPage{}, nil
	}
	return db.enrichHybridMatches(ctx, f, merged, vecSnippets, ftsSnippets)
}

// hybridVectorLeg over-fetches k semantic hits, drops any whose session
// fails the filter's metadata scope (the same lookup searchContentSemantic
// uses), and returns the survivors as rank-ordered kitvec hits plus their raw
// (unredacted) snippets keyed by matchKey. Session-set filtering runs before
// the merge so reciprocal-rank fusion only ranks eligible documents.
func (db *DB) hybridVectorLeg(
	ctx context.Context, f ContentSearchFilter, searcher VectorSearcher, k int,
) ([]kitvec.Hit[matchKey], map[matchKey]string, error) {
	hits, err := searcher.SemanticSearch(ctx, f.Pattern, k)
	if err != nil {
		return nil, nil, err
	}
	if len(hits) == 0 {
		return nil, nil, nil
	}
	allowed, err := db.semanticAllowedSessionIDs(ctx, f, uniqueSessionIDs(hits))
	if err != nil {
		return nil, nil, err
	}
	leg := make([]kitvec.Hit[matchKey], 0, len(hits))
	snippets := make(map[matchKey]string, len(hits))
	for _, h := range hits {
		if !allowed[h.SessionID] {
			continue
		}
		key := matchKey{SessionID: h.SessionID, Ordinal: h.Ordinal}
		leg = append(leg, kitvec.Hit[matchKey]{Doc: key, Score: h.Score})
		snippets[key] = h.Snippet
	}
	return leg, snippets, nil
}

// hybridFTSLeg runs a rank-ordered FTS query over the embedded universe
// (role user/assistant, is_system = 0, system-prefix excluded -- the same
// predicate ScanEmbeddableMessages uses), scoped to qualifying sessions in
// SQL, and returns up to k hits as rank-ordered kitvec hits plus their raw
// (unredacted) snippet() output keyed by matchKey.
func (db *DB) hybridFTSLeg(
	ctx context.Context, f ContentSearchFilter, k int,
) ([]kitvec.Hit[matchKey], map[matchKey]string, error) {
	scope, scopeArgs := sessionScopeSubquery(f)
	query := fmt.Sprintf(`
		SELECT m.session_id, m.ordinal,
		       snippet(messages_fts, 0, '', '', '...', 32) AS snip
		FROM messages_fts f JOIN messages m ON m.id = f.rowid
		WHERE messages_fts MATCH ? AND m.role IN ('user','assistant')
		  AND m.is_system = 0 AND %s
		  AND m.%s
		ORDER BY f.rank LIMIT ?`,
		SystemPrefixSQL("m.content", "m.role"), scope)

	args := []any{PrepareFTSQuery(f.Pattern)}
	args = append(args, scopeArgs...)
	args = append(args, k)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, classifyFTSError(fmt.Errorf("hybrid search fts leg: %w", err))
	}
	defer rows.Close()

	var leg []kitvec.Hit[matchKey]
	snippets := make(map[matchKey]string)
	for rows.Next() {
		var key matchKey
		var snip string
		if err := rows.Scan(&key.SessionID, &key.Ordinal, &snip); err != nil {
			return nil, nil, fmt.Errorf("scan hybrid fts hit: %w", err)
		}
		leg = append(leg, kitvec.Hit[matchKey]{Doc: key})
		snippets[key] = snip
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return leg, snippets, nil
}

// enrichHybridMatches looks up session/message metadata for the fused hits
// (reusing enrichSemanticHits' CTE join) and assembles the final page in
// fused-score order. Each match's approximate centering text prefers the
// vector leg's chunk snippet and falls back to the FTS leg's snippet()
// output when the key only came from the FTS leg; either way the returned
// snippet itself is built (and redacted) from the message's full content via
// semanticSnippet, the same guarantee mode "semantic" gives.
func (db *DB) enrichHybridMatches(
	ctx context.Context, f ContentSearchFilter, merged []kitvec.Hit[matchKey],
	vecSnippets, ftsSnippets map[matchKey]string,
) (ContentSearchPage, error) {
	asHits := make([]VectorHit, len(merged))
	for i, h := range merged {
		asHits[i] = VectorHit{SessionID: h.Doc.SessionID, Ordinal: h.Doc.Ordinal}
	}
	meta, err := db.enrichSemanticHits(ctx, asHits)
	if err != nil {
		return ContentSearchPage{}, err
	}

	out := make([]ContentMatch, 0, len(merged))
	for _, h := range merged {
		info, ok := meta[semanticHitKey{h.Doc.SessionID, h.Doc.Ordinal}]
		if !ok {
			continue
		}
		approx, ok := vecSnippets[h.Doc]
		if !ok {
			approx = ftsSnippets[h.Doc]
		}
		score := float64(h.Score)
		out = append(out, ContentMatch{
			SessionID: h.Doc.SessionID,
			Project:   info.project,
			Agent:     info.agent,
			Location:  "message",
			Role:      info.role,
			Ordinal:   h.Doc.Ordinal,
			Timestamp: info.timestamp,
			Snippet:   f.semanticSnippet(info.content, approx),
			Score:     &score,
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

// semanticAllowedSessionIDs runs one query returning the subset of ids that
// pass the ContentSearchFilter's metadata scope (project, agent, date
// range, one-shot/automated, ...), reusing buildSessionFilter via the same
// SessionFilter mapping sessionScopeSubquery uses so the two paths cannot
// drift apart.
func (db *DB) semanticAllowedSessionIDs(
	ctx context.Context, f ContentSearchFilter, ids []string,
) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	where, args := buildSessionFilter(contentSessionFilter(f))
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	for _, id := range ids {
		args = append(args, id)
	}
	query := "SELECT id FROM sessions WHERE " + where +
		" AND id IN (" + placeholders + ")"

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic search session scope: %w", err)
	}
	defer rows.Close()

	allowed := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan semantic session id: %w", err)
		}
		allowed[id] = true
	}
	return allowed, rows.Err()
}

// semanticHitKey identifies a (session_id, ordinal) pair for enrichment
// lookup.
type semanticHitKey struct {
	sessionID string
	ordinal   int
}

// semanticHitInfo is the session/message metadata enrichSemanticHits attaches
// to a surviving hit. content is the message's full, un-truncated content:
// semantic/hybrid snippets are built from it (see semanticSnippet) rather
// than from the searcher's pre-truncated chunk/snippet text, so secret
// redaction sees the same whole-body context the substring/regex/fts paths
// give it instead of a fragment that can split a secret at the truncation
// boundary.
type semanticHitInfo struct {
	project, agent, role, timestamp, content string
}

// enrichSemanticHits looks up session/message metadata for hits' (session_id,
// ordinal) pairs in one query via a "WITH hits(session_id, ordinal) AS
// (VALUES ...)" CTE joined to messages/sessions. SQLite versions without
// row-value IN support over VALUES rule out "(session_id, ordinal) IN
// (VALUES ...)"; the CTE join form works everywhere.
func (db *DB) enrichSemanticHits(
	ctx context.Context, hits []VectorHit,
) (map[semanticHitKey]semanticHitInfo, error) {
	values := make([]string, len(hits))
	args := make([]any, 0, len(hits)*2)
	for i, h := range hits {
		values[i] = "(?, ?)"
		args = append(args, h.SessionID, h.Ordinal)
	}
	query := "WITH hits(session_id, ordinal) AS (VALUES " +
		strings.Join(values, ", ") + ") " +
		"SELECT m.session_id, s.project, s.agent, m.role, m.ordinal, " +
		"COALESCE(m.timestamp, ''), m.content " +
		"FROM hits h " +
		"JOIN messages m ON m.session_id = h.session_id AND m.ordinal = h.ordinal " +
		"JOIN sessions s ON s.id = m.session_id"

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic search enrich: %w", err)
	}
	defer rows.Close()

	out := make(map[semanticHitKey]semanticHitInfo, len(hits))
	for rows.Next() {
		var key semanticHitKey
		var info semanticHitInfo
		if err := rows.Scan(&key.sessionID, &info.project, &info.agent,
			&info.role, &key.ordinal, &info.timestamp, &info.content); err != nil {
			return nil, fmt.Errorf("scan semantic hit: %w", err)
		}
		out[key] = info
	}
	return out, rows.Err()
}

// snippetTruncationMarkers are the elision markers left by the two sources of
// approximate snippet text semantic/hybrid modes locate within a message's
// full content: the vector index's trailing unicode ellipsis
// (internal/vector's truncateRunes) and FTS5 snippet()'s literal "..." marker
// (used at both ends), configured as hybridFTSLeg's 5th snippet() argument.
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

// semanticSnippet builds the returned snippet for "semantic" and "hybrid"
// matches from the message's full content, not from the searcher's
// pre-truncated approx (chunk or FTS snippet() text): redaction
// (buildSnippet -> secrets.RedactWindow) must see the whole message so a
// secret straddling approx's truncation boundary cannot leak a fragment that
// full-content redaction would otherwise catch. approx is used only to
// center the window; when it cannot be located in content, FTSSnippetRange
// centers on the query pattern instead, and failing that on the start of
// content -- content is still what gets redacted either way.
func (f ContentSearchFilter) semanticSnippet(content, approx string) string {
	if start, end, ok := approxSnippetSpan(content, approx); ok {
		return f.buildSnippet(content, start, end)
	}
	start, end := FTSSnippetRange(f.Pattern, content)
	return f.buildSnippet(content, start, end)
}
