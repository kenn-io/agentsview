package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
)

const (
	pgSnippetRadius = 60 // chars of context on each side of a match
)

// SearchContent implements content search for the PostgreSQL read-only store.
// The "fts" mode falls back to ILIKE over message content, with one predicate
// per prepared FTS term to preserve SQLite's implicit-AND semantics.
func (s *Store) SearchContent(
	ctx context.Context, f db.ContentSearchFilter,
) (db.ContentSearchPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxContentSearchLimit {
		f.Limit = db.DefaultContentSearchLimit
	}
	if f.Pattern == "" {
		return db.ContentSearchPage{}, nil
	}

	// Semantic and hybrid validate Sources themselves (messages only) ahead
	// of the substring/regex/fts source-set default just below, which fills
	// in tool_input/tool_result that neither mode supports -- mirroring
	// internal/db's SearchContent so an empty Sources field is not defaulted
	// out from under ValidateSemanticFilter's empty-or-messages-only check.
	if f.Mode == "semantic" || f.Mode == "hybrid" {
		// Validate input the same way SQLite's semantic/hybrid paths do
		// before reporting the capability gate: an invalid request (bad
		// cursor, non-messages source) must return the same 400
		// SearchInputError on every backend rather than a 501 here and a
		// 400 on SQLite (backend parity, see AGENTS.md).
		if err := db.ValidateSemanticFilter(f); err != nil {
			return db.ContentSearchPage{}, err
		}
		// No VectorSearcher seam on the PostgreSQL store yet (HasSemantic
		// always false): gate after input validation.
		return db.ContentSearchPage{}, db.ErrSemanticUnavailable
	}

	if len(f.Sources) == 0 {
		f.Sources = []string{"messages", "tool_input", "tool_result"}
	}
	for _, src := range f.Sources {
		if src != "messages" && src != "tool_input" && src != "tool_result" {
			return db.ContentSearchPage{},
				&db.SearchInputError{Msg: fmt.Sprintf("search: unknown source %q", src)}
		}
	}
	switch f.Mode {
	case "", "substring":
		return s.searchContentSubstringPG(ctx, f)
	case "fts":
		f.Sources = []string{"messages"}
		return s.searchContentSubstringPG(ctx, f)
	case "regex":
		return s.searchContentRegexPG(ctx, f)
	default:
		return db.ContentSearchPage{},
			&db.SearchInputError{Msg: fmt.Sprintf("search: invalid mode %q", f.Mode)}
	}
}

// pgHasSource reports whether src is in f.Sources.
func pgHasSource(f db.ContentSearchFilter, src string) bool {
	return slices.Contains(f.Sources, src)
}

// pgSessionFilter builds a db.SessionFilter from a ContentSearchFilter.
func pgSessionFilter(f db.ContentSearchFilter) db.SessionFilter {
	return db.SessionFilter{
		Project: f.Project, ExcludeProject: f.ExcludeProject,
		Machine: f.Machine, GitBranch: f.GitBranch, Agent: f.Agent,
		Date: f.Date, DateFrom: f.DateFrom, DateTo: f.DateTo,
		ActiveSince:      f.ActiveSince,
		ExcludeOneShot:   !f.IncludeOneShot,
		ExcludeAutomated: !f.IncludeAutomated,
		IncludeChildren:  f.IncludeChildren,
	}
}

// searchContentSubstringPG runs ILIKE-based UNION ALL across the selected
// sources, scoped to qualifying sessions via a WITH scoped CTE.
func (s *Store) searchContentSubstringPG(
	ctx context.Context, f db.ContentSearchFilter,
) (db.ContentSearchPage, error) {
	scopeWhere, scopeArgs := buildPGSessionFilter(pgSessionFilter(f))
	escapedPat := escapeLike(f.Pattern)

	pb := &paramBuilder{
		n:    len(scopeArgs),
		args: append([]any{}, scopeArgs...),
	}

	var branches []string
	if pgHasSource(f, "messages") {
		branches = append(branches, pgMessagesBranch(f, escapedPat, pb))
	}
	if pgHasSource(f, "tool_input") {
		branches = append(branches, pgToolInputBranch(f, escapedPat, pb))
	}
	if pgHasSource(f, "tool_result") {
		branches = append(branches, pgToolResultContentBranch(f, escapedPat, pb))
		branches = append(branches, pgToolResultEventsBranch(f, escapedPat, pb))
	}
	if len(branches) == 0 {
		return db.ContentSearchPage{}, nil
	}

	limitP := pb.add(f.Limit + 1)
	offsetP := pb.add(f.Cursor)
	query := "WITH scoped AS (SELECT id FROM sessions WHERE " + scopeWhere + ") " +
		"SELECT session_id, project, agent, location, role, tool_name, " +
		"ordinal, ts, snippet FROM (" +
		strings.Join(branches, " UNION ALL ") +
		") sub ORDER BY sort_ts DESC NULLS LAST, session_id ASC, ordinal ASC, src ASC, row_id ASC " +
		"LIMIT " + limitP + " OFFSET " + offsetP

	return s.scanPGContentMatches(ctx, query, pb.args, f.Limit, f.Cursor,
		func(body string) string { return pgSubstringSnippet(f, body) })
}

// pgMessagesBranch builds the messages source branch SQL.
// Placeholders continue from pb's current position.
func pgMessagesBranch(
	f db.ContentSearchFilter, escapedPat string, pb *paramBuilder,
) string {
	contentPred := pgContentSearchPredicate(
		"m.content", f, escapedPat, pb,
	)

	sysPred := "TRUE"
	if f.ExcludeSystem {
		sysPred = "m.is_system = FALSE AND " +
			db.PostgresSystemPrefixSQL("m.content", "m.role")
	}

	// Select the full content; the snippet is windowed and redacted in Go.
	return fmt.Sprintf(`
		SELECT m.session_id, s.project, s.agent, 'message' AS location,
			m.role AS role, '' AS tool_name, m.ordinal,
			COALESCE(m.timestamp::text, '') AS ts,
			m.content AS snippet, 0 AS src, 0::bigint AS row_id,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		JOIN scoped sc ON sc.id = m.session_id
		WHERE %s
		  AND %s`,
		contentPred, sysPred)
}

func pgContentSearchPredicate(
	column string, f db.ContentSearchFilter, escapedPat string,
	pb *paramBuilder,
) string {
	if f.Mode != "fts" {
		ilikePat := "%" + escapedPat + "%"
		ilikeParam := pb.add(ilikePat)
		return fmt.Sprintf(
			"%s ILIKE '%%'||%s||'%%' ESCAPE E'\\\\'",
			column, ilikeParam,
		)
	}

	terms := db.FTSTerms(db.PrepareFTSQuery(f.Pattern))
	clauses := make([]string, 0, len(terms))
	for _, term := range terms {
		if term == "" {
			continue
		}
		termParam := pb.add(escapeLike(term))
		clauses = append(clauses, fmt.Sprintf(
			"%s ILIKE '%%'||%s||'%%' ESCAPE E'\\\\'",
			column, termParam,
		))
	}
	if len(clauses) == 0 {
		return "FALSE"
	}
	return strings.Join(clauses, " AND ")
}

// pgToolInputBranch builds the tool_input source branch SQL.
func pgToolInputBranch(
	f db.ContentSearchFilter, escapedPat string, pb *paramBuilder,
) string {
	ilikePat := "%" + escapedPat + "%"
	ilikeParam := pb.add(ilikePat)

	return fmt.Sprintf(`
		SELECT tc.session_id, s.project, s.agent, 'tool_input' AS location,
			'assistant' AS role, tc.tool_name, tc.message_ordinal AS ordinal,
			COALESCE(m.timestamp::text, '') AS ts,
			tc.input_json AS snippet, 1 AS src, tc.id AS row_id,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts
		FROM tool_calls tc
		JOIN sessions s ON s.id = tc.session_id
		JOIN scoped sc ON sc.id = tc.session_id
		JOIN messages m ON m.session_id = tc.session_id
			AND m.ordinal = tc.message_ordinal
		WHERE tc.input_json ILIKE '%%'||%s||'%%' ESCAPE E'\\'`,
		ilikeParam)
}

// pgToolResultContentBranch matches result_content when the call has no result
// events sharing the same non-empty tool_use_id. The dedup is skipped for empty
// IDs (mirrors SQLite) so one empty-ID event cannot suppress the result_content
// of other empty-ID calls in the session.
func pgToolResultContentBranch(
	f db.ContentSearchFilter, escapedPat string, pb *paramBuilder,
) string {
	ilikePat := "%" + escapedPat + "%"
	ilikeParam := pb.add(ilikePat)

	return fmt.Sprintf(`
		SELECT tc.session_id, s.project, s.agent, 'tool_result' AS location,
			'assistant' AS role, tc.tool_name, tc.message_ordinal AS ordinal,
			COALESCE(m.timestamp::text, '') AS ts,
			tc.result_content AS snippet, 2 AS src, tc.id AS row_id,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts
		FROM tool_calls tc
		JOIN sessions s ON s.id = tc.session_id
		JOIN scoped sc ON sc.id = tc.session_id
		JOIN messages m ON m.session_id = tc.session_id
			AND m.ordinal = tc.message_ordinal
		WHERE tc.result_content ILIKE '%%'||%s||'%%' ESCAPE E'\\'
		  AND NOT EXISTS (
			SELECT 1 FROM tool_result_events tre
			WHERE tre.session_id = tc.session_id
			  AND tre.tool_use_id = tc.tool_use_id
			  AND tc.tool_use_id <> ''
		  )`,
		ilikeParam)
}

// pgToolResultEventsBranch matches tool_result_events.content.
func pgToolResultEventsBranch(
	f db.ContentSearchFilter, escapedPat string, pb *paramBuilder,
) string {
	ilikePat := "%" + escapedPat + "%"
	ilikeParam := pb.add(ilikePat)

	return fmt.Sprintf(`
		SELECT tre.session_id, s.project, s.agent, 'tool_result' AS location,
			'assistant' AS role, '' AS tool_name,
			tre.tool_call_message_ordinal AS ordinal,
			COALESCE(tre.timestamp::text, '') AS ts,
			tre.content AS snippet, 3 AS src, tre.id AS row_id,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts
		FROM tool_result_events tre
		JOIN sessions s ON s.id = tre.session_id
		JOIN scoped sc ON sc.id = tre.session_id
		WHERE tre.content ILIKE '%%'||%s||'%%' ESCAPE E'\\'`,
		ilikeParam)
}

// scanPGContentMatches runs query and assembles a ContentSearchPage. The
// query's final column is the full source field; makeSnippet derives the
// windowed, redacted snippet so redaction sees whole secrets.
func (s *Store) scanPGContentMatches(
	ctx context.Context, query string, args []any, limit, cursor int,
	makeSnippet func(body string) string,
) (db.ContentSearchPage, error) {
	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return db.ContentSearchPage{}, fmt.Errorf("pg content search: %w", err)
	}
	defer rows.Close()

	out := make([]db.ContentMatch, 0)
	for rows.Next() {
		var m db.ContentMatch
		var body string
		if err := rows.Scan(
			&m.SessionID, &m.Project, &m.Agent,
			&m.Location, &m.Role, &m.ToolName, &m.Ordinal,
			&m.Timestamp, &body,
		); err != nil {
			return db.ContentSearchPage{}, fmt.Errorf("scan pg match: %w", err)
		}
		m.Snippet = makeSnippet(body)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return db.ContentSearchPage{}, err
	}
	page := db.ContentSearchPage{Matches: out}
	if len(out) > limit {
		page.Matches = out[:limit]
		page.NextCursor = cursor + limit
	}
	return page, nil
}

// searchContentRegexPG: ILIKE prefilter on literal prefix then Go RE2 match.
func (s *Store) searchContentRegexPG(
	ctx context.Context, f db.ContentSearchFilter,
) (db.ContentSearchPage, error) {
	re, err := regexp.Compile(f.Pattern)
	if err != nil {
		return db.ContentSearchPage{},
			&db.SearchInputError{Msg: fmt.Sprintf("search: invalid regex: %v", err)}
	}
	lit := literalPrefixPG(f.Pattern)

	rows, err := s.pgRegexCandidateRows(ctx, f, lit)
	if err != nil {
		return db.ContentSearchPage{}, err
	}
	defer rows.Close()

	out := make([]db.ContentMatch, 0)
	seen := 0
	for rows.Next() {
		var m db.ContentMatch
		var body string
		if err := rows.Scan(
			&m.SessionID, &m.Project, &m.Agent,
			&m.Location, &m.Role, &m.ToolName, &m.Ordinal,
			&m.Timestamp, &body,
		); err != nil {
			return db.ContentSearchPage{},
				fmt.Errorf("scan pg regex candidate: %w", err)
		}
		loc := re.FindStringIndex(body)
		if loc == nil {
			continue
		}
		if seen < f.Cursor {
			seen++
			continue
		}
		m.Snippet = pgBuildSnippet(f, body, loc[0], loc[1])
		out = append(out, m)
		if len(out) > f.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return db.ContentSearchPage{}, err
	}
	page := db.ContentSearchPage{Matches: out}
	if len(out) > f.Limit {
		page.Matches = out[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	return page, nil
}

// pgRegexCandidateRows fetches full-body rows for regex pre-filtering.
func (s *Store) pgRegexCandidateRows(
	ctx context.Context, f db.ContentSearchFilter, lit string,
) (*sql.Rows, error) {
	scopeWhere, scopeArgs := buildPGSessionFilter(pgSessionFilter(f))

	pb := &paramBuilder{
		n:    len(scopeArgs),
		args: append([]any{}, scopeArgs...),
	}

	var branches []string
	if pgHasSource(f, "messages") {
		branches = append(branches, pgMessagesCandidateBranch(f, lit, pb))
	}
	if pgHasSource(f, "tool_input") {
		branches = append(branches, pgToolInputCandidateBranch(f, lit, pb))
	}
	if pgHasSource(f, "tool_result") {
		branches = append(branches,
			pgToolResultContentCandidateBranch(f, lit, pb))
		branches = append(branches,
			pgToolResultEventsCandidateBranch(f, lit, pb))
	}
	if len(branches) == 0 {
		q := "SELECT '' AS session_id, '' AS project, '' AS agent, " +
			"'' AS location, '' AS role, '' AS tool_name, 0 AS ordinal, " +
			"'' AS ts, '' AS body WHERE FALSE"
		return s.pg.QueryContext(ctx, q)
	}

	query := "WITH scoped AS (SELECT id FROM sessions WHERE " + scopeWhere + ") " +
		"SELECT session_id, project, agent, location, role, tool_name, " +
		"ordinal, ts, body FROM (" +
		strings.Join(branches, " UNION ALL ") +
		") sub ORDER BY sort_ts DESC NULLS LAST, session_id ASC, ordinal ASC, src ASC, row_id ASC"
	return s.pg.QueryContext(ctx, query, pb.args...)
}

// pgPrefilterClause returns an ILIKE clause for lit, or IS NOT NULL when lit
// is empty (full scan needed for zero-literal regexes).
func pgPrefilterClause(col, lit string, pb *paramBuilder) string {
	if lit == "" {
		return col + " IS NOT NULL"
	}
	escaped := escapeLike(lit)
	p := pb.add("%" + escaped + "%")
	return fmt.Sprintf("%s ILIKE '%%'||%s||'%%' ESCAPE E'\\\\'", col, p)
}

// pgMessagesCandidateBranch: candidate rows for regex from messages.
func pgMessagesCandidateBranch(
	f db.ContentSearchFilter, lit string, pb *paramBuilder,
) string {
	prefilter := pgPrefilterClause("m.content", lit, pb)

	sysPred := "TRUE"
	if f.ExcludeSystem {
		sysPred = "m.is_system = FALSE AND " +
			db.PostgresSystemPrefixSQL("m.content", "m.role")
	}

	return fmt.Sprintf(`
		SELECT m.session_id, s.project, s.agent, 'message' AS location,
			m.role AS role, '' AS tool_name, m.ordinal,
			COALESCE(m.timestamp::text, '') AS ts,
			m.content AS body, 0 AS src, 0::bigint AS row_id,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		JOIN scoped sc ON sc.id = m.session_id
		WHERE %s AND %s`,
		prefilter, sysPred)
}

// pgToolInputCandidateBranch: candidate rows for regex from tool_input.
func pgToolInputCandidateBranch(
	f db.ContentSearchFilter, lit string, pb *paramBuilder,
) string {
	prefilter := pgPrefilterClause("tc.input_json", lit, pb)

	return fmt.Sprintf(`
		SELECT tc.session_id, s.project, s.agent, 'tool_input' AS location,
			'assistant' AS role, tc.tool_name, tc.message_ordinal AS ordinal,
			COALESCE(m.timestamp::text, '') AS ts,
			tc.input_json AS body, 1 AS src, tc.id AS row_id,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts
		FROM tool_calls tc
		JOIN sessions s ON s.id = tc.session_id
		JOIN scoped sc ON sc.id = tc.session_id
		JOIN messages m ON m.session_id = tc.session_id
			AND m.ordinal = tc.message_ordinal
		WHERE %s`,
		prefilter)
}

// pgToolResultContentCandidateBranch: candidate result_content rows (no events).
func pgToolResultContentCandidateBranch(
	f db.ContentSearchFilter, lit string, pb *paramBuilder,
) string {
	prefilter := pgPrefilterClause("tc.result_content", lit, pb)

	return fmt.Sprintf(`
		SELECT tc.session_id, s.project, s.agent, 'tool_result' AS location,
			'assistant' AS role, tc.tool_name, tc.message_ordinal AS ordinal,
			COALESCE(m.timestamp::text, '') AS ts,
			tc.result_content AS body, 2 AS src, tc.id AS row_id,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts
		FROM tool_calls tc
		JOIN sessions s ON s.id = tc.session_id
		JOIN scoped sc ON sc.id = tc.session_id
		JOIN messages m ON m.session_id = tc.session_id
			AND m.ordinal = tc.message_ordinal
		WHERE %s
		  AND NOT EXISTS (
			SELECT 1 FROM tool_result_events tre
			WHERE tre.session_id = tc.session_id
			  AND tre.tool_use_id = tc.tool_use_id
			  AND tc.tool_use_id <> ''
		  )`,
		prefilter)
}

// pgToolResultEventsCandidateBranch: candidate tool_result_events.content rows.
func pgToolResultEventsCandidateBranch(
	f db.ContentSearchFilter, lit string, pb *paramBuilder,
) string {
	prefilter := pgPrefilterClause("tre.content", lit, pb)

	return fmt.Sprintf(`
		SELECT tre.session_id, s.project, s.agent, 'tool_result' AS location,
			'assistant' AS role, '' AS tool_name,
			tre.tool_call_message_ordinal AS ordinal,
			COALESCE(tre.timestamp::text, '') AS ts,
			tre.content AS body, 3 AS src, tre.id AS row_id,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts
		FROM tool_result_events tre
		JOIN sessions s ON s.id = tre.session_id
		JOIN scoped sc ON sc.id = tre.session_id
		WHERE %s`,
		prefilter)
}

// pgSnippetBounds returns the rune-snapped byte window around [start,end),
// mirroring snippetBounds in internal/db/search_content.go.
func pgSnippetBounds(text string, start, end int) (int, int) {
	lo := max(start-pgSnippetRadius, 0)
	hi := min(end+pgSnippetRadius, len(text))
	for lo < start && !utf8.RuneStart(text[lo]) {
		lo++
	}
	for hi > end && hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi--
	}
	return lo, hi
}

// pgBuildSnippet windows body around [start,end) and, unless reveal is set,
// masks secrets overlapping the window (including straddling ones) via
// secrets.RedactWindow, so a pre-truncated snippet can never leak a fragment.
func pgBuildSnippet(f db.ContentSearchFilter, body string, start, end int) string {
	lo, hi := pgSnippetBounds(body, start, end)
	if f.RevealSecrets {
		return body[lo:hi]
	}
	return secrets.RedactWindow(body, lo, hi)
}

// pgSubstringSnippet builds a substring-match snippet: it locates the
// case-insensitive pattern (the ILIKE already matched, so it is present; fall
// back to the start) and windows it. It uses db.CaseInsensitiveIndex so the
// offset indexes body directly even when lowercasing would change byte length.
func pgSubstringSnippet(f db.ContentSearchFilter, body string) string {
	if f.Mode == "fts" {
		start, end := db.FTSSnippetRange(f.Pattern, body)
		return pgBuildSnippet(f, body, start, end)
	}
	off := max(db.CaseInsensitiveIndex(body, f.Pattern), 0)
	return pgBuildSnippet(f, body, off, min(off+len(f.Pattern), len(body)))
}

// literalPrefixPG returns the required literal prefix from a regex pattern,
// identical to the SQLite-side literalPrefix.
func literalPrefixPG(pattern string) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return ""
	}
	prefix, _ := re.LiteralPrefix()
	return prefix
}
