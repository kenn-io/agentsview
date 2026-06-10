package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const attachToolCallBatchSize = 500

// GetMessages returns paginated messages for a session.
func (s *Store) GetMessages(
	ctx context.Context,
	sessionID string, from, limit int, asc bool,
) ([]db.Message, error) {
	if limit <= 0 || limit > db.MaxMessageLimit {
		limit = db.DefaultMessageLimit
	}

	dir := "ASC"
	op := ">="
	if !asc {
		dir = "DESC"
		op = "<="
	}

	query := fmt.Sprintf(`
		SELECT session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, has_tool_use,
			content_length, is_system, model, token_usage,
			context_tokens, output_tokens,
			has_context_tokens, has_output_tokens,
			claude_message_id, claude_request_id,
			source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain,
			is_compact_boundary
		FROM messages
		WHERE session_id = $1 AND ordinal %s $2
		ORDER BY ordinal %s
		LIMIT $3`, op, dir)

	rows, err := s.pg.QueryContext(
		ctx, query, sessionID, from, limit,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"querying messages: %w", err,
		)
	}
	defer rows.Close()

	msgs, err := scanPGMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// GetAllMessages returns all messages for a session ordered
// by ordinal.
func (s *Store) GetAllMessages(
	ctx context.Context, sessionID string,
) ([]db.Message, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, has_tool_use,
			content_length, is_system, model, token_usage,
			context_tokens, output_tokens,
			has_context_tokens, has_output_tokens,
			claude_message_id, claude_request_id,
			source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain,
			is_compact_boundary
		FROM messages
		WHERE session_id = $1
		ORDER BY ordinal ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf(
			"querying all messages: %w", err,
		)
	}
	defer rows.Close()

	msgs, err := scanPGMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// SearchSession performs ILIKE substring search within a single
// session's messages, returning matching ordinals.
func (s *Store) SearchSession(
	ctx context.Context, sessionID, query string,
) ([]int, error) {
	if query == "" {
		return nil, nil
	}
	like := "%" + escapeLike(query) + "%"
	rows, err := s.pg.QueryContext(ctx, `
		SELECT DISTINCT m.ordinal
		FROM messages m
		LEFT JOIN tool_calls tc
			ON tc.session_id = m.session_id
			AND tc.message_ordinal = m.ordinal
		WHERE m.session_id = $1
			AND m.is_system = FALSE
			AND `+db.SystemPrefixSQL("m.content", "m.role")+`
			AND (m.content ILIKE $2
				OR tc.result_content ILIKE $2)
		ORDER BY m.ordinal ASC`,
		sessionID, like,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"searching session: %w", err,
		)
	}
	defer rows.Close()

	var ordinals []int
	for rows.Next() {
		var ord int
		if err := rows.Scan(&ord); err != nil {
			return nil, fmt.Errorf(
				"scanning ordinal: %w", err,
			)
		}
		ordinals = append(ordinals, ord)
	}
	return ordinals, rows.Err()
}

// HasFTS returns true because ILIKE search is available.
func (s *Store) HasFTS() bool { return true }

// escapeLike escapes SQL LIKE metacharacters so the bind
// parameter is treated as a literal substring.
func escapeLike(v string) string {
	return db.EscapeLikePattern(v)
}

// stripFTSQuotes removes surrounding double quotes that
// prepareFTSQuery adds for SQLite FTS phrase matching.
func stripFTSQuotes(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// Search performs ILIKE-based full-text search across messages,
// grouped to one result per session via DISTINCT ON, UNION'd with a
// session name (display_name / first_message) branch.
func (s *Store) Search(
	ctx context.Context, f db.SearchFilter,
) (db.SearchPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxSearchLimit {
		f.Limit = db.DefaultSearchLimit
	}

	searchTerm := stripFTSQuotes(f.Query)
	if searchTerm == "" {
		return db.SearchPage{}, nil
	}

	// Validate Sort before interpolating into ORDER BY.
	// session_id ASC is a deterministic tie-breaker for both modes,
	// preventing pagination instability when sort keys are equal.
	// NULLS LAST ensures sessions with NULL timestamps sort after
	// sessions with real timestamps under DESC ordering.
	// match_priority: 1 = message content match, 2 = name-only match.
	// This ensures content matches always rank above name-only fallbacks
	// regardless of match_pos (name-only rows have match_pos=0 which would
	// otherwise sort them before content matches under match_pos ASC alone).
	// match_priority: 1 = message content match, 2 = name-only match.
	// Only applied in relevance mode so content matches rank above name-only
	// fallbacks. Recency mode orders purely by time so the newest session
	// wins regardless of match type.
	outerOrderBy := "match_priority ASC, match_pos ASC, session_ended_at DESC NULLS LAST, session_id ASC"
	if f.Sort == "recency" {
		outerOrderBy = "session_ended_at DESC NULLS LAST, session_id ASC"
	}

	// $1 = escaped ILIKE pattern (for WHERE clause)
	// $2 = raw search term (for POSITION — case folded in expression)
	args := []any{escapeLike(searchTerm), searchTerm}
	argIdx := 3

	msgProjectClause := ""
	nameProjectClause := ""
	if f.Project != "" {
		msgProjectClause = fmt.Sprintf("AND s.project = $%d", argIdx)
		nameProjectClause = fmt.Sprintf("AND s.project = $%d", argIdx)
		args = append(args, f.Project)
		argIdx++
	}

	query := fmt.Sprintf(`
		WITH msg_matches AS (
			SELECT DISTINCT ON (m.session_id)
				m.session_id,
				s.project,
				s.agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at) AS session_ended_at,
				m.ordinal,
				POSITION(LOWER($2) IN LOWER(m.content)) AS match_pos,
				CASE
					WHEN POSITION(LOWER($2) IN LOWER(m.content)) > 100
						THEN '...' || SUBSTRING(m.content
							FROM GREATEST(1, POSITION(
								LOWER($2) IN LOWER(m.content)
							) - 50) FOR 200) || '...'
					ELSE SUBSTRING(m.content FROM 1 FOR 200)
						|| CASE WHEN LENGTH(m.content) > 200
							THEN '...' ELSE '' END
				END AS snippet
			FROM messages m
			JOIN sessions s ON m.session_id = s.id
			WHERE m.content ILIKE '%%' || $1 || '%%' ESCAPE E'\\'
				AND s.deleted_at IS NULL
				AND m.is_system = FALSE
				AND `+db.SystemPrefixSQL("m.content", "m.role")+`
				%s
			ORDER BY m.session_id,
				POSITION(LOWER($2) IN LOWER(m.content)) ASC,
				m.ordinal ASC
		),
		name_matches AS (
			SELECT
				s.id AS session_id,
				s.project,
				s.agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at) AS session_ended_at,
				-1 AS ordinal,
				0 AS match_pos,
				CASE
					WHEN COALESCE(s.display_name, s.session_name) ILIKE '%%' || $1 || '%%' ESCAPE E'\\'
						THEN COALESCE(s.display_name, s.session_name, '')
					WHEN s.first_message ILIKE '%%' || $1 || '%%' ESCAPE E'\\'
						THEN COALESCE(s.first_message, '')
					ELSE COALESCE(s.display_name, s.session_name, s.first_message, '')
				END AS snippet
			FROM sessions s
			WHERE (COALESCE(s.display_name, s.session_name) ILIKE '%%' || $1 || '%%' ESCAPE E'\\'
				OR s.first_message ILIKE '%%' || $1 || '%%' ESCAPE E'\\')
				AND s.deleted_at IS NULL
				AND EXISTS (
					SELECT 1 FROM messages mx
					WHERE mx.session_id = s.id
					  AND mx.is_system = FALSE
					  AND `+db.SystemPrefixSQL("mx.content", "mx.role")+`
				)
				AND s.id NOT IN (SELECT session_id FROM msg_matches)
				%s
		)
		-- rank is a constant 1.0 because PostgreSQL ILIKE has no
	-- relevance scoring engine (unlike SQLite FTS5). Ordering
	-- uses match_pos and session_ended_at instead.
	SELECT session_id, project, agent, name,
			session_ended_at, ordinal,
			snippet, 1.0 AS rank, match_pos
		FROM (
			SELECT *, 1 AS match_priority FROM msg_matches
			UNION ALL
			SELECT *, 2 AS match_priority FROM name_matches
		) combined
		ORDER BY %s
		LIMIT $%d OFFSET $%d`,
		msgProjectClause,
		nameProjectClause,
		outerOrderBy,
		argIdx, argIdx+1,
	)
	args = append(args, f.Limit+1, f.Cursor)

	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return db.SearchPage{},
			fmt.Errorf("searching: %w", err)
	}
	defer rows.Close()

	results := []db.SearchResult{}
	for rows.Next() {
		var r db.SearchResult
		var endedAt *time.Time
		var matchPos int
		if err := rows.Scan(
			&r.SessionID, &r.Project, &r.Agent, &r.Name,
			&endedAt, &r.Ordinal,
			&r.Snippet, &r.Rank, &matchPos,
		); err != nil {
			return db.SearchPage{},
				fmt.Errorf(
					"scanning search result: %w", err,
				)
		}
		if endedAt != nil {
			r.SessionEndedAt = FormatISO8601(*endedAt)
		}
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

// attachToolCalls loads tool_calls for the given messages and
// attaches them to each message's ToolCalls field.
func (s *Store) attachToolCalls(
	ctx context.Context, msgs []db.Message,
) error {
	if len(msgs) == 0 {
		return nil
	}

	ordToIdx := make(map[int]int, len(msgs))
	sessionID := msgs[0].SessionID
	ordinals := make([]int, 0, len(msgs))
	for i, m := range msgs {
		ordToIdx[m.Ordinal] = i
		ordinals = append(ordinals, m.Ordinal)
	}

	for i := 0; i < len(ordinals); i += attachToolCallBatchSize {
		end := min(i+attachToolCallBatchSize, len(ordinals))
		if err := s.attachToolCallsBatch(
			ctx, msgs, ordToIdx, sessionID,
			ordinals[i:end],
		); err != nil {
			return err
		}
	}
	if err := s.attachToolResultEvents(
		ctx, msgs, ordToIdx, sessionID, ordinals,
	); err != nil {
		return err
	}
	return nil
}

func (s *Store) attachToolCallsBatch(
	ctx context.Context,
	msgs []db.Message,
	ordToIdx map[int]int,
	sessionID string,
	batch []int,
) error {
	if len(batch) == 0 {
		return nil
	}

	args := []any{sessionID}
	phs := make([]string, len(batch))
	for i, ord := range batch {
		args = append(args, ord)
		phs[i] = fmt.Sprintf("$%d", i+2)
	}

	query := fmt.Sprintf(`
		SELECT message_ordinal, session_id, tool_name,
			category,
			COALESCE(tool_use_id, ''),
			COALESCE(input_json, ''),
			COALESCE(skill_name, ''),
			COALESCE(result_content_length, 0),
			COALESCE(result_content, ''),
			COALESCE(subagent_session_id, '')
		FROM tool_calls
		WHERE session_id = $1
			AND message_ordinal IN (%s)
		ORDER BY message_ordinal, call_index`,
		strings.Join(phs, ","))

	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf(
			"querying tool_calls: %w", err,
		)
	}
	defer rows.Close()

	for rows.Next() {
		var tc db.ToolCall
		var msgOrdinal int
		if err := rows.Scan(
			&msgOrdinal, &tc.SessionID,
			&tc.ToolName, &tc.Category,
			&tc.ToolUseID, &tc.InputJSON, &tc.SkillName,
			&tc.ResultContentLength, &tc.ResultContent,
			&tc.SubagentSessionID,
		); err != nil {
			return fmt.Errorf(
				"scanning tool_call: %w", err,
			)
		}
		if idx, ok := ordToIdx[msgOrdinal]; ok {
			msgs[idx].ToolCalls = append(
				msgs[idx].ToolCalls, tc,
			)
		}
	}
	return rows.Err()
}

func (s *Store) attachToolResultEvents(
	ctx context.Context,
	msgs []db.Message,
	ordToIdx map[int]int,
	sessionID string,
	ordinals []int,
) error {
	for i := 0; i < len(ordinals); i += attachToolCallBatchSize {
		end := min(i+attachToolCallBatchSize, len(ordinals))
		if err := s.attachToolResultEventsBatch(
			ctx, msgs, ordToIdx, sessionID, ordinals[i:end],
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) attachToolResultEventsBatch(
	ctx context.Context,
	msgs []db.Message,
	ordToIdx map[int]int,
	sessionID string,
	ordinals []int,
) error {
	if len(ordinals) == 0 {
		return nil
	}

	args := []any{sessionID}
	phs := make([]string, len(ordinals))
	for i, ord := range ordinals {
		args = append(args, ord)
		phs[i] = fmt.Sprintf("$%d", i+2)
	}

	query := fmt.Sprintf(`
		SELECT tool_call_message_ordinal, call_index,
			COALESCE(tool_use_id, ''),
			COALESCE(agent_id, ''),
			COALESCE(subagent_session_id, ''),
			source, status, content, content_length,
			timestamp, event_index
		FROM tool_result_events
		WHERE session_id = $1
			AND tool_call_message_ordinal IN (%s)
		ORDER BY tool_call_message_ordinal, call_index, event_index`,
		strings.Join(phs, ","))

	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying tool_result_events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			msgOrdinal int
			callIndex  int
			ev         db.ToolResultEvent
			ts         *time.Time
		)
		if err := rows.Scan(
			&msgOrdinal, &callIndex,
			&ev.ToolUseID, &ev.AgentID,
			&ev.SubagentSessionID,
			&ev.Source, &ev.Status,
			&ev.Content, &ev.ContentLength,
			&ts, &ev.EventIndex,
		); err != nil {
			return fmt.Errorf("scanning tool_result_event: %w", err)
		}
		if ts != nil {
			ev.Timestamp = FormatISO8601(*ts)
		}
		idx, ok := ordToIdx[msgOrdinal]
		if !ok {
			continue
		}
		if callIndex < 0 || callIndex >= len(msgs[idx].ToolCalls) {
			continue
		}
		msgs[idx].ToolCalls[callIndex].ResultEvents = append(
			msgs[idx].ToolCalls[callIndex].ResultEvents,
			ev,
		)
	}
	return rows.Err()
}

// scanPGMessages scans message rows from PostgreSQL,
// converting TIMESTAMPTZ to string.
//
// The PG messages table has no id column (composite PK on
// session_id, ordinal), so we synthesize Message.ID = int64(ordinal)
// to match the convention used by TurnRow.MessageID and
// CallRow.MessageID in session_timing.go. The frontend keys
// {#each messages (message.id)} and looks up turns via
// turnByMessage.get(message.id); both depend on Message.ID being
// non-zero, unique within a session, and equal to int64(ordinal)
// so it joins with TurnRow.MessageID.
func scanPGMessages(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]db.Message, error) {
	msgs := []db.Message{}
	for rows.Next() {
		var m db.Message
		var ts *time.Time
		var tokenUsage string
		if err := rows.Scan(
			&m.SessionID, &m.Ordinal, &m.Role,
			&m.Content, &m.ThinkingText, &ts, &m.HasThinking,
			&m.HasToolUse, &m.ContentLength, &m.IsSystem,
			&m.Model, &tokenUsage,
			&m.ContextTokens, &m.OutputTokens,
			&m.HasContextTokens, &m.HasOutputTokens,
			&m.ClaudeMessageID, &m.ClaudeRequestID,
			&m.SourceType, &m.SourceSubtype, &m.SourceUUID,
			&m.SourceParentUUID, &m.IsSidechain,
			&m.IsCompactBoundary,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning message: %w", err,
			)
		}
		m.ID = int64(m.Ordinal)
		if ts != nil {
			m.Timestamp = FormatISO8601(*ts)
		}
		if tokenUsage != "" {
			m.TokenUsage = []byte(tokenUsage)
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}
