package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) GetMessages(
	ctx context.Context, sessionID string, from, limit int, asc bool,
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
	rows, err := s.queryContext(ctx, `
		SELECT id, session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, has_tool_use, content_length,
			is_system, model, token_usage, context_tokens, output_tokens,
			has_context_tokens, has_output_tokens, claude_message_id,
			claude_request_id, source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain, is_compact_boundary
		FROM messages
		WHERE session_id = ? AND ordinal `+op+` ?
		ORDER BY ordinal `+dir+`
		LIMIT ?`,
		sessionID, from, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb messages: %w", err)
	}
	defer rows.Close()
	msgs, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

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
	roleClause, roleArgs := duckRoleFilterClause(roles)
	query := `
		SELECT id, session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, has_tool_use, content_length,
			is_system, model, token_usage, context_tokens, output_tokens,
			has_context_tokens, has_output_tokens, claude_message_id,
			claude_request_id, source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain, is_compact_boundary
		FROM messages
		WHERE session_id = ? AND ordinal ` + op + ` ?` + roleClause + `
		ORDER BY ordinal ` + dir + `
		LIMIT ?`
	args := append([]any{sessionID, from}, roleArgs...)
	args = append(args, limit)

	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb role-filtered messages: %w", err)
	}
	defer rows.Close()
	msgs, err := scanMessages(rows)
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
	roleClause, roleArgs := duckRoleFilterClause(w.Roles)

	beforeQuery := `
		SELECT id, session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, has_tool_use, content_length,
			is_system, model, token_usage, context_tokens, output_tokens,
			has_context_tokens, has_output_tokens, claude_message_id,
			claude_request_id, source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain, is_compact_boundary
		FROM messages
		WHERE session_id = ? AND ordinal < ?` + roleClause + `
		ORDER BY ordinal DESC LIMIT ?`
	beforeArgs := append([]any{sessionID, anchor}, roleArgs...)
	beforeArgs = append(beforeArgs, beforeLimit)
	before, err := s.queryMessageRows(ctx, beforeQuery, beforeArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb before-window messages: %w", err)
	}
	slices.Reverse(before)

	anchorQuery := `
		SELECT id, session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, has_tool_use, content_length,
			is_system, model, token_usage, context_tokens, output_tokens,
			has_context_tokens, has_output_tokens, claude_message_id,
			claude_request_id, source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain, is_compact_boundary
		FROM messages WHERE session_id = ? AND ordinal = ?`
	anchorMsgs, err := s.queryMessageRows(ctx, anchorQuery, sessionID, anchor)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb anchor message: %w", err)
	}

	afterQuery := `
		SELECT id, session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, has_tool_use, content_length,
			is_system, model, token_usage, context_tokens, output_tokens,
			has_context_tokens, has_output_tokens, claude_message_id,
			claude_request_id, source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain, is_compact_boundary
		FROM messages
		WHERE session_id = ? AND ordinal > ?` + roleClause + `
		ORDER BY ordinal ASC LIMIT ?`
	afterArgs := append([]any{sessionID, anchor}, roleArgs...)
	afterArgs = append(afterArgs, afterLimit)
	after, err := s.queryMessageRows(ctx, afterQuery, afterArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb after-window messages: %w", err)
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
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// duckRoleFilterClause returns an "AND role IN (...)" clause and its bind
// args for the given roles, or ("", nil) when roles is empty.
func duckRoleFilterClause(roles []string) (string, []any) {
	if len(roles) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(roles))
	args := make([]any, len(roles))
	for i, r := range roles {
		placeholders[i] = "?"
		args[i] = r
	}
	return " AND role IN (" + strings.Join(placeholders, ",") + ")", args
}

func (s *Store) GetAllMessages(ctx context.Context, sessionID string) ([]db.Message, error) {
	rows, err := s.queryContext(ctx, `
		SELECT id, session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, has_tool_use, content_length,
			is_system, model, token_usage, context_tokens, output_tokens,
			has_context_tokens, has_output_tokens, claude_message_id,
			claude_request_id, source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain, is_compact_boundary
		FROM messages
		WHERE session_id = ?
		ORDER BY ordinal ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all duckdb messages: %w", err)
	}
	defer rows.Close()
	msgs, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

func scanMessages(rows *sql.Rows) ([]db.Message, error) {
	var msgs []db.Message
	for rows.Next() {
		var m db.Message
		var ts any
		var tokenUsage string
		if err := rows.Scan(
			&m.ID, &m.SessionID, &m.Ordinal, &m.Role, &m.Content,
			&m.ThinkingText, &ts, &m.HasThinking, &m.HasToolUse,
			&m.ContentLength, &m.IsSystem, &m.Model, &tokenUsage,
			&m.ContextTokens, &m.OutputTokens,
			&m.HasContextTokens, &m.HasOutputTokens,
			&m.ClaudeMessageID, &m.ClaudeRequestID,
			&m.SourceType, &m.SourceSubtype, &m.SourceUUID,
			&m.SourceParentUUID, &m.IsSidechain, &m.IsCompactBoundary,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb message: %w", err)
		}
		m.Timestamp = formatDBTime(ts)
		m.TokenUsage = []byte(tokenUsage)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (s *Store) attachToolCalls(ctx context.Context, msgs []db.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	index := make(map[int]int, len(msgs))
	sessionID := msgs[0].SessionID
	for i, msg := range msgs {
		index[msg.Ordinal] = i
	}
	rows, err := s.queryContext(ctx, `
		SELECT m.ordinal, tc.call_index, tc.tool_name, tc.category,
			COALESCE(tc.tool_use_id, ''), COALESCE(tc.input_json, ''),
			COALESCE(tc.skill_name, ''), COALESCE(tc.result_content_length, 0),
			COALESCE(tc.result_content, ''),
			COALESCE(tc.subagent_session_id, ''),
			COALESCE(tc.file_path, '')
		FROM tool_calls tc
		JOIN messages m ON m.session_id = tc.session_id
			AND m.id = tc.message_id
		WHERE tc.session_id = ?
		ORDER BY m.ordinal, tc.call_index`,
		sessionID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal, callIndex int
		var tc db.ToolCall
		if err := rows.Scan(&ordinal, &callIndex, &tc.ToolName,
			&tc.Category, &tc.ToolUseID, &tc.InputJSON,
			&tc.SkillName, &tc.ResultContentLength,
			&tc.ResultContent, &tc.SubagentSessionID, &tc.FilePath); err != nil {
			return err
		}
		tc.CallIndex = callIndex
		// A negative call_index (corrupt or malformed mirror row) would
		// skip the grow loop and panic on the [callIndex] assignment, so
		// guard it the same way the Postgres store does.
		if i, ok := index[ordinal]; ok && callIndex >= 0 {
			for len(msgs[i].ToolCalls) <= callIndex {
				msgs[i].ToolCalls = append(msgs[i].ToolCalls, db.ToolCall{})
			}
			msgs[i].ToolCalls[callIndex] = tc
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return s.attachToolResultEvents(ctx, msgs, index, sessionID)
}

func (s *Store) attachToolResultEvents(
	ctx context.Context, msgs []db.Message, index map[int]int, sessionID string,
) error {
	rows, err := s.queryContext(ctx, `
		SELECT tool_call_message_ordinal, call_index,
			COALESCE(tool_use_id, ''), COALESCE(agent_id, ''),
			COALESCE(subagent_session_id, ''), source, status,
			content, content_length, timestamp, event_index
		FROM tool_result_events
		WHERE session_id = ?
		ORDER BY tool_call_message_ordinal, call_index, event_index`,
		sessionID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal, callIndex int
		var ev db.ToolResultEvent
		var ts any
		if err := rows.Scan(&ordinal, &callIndex, &ev.ToolUseID,
			&ev.AgentID, &ev.SubagentSessionID, &ev.Source, &ev.Status,
			&ev.Content, &ev.ContentLength, &ts, &ev.EventIndex); err != nil {
			return err
		}
		ev.Timestamp = formatDBTime(ts)
		if i, ok := index[ordinal]; ok && callIndex >= 0 && callIndex < len(msgs[i].ToolCalls) {
			msgs[i].ToolCalls[callIndex].ResultEvents = append(
				msgs[i].ToolCalls[callIndex].ResultEvents, ev,
			)
		}
	}
	return rows.Err()
}

func (s *Store) GetSessionActivity(ctx context.Context, sessionID string) (*db.SessionActivityResponse, error) {
	msgs, err := s.GetAllMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	visible := make([]db.Message, 0, len(msgs))
	var minTime, maxTime time.Time
	for _, msg := range msgs {
		if msg.IsSystem || hasSystemPrefix(msg) || msg.Timestamp == "" {
			continue
		}
		if t, ok := parseTimestamp(msg.Timestamp); ok {
			if len(visible) == 0 || t.Before(minTime) {
				minTime = t
			}
			if len(visible) == 0 || t.After(maxTime) {
				maxTime = t
			}
			visible = append(visible, msg)
		}
	}
	if len(visible) == 0 {
		return &db.SessionActivityResponse{
			Buckets:       []db.SessionActivityBucket{},
			TotalMessages: len(msgs),
		}, nil
	}

	anchor := minTime.Unix()
	interval := db.SnapInterval(maxTime.Unix() - minTime.Unix())
	type bucketRow struct {
		userCount    int
		asstCount    int
		firstOrdinal *int
	}
	populated := map[int]bucketRow{}
	maxIdx := 0
	for _, msg := range visible {
		t, _ := parseTimestamp(msg.Timestamp)
		idx := int((t.Unix() - anchor) / interval)
		row := populated[idx]
		switch msg.Role {
		case "user":
			row.userCount++
		case "assistant":
			row.asstCount++
		}
		if row.firstOrdinal == nil || msg.Ordinal < *row.firstOrdinal {
			ord := msg.Ordinal
			row.firstOrdinal = &ord
		}
		populated[idx] = row
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	buckets := make([]db.SessionActivityBucket, maxIdx+1)
	for i := range buckets {
		start := time.Unix(anchor+int64(i)*interval, 0).UTC()
		end := start.Add(time.Duration(interval) * time.Second)
		row := populated[i]
		buckets[i] = db.SessionActivityBucket{
			StartTime:      start.Format(time.RFC3339),
			EndTime:        end.Format(time.RFC3339),
			UserCount:      row.userCount,
			AssistantCount: row.asstCount,
			FirstOrdinal:   row.firstOrdinal,
		}
	}
	return &db.SessionActivityResponse{
		Buckets:         buckets,
		IntervalSeconds: interval,
		TotalMessages:   len(msgs),
	}, nil
}

func (s *Store) GetSessionTiming(ctx context.Context, sessionID string) (*db.SessionTiming, error) {
	sess, err := s.GetSessionFull(ctx, sessionID)
	if err != nil || sess == nil {
		return nil, err
	}
	turnRows, err := s.queryTurnRows(ctx, sess)
	if err != nil {
		return nil, err
	}
	callRows, err := s.queryCallRows(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return db.AssembleTiming(sess, turnRows, callRows, time.Now().UTC()), nil
}

func (s *Store) queryTurnRows(
	ctx context.Context, sess *db.Session,
) ([]db.TurnRow, error) {
	rows, err := s.queryContext(ctx, `
		SELECT id, ordinal, timestamp, has_tool_use
		FROM messages
		WHERE session_id = ?
		ORDER BY ordinal`,
		sess.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb timing turns: %w", err)
	}
	defer rows.Close()

	var out []db.TurnRow
	for rows.Next() {
		var r db.TurnRow
		var ts any
		if err := rows.Scan(&r.MessageID, &r.Ordinal, &ts, &r.HasToolUse); err != nil {
			return nil, fmt.Errorf("scanning duckdb timing turn: %w", err)
		}
		r.Timestamp = formatDBTime(ts)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if !out[i].HasToolUse {
			continue
		}
		next := ""
		if i+1 < len(out) {
			next = out[i+1].Timestamp
		} else if sess.EndedAt != nil {
			next = *sess.EndedAt
		}
		if dur, ok := timingMillis(out[i].Timestamp, next); ok {
			out[i].DurationMs = &dur
		}
	}
	return out, nil
}

func (s *Store) queryCallRows(
	ctx context.Context, sessionID string,
) ([]db.CallRow, error) {
	rows, err := s.queryContext(ctx, `
		SELECT tc.message_id, COALESCE(tc.tool_use_id, ''),
			tc.tool_name, tc.category, tc.skill_name,
			tc.subagent_session_id, COALESCE(tc.input_json, ''),
			s_sub.started_at, s_sub.ended_at
		FROM tool_calls tc
		LEFT JOIN sessions s_sub ON s_sub.id = tc.subagent_session_id
		WHERE tc.session_id = ?
		ORDER BY tc.message_id, tc.call_index`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb timing calls: %w", err)
	}
	defer rows.Close()

	var out []db.CallRow
	now := time.Now().UTC().Format(time.RFC3339)
	for rows.Next() {
		var r db.CallRow
		var skill, sub sql.NullString
		var startedAt, endedAt any
		if err := rows.Scan(
			&r.MessageID, &r.ToolUseID, &r.ToolName, &r.Category,
			&skill, &sub, &r.InputJSON, &startedAt, &endedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb timing call: %w", err)
		}
		if skill.Valid {
			value := skill.String
			r.SkillName = &value
		}
		if sub.Valid {
			value := sub.String
			r.SubagentSessionID = &value
			if dur, ok := timingMillis(formatDBTime(startedAt), firstNonEmpty(formatDBTime(endedAt), now)); ok {
				r.DurationMs = &dur
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func timingMillis(start, end string) (int64, bool) {
	startTime, ok := parseTimestamp(start)
	if !ok {
		return 0, false
	}
	endTime, ok := parseTimestamp(end)
	if !ok {
		return 0, false
	}
	if endTime.Before(startTime) {
		return 0, false
	}
	return endTime.Sub(startTime).Milliseconds(), true
}

func hasSystemPrefix(msg db.Message) bool {
	return db.IsSystemPrefixed(msg.Content, msg.Role)
}
