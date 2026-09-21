package db

import (
	"cmp"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// termsSnippetSeparator joins the per-term windows of a terms snippet when
// the matched terms sit too far apart to share one window.
const termsSnippetSeparator = " ... "

// ParseContentSearchTerms returns de-duplicated whitespace-separated literal
// terms while preserving first-seen spelling and order.
func ParseContentSearchTerms(pattern string) []string {
	fields := strings.Fields(pattern)
	seen := make(map[string]struct{}, len(fields))
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		key := strings.ToLower(field)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		terms = append(terms, field)
	}
	return terms
}

// ValidateTermsFilter validates the message-only, scope-aware terms mode.
func ValidateTermsFilter(f ContentSearchFilter) error {
	for _, source := range f.Sources {
		if source != "messages" {
			return searchInputErrorf(
				"search: terms searches messages only (got source %q)", source)
		}
	}
	switch f.Scope {
	case "", "all", "top", "subordinate":
		return nil
	default:
		return searchInputErrorf("search: invalid scope %q", f.Scope)
	}
}

// termsSQLFragments holds the SQL that differs between the backends that
// implement terms mode. Everything else in the query is shared so the
// exchange definition cannot drift between them.
type termsSQLFragments struct {
	systemPrefix func(contentCol, roleCol string) string
	// bodyAgg concatenates an exchange's message content in ordinal order.
	bodyAgg string
	// timestamp and sessionSort select the match timestamp and the session
	// recency key; orderBySort orders by that key, newest first.
	timestamp, sessionSort, orderBySort string
}

var termsSQLByDialect = map[string]termsSQLFragments{
	"sqlite": {
		systemPrefix: SystemPrefixSQL,
		bodyAgg:      "GROUP_CONCAT(content, char(10) ORDER BY ordinal)",
		timestamp:    "COALESCE(m.timestamp,'')",
		sessionSort:  "COALESCE(s.ended_at, s.started_at, s.created_at, '')",
		orderBySort:  "julianday(sort_ts) DESC",
	},
	"postgres": {
		systemPrefix: PostgresSystemPrefixSQL,
		bodyAgg:      `STRING_AGG(content, E'\n' ORDER BY ordinal)`,
		timestamp:    "m.timestamp",
		sessionSort:  "COALESCE(s.ended_at, s.started_at, s.created_at)",
		orderBySort:  "sort_ts DESC NULLS LAST",
	},
}

// BuildTermsSearchSQL renders the terms-mode query for a dialect. It groups
// embeddable user/assistant rows into a transient exchange: one user message
// and the following assistant run on the same sidechain branch. Every term
// must occur literally somewhere in that exchange. Tool and system content
// never participates.
//
// Session scope follows the semantic modes: Scope governs subordinate
// visibility, so the sidebar-child exclusion is omitted and child sessions
// are exempt from the one-shot gate. Only sessions where each term occurs in
// some message are grouped, which keeps the aggregation off the rest of the
// archive; a term holds no whitespace, so it cannot span the newline that
// joins two messages and the prefilter never drops a real match. Assistant
// rows before a session's first user message anchor no exchange, so those
// groups are dropped and assistant-only sessions never match.
//
// Columns: session_id, project, agent, transcript_revision, location, role,
// start_ordinal, timestamp, body, end_ordinal, relationship_type,
// parent_session_id, is_sidechain, subordinate. ScanTermsMatches reads them.
func BuildTermsSearchSQL(
	f ContentSearchFilter, terms []string, dialect QueryDialect,
) (string, []any, error) {
	frag, ok := termsSQLByDialect[dialect.name]
	if !ok {
		return "", nil, searchInputErrorf(
			"search: terms mode is not supported on the %s backend", dialect.name)
	}
	sf := ContentSessionFilter(f)
	sf.ChildExemptOneShot = true
	where, scopeArgs := BuildSessionBaseFilterSQL(sf, dialect)
	b := NewQueryBuilder(dialect, len(scopeArgs))
	if ids := NormalizeExcludeSessionIDs(f.ExcludeSessionIDs); len(ids) > 0 {
		phs := make([]string, len(ids))
		for i, id := range ids {
			phs[i] = b.Add(id)
		}
		where += " AND id NOT IN (" + strings.Join(phs, ",") + ")"
	}

	var prefilter strings.Builder
	for _, term := range terms {
		prefilter.WriteString(`
			  AND m.session_id IN (
				SELECT session_id FROM messages
				WHERE session_id IN (SELECT id FROM scoped)
				  AND ` + b.ContainsPredicate("content", term) + ")")
	}
	predicates := make([]string, 0, len(terms)+1)
	for _, term := range terms {
		predicates = append(predicates, b.ContainsPredicate("body", term))
	}
	switch f.Scope {
	case "top":
		predicates = append(predicates, "subordinate = "+dialect.falseLiteral)
	case "subordinate":
		predicates = append(predicates, "subordinate = "+dialect.trueLiteral)
	}

	query := fmt.Sprintf(`
		WITH scoped AS (
			SELECT id FROM sessions WHERE %s
		), eligible AS (
			SELECT m.session_id, s.project, s.agent,
				COALESCE(s.transcript_revision,'') AS transcript_revision,
				COALESCE(s.relationship_type,'') AS relationship_type,
				COALESCE(s.parent_session_id,'') AS parent_session_id,
				m.role, m.ordinal, %s AS ts, m.content, m.is_sidechain,
				%s AS sort_ts,
				CASE WHEN m.is_sidechain = %s OR %s
					THEN %s ELSE %s END AS subordinate,
				CASE WHEN m.role = 'user' THEN 1 ELSE 0 END AS user_start
			FROM messages m
			JOIN sessions s ON s.id = m.session_id
			JOIN scoped sc ON sc.id = m.session_id
			WHERE m.role IN ('user','assistant')
			  AND m.is_system = %s AND %s%s
		), tagged AS (
			SELECT *, SUM(user_start) OVER (
				PARTITION BY session_id, is_sidechain
				ORDER BY ordinal ROWS UNBOUNDED PRECEDING
			) AS exchange_no
			FROM eligible
		), exchanges AS (
			SELECT session_id, project, agent, transcript_revision, relationship_type,
				parent_session_id, is_sidechain, subordinate, exchange_no,
				MIN(ordinal) AS start_ordinal, MAX(ordinal) AS end_ordinal,
				CASE WHEN exchange_no > 0 THEN 'user' ELSE 'assistant' END AS role,
				MIN(ts) AS ts,
				%s AS body,
				MAX(sort_ts) AS sort_ts
			FROM tagged
			WHERE exchange_no > 0
			GROUP BY session_id, project, agent, transcript_revision, relationship_type,
				parent_session_id, is_sidechain, subordinate, exchange_no
		)
		SELECT session_id, project, agent, transcript_revision, 'message', role, start_ordinal,
			ts, body, end_ordinal, relationship_type, parent_session_id,
			is_sidechain, subordinate
		FROM exchanges
		WHERE %s
		ORDER BY subordinate ASC, %s, session_id ASC, start_ordinal ASC
		LIMIT %s OFFSET %s`,
		where, frag.timestamp, frag.sessionSort,
		dialect.trueLiteral, SubordinateSessionSQL("s"),
		dialect.trueLiteral, dialect.falseLiteral,
		dialect.falseLiteral, frag.systemPrefix("m.content", "m.role"),
		prefilter.String(), frag.bodyAgg,
		strings.Join(predicates, " AND "), frag.orderBySort,
		b.Add(f.Limit+1), b.Add(f.Cursor))
	return query, append(scopeArgs, b.Args()...), nil
}

// ScanTermsMatches reads BuildTermsSearchSQL rows into a page. The timestamp
// column's Go type differs per backend, so the caller supplies its scan
// destination and a formatter that reads the value scanned for the current
// row.
func ScanTermsMatches(
	rows *sql.Rows, f ContentSearchFilter, terms []string,
	timestampDest any, timestamp func() string,
) (ContentSearchPage, error) {
	matches := make([]ContentMatch, 0, f.Limit+1)
	for rows.Next() {
		var match ContentMatch
		var body string
		var endOrdinal int
		if err := rows.Scan(
			&match.SessionID, &match.Project, &match.Agent,
			&match.TranscriptRevision, &match.Location,
			&match.Role, &match.Ordinal, timestampDest, &body, &endOrdinal,
			&match.Relationship, &match.ParentSessionID, &match.Sidechain,
			&match.Subordinate,
		); err != nil {
			return ContentSearchPage{}, fmt.Errorf("scan terms match: %w", err)
		}
		match.Timestamp = timestamp()
		match.OrdinalRange = [2]int{match.Ordinal, endOrdinal}
		match.Snippet = f.TermsSnippet(body, terms)
		matches = append(matches, match)
	}
	if err := rows.Err(); err != nil {
		return ContentSearchPage{}, fmt.Errorf("iterate terms matches: %w", err)
	}
	return f.Page(matches), nil
}

// TermsSnippet returns redacted context around the first occurrence of every
// term in an exchange body. Terms close enough to share context produce one
// window; distant terms produce separate windows joined by
// termsSnippetSeparator, so the snippet grows with the number of terms and
// never with the length of the exchange.
func (f ContentSearchFilter) TermsSnippet(body string, terms []string) string {
	type window struct{ lo, hi int }
	windows := make([]window, 0, len(terms))
	for _, term := range terms {
		start, end, ok := CaseInsensitiveSpan(body, term)
		if !ok {
			continue
		}
		lo, hi := snippetBounds(body, start, end, contentSnippetRadius)
		windows = append(windows, window{lo, hi})
	}
	if len(windows) == 0 {
		return f.buildSnippet(body, 0, 0)
	}
	slices.SortFunc(windows, func(a, b window) int { return cmp.Compare(a.lo, b.lo) })
	merged := windows[:1]
	for _, w := range windows[1:] {
		last := &merged[len(merged)-1]
		if w.lo <= last.hi {
			last.hi = max(last.hi, w.hi)
			continue
		}
		merged = append(merged, w)
	}
	parts := make([]string, len(merged))
	for i, w := range merged {
		parts[i] = f.redactedWindow(body, w.lo, w.hi)
	}
	return strings.Join(parts, termsSnippetSeparator)
}
