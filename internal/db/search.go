package db

import (
	"context"
	"fmt"
	"strings"
)

const (
	DefaultSearchLimit = 50
	MaxSearchLimit     = 500
	snippetTokenLength = 32
)

// SystemMsgPrefixes lists content prefixes that identify system-injected
// user messages. These are excluded from search results even when the
// is_system column has not been backfilled (e.g. Claude sessions parsed
// before schema version 2). Keep in sync with the frontend list in
// frontend/src/lib/utils/messages.ts.
var SystemMsgPrefixes = []string{
	"This session is being continued",
	"[Request interrupted",
	"<task-notification>",
	"<command-message>",
	"<command-name>",
	"<local-command-",
	"Stop hook feedback:",
}

// SystemPrefixSQL returns a SQL clause that excludes user messages
// matching any system prefix. The column alias for content must be
// passed (e.g. "m.content" or "m2.content"). Uses case-sensitive
// substr comparison, which behaves identically on SQLite and
// PostgreSQL (unlike LIKE, which is case-insensitive on SQLite).
func SystemPrefixSQL(contentCol, roleCol string) string {
	// LTRIM strips the same whitespace as Go's strings.TrimSpace,
	// JS .trim(), and the parser's isSystem helpers: ASCII whitespace,
	// BOM (U+FEFF), and Unicode
	// spaces (U+0085, U+00A0, U+1680, U+2000–U+200A, U+2028,
	// U+2029, U+202F, U+205F, U+3000). Both SQLite and PostgreSQL
	// handle multi-byte UTF-8 characters in the trim set correctly.
	trimmed := "LTRIM(" + contentCol + ", ' \t\n\v\f\r" +
		"\u0085\u00A0\u1680" +
		"\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200A" +
		"\u2028\u2029\u202F\u205F\u3000\uFEFF')"
	parts := make([]string, len(SystemMsgPrefixes))
	for i, p := range SystemMsgPrefixes {
		parts[i] = fmt.Sprintf(
			"substr(%s, 1, %d) = '%s'", trimmed, len(p), p,
		)
	}
	return "NOT (" + roleCol + " = 'user' AND (" +
		strings.Join(parts, " OR ") + "))"
}

// SearchResult holds a session-level match with the best-ranked snippet.
type SearchResult struct {
	SessionID      string  `json:"session_id"`
	Project        string  `json:"project"`
	Agent          string  `json:"agent"`
	Name           string  `json:"name"`
	Ordinal        int     `json:"ordinal"`
	SessionEndedAt string  `json:"session_ended_at"`
	Snippet        string  `json:"snippet"`
	Rank           float64 `json:"rank"`
}

// SearchFilter specifies search parameters.
type SearchFilter struct {
	Query   string
	Project string
	Sort    string // "relevance" (default) or "recency"
	Cursor  int    // offset for pagination
	Limit   int
}

// SearchPage holds paginated search results.
type SearchPage struct {
	Results    []SearchResult `json:"results"`
	NextCursor int            `json:"next_cursor,omitempty"`
}

// Search performs FTS5 full-text search across messages, grouped by session,
// plus a LIKE-based search on session display names and first messages.
//
// Results come from two branches joined with UNION ALL:
//
//  1. FTS branch — message content matches. ROW_NUMBER() picks the single
//     best-ranked message per session (rank ASC, ordinal ASC, rowid ASC).
//     The outer JOIN messages_fts includes a MATCH clause to prevent segment
//     duplicates. Ordinal is the matched message's ordinal (≥ 0).
//
//  2. Name branch — display_name / first_message LIKE matches that are NOT
//     already covered by the FTS branch. Ordinal is -1 (no specific message
//     to navigate to).
func (db *DB) Search(
	ctx context.Context, f SearchFilter,
) (SearchPage, error) {
	if f.Limit <= 0 || f.Limit > MaxSearchLimit {
		f.Limit = DefaultSearchLimit
	}
	f.Query = PrepareFTSQuery(f.Query)

	// ORDER BY for the outer query. FTS5 ranks are negative (lower = better),
	// so rank ASC places message matches (negative rank) before name-only rows
	// (rank=0.0). Within FTS results, match_pos ASC prefers earlier positions.
	// julianday() normalises RFC3339Nano text to a numeric value, avoiding
	// lexicographic misorderings from variable fractional-second precision
	// (e.g. "…T12:00:00Z" vs "…T12:00:00.123Z"). SQLite NULLs sort smaller
	// than any value, so julianday(NULL) DESC naturally places NULLs last.
	orderBy := "rank ASC, match_pos ASC, julianday(session_ended_at) DESC, session_id ASC"
	if f.Sort == "recency" {
		orderBy = "julianday(session_ended_at) DESC, session_id ASC"
	}

	// innerWhere is used in three places: the ROW_NUMBER inner subquery,
	// the outer MATCH re-filter, and the NOT IN subquery for the name branch.
	innerWhere := []string{
		"messages_fts MATCH ?",
		"s2.deleted_at IS NULL",
		"m2.is_system = 0",
		SystemPrefixSQL("m2.content", "m2.role"),
	}
	ftsArgs := []any{f.Query} // args for one copy of innerWhere

	nameProjectClause := ""
	var nameProjectArgs []any
	if f.Project != "" {
		innerWhere = append(innerWhere, "s2.project = ?")
		ftsArgs = append(ftsArgs, f.Project)
		nameProjectClause = "AND s.project = ?"
		nameProjectArgs = []any{f.Project}
	}

	innerWhereSQL := strings.Join(innerWhere, " AND ")
	// Strip FTS quoting before substring operations. PrepareFTSQuery wraps
	// each term in double quotes for FTS (e.g. "fix bug" → `"fix" "bug"`).
	// LIKE and instr() must use the plain text form so name/content substring
	// searches work correctly.
	plainQuery := StripFTSQuotes(f.Query)
	if plainQuery == "" {
		return SearchPage{}, nil
	}
	likePattern := "%" + escapeLike(plainQuery) + "%"

	// Build args in the order the SQL placeholders appear.
	// Position 0 (? AS best_query in the ROW_NUMBER SELECT) is
	// prepended after this block — see args2 below.
	//
	//  pos | SQL clause                                  | value
	//  ----+---------------------------------------------+------------
	//   0  | SELECT ? AS best_query (ROW_NUMBER)         | plainQuery  ← prepended in args2
	//   1  | WHERE messages_fts MATCH ? (ROW_NUMBER)     | ftsArgs[0] (f.Query)
	//  [1+]| AND s2.project = ? (if project set)         | ftsArgs[1] (f.Project)
	//   2  | WHERE messages_fts MATCH ? (outer JOIN)     | f.Query
	//   3  | WHEN COALESCE(display_name,session_name) LIKE ? (CASE) | likePattern
	//   4  | WHEN s.first_message LIKE ? (CASE)          | likePattern
	//   5  | WHERE COALESCE(display_name,session_name) LIKE ? (name WHERE) | likePattern
	//   6  | WHERE s.first_message LIKE ? (name WHERE)   | likePattern
	//  [7] | AND s.project = ? (name branch, optional)   | f.Project
	//   8  | WHERE messages_fts MATCH ? (NOT IN)         | ftsArgs[0]
	//  [8+]| AND s2.project = ? (NOT IN, if set)         | ftsArgs[1]
	//   9  | LIMIT ? OFFSET ?                            | f.Limit+1, f.Cursor
	args := make([]any, 0, len(ftsArgs)*2+6+len(nameProjectArgs))
	args = append(args, ftsArgs...)          // (1) ROW_NUMBER WHERE
	args = append(args, f.Query)             // (2) outer MATCH re-filter
	args = append(args, likePattern)         // (3) CASE COALESCE(display_name,session_name) LIKE
	args = append(args, likePattern)         // (4) CASE first_message LIKE
	args = append(args, likePattern)         // (5) name WHERE COALESCE(display_name,session_name) LIKE
	args = append(args, likePattern)         // (6) name WHERE first_message LIKE
	args = append(args, nameProjectArgs...)  // (7) optional name branch project
	args = append(args, ftsArgs...)          // (8) NOT IN WHERE
	args = append(args, f.Limit+1, f.Cursor) // (9) LIMIT / OFFSET

	query := fmt.Sprintf(`
		SELECT session_id, project, agent, name,
			session_ended_at, ordinal, snippet, rank, match_pos
		FROM (
			-- FTS branch: message content matches
			SELECT m.session_id, s.project, s.agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at, '') AS session_ended_at,
				best.best_ordinal AS ordinal,
				snippet(messages_fts, 0, '<mark>', '</mark>',
					'...', %d) AS snippet,
				best.best_rank AS rank,
				instr(LOWER(m.content), LOWER(best.best_query))
					AS match_pos
			FROM (
				SELECT session_id, best_rowid, best_ordinal, best_rank, best_query
				FROM (
					SELECT m2.session_id,
						messages_fts.rowid AS best_rowid,
						m2.ordinal AS best_ordinal,
						rank AS best_rank,
						? AS best_query,
						ROW_NUMBER() OVER (
							PARTITION BY m2.session_id
							ORDER BY rank ASC, m2.ordinal ASC,
								messages_fts.rowid ASC
						) AS rn
					FROM messages_fts
					JOIN messages m2 ON messages_fts.rowid = m2.id
					JOIN sessions s2 ON m2.session_id = s2.id
					WHERE %s
				)
				WHERE rn = 1
			) AS best
			JOIN messages_fts ON messages_fts.rowid = best.best_rowid
			JOIN messages m ON m.id = best.best_rowid
			JOIN sessions s ON m.session_id = s.id
			WHERE messages_fts MATCH ?

			UNION ALL

			-- Name branch: display_name / session_name / first_message matches not in FTS branch
			SELECT s.id, s.project, s.agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at, '') AS session_ended_at,
				-1 AS ordinal,
				CASE
					WHEN COALESCE(s.display_name, s.session_name) LIKE ? ESCAPE '\'
						THEN COALESCE(s.display_name, s.session_name, '')
					WHEN s.first_message LIKE ? ESCAPE '\'
						THEN COALESCE(s.first_message, '')
					ELSE COALESCE(s.display_name, s.session_name, s.first_message, '')
				END AS snippet,
				0.0 AS rank,
				0 AS match_pos
			FROM sessions s
			WHERE (COALESCE(s.display_name, s.session_name) LIKE ? ESCAPE '\'
				OR s.first_message LIKE ? ESCAPE '\')
				AND s.deleted_at IS NULL
				AND EXISTS (
					SELECT 1 FROM messages mx
					WHERE mx.session_id = s.id
					  AND mx.is_system = 0
					  AND `+SystemPrefixSQL("mx.content", "mx.role")+`
				)
				%s
				AND s.id NOT IN (
					SELECT m2.session_id
					FROM messages_fts
					JOIN messages m2 ON messages_fts.rowid = m2.id
					JOIN sessions s2 ON m2.session_id = s2.id
					WHERE %s
				)
		)
		ORDER BY %s
		LIMIT ? OFFSET ?`,
		snippetTokenLength,
		innerWhereSQL,     // ROW_NUMBER inner WHERE (%s at rn subquery)
		nameProjectClause, // optional project filter for name branch (%s)
		innerWhereSQL,     // NOT IN subquery WHERE (%s)
		orderBy,           // ORDER BY (%s)
	)

	// Replace the ROW_NUMBER inner subquery's ? for best_query with args
	// re-ordered: the first innerWhere param (f.Query) was already included in
	// ftsArgs above at position (1); best_query needs a second copy of f.Query
	// injected at the right position. Re-build args with the extra copy.
	//
	// The inner subquery's SELECT has `? AS best_query` before the WHERE, so
	// its ? comes before innerWhere's ?s. Rebuild:
	args2 := make([]any, 0, len(args)+1)
	args2 = append(args2, plainQuery) // best_query: plain text for instr()
	args2 = append(args2, args...)
	args = args2

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return SearchPage{}, fmt.Errorf("searching: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var matchPos int
		if err := rows.Scan(
			&r.SessionID, &r.Project, &r.Agent, &r.Name,
			&r.SessionEndedAt, &r.Ordinal,
			&r.Snippet, &r.Rank, &matchPos,
		); err != nil {
			return SearchPage{},
				fmt.Errorf("scanning result: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return SearchPage{}, err
	}

	page := SearchPage{Results: results}
	if len(results) > f.Limit {
		page.Results = results[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	return page, nil
}

// SearchSession performs a case-insensitive substring search within a single
// session's messages, returning matching ordinals in document order.
// This is used by the in-session find bar (analogous to browser Cmd+F).
// Both message content and tool-call result_content are searched so that
// matches inside tool output blocks are reachable. Only fields that the
// frontend renders and highlights are included to avoid phantom matches.
func (db *DB) SearchSession(
	ctx context.Context, sessionID, query string,
) ([]int, error) {
	if query == "" {
		return nil, nil
	}
	// Use LIKE for substring semantics consistent with browser find-bar UX.
	// SQLite LIKE is case-insensitive for ASCII by default.
	// LEFT JOIN tool_calls so that a hit in result_content also surfaces
	// the parent message ordinal; DISTINCT collapses multiple tool calls
	// on the same message into a single result.
	like := "%" + escapeLike(query) + "%"
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT DISTINCT m.ordinal
		 FROM messages m
		 LEFT JOIN tool_calls tc ON tc.message_id = m.id
		 WHERE m.session_id = ?
		   AND m.is_system = 0
		   AND `+SystemPrefixSQL("m.content", "m.role")+`
		   AND (m.content LIKE ? ESCAPE '\'
		        OR tc.result_content LIKE ? ESCAPE '\')
		 ORDER BY m.ordinal ASC`,
		sessionID, like, like,
	)
	if err != nil {
		return nil, fmt.Errorf("session search: %w", err)
	}
	defer rows.Close()

	var ordinals []int
	for rows.Next() {
		var ord int
		if err := rows.Scan(&ord); err != nil {
			return nil, fmt.Errorf("scanning ordinal: %w", err)
		}
		ordinals = append(ordinals, ord)
	}
	return ordinals, rows.Err()
}

// PrepareFTSQuery turns a user's raw search input into a well-formed SQLite
// FTS5 MATCH expression. Each whitespace-separated term is wrapped in double
// quotes (with any embedded quote doubled, per FTS5 escaping), which makes
// punctuation literal inside the term and combines the terms under FTS5's
// implicit AND. Quoting is what prevents a single token containing an FTS5
// operator character (e.g. "error-401" or "status:500") from being parsed as
// query syntax and raising a malformed-query error (an HTTP 500).
//
// An empty/whitespace-only input is returned unchanged. An input the caller
// already opened with a double quote is treated as a deliberate FTS5 expression
// (including an explicit "exact phrase") and is passed through untouched, so
// exact-phrase matching remains opt-in via a leading quote.
//
// This is the single source of truth shared by the SQLite, PostgreSQL, and HTTP
// search paths so the same user query behaves identically across backends.
func PrepareFTSQuery(raw string) string {
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

// FTSTerms decomposes a PrepareFTSQuery output back into its individual terms,
// un-doubling escaped quotes inside quoted terms and collecting bare tokens. A
// multi-term AND query like `"error" "401"` yields ["error", "401"], a single
// quoted operator token `"error-401"` yields ["error-401"], and an explicit
// exact phrase `"fix bug"` yields a single ["fix bug"] term. This lets the
// substring backends (SQLite name-branch LIKE/instr, PostgreSQL ILIKE)
// reconstruct the same AND-of-terms vs. exact-phrase semantics the FTS engine
// applies, keeping behavior identical across backends.
func FTSTerms(v string) []string {
	if !strings.Contains(v, `"`) {
		if v = strings.TrimSpace(v); v == "" {
			return nil
		}
		return strings.Fields(v)
	}
	var terms []string
	var cur strings.Builder
	inQuote := false
	hasTerm := false
	flush := func() {
		if hasTerm {
			terms = append(terms, cur.String())
			cur.Reset()
			hasTerm = false
		}
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '"':
			if inQuote && i+1 < len(v) && v[i+1] == '"' {
				// Doubled quote inside a quoted term is a literal quote.
				cur.WriteByte('"')
				hasTerm = true
				i++
				continue
			}
			inQuote = !inQuote
			hasTerm = true
		case !inQuote && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			flush()
		default:
			cur.WriteByte(c)
			hasTerm = true
		}
	}
	flush()
	return terms
}

// StripFTSQuotes reverses PrepareFTSQuery into a plain substring suitable for
// LIKE and instr() operations (name-branch matching, snippet centering). It
// rejoins the parsed FTS terms with single spaces. So `"unique" "phrase"`
// becomes "unique phrase", a single quoted token like `"error-401"` becomes
// "error-401", and an explicit phrase `"fix bug"` becomes "fix bug". Input with
// no quotes is returned unchanged.
func StripFTSQuotes(v string) string {
	if !strings.Contains(v, `"`) {
		return v
	}
	return strings.Join(FTSTerms(v), " ")
}
