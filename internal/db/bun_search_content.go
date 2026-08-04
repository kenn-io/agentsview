package db

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

type bunContentSession struct {
	ID               string  `bun:"id"`
	Project          string  `bun:"project"`
	Agent            string  `bun:"agent"`
	RelationshipType string  `bun:"relationship_type"`
	ParentSessionID  *string `bun:"parent_session_id"`
}

type bunContentMessage struct {
	SessionID   string              `bun:"session_id"`
	Ordinal     int                 `bun:"ordinal"`
	Role        string              `bun:"role"`
	Content     string              `bun:"content"`
	Timestamp   *bunmodel.Timestamp `bun:"timestamp"`
	IsSystem    bool                `bun:"is_system"`
	IsSidechain bool                `bun:"is_sidechain"`
}

type bunContentMessageKey struct {
	sessionID string
	ordinal   int
}

type bunContentCandidate struct {
	SessionID  string  `bun:"session_id"`
	Ordinal    int     `bun:"ordinal"`
	Location   string  `bun:"location"`
	ToolName   string  `bun:"tool_name"`
	Body       string  `bun:"body"`
	Timestamp  *string `bun:"source_timestamp"`
	CallIndex  int     `bun:"call_index"`
	EventIndex int     `bun:"event_index"`
}

// SearchContent resolves dialect-specific lexical hits through canonical Bun
// session and message rows. Semantic and hybrid search join this dispatcher
// when their capability slice is installed.
func (s *BunStore) SearchContent(
	ctx context.Context, filter ContentSearchFilter,
) (ContentSearchPage, error) {
	if filter.Limit <= 0 || filter.Limit > MaxContentSearchLimit {
		filter.Limit = DefaultContentSearchLimit
	}
	if filter.Pattern == "" {
		return ContentSearchPage{}, nil
	}
	if filter.Mode == "semantic" || filter.Mode == "hybrid" {
		return ContentSearchPage{}, ErrSemanticUnavailable
	}
	if len(filter.Sources) == 0 {
		filter.Sources = []string{"messages", "tool_input", "tool_result"}
	}
	for _, source := range filter.Sources {
		if source != "messages" && source != "tool_input" && source != "tool_result" {
			return ContentSearchPage{}, searchInputErrorf(
				"search: unknown source %q", source,
			)
		}
	}
	switch filter.Mode {
	case "", "substring", "regex", "fts":
	default:
		return ContentSearchPage{}, searchInputErrorf(
			"search: invalid mode %q", filter.Mode,
		)
	}
	contentCapability := s.backend.Capabilities().ContentSearch
	if filter.Mode == "fts" && contentCapability != nil &&
		!contentCapability.Available() {
		return ContentSearchPage{}, errFTSUnavailable
	}

	var page ContentSearchPage
	err := s.consistentView(ctx, func(store bun.IDB) error {
		attemptFilter := filter
		attemptFilter.Limit = filter.Limit + 1
		var hits []ContentSearchHit
		var err error
		switch filter.Mode {
		case "", "substring":
			hits, err = s.bunContentSubstringHits(ctx, store, attemptFilter)
		case "fts":
			if contentCapability != nil {
				hits, err = contentCapability.SearchContent(ctx, store, attemptFilter)
			} else if s.backend.Name() == "postgres" || s.backend.Name() == "duckdb" {
				hits, err = s.bunContentPortableFTSHits(ctx, store, attemptFilter)
			} else {
				return errFTSUnavailable
			}
		case "regex":
			hits, err = s.bunContentRegexHits(ctx, store, attemptFilter)
		}
		if err != nil {
			return fmt.Errorf("searching content candidates: %w", err)
		}
		attempt, err := s.hydrateContentSearchHits(ctx, store, filter, hits)
		if err != nil {
			return err
		}
		page = attempt
		return nil
	})
	return page, err
}

func (s *BunStore) hydrateContentSearchHits(
	ctx context.Context, store bun.IDB, filter ContentSearchFilter,
	hits []ContentSearchHit,
) (ContentSearchPage, error) {
	if len(hits) == 0 {
		return ContentSearchPage{}, nil
	}
	ids := make([]string, 0, len(hits))
	seen := make(map[string]struct{}, len(hits))
	for _, hit := range hits {
		if hit.SessionID == "" {
			continue
		}
		if _, ok := seen[hit.SessionID]; ok {
			continue
		}
		seen[hit.SessionID] = struct{}{}
		ids = append(ids, hit.SessionID)
	}
	where, args := BuildSessionFilterSQL(
		contentSessionFilter(filter), s.backend.SessionQueryDialect(),
	)
	var sessions []bunContentSession
	query := store.NewSelect().TableExpr("sessions AS session").
		ColumnExpr("session.id AS id").ColumnExpr("session.project AS project").
		ColumnExpr("session.agent AS agent").
		ColumnExpr("session.relationship_type AS relationship_type").
		ColumnExpr("session.parent_session_id AS parent_session_id").
		Where("session.id IN (?)", bun.List(ids))
	query = applyBunWhere(query, where, args)
	if err := query.Scan(ctx, &sessions); err != nil {
		return ContentSearchPage{}, fmt.Errorf("hydrating content search sessions: %w", err)
	}
	bySession := make(map[string]bunContentSession, len(sessions))
	for _, session := range sessions {
		bySession[session.ID] = session
	}

	messages, err := bunContentMessagesForHits(ctx, store, hits)
	if err != nil {
		return ContentSearchPage{}, err
	}
	page := ContentSearchPage{
		Matches: make([]ContentMatch, 0, min(len(hits), filter.Limit+1)),
	}
	for _, hit := range hits {
		session, ok := bySession[hit.SessionID]
		if !ok {
			continue
		}
		message := messages[bunContentMessageKey{hit.SessionID, hit.Ordinal}]
		ordinalRange := [2]int{hit.OrdinalStart, hit.OrdinalEnd}
		if ordinalRange == [2]int{} && hit.Ordinal != 0 {
			ordinalRange = [2]int{hit.Ordinal, hit.Ordinal}
		}
		parentID := ""
		if session.ParentSessionID != nil {
			parentID = *session.ParentSessionID
		}
		role := message.Role
		if hit.Location != "message" {
			role = "assistant"
		}
		page.Matches = append(page.Matches, ContentMatch{
			SessionID: hit.SessionID, Project: session.Project, Agent: session.Agent,
			Location: hit.Location, Role: role, ToolName: hit.ToolName,
			Ordinal: hit.Ordinal, Timestamp: bunAnalyticsTimeString(message.Timestamp),
			Snippet: hit.Snippet, Score: hit.Score, OrdinalRange: ordinalRange,
			Subordinate: hit.Subordinate || message.IsSidechain ||
				SubordinateSession(session.RelationshipType, parentID),
			Relationship: session.RelationshipType, ParentSessionID: parentID,
			Sidechain: message.IsSidechain,
		})
		if hit.Timestamp != "" {
			page.Matches[len(page.Matches)-1].Timestamp = hit.Timestamp
		}
	}
	if len(page.Matches) > filter.Limit {
		page.Matches = page.Matches[:filter.Limit]
		page.NextCursor = filter.Cursor + filter.Limit
	}
	anchors := make([]UnitAnchor, len(page.Matches))
	for i, match := range page.Matches {
		message := messages[bunContentMessageKey{match.SessionID, match.Ordinal}]
		anchors[i] = UnitAnchor{
			SessionID: match.SessionID, Ordinal: match.Ordinal,
			Role: message.Role, Sidechain: message.IsSidechain,
			Embeddable: !message.IsSystem &&
				!IsSystemPrefixed(message.Content, message.Role),
			Missing: message.Role == "",
		}
	}
	ranges, err := DeriveUnitRanges(
		ctx, bunUnitBoundsQuerier{store: store, parent: s}, anchors,
	)
	if err != nil {
		return ContentSearchPage{}, fmt.Errorf("deriving Bun content ranges: %w", err)
	}
	for i := range page.Matches {
		anchorRange := [2]int{page.Matches[i].Ordinal, page.Matches[i].Ordinal}
		if page.Matches[i].OrdinalRange == anchorRange {
			page.Matches[i].OrdinalRange = ranges[i]
		}
	}
	return page, nil
}

func (s *BunStore) bunContentSubstringHits(
	ctx context.Context, store bun.IDB, filter ContentSearchFilter,
) ([]ContentSearchHit, error) {
	rows, err := s.bunContentCandidateRows(
		ctx, store, filter, filter.Pattern, false,
	)
	if err != nil {
		return nil, err
	}
	hits := make([]ContentSearchHit, len(rows))
	for i, row := range rows {
		hits[i] = bunContentHitFromCandidate(
			row, filter.substringSnippet(row.Body),
		)
	}
	return hits, nil
}

func (s *BunStore) bunContentRegexHits(
	ctx context.Context, store bun.IDB, filter ContentSearchFilter,
) ([]ContentSearchHit, error) {
	re, err := regexp.Compile(filter.Pattern)
	if err != nil {
		return nil, searchInputErrorf("search: invalid regex: %v", err)
	}
	literal := literalPrefix(filter.Pattern)
	const minBatch = 200
	batchSize := max(filter.Limit*4, minBatch)
	rawOffset := 0
	confirmed := 0
	hits := make([]ContentSearchHit, 0, filter.Limit)
	for len(hits) < filter.Limit {
		batchFilter := filter
		batchFilter.Cursor = rawOffset
		batchFilter.Limit = batchSize
		rows, err := s.bunContentCandidateRows(
			ctx, store, batchFilter, literal, literal == "",
		)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			span := re.FindStringIndex(row.Body)
			if span == nil {
				continue
			}
			if confirmed < filter.Cursor {
				confirmed++
				continue
			}
			hits = append(hits, bunContentHitFromCandidate(
				row, filter.buildSnippet(row.Body, span[0], span[1]),
			))
			if len(hits) >= filter.Limit {
				break
			}
		}
		rawOffset += len(rows)
		if len(rows) < batchSize {
			break
		}
	}
	return hits, nil
}

func (s *BunStore) bunContentPortableFTSHits(
	ctx context.Context, store bun.IDB, filter ContentSearchFilter,
) ([]ContentSearchHit, error) {
	terms := FTSTerms(PrepareFTSQuery(filter.Pattern))
	if len(terms) == 0 {
		return nil, nil
	}
	where, scopeArgs := BuildSessionFilterSQL(
		contentSessionFilter(filter), s.backend.SessionQueryDialect(),
	)
	predicates := make([]string, len(terms))
	args := make([]any, 0, len(terms)+len(scopeArgs)+2)
	for i, term := range terms {
		predicates[i] = "LOWER(message.content) LIKE ? ESCAPE '\\'"
		args = append(args, "%"+EscapeLikePattern(strings.ToLower(term))+"%")
	}
	system := "1=1"
	if filter.ExcludeSystem {
		system = "message.is_system = FALSE AND " +
			s.bunContentSystemPrefixSQL("message.content", "message.role")
	}
	query := `SELECT message.session_id, message.ordinal,
		'message' AS location, '' AS tool_name, message.content AS body,
		-1 AS call_index, -1 AS event_index
		FROM messages AS message
		JOIN sessions AS session ON session.id = message.session_id
		WHERE ` + strings.Join(predicates, " AND ") + `
			AND ` + system + `
			AND message.session_id IN (SELECT id FROM sessions WHERE ` + where + `)
		ORDER BY COALESCE(session.ended_at, session.started_at, session.created_at)
			DESC NULLS LAST, message.session_id ASC, message.ordinal ASC,
			COALESCE(message.id, 0) ASC
		LIMIT ? OFFSET ?`
	args = append(args, scopeArgs...)
	args = append(args, filter.Limit, filter.Cursor)
	var rows []bunContentCandidate
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("querying portable Bun FTS candidates: %w", err)
	}
	hits := make([]ContentSearchHit, len(rows))
	for i, row := range rows {
		hits[i] = bunContentHitFromCandidate(row, filter.ftsSnippet(row.Body))
	}
	return hits, nil
}

func (s *BunStore) bunContentCandidateRows(
	ctx context.Context, store bun.IDB, filter ContentSearchFilter,
	literal string, matchAll bool,
) ([]bunContentCandidate, error) {
	where, scopeArgs := BuildSessionFilterSQL(
		contentSessionFilter(filter), s.backend.SessionQueryDialect(),
	)
	scope := func(column string) string {
		return column + " IN (SELECT id FROM sessions WHERE " + where + ")"
	}
	pattern := "%" + EscapeLikePattern(strings.ToLower(literal)) + "%"
	var branches []string
	var args []any
	addArgs := func() {
		if !matchAll {
			args = append(args, pattern)
		}
		args = append(args, scopeArgs...)
	}
	predicate := func(column string) string {
		if matchAll {
			return column + " IS NOT NULL"
		}
		return "LOWER(COALESCE(" + column + ", '')) LIKE ? ESCAPE '\\'"
	}
	if hasSource(filter, "messages") {
		system := "1=1"
		if filter.ExcludeSystem {
			system = "message.is_system = FALSE AND " +
				s.bunContentSystemPrefixSQL("message.content", "message.role")
		}
		branches = append(branches, fmt.Sprintf(`
			SELECT message.session_id, message.ordinal, 'message' AS location,
				'' AS tool_name, message.content AS body,
				CAST(message.timestamp AS VARCHAR) AS source_timestamp,
				-1 AS call_index, -1 AS event_index,
				COALESCE(session.ended_at, session.started_at, session.created_at) AS sort_ts,
				0 AS source_order, COALESCE(message.id, 0) AS row_order
			FROM messages AS message
			JOIN sessions AS session ON session.id = message.session_id
			WHERE %s AND %s AND %s`, predicate("message.content"),
			system, scope("message.session_id")))
		addArgs()
	}
	if hasSource(filter, "tool_input") {
		branches = append(branches, fmt.Sprintf(`
			SELECT call.session_id, call.message_ordinal AS ordinal,
				'tool_input' AS location, call.tool_name,
				COALESCE(call.input_json, '') AS body,
				CAST(NULL AS VARCHAR) AS source_timestamp,
				call.call_index, -1 AS event_index,
				COALESCE(session.ended_at, session.started_at, session.created_at) AS sort_ts,
				1 AS source_order, COALESCE(call.id, 0) AS row_order
			FROM tool_calls AS call
			JOIN sessions AS session ON session.id = call.session_id
			WHERE %s AND %s`, predicate("call.input_json"),
			scope("call.session_id")))
		addArgs()
	}
	if hasSource(filter, "tool_result") {
		branches = append(branches, fmt.Sprintf(`
			SELECT call.session_id, call.message_ordinal AS ordinal,
				'tool_result' AS location, call.tool_name,
				COALESCE(call.result_content, '') AS body,
				CAST(NULL AS VARCHAR) AS source_timestamp,
				call.call_index, -1 AS event_index,
				COALESCE(session.ended_at, session.started_at, session.created_at) AS sort_ts,
				2 AS source_order, COALESCE(call.id, 0) AS row_order
			FROM tool_calls AS call
			JOIN sessions AS session ON session.id = call.session_id
			WHERE %s
				AND NOT EXISTS (
					SELECT 1 FROM tool_result_events AS event
					WHERE event.session_id = call.session_id
						AND event.tool_call_message_ordinal = call.message_ordinal
						AND event.call_index = call.call_index
						AND call.tool_use_id <> ''
				)
				AND %s`, predicate("call.result_content"), scope("call.session_id")))
		addArgs()
		branches = append(branches, fmt.Sprintf(`
			SELECT event.session_id,
				event.tool_call_message_ordinal AS ordinal,
				'tool_result' AS location, COALESCE(call.tool_name, '') AS tool_name,
				event.content AS body,
				CAST(event.timestamp AS VARCHAR) AS source_timestamp,
				event.call_index, event.event_index,
				COALESCE(session.ended_at, session.started_at, session.created_at) AS sort_ts,
				3 AS source_order, COALESCE(event.id, 0) AS row_order
			FROM tool_result_events AS event
			JOIN sessions AS session ON session.id = event.session_id
			LEFT JOIN tool_calls AS call
				ON call.session_id = event.session_id
				AND call.message_ordinal = event.tool_call_message_ordinal
				AND call.call_index = event.call_index
			WHERE %s AND %s`, predicate("event.content"),
			scope("event.session_id")))
		addArgs()
	}
	if len(branches) == 0 {
		return nil, nil
	}
	orderTimestamp := "sort_ts"
	if s.backend.Name() == "sqlite" {
		orderTimestamp = "julianday(sort_ts)"
	}
	query := `SELECT session_id, ordinal, location, tool_name, body,
		source_timestamp,
		call_index, event_index FROM (` + strings.Join(branches, " UNION ALL ") + `)
		ORDER BY ` + orderTimestamp + ` DESC NULLS LAST, session_id ASC,
			ordinal ASC, source_order ASC, row_order ASC
		LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Cursor)
	var rows []bunContentCandidate
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("querying Bun content candidates: %w", err)
	}
	return rows, nil
}

func bunContentHitFromCandidate(
	row bunContentCandidate, snippet string,
) ContentSearchHit {
	callIndex := row.CallIndex
	eventIndex := row.EventIndex
	hit := ContentSearchHit{
		SessionID: row.SessionID, Ordinal: row.Ordinal,
		OrdinalStart: row.Ordinal, OrdinalEnd: row.Ordinal,
		Location: row.Location, ToolName: row.ToolName,
		Snippet:   snippet,
		Timestamp: normalizeBunContentTimestamp(row.Timestamp),
	}
	if row.CallIndex >= 0 {
		hit.CallIndex = &callIndex
	}
	if row.EventIndex >= 0 {
		hit.EventIndex = &eventIndex
	}
	return hit
}

func normalizeBunContentTimestamp(value *string) string {
	if value == nil || *value == "" {
		return ""
	}
	if parsed, err := bunmodel.ParseTimestamp(*value); err == nil {
		return parsed.UTC().Format(time.RFC3339Nano)
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999Z07",
		"2006-01-02 15:04:05Z07",
	} {
		if parsed, err := time.Parse(layout, *value); err == nil {
			return parsed.UTC().Format(time.RFC3339Nano)
		}
	}
	return *value
}

func (s *BunStore) bunContentSystemPrefixSQL(content, role string) string {
	switch s.backend.Name() {
	case "postgres":
		return PostgresSystemPrefixSQL(content, role)
	case "duckdb":
		return DuckDBSystemPrefixSQL(content, role)
	default:
		return SystemPrefixSQL(content, role)
	}
}

func bunContentMessagesForHits(
	ctx context.Context, store bun.IDB, hits []ContentSearchHit,
) (map[bunContentMessageKey]bunContentMessage, error) {
	out := make(map[bunContentMessageKey]bunContentMessage, len(hits))
	const chunkSize = 400
	for start := 0; start < len(hits); start += chunkSize {
		chunk := hits[start:min(start+chunkSize, len(hits))]
		values := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*2)
		for i, hit := range chunk {
			values[i] = "(?, ?)"
			args = append(args, hit.SessionID, hit.Ordinal)
		}
		var rows []bunContentMessage
		query := `WITH refs(session_id, ordinal) AS (VALUES ` +
			strings.Join(values, ", ") + `)
			SELECT message.session_id, message.ordinal, message.role,
				message.content, message.timestamp, message.is_system,
				message.is_sidechain
			FROM refs
			JOIN messages AS message
				ON message.session_id = refs.session_id
				AND message.ordinal = refs.ordinal`
		if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
			return nil, fmt.Errorf("hydrating content search messages: %w", err)
		}
		for _, row := range rows {
			out[bunContentMessageKey{row.SessionID, row.Ordinal}] = row
		}
	}
	return out, nil
}
