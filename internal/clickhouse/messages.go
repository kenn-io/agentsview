package clickhouse

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// revisionCol selects the session transcript revision as a window
// statement's first column; it binds the session ID once more, ahead of
// the statement's other arguments. The aggregate yields an empty string
// when the session row is missing instead of failing the statement.
const revisionCol = `(SELECT any(transcript_revision) FROM sessions WHERE id = ?)`

func (s *Store) GetResumeModelCounts(ctx context.Context, sessionID string) ([]db.ModelCount, error) {
	rows, err := s.queryContext(ctx, `
		SELECT model, toInt64(COUNT(*))
		FROM messages
		WHERE session_id = ?
			AND role = 'assistant'
			AND model != ''
			AND model != '<synthetic>'
		GROUP BY model
		ORDER BY model`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse resume model counts: %w", err)
	}
	defer rows.Close()
	var counts []db.ModelCount
	for rows.Next() {
		var count db.ModelCount
		if err := rows.Scan(&count.Model, &count.Count); err != nil {
			return nil, fmt.Errorf("scanning clickhouse resume model count: %w", err)
		}
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse resume model counts: %w", err)
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
			return nil, fmt.Errorf("scanning clickhouse message: %w", err)
		}
		m.Timestamp = formatDBTime(ts)
		m.TokenUsage = db.DecodeStoredTokenUsage(tokenUsage)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (s *Store) queryTurnRows(ctx context.Context, sess *db.Session) ([]db.TurnRow, error) {
	rows, err := s.queryContext(ctx, `
		SELECT id, ordinal, timestamp, has_tool_use,
			role, is_system,
			CASE WHEN `+db.ClickHouseSystemPrefixSQL("content", "role")+` THEN false ELSE true END,
			COALESCE(source_subtype, ''), content_length
		FROM messages
		WHERE session_id = ?
		ORDER BY ordinal`, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse timing turns: %w", err)
	}
	defer rows.Close()

	var out []db.TurnRow
	for rows.Next() {
		var r db.TurnRow
		var ts any
		if err := rows.Scan(
			&r.MessageID, &r.Ordinal, &ts, &r.HasToolUse,
			&r.Role, &r.IsSystem, &r.IsSystemPrefixed, &r.SourceSubtype, &r.ContentLength,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse timing turn: %w", err)
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

// queryCallRows joins each tool call to its first started and last
// completed execution event through pre-aggregated subqueries; ClickHouse
// does not evaluate the correlated scalar subqueries the other stores use.
func (s *Store) queryCallRows(ctx context.Context, sessionID string) ([]db.CallRow, error) {
	rows, err := s.queryContext(ctx, `
		SELECT tc.message_id, tc.tool_use_id,
			tc.tool_name, tc.category, tc.skill_name,
			tc.subagent_session_id, tc.input_json,
			started.started_at, completed.completed_at,
			s_sub.started_at, s_sub.ended_at
		FROM tool_calls tc
		LEFT JOIN (
			SELECT tool_call_message_ordinal, call_index,
				argMin(timestamp, event_index) AS started_at
			FROM tool_result_events
			WHERE session_id = ? AND source = 'tool_execution'
				AND status = 'started' AND timestamp IS NOT NULL
			GROUP BY tool_call_message_ordinal, call_index
		) AS started ON started.tool_call_message_ordinal = tc.message_ordinal
			AND started.call_index = tc.call_index
		LEFT JOIN (
			SELECT tool_call_message_ordinal, call_index,
				argMax(timestamp, event_index) AS completed_at
			FROM tool_result_events
			WHERE session_id = ? AND source = 'tool_execution'
				AND status IN ('completed', 'errored') AND timestamp IS NOT NULL
			GROUP BY tool_call_message_ordinal, call_index
		) AS completed ON completed.tool_call_message_ordinal = tc.message_ordinal
			AND completed.call_index = tc.call_index
		LEFT JOIN sessions s_sub ON s_sub.id = tc.subagent_session_id
		WHERE tc.session_id = ?
		ORDER BY tc.message_id, tc.call_index`, sessionID, sessionID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse timing calls: %w", err)
	}
	defer rows.Close()

	var out []db.CallRow
	for rows.Next() {
		var r db.CallRow
		var skill, sub string
		var executionStarted, executionCompleted, startedAt, endedAt any
		if err := rows.Scan(
			&r.MessageID, &r.ToolUseID, &r.ToolName, &r.Category,
			&skill, &sub, &r.InputJSON, &executionStarted, &executionCompleted,
			&startedAt, &endedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse timing call: %w", err)
		}
		if skill != "" {
			r.SkillName = &skill
		}
		if sub != "" {
			r.SubagentSessionID = &sub
		}
		r.ExecutionStart = formatDBTime(executionStarted)
		r.ExecutionEnd = formatDBTime(executionCompleted)
		r.SubagentStart = formatDBTime(startedAt)
		r.SubagentEnd = formatDBTime(endedAt)
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
