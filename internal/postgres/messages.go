package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wesm/agentsview/internal/db"
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
		SELECT session_id, ordinal, role, content,
			timestamp, has_thinking, has_tool_use,
			content_length
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
		SELECT session_id, ordinal, role, content,
			timestamp, has_thinking, has_tool_use,
			content_length
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

// GetMinimap returns lightweight metadata for all messages
// in a session.
func (s *Store) GetMinimap(
	ctx context.Context, sessionID string,
) ([]db.MinimapEntry, error) {
	return s.GetMinimapFrom(ctx, sessionID, 0)
}

// GetMinimapFrom returns lightweight metadata for messages
// starting at ordinal >= from.
func (s *Store) GetMinimapFrom(
	ctx context.Context, sessionID string, from int,
) ([]db.MinimapEntry, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT ordinal, role, content_length,
			has_thinking, has_tool_use
		FROM messages
		WHERE session_id = $1 AND ordinal >= $2
		ORDER BY ordinal ASC`, sessionID, from)
	if err != nil {
		return nil, fmt.Errorf(
			"querying minimap: %w", err,
		)
	}
	defer rows.Close()

	entries := []db.MinimapEntry{}
	for rows.Next() {
		var e db.MinimapEntry
		if err := rows.Scan(
			&e.Ordinal, &e.Role, &e.ContentLength,
			&e.HasThinking, &e.HasToolUse,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning minimap entry: %w", err,
			)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
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
	r := strings.NewReplacer(
		`\`, `\\`, `%`, `\%`, `_`, `\_`,
	)
	return r.Replace(v)
}

// stripFTSQuotes removes surrounding double quotes that
// prepareFTSQuery adds for SQLite FTS phrase matching.
func stripFTSQuotes(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// Search performs ILIKE-based search across messages.
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

	whereClauses := []string{
		`m.content ILIKE '%' || $1 || '%' ESCAPE E'\\'`,
		"s.deleted_at IS NULL",
	}
	args := []any{escapeLike(searchTerm), searchTerm}
	argIdx := 3

	if f.Project != "" {
		whereClauses = append(
			whereClauses,
			fmt.Sprintf("s.project = $%d", argIdx),
		)
		args = append(args, f.Project)
		argIdx++
	}

	query := fmt.Sprintf(`
		SELECT m.session_id, s.project, m.ordinal,
			m.role,
			COALESCE(
				TO_CHAR(m.timestamp AT TIME ZONE 'UTC',
					'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
				''
			),
			CASE WHEN POSITION(
				LOWER($2) IN LOWER(m.content)) > 100
				THEN '...' || SUBSTRING(m.content
					FROM GREATEST(1, POSITION(
						LOWER($2) IN LOWER(m.content)
					) - 50) FOR 200) || '...'
				ELSE SUBSTRING(m.content FROM 1 FOR 200)
					|| CASE WHEN LENGTH(m.content) > 200
						THEN '...' ELSE '' END
			END AS snippet,
			1.0 AS rank
		FROM messages m
		JOIN sessions s ON m.session_id = s.id
		WHERE %s
		ORDER BY m.timestamp DESC NULLS LAST, m.session_id, m.ordinal
		LIMIT $%d OFFSET $%d`,
		strings.Join(whereClauses, " AND "),
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
		if err := rows.Scan(
			&r.SessionID, &r.Project, &r.Ordinal,
			&r.Role, &r.Timestamp, &r.Snippet, &r.Rank,
		); err != nil {
			return db.SearchPage{},
				fmt.Errorf(
					"scanning search result: %w", err,
				)
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

// scanPGMessages scans message rows from PostgreSQL,
// converting TIMESTAMPTZ to string.
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
		if err := rows.Scan(
			&m.SessionID, &m.Ordinal, &m.Role,
			&m.Content, &ts, &m.HasThinking,
			&m.HasToolUse, &m.ContentLength,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning message: %w", err,
			)
		}
		if ts != nil {
			m.Timestamp = FormatISO8601(*ts)
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}
