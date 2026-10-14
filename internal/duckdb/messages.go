package duckdb

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// duckRevisionCol selects the session transcript revision as a window
// statement's first column; it binds the session ID once more, ahead of
// the statement's other arguments.
const duckRevisionCol = `COALESCE((SELECT transcript_revision FROM sessions WHERE id = ?), '')`

func (s *Store) GetResumeModelCounts(
	ctx context.Context, sessionID string,
) ([]db.ModelCount, error) {
	rows, err := s.queryContext(ctx, `
		SELECT model, COUNT(*)
		FROM messages
		WHERE session_id = ?
			AND role = 'assistant'
			AND model != ''
			AND model != '<synthetic>'
		GROUP BY model`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb resume model counts: %w", err)
	}
	defer rows.Close()
	var counts []db.ModelCount
	for rows.Next() {
		var count db.ModelCount
		if err := rows.Scan(&count.Model, &count.Count); err != nil {
			return nil, fmt.Errorf("scanning duckdb resume model count: %w", err)
		}
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb resume model counts: %w", err)
	}
	return counts, nil
}

func scanMessages(rows db.MessageRows) ([]db.Message, error) {
	var msgs []db.Message
	for rows.Next() {
		var m db.Message
		var ts any
		var tokenUsage string
		if err := rows.Scan(
			&m.ID, &m.SessionID, &m.Ordinal, &m.Role, &m.Content,
			&m.ThinkingText, &ts, &m.HasThinking, &m.HasToolUse,
			&m.ContentLength, &m.IsSystem, &m.Model, &m.ReasoningEffort, &tokenUsage,
			&m.ContextTokens, &m.OutputTokens,
			&m.ProviderID,
			&m.HasContextTokens, &m.HasOutputTokens,
			&m.ClaudeMessageID, &m.ClaudeRequestID,
			&m.SourceType, &m.SourceSubtype, &m.PromptSource, &m.SourceUUID,
			&m.SourceParentUUID, &m.IsSidechain, &m.IsCompactBoundary,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb message: %w", err)
		}
		m.Timestamp = formatDBTime(ts)
		// This assigned []byte(tokenUsage) unconditionally, so the ""
		// nearly every row holds became a non-nil, zero-length
		// jsontext.Value and every duckdb serve response failed to
		// marshal. Validation happens only here, on read (see
		// db.DecodeStoredTokenUsage).
		m.TokenUsage = db.DecodeStoredTokenUsage(tokenUsage)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (s *Store) queryTurnRows(
	ctx context.Context, sess *db.Session,
) ([]db.TurnRow, error) {
	rows, err := s.queryContext(ctx, `
		SELECT id, ordinal, timestamp, has_tool_use,
			role, is_system,
			CASE WHEN `+db.DuckDBSystemPrefixSQL("content", "role")+` THEN FALSE ELSE TRUE END,
			COALESCE(source_subtype, ''), content_length
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
		if err := rows.Scan(&r.MessageID, &r.Ordinal, &ts, &r.HasToolUse, &r.Role, &r.IsSystem, &r.IsSystemPrefixed, &r.SourceSubtype, &r.ContentLength); err != nil {
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
			(
				SELECT tre.timestamp
				FROM tool_result_events tre
				WHERE tre.session_id = tc.session_id
					AND tre.tool_call_message_ordinal = m.ordinal
					AND tre.call_index = tc.call_index
					AND tre.source = 'tool_execution'
					AND tre.status = 'started'
					AND tre.timestamp IS NOT NULL
				ORDER BY tre.event_index ASC
				LIMIT 1
			) AS execution_started_at,
			(
				SELECT tre.timestamp
				FROM tool_result_events tre
				WHERE tre.session_id = tc.session_id
					AND tre.tool_call_message_ordinal = m.ordinal
					AND tre.call_index = tc.call_index
					AND tre.source = 'tool_execution'
					AND tre.status IN ('completed', 'errored')
					AND tre.timestamp IS NOT NULL
				ORDER BY tre.event_index DESC
				LIMIT 1
			) AS execution_completed_at
			,s_sub.started_at
			,s_sub.ended_at
		FROM tool_calls tc
		JOIN messages m ON m.id = tc.message_id
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
	for rows.Next() {
		var r db.CallRow
		var skill, sub sql.NullString
		var executionStarted, executionCompleted, subagentStarted, subagentEnded any
		if err := rows.Scan(
			&r.MessageID, &r.ToolUseID, &r.ToolName, &r.Category,
			&skill, &sub, &r.InputJSON, &executionStarted, &executionCompleted,
			&subagentStarted, &subagentEnded,
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
		}
		r.ExecutionStart = formatDBTime(executionStarted)
		r.ExecutionEnd = formatDBTime(executionCompleted)
		r.SubagentStart = formatDBTime(subagentStarted)
		r.SubagentEnd = formatDBTime(subagentEnded)
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
