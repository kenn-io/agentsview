package postgres

import (
	"context"
	"fmt"
	"slices"
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

const pgMessageCols = `session_id, ordinal, role, content, thinking_text,
	timestamp, has_thinking, has_tool_use,
	content_length, is_system, model, token_usage,
	context_tokens, output_tokens,
	has_context_tokens, has_output_tokens,
	claude_message_id, claude_request_id,
	source_type, source_subtype, source_uuid,
	source_parent_uuid, is_sidechain,
	is_compact_boundary`

// GetMessagesWindow mirrors internal/db's GetMessagesWindow: linear mode
// (optionally role-filtered) delegates to GetMessages when Roles is empty;
// Around mode merges three queries (before/anchor/after) into one ascending
// slice. The anchor query has no role predicate so the anchor row is always
// present regardless of Roles; before/after apply the role filter first, so
// Before/After count role-matching messages, not raw ordinal distance.
func (s *Store) GetMessagesWindow(
	ctx context.Context, sessionID string, w db.MessageWindow,
) ([]db.Message, error) {
	if w.Around != nil {
		return s.getMessagesAroundAnchor(ctx, sessionID, w)
	}
	from := 0
	if w.From != nil {
		from = *w.From
	}
	if len(w.Roles) == 0 {
		return s.GetMessages(ctx, sessionID, from, w.Limit, w.Asc)
	}
	return s.getMessagesLinearRoleFiltered(ctx, sessionID, from, w.Limit, w.Asc, w.Roles)
}

func (s *Store) getMessagesLinearRoleFiltered(
	ctx context.Context,
	sessionID string, from, limit int, asc bool, roles []string,
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
	roleClause, roleArgs := pgRoleFilterClause(roles, 3)
	query := fmt.Sprintf(`
		SELECT %s
		FROM messages
		WHERE session_id = $1 AND ordinal %s $2%s
		ORDER BY ordinal %s
		LIMIT $%d`, pgMessageCols, op, roleClause, dir, len(roleArgs)+3)
	args := append([]any{sessionID, from}, roleArgs...)
	args = append(args, limit)

	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying role-filtered messages: %w", err)
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

func (s *Store) getMessagesAroundAnchor(
	ctx context.Context, sessionID string, w db.MessageWindow,
) ([]db.Message, error) {
	anchor := *w.Around
	beforeLimit := max(w.Before, 0)
	afterLimit := max(w.After, 0)
	roleClause, roleArgs := pgRoleFilterClause(w.Roles, 3)

	beforeQuery := fmt.Sprintf(`
		SELECT %s FROM messages
		WHERE session_id = $1 AND ordinal < $2%s
		ORDER BY ordinal DESC LIMIT $%d`,
		pgMessageCols, roleClause, len(roleArgs)+3)
	beforeArgs := append([]any{sessionID, anchor}, roleArgs...)
	beforeArgs = append(beforeArgs, beforeLimit)
	before, err := s.queryMessageRows(ctx, beforeQuery, beforeArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying before-window messages: %w", err)
	}
	slices.Reverse(before)

	anchorQuery := fmt.Sprintf(`
		SELECT %s FROM messages WHERE session_id = $1 AND ordinal = $2`,
		pgMessageCols)
	anchorMsgs, err := s.queryMessageRows(ctx, anchorQuery, sessionID, anchor)
	if err != nil {
		return nil, fmt.Errorf("querying anchor message: %w", err)
	}

	afterQuery := fmt.Sprintf(`
		SELECT %s FROM messages
		WHERE session_id = $1 AND ordinal > $2%s
		ORDER BY ordinal ASC LIMIT $%d`,
		pgMessageCols, roleClause, len(roleArgs)+3)
	afterArgs := append([]any{sessionID, anchor}, roleArgs...)
	afterArgs = append(afterArgs, afterLimit)
	after, err := s.queryMessageRows(ctx, afterQuery, afterArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying after-window messages: %w", err)
	}

	msgs := make([]db.Message, 0, len(before)+len(anchorMsgs)+len(after))
	msgs = append(msgs, before...)
	msgs = append(msgs, anchorMsgs...)
	msgs = append(msgs, after...)
	if err := s.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// queryMessageRows runs query and scans the resulting message rows without
// attaching tool calls; callers batch that across the merged window set.
func (s *Store) queryMessageRows(
	ctx context.Context, query string, args ...any,
) ([]db.Message, error) {
	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPGMessages(rows)
}

// pgRoleFilterClause returns an "AND role IN ($n, ...)" clause and its bind
// args for the given roles, or ("", nil) when roles is empty. startAt is the
// first placeholder ordinal to use (the caller's query already consumes
// $1..$(startAt-1)).
func pgRoleFilterClause(roles []string, startAt int) (string, []any) {
	if len(roles) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(roles))
	args := make([]any, len(roles))
	for i, r := range roles {
		placeholders[i] = fmt.Sprintf("$%d", startAt+i)
		args[i] = r
	}
	return " AND role IN (" + strings.Join(placeholders, ",") + ")", args
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
			AND `+db.PostgresSystemPrefixSQL("m.content", "m.role")+`
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

// HasSemantic returns false: the PostgreSQL store has no VectorSearcher seam
// yet, so SearchContent rejects "semantic"/"hybrid" modes up front with
// db.ErrSemanticUnavailable.
func (s *Store) HasSemantic() bool { return false }

// escapeLike escapes SQL LIKE metacharacters so the bind
// parameter is treated as a literal substring.
func escapeLike(v string) string {
	return db.EscapeLikePattern(v)
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

	// plainTerm is the de-quoted query joined back into one string. It feeds
	// the name-branch ILIKE (matching the typed text against the short session
	// name) and centers the message snippet, mirroring SQLite's plainQuery.
	// terms is the per-term decomposition: every term must appear in the
	// message content (AND), matching SQLite FTS5's implicit AND so the same
	// user query behaves identically across backends. An explicit exact phrase
	// (user-supplied leading quote) collapses to a single term, preserving the
	// exact-phrase opt-in.
	plainTerm := db.StripFTSQuotes(f.Query)
	terms := db.FTSTerms(f.Query)
	if plainTerm == "" || len(terms) == 0 {
		return db.SearchPage{}, nil
	}
	// firstTerm anchors POSITION-based ordering and snippet centering.
	firstTerm := terms[0]

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

	// $1 = escaped ILIKE pattern for the name branch (full plain term)
	// $2 = raw first term (for POSITION — case folded in expression)
	args := []any{escapeLike(plainTerm), firstTerm}
	argIdx := 3

	// Message branch matches every term (AND). Each term gets its own escaped
	// ILIKE placeholder so a multi-word query requires all terms to be present
	// without demanding they be contiguous, exactly like SQLite FTS5.
	termClauses := make([]string, len(terms))
	for i, t := range terms {
		termClauses[i] = fmt.Sprintf(
			"m.content ILIKE '%%' || $%d || '%%' ESCAPE E'\\\\'", argIdx)
		args = append(args, escapeLike(t))
		argIdx++
	}
	msgTermPredicate := strings.Join(termClauses, "\n\t\t\t\t\tAND ")

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
			WHERE %s
				AND s.deleted_at IS NULL
				AND m.is_system = FALSE
				AND `+db.PostgresSystemPrefixSQL("m.content", "m.role")+`
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
					  AND `+db.PostgresSystemPrefixSQL("mx.content", "mx.role")+`
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
		msgTermPredicate,
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
			COALESCE(subagent_session_id, ''),
			COALESCE(file_path, ''),
			COALESCE(call_index, 0)
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
			&tc.FilePath, &tc.CallIndex,
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
