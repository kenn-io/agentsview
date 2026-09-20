package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
)

// embeddableMessagePredicate matches messages the search surfaces: not
// flagged as system and not carrying a system prefix.
func embeddableMessagePredicate(alias string) string {
	return alias + ".is_system = false AND " +
		db.ClickHouseSystemPrefixSQL(alias+".content", alias+".role")
}

// toolCallMessageJoin joins tool calls to their message on the mirror's
// stable (session_id, ordinal) key.
const toolCallMessageJoin = `JOIN messages m ON m.session_id = tc.session_id
			AND m.ordinal = tc.message_ordinal`

// toolCallWithoutEvents excludes tool calls whose result events are stored
// separately, so a result is not reported twice.
const toolCallWithoutEvents = `NOT (tc.tool_use_id <> '' AND (tc.session_id, tc.tool_use_id) IN (
				SELECT session_id, tool_use_id FROM tool_result_events))`

func (s *Store) Search(ctx context.Context, f db.SearchFilter) (db.SearchPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxSearchLimit {
		f.Limit = db.DefaultSearchLimit
	}
	if f.Query == "" {
		return db.SearchPage{}, nil
	}
	// plainTerm is the de-quoted query joined back into one string; it feeds
	// the name-branch ILIKE. terms is the per-term decomposition: every term
	// must appear in the message content (AND), matching SQLite FTS5's
	// implicit AND. An explicit exact phrase collapses to a single term.
	f.Query = db.PrepareFTSQuery(f.Query)
	plainTerm := db.StripFTSQuotes(f.Query)
	terms := db.FTSTerms(f.Query)
	if plainTerm == "" || len(terms) == 0 {
		return db.SearchPage{}, nil
	}
	firstTerm := terms[0]
	namePattern := "%" + db.EscapeLikePattern(plainTerm) + "%"
	project := ""
	nameProject := ""
	args := []any{firstTerm, firstTerm}

	termClauses := make([]string, len(terms))
	for i, t := range terms {
		termClauses[i] = "m.content ILIKE ?"
		args = append(args, "%"+db.EscapeLikePattern(t)+"%")
	}
	msgTermPredicate := strings.Join(termClauses, "\n\t\t\t\tAND ")
	if f.Project != "" {
		project = "AND s.project = ?"
		args = append(args, f.Project)
		nameProject = "AND s.project = ?"
	}
	dateBuilder := db.NewQueryBuilder(db.ClickHouseQueryDialect(), 0)
	var nameProjectDates strings.Builder
	var projectDates strings.Builder
	for _, pred := range dateBuilder.SessionDateRangePredicates(
		f.DateFrom, f.DateTo, "", func(col string) string { return "s." + col },
	) {
		projectDates.WriteString(" AND " + pred)
		nameProjectDates.WriteString(" AND " + pred)
	}
	nameProject += nameProjectDates.String()
	project += projectDates.String()
	args = append(args, dateBuilder.Args()...)
	args = append(args, namePattern, namePattern, namePattern, namePattern)
	if f.Project != "" {
		args = append(args, f.Project)
	}
	args = append(args, dateBuilder.Args()...)
	orderBy := "match_priority ASC, match_pos ASC, session_ended_at DESC, session_id ASC"
	if f.Sort == "recency" {
		orderBy = "session_ended_at DESC, session_id ASC"
	}
	args = append(args, f.Limit+1, f.Cursor)
	rows, err := s.queryContext(ctx, `
		WITH msg_ranked AS (
			SELECT m.session_id AS session_id, s.project AS project, s.agent AS agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at, s.created_at) AS session_ended_at,
				m.ordinal AS ordinal, substringUTF8(m.content, 1, 200) AS snippet,
				1.0 AS rank, 1 AS match_priority,
				positionCaseInsensitiveUTF8(m.content, ?) AS match_pos,
				ROW_NUMBER() OVER (
					PARTITION BY m.session_id
					ORDER BY positionCaseInsensitiveUTF8(m.content, ?) ASC,
						m.ordinal ASC, m.id ASC
				) AS rn
			FROM messages m
			JOIN sessions s ON s.id = m.session_id
			WHERE `+msgTermPredicate+`
				AND s.deleted_at IS NULL
				AND `+embeddableMessagePredicate("m")+`
				`+project+`
		),
		msg_matches AS (
			SELECT session_id, project, agent, name, session_ended_at,
				ordinal, snippet, rank, match_priority, match_pos
			FROM msg_ranked
			WHERE rn = 1
		),
		name_matches AS (
			SELECT s.id AS session_id, s.project AS project, s.agent AS agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at, s.created_at) AS session_ended_at,
				toInt64(-1) AS ordinal,
				CASE
					WHEN COALESCE(s.display_name, s.session_name) ILIKE ?
						THEN COALESCE(s.display_name, s.session_name, '')
					WHEN s.first_message ILIKE ?
						THEN COALESCE(s.first_message, '')
					ELSE COALESCE(s.display_name, s.session_name, s.first_message, '')
				END AS snippet,
				1.0 AS rank, 2 AS match_priority, toUInt64(0) AS match_pos
			FROM sessions s
			WHERE (COALESCE(s.display_name, s.session_name) ILIKE ?
				OR s.first_message ILIKE ?)
				AND s.deleted_at IS NULL
				AND s.id IN (
					SELECT mx.session_id FROM messages mx
					WHERE `+embeddableMessagePredicate("mx")+`
				)
				AND s.id NOT IN (SELECT session_id FROM msg_matches)
				`+nameProject+`
		)
		SELECT session_id, project, agent, name,
			session_ended_at, ordinal, snippet, rank
		FROM (
			SELECT * FROM msg_matches
			UNION ALL
			SELECT * FROM name_matches
		) AS combined
		ORDER BY `+orderBy+`
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return db.SearchPage{}, fmt.Errorf("clickhouse search: %w", err)
	}
	defer rows.Close()
	var results []db.SearchResult
	for rows.Next() {
		var r db.SearchResult
		var ended any
		if err := rows.Scan(&r.SessionID, &r.Project, &r.Agent, &r.Name,
			&ended, &r.Ordinal, &r.Snippet, &r.Rank); err != nil {
			return db.SearchPage{}, fmt.Errorf("scanning clickhouse search result: %w", err)
		}
		r.SessionEndedAt = formatDBTime(ended)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return db.SearchPage{}, err
	}
	page := db.SearchPage{Results: results}
	if len(results) > f.Limit {
		page.Results = results[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	return page, nil
}

func (s *Store) SearchSession(ctx context.Context, sessionID, query string) ([]int, error) {
	if query == "" {
		return nil, nil
	}
	pattern := "%" + db.EscapeLikePattern(query) + "%"
	rows, err := s.queryContext(ctx, `
		SELECT DISTINCT m.ordinal
		FROM messages m
		LEFT JOIN tool_calls tc
			ON tc.session_id = m.session_id
			AND tc.message_ordinal = m.ordinal
		LEFT JOIN tool_result_events tre
			ON tre.session_id = tc.session_id
			AND tre.tool_call_message_ordinal = m.ordinal
			AND tre.call_index = tc.call_index
		WHERE m.session_id = ?
			AND `+embeddableMessagePredicate("m")+`
			AND (m.content ILIKE ?
				OR tc.result_content ILIKE ?
				OR tre.content ILIKE ?)
		ORDER BY m.ordinal ASC`,
		sessionID, pattern, pattern, pattern)
	if err != nil {
		return nil, fmt.Errorf("clickhouse session search: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var ordinal int
		if err := rows.Scan(&ordinal); err != nil {
			return nil, err
		}
		out = append(out, ordinal)
	}
	return out, rows.Err()
}

func (s *Store) SearchContent(ctx context.Context, f db.ContentSearchFilter) (db.ContentSearchPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxContentSearchLimit {
		f.Limit = db.DefaultContentSearchLimit
	}
	if f.Pattern == "" {
		return db.ContentSearchPage{}, nil
	}
	if f.Mode == "semantic" || f.Mode == "hybrid" {
		// Invalid input returns the same 400 on every backend before the
		// capability gate reports 501 (backend parity, see AGENTS.md).
		if err := db.ValidateSemanticFilter(f); err != nil {
			return db.ContentSearchPage{}, err
		}
		if s.getVectorSearcher() == nil {
			return db.ContentSearchPage{}, s.semanticUnavailableError()
		}
		if f.Mode == "hybrid" {
			return s.searchContentHybrid(ctx, f)
		}
		return s.searchContentSemantic(ctx, f)
	}
	if len(f.Sources) == 0 {
		f.Sources = []string{"messages", "tool_input", "tool_result"}
	}
	for _, source := range f.Sources {
		if source != "messages" && source != "tool_input" && source != "tool_result" {
			return db.ContentSearchPage{},
				&db.SearchInputError{Msg: fmt.Sprintf("search: unknown source %q", source)}
		}
	}
	switch f.Mode {
	case "", "substring", "regex":
	case "fts":
		f.Sources = []string{"messages"}
	default:
		return db.ContentSearchPage{},
			&db.SearchInputError{Msg: fmt.Sprintf("search: invalid mode %q", f.Mode)}
	}
	var matches []db.ContentMatch
	var err error
	if f.Mode == "regex" {
		matches, err = s.collectContentRegexMatches(ctx, f)
	} else {
		matches, err = s.collectContentSubstringMatches(ctx, f)
	}
	if err != nil {
		return db.ContentSearchPage{}, err
	}
	page := f.Page(matches)
	if err := s.deriveLexicalUnits(ctx, page.Matches); err != nil {
		return db.ContentSearchPage{}, err
	}
	return page, nil
}

func contentScope(f db.ContentSearchFilter) (string, []any) {
	scopeWhere, scopeArgs := db.BuildContentScopeSQL(f, db.ClickHouseQueryDialect())
	return db.AppendExcludeSessionIDs(scopeWhere, scopeArgs, "id", f.ExcludeSessionIDs)
}

// contentSearchPredicate returns the ILIKE predicate for one source column.
// FTS mode requires every term; substring mode requires the literal pattern.
func contentSearchPredicate(column string, f db.ContentSearchFilter, args *[]any) string {
	if f.Mode != "fts" {
		*args = append(*args, "%"+db.EscapeLikePattern(f.Pattern)+"%")
		return column + " ILIKE ?"
	}
	terms := db.FTSTerms(db.PrepareFTSQuery(f.Pattern))
	clauses := make([]string, 0, len(terms))
	for _, term := range terms {
		if term == "" {
			continue
		}
		*args = append(*args, "%"+db.EscapeLikePattern(term)+"%")
		clauses = append(clauses, column+" ILIKE ?")
	}
	if len(clauses) == 0 {
		return "false"
	}
	return strings.Join(clauses, " AND ")
}

func (s *Store) collectContentSubstringMatches(
	ctx context.Context, f db.ContentSearchFilter,
) ([]db.ContentMatch, error) {
	scopeWhere, scopeArgs := contentScope(f)
	var branches []string
	var args []any
	addSearchArgs := func(column string) string {
		predicate := contentSearchPredicate(column, f, &args)
		args = append(args, scopeArgs...)
		return predicate
	}
	for _, source := range f.Sources {
		switch source {
		case "messages":
			sysPred := "true"
			if f.ExcludeSystem {
				sysPred = embeddableMessagePredicate("m")
			}
			contentPred := addSearchArgs("m.content")
			branches = append(branches, `
				SELECT m.session_id AS session_id, s.project AS project, s.agent AS agent,
					'message' AS location, m.role AS role, '' AS tool_name, m.ordinal AS ordinal,
					m.timestamp AS ts,
					m.content AS body,
					COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
					0 AS src, m.id AS row_id,
					toInt64(0) AS call_index, toInt64(0) AS event_index
				FROM messages m JOIN sessions s ON s.id = m.session_id
				WHERE `+contentPred+`
					AND `+sysPred+`
					AND m.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)`)
		case "tool_input":
			inputPred := addSearchArgs("tc.input_json")
			branches = append(branches, `
				SELECT tc.session_id AS session_id, s.project AS project, s.agent AS agent,
					'tool_input' AS location, 'assistant' AS role, tc.tool_name AS tool_name,
					m.ordinal AS ordinal,
					m.timestamp AS ts,
					tc.input_json AS body,
					COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
					1 AS src, tc.message_id AS row_id,
					tc.call_index AS call_index, toInt64(0) AS event_index
				FROM tool_calls tc JOIN sessions s ON s.id = tc.session_id
				`+toolCallMessageJoin+`
				WHERE `+inputPred+`
					AND tc.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)`)
		case "tool_result":
			contentPred := addSearchArgs("tc.result_content")
			branches = append(branches, `
				SELECT tc.session_id AS session_id, s.project AS project, s.agent AS agent,
					'tool_result' AS location, 'assistant' AS role, tc.tool_name AS tool_name,
					m.ordinal AS ordinal,
					m.timestamp AS ts,
					tc.result_content AS body,
					COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
					2 AS src, tc.message_id AS row_id,
					tc.call_index AS call_index, toInt64(0) AS event_index
				FROM tool_calls tc JOIN sessions s ON s.id = tc.session_id
				`+toolCallMessageJoin+`
				WHERE `+contentPred+`
					AND `+toolCallWithoutEvents+`
					AND tc.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)`)
			eventPred := addSearchArgs("tre.content")
			branches = append(branches, `
				SELECT tre.session_id AS session_id, s.project AS project, s.agent AS agent,
					'tool_result' AS location, 'assistant' AS role, '' AS tool_name,
					tre.tool_call_message_ordinal AS ordinal,
					tre.timestamp AS ts,
					tre.content AS body,
					COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
					3 AS src, toInt64(0) AS row_id,
					tre.call_index AS call_index,
					tre.event_index AS event_index
				FROM tool_result_events tre JOIN sessions s ON s.id = tre.session_id
				WHERE `+eventPred+`
					AND tre.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)`)
		default:
			return nil, &db.SearchInputError{Msg: fmt.Sprintf("search: unknown source %q", source)}
		}
	}
	if len(branches) == 0 {
		return nil, nil
	}
	query := `
		SELECT session_id, project, agent, location, role, tool_name,
			ordinal, ts, body
		FROM (` + strings.Join(branches, " UNION ALL ") + `) AS matches
		ORDER BY sort_ts DESC, session_id ASC, ordinal ASC,
			src ASC, row_id ASC, call_index ASC, event_index ASC
		LIMIT ? OFFSET ?`
	args = append(args, f.Limit+1, f.Cursor)
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse content search: %w", err)
	}
	return scanContentRows(rows, func(body string) string {
		if f.Mode == "fts" {
			start, end := db.FTSSnippetRange(f.Pattern, body)
			return contentSnippet(f, body, start, end)
		}
		start, end, _ := db.CaseInsensitiveSpan(body, f.Pattern)
		return contentSnippet(f, body, start, end)
	})
}

// collectContentRegexMatches loads every in-scope body for the requested
// sources, filters with Go's regexp, and pages the sorted result.
func (s *Store) collectContentRegexMatches(
	ctx context.Context, f db.ContentSearchFilter,
) ([]db.ContentMatch, error) {
	re, err := regexp.Compile(f.Pattern)
	if err != nil {
		return nil, &db.SearchInputError{Msg: fmt.Sprintf("search: invalid regex: %v", err)}
	}
	scopeWhere, scopeArgs := contentScope(f)
	var all []contentCandidate
	for _, source := range f.Sources {
		candidates, err := s.collectContentSource(ctx, source, scopeWhere, scopeArgs, f)
		if err != nil {
			return nil, err
		}
		all = append(all, candidates...)
	}
	filtered := all[:0]
	for _, m := range all {
		if loc := re.FindStringIndex(m.body); loc != nil {
			m.match.Snippet = contentSnippet(f, m.body, loc[0], loc[1])
			filtered = append(filtered, m)
		}
	}
	all = filtered
	sortContentCandidates(all)
	if f.Cursor > 0 {
		if f.Cursor >= len(all) {
			return nil, nil
		}
		all = all[f.Cursor:]
	}
	if len(all) > f.Limit+1 {
		all = all[:f.Limit+1]
	}
	out := make([]db.ContentMatch, len(all))
	for i, candidate := range all {
		out[i] = candidate.match
	}
	return out, nil
}

func (s *Store) collectContentSource(
	ctx context.Context, source, scopeWhere string, scopeArgs []any, f db.ContentSearchFilter,
) ([]contentCandidate, error) {
	var query string
	args := append([]any{}, scopeArgs...)
	switch source {
	case "messages":
		query = `SELECT m.session_id, s.project, s.agent, 'message',
			m.role, '', m.ordinal, m.timestamp,
			m.content,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
			0 AS src, m.id AS row_id,
			toInt64(0) AS call_index, toInt64(0) AS event_index
			FROM messages m JOIN sessions s ON s.id = m.session_id
			WHERE m.session_id IN (SELECT id FROM sessions WHERE ` + scopeWhere + `)`
		if f.ExcludeSystem {
			query += " AND " + embeddableMessagePredicate("m")
		}
		query += " ORDER BY m.session_id, m.ordinal, m.id"
	case "tool_input":
		query = `SELECT tc.session_id, s.project, s.agent, 'tool_input',
			'assistant', tc.tool_name, m.ordinal, m.timestamp,
			tc.input_json,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
			1 AS src, tc.message_id AS row_id,
			tc.call_index AS call_index, toInt64(0) AS event_index
			FROM tool_calls tc JOIN sessions s ON s.id = tc.session_id
			` + toolCallMessageJoin + `
			WHERE tc.session_id IN (SELECT id FROM sessions WHERE ` + scopeWhere + `)
			ORDER BY tc.session_id, m.ordinal, tc.message_id, tc.call_index`
	case "tool_result":
		query = `SELECT session_id, project, agent, location, role,
			tool_name, ordinal, ts, body, sort_ts, src, row_id, call_index, event_index
			FROM (
				SELECT tc.session_id AS session_id, s.project AS project, s.agent AS agent,
					'tool_result' AS location, 'assistant' AS role, tc.tool_name AS tool_name,
					m.ordinal AS ordinal, m.timestamp AS ts,
					tc.result_content AS body,
					COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
					2 AS src, tc.message_id AS row_id,
					tc.call_index AS call_index, toInt64(0) AS event_index
				FROM tool_calls tc JOIN sessions s ON s.id = tc.session_id
				` + toolCallMessageJoin + `
				WHERE tc.session_id IN (SELECT id FROM sessions WHERE ` + scopeWhere + `)
					AND ` + toolCallWithoutEvents + `
				UNION ALL
				SELECT tre.session_id AS session_id, s.project AS project, s.agent AS agent,
					'tool_result' AS location, 'assistant' AS role, '' AS tool_name,
					tre.tool_call_message_ordinal AS ordinal, tre.timestamp AS ts,
					tre.content AS body,
					COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
					3 AS src, toInt64(0) AS row_id,
					tre.call_index AS call_index, tre.event_index AS event_index
				FROM tool_result_events tre JOIN sessions s ON s.id = tre.session_id
				WHERE tre.session_id IN (SELECT id FROM sessions WHERE ` + scopeWhere + `)
			) AS results
			ORDER BY session_id, ordinal, src, row_id, call_index, event_index`
		args = append(args, scopeArgs...)
	default:
		return nil, &db.SearchInputError{Msg: fmt.Sprintf("search: unknown source %q", source)}
	}
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse content search: %w", err)
	}
	return scanContentCandidateRows(rows)
}

func contentSnippet(f db.ContentSearchFilter, body string, start, end int) string {
	lo, hi := snippetBounds(body, start, end, 60)
	if f.RevealSecrets {
		return body[lo:hi]
	}
	return secrets.RedactWindow(body, lo, hi)
}

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

type contentCandidate struct {
	match      db.ContentMatch
	body       string
	sortTime   time.Time
	hasSort    bool
	sourceRank int
	rowID      int64
	callIndex  int
	eventIndex int
}

func sortContentCandidates(all []contentCandidate) {
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.hasSort && b.hasSort && !a.sortTime.Equal(b.sortTime) {
			return a.sortTime.After(b.sortTime)
		}
		if a.hasSort != b.hasSort {
			return a.hasSort
		}
		if a.match.SessionID != b.match.SessionID {
			return a.match.SessionID < b.match.SessionID
		}
		if a.match.Ordinal != b.match.Ordinal {
			return a.match.Ordinal < b.match.Ordinal
		}
		if a.sourceRank != b.sourceRank {
			return a.sourceRank < b.sourceRank
		}
		if a.rowID != b.rowID {
			return a.rowID < b.rowID
		}
		if a.callIndex != b.callIndex {
			return a.callIndex < b.callIndex
		}
		if a.eventIndex != b.eventIndex {
			return a.eventIndex < b.eventIndex
		}
		if a.match.Location != b.match.Location {
			return a.match.Location < b.match.Location
		}
		if a.match.ToolName != b.match.ToolName {
			return a.match.ToolName < b.match.ToolName
		}
		if a.match.Role != b.match.Role {
			return a.match.Role < b.match.Role
		}
		if a.match.Timestamp != b.match.Timestamp {
			return a.match.Timestamp < b.match.Timestamp
		}
		if a.match.Project != b.match.Project {
			return a.match.Project < b.match.Project
		}
		if a.match.Agent != b.match.Agent {
			return a.match.Agent < b.match.Agent
		}
		return a.match.Snippet < b.match.Snippet
	})
}

func scanContentRows(rows *sql.Rows, makeSnippet func(string) string) ([]db.ContentMatch, error) {
	defer rows.Close()
	var out []db.ContentMatch
	for rows.Next() {
		var m db.ContentMatch
		var body string
		var ts any
		if err := rows.Scan(&m.SessionID, &m.Project, &m.Agent,
			&m.Location, &m.Role, &m.ToolName, &m.Ordinal, &ts, &body); err != nil {
			return nil, fmt.Errorf("scanning clickhouse content match: %w", err)
		}
		m.Timestamp = formatDBTime(ts)
		m.Snippet = makeSnippet(body)
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanContentCandidateRows(rows *sql.Rows) ([]contentCandidate, error) {
	defer rows.Close()
	var out []contentCandidate
	for rows.Next() {
		var candidate contentCandidate
		var sortTS, ts any
		if err := rows.Scan(
			&candidate.match.SessionID, &candidate.match.Project,
			&candidate.match.Agent, &candidate.match.Location,
			&candidate.match.Role, &candidate.match.ToolName,
			&candidate.match.Ordinal, &ts,
			&candidate.body, &sortTS, &candidate.sourceRank,
			&candidate.rowID, &candidate.callIndex, &candidate.eventIndex,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse content candidate: %w", err)
		}
		candidate.match.Timestamp = formatDBTime(ts)
		candidate.sortTime, candidate.hasSort = parseTimestamp(formatDBTime(sortTS))
		candidate.match.Snippet = candidate.body
		out = append(out, candidate)
	}
	return out, rows.Err()
}
