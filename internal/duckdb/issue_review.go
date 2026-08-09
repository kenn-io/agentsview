package duckdb

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// GetAnalyticsIssueReview runs the shared detector over the derived mirror.
func (s *Store) GetAnalyticsIssueReview(ctx context.Context, f db.AnalyticsFilter, q db.IssueReviewQuery) (db.IssueReviewResponse, error) {
	key := db.IssueReviewCacheKey(f, q)
	if cached, ok := s.issueReviewCache.Get(key, q); ok {
		return cached, nil
	}
	sessions, err := s.issueReviewSessions(ctx, f, q)
	if err != nil {
		return db.IssueReviewResponse{}, err
	}
	messages, calls, err := s.issueReviewRows(ctx, sessions)
	if err != nil {
		return db.IssueReviewResponse{}, err
	}
	response := db.AnalyzeIssueReviewBase(sessions, messages, calls, nil)
	response.TelemetryStatus = "unsupported"
	s.issueReviewCache.Put(key, response)
	return db.FilterIssueReview(response, q), nil
}

func (s *Store) issueReviewSessions(ctx context.Context, f db.AnalyticsFilter, q db.IssueReviewQuery) ([]db.IssueReviewSession, error) {
	where, args := duckBuildAnalyticsWhere(f, "COALESCE(s.started_at,s.created_at)", "s.", true, true)
	if q.SessionID != "" {
		where += " AND s.id = ?"
		args = append(args, q.SessionID)
	}
	if q.Folder != "" {
		where += " AND s.cwd = ?"
		args = append(args, q.Folder)
	}
	if q.Outcome != "" {
		where += " AND s.outcome = ?"
		args = append(args, q.Outcome)
	}
	rows, err := s.queryContext(ctx, `SELECT s.id,substr(COALESCE(NULLIF(s.display_name,''),NULLIF(s.session_name,''),NULLIF(s.first_message,''),NULLIF(s.project,''),s.id),1,160),s.project,s.cwd,s.agent,COALESCE(s.started_at,s.created_at),s.outcome FROM sessions s WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb issue review sessions: %w", err)
	}
	defer rows.Close()
	var out []db.IssueReviewSession
	for rows.Next() {
		var row db.IssueReviewSession
		var ts any
		if err := rows.Scan(&row.ID, &row.Name, &row.Project, &row.CWD, &row.Agent, &ts, &row.Outcome); err != nil {
			return nil, fmt.Errorf("scanning duckdb issue review session: %w", err)
		}
		row.Date = analyticsLocalDate(formatDBTime(ts), f.Timezone)
		row.Incomplete = row.Outcome == "errored" || row.Outcome == "abandoned"
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) issueReviewRows(ctx context.Context, sessions []db.IssueReviewSession) ([]db.IssueReviewMessage, []db.IssueReviewToolCall, error) {
	if len(sessions) == 0 {
		return nil, nil, nil
	}
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.ID
	}
	var messages []db.IssueReviewMessage
	var calls []db.IssueReviewToolCall
	const chunkSize = 400
	for start := 0; start < len(ids); start += chunkSize {
		end := min(start+chunkSize, len(ids))
		args, placeholders := stringInArgs(ids[start:end])
		in := "(" + strings.Join(placeholders, ",") + ")"
		rows, err := s.queryContext(ctx, `SELECT session_id,ordinal,role,substr(content,1,`+fmt.Sprint(db.IssueReviewMessageScanLimit)+`),timestamp,is_system,source_type,source_subtype,COALESCE(NULLIF(source_uuid,''),NULLIF(claude_message_id,''),'') FROM messages WHERE session_id IN `+in+` AND NOT is_system AND `+db.IssueReviewMessagePredicate("role", "content")+` ORDER BY session_id,ordinal`, args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var row db.IssueReviewMessage
			var ts any
			if err := rows.Scan(&row.SessionID, &row.Ordinal, &row.Role, &row.Content, &ts, &row.IsSystem, &row.SourceType, &row.SourceSubtype, &row.StableID); err != nil {
				rows.Close()
				return nil, nil, err
			}
			row.Timestamp = formatDBTime(ts)
			messages = append(messages, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()

		queryArgs := append(append([]any{}, args...), args...)
		result := "COALESCE(es.content,tc.result_content,'')"
		rows, err = s.queryContext(ctx, `WITH events AS (
			SELECT tre.*,
				ROW_NUMBER() OVER (PARTITION BY tre.session_id,tre.tool_call_message_ordinal,tre.call_index ORDER BY tre.event_index DESC,tre.id DESC) AS latest_rank,
				MIN(CASE WHEN tre.source='tool_execution' AND tre.status='started' THEN tre.timestamp END) OVER (PARTITION BY tre.session_id,tre.tool_call_message_ordinal,tre.call_index) AS started,
				MAX(CASE WHEN tre.source='tool_execution' AND tre.status IN ('completed','errored') THEN tre.timestamp END) OVER (PARTITION BY tre.session_id,tre.tool_call_message_ordinal,tre.call_index) AS ended
			FROM tool_result_events tre WHERE tre.session_id IN `+in+`
		), event_summary AS (
			SELECT session_id,tool_call_message_ordinal,call_index,content,status,source,started,ended FROM events WHERE latest_rank=1
		)
		SELECT tc.session_id,m.ordinal,COALESCE(tc.call_index,0),tc.tool_name,tc.category,COALESCE(tc.tool_use_id,''),substr(COALESCE(tc.input_json,''),1,`+fmt.Sprint(db.IssueReviewInputLimit)+`),substr(`+result+`,1,`+fmt.Sprint(db.IssueReviewResultEdgeLimit)+`),CASE WHEN `+db.IssueReviewTailPredicate("es.status", result)+` THEN substr(`+result+`,-`+fmt.Sprint(db.IssueReviewResultEdgeLimit)+`) ELSE '' END,COALESCE(es.status,''),COALESCE(es.source,''),m.timestamp,es.started,es.ended
		FROM tool_calls tc JOIN messages m ON m.id=tc.message_id
		LEFT JOIN event_summary es ON es.session_id=tc.session_id AND es.tool_call_message_ordinal=m.ordinal AND es.call_index=COALESCE(tc.call_index,0)
		WHERE tc.session_id IN `+in+` ORDER BY tc.session_id,m.ordinal,tc.call_index`, queryArgs...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var row db.IssueReviewToolCall
			var resultHead, resultTail string
			var messageTS, started, ended any
			if err := rows.Scan(&row.SessionID, &row.MessageOrdinal, &row.CallIndex, &row.Tool, &row.Category, &row.ToolUseID, &row.Input, &resultHead, &resultTail, &row.EventStatus, &row.EventSource, &messageTS, &started, &ended); err != nil {
				rows.Close()
				return nil, nil, err
			}
			row.Result = db.JoinIssueReviewResult(resultHead, resultTail)
			row.Timestamp = formatDBTime(messageTS)
			startedAt, startOK := parseAnalyticsTime(formatDBTime(started))
			endedAt, endOK := parseAnalyticsTime(formatDBTime(ended))
			if startOK && endOK && !endedAt.Before(startedAt) {
				value := endedAt.Sub(startedAt).Milliseconds()
				row.DurationMS = &value
				row.DurationSource = "tool_execution"
			}
			calls = append(calls, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()
	}
	return messages, calls, nil
}
