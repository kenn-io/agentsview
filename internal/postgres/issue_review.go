package postgres

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// GetAnalyticsIssueReview mirrors the SQLite detector over PostgreSQL rows.
func (s *Store) GetAnalyticsIssueReview(ctx context.Context, f db.AnalyticsFilter, q db.IssueReviewQuery) (db.IssueReviewResponse, error) {
	key := db.IssueReviewCacheKey(f, q)
	response, ok := s.issueReviewCache.Get(key, q.Refresh)
	if !ok {
		sessions, err := s.issueReviewSessions(ctx, f, q)
		if err != nil {
			return db.IssueReviewResponse{}, err
		}
		messages, calls, err := s.issueReviewRows(ctx, sessions)
		if err != nil {
			return db.IssueReviewResponse{}, err
		}
		response = db.AnalyzeIssueReviewBase(sessions, messages, calls, nil)
		response.TelemetryStatus = "unsupported"
		s.issueReviewCache.Put(key, response)
	}
	states, err := s.issueReviewFindingStates(ctx)
	if err != nil {
		return db.IssueReviewResponse{}, err
	}
	return db.FilterIssueReviewWithStates(response, states, q, time.Now()), nil
}

func (s *Store) PutIssueReviewFindingState(ctx context.Context, state db.IssueReviewFindingState) error {
	_, err := s.pg.ExecContext(ctx, `
		INSERT INTO issue_review_finding_states
			(finding_id, review_state, accepted_last_seen, suppressed_until, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT(finding_id) DO UPDATE SET
			review_state = EXCLUDED.review_state,
			accepted_last_seen = EXCLUDED.accepted_last_seen,
			suppressed_until = EXCLUDED.suppressed_until,
			updated_at = EXCLUDED.updated_at`,
		state.FindingID, state.ReviewState, state.AcceptedLastSeen,
		state.SuppressedUntil, state.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("saving issue review finding state: %w", err)
	}
	return nil
}

func (s *Store) DeleteIssueReviewFindingState(ctx context.Context, findingID string) error {
	if _, err := s.pg.ExecContext(ctx,
		"DELETE FROM issue_review_finding_states WHERE finding_id = $1", findingID,
	); err != nil {
		return fmt.Errorf("deleting issue review finding state: %w", err)
	}
	return nil
}

func (s *Store) issueReviewFindingStates(ctx context.Context) ([]db.IssueReviewFindingState, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT finding_id, review_state, accepted_last_seen,
			suppressed_until, updated_at
		FROM issue_review_finding_states`)
	if err != nil {
		return nil, fmt.Errorf("querying issue review finding states: %w", err)
	}
	defer rows.Close()
	var states []db.IssueReviewFindingState
	for rows.Next() {
		var state db.IssueReviewFindingState
		if err := rows.Scan(&state.FindingID, &state.ReviewState,
			&state.AcceptedLastSeen, &state.SuppressedUntil, &state.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning issue review finding state: %w", err)
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

func (s *Store) issueReviewSessions(ctx context.Context, f db.AnalyticsFilter, q db.IssueReviewQuery) ([]db.IssueReviewSession, error) {
	pb := &paramBuilder{}
	where := buildAnalyticsWhere(f, pgDateCol, pb)
	if q.SessionID != "" {
		where += " AND id = " + pb.add(q.SessionID)
	}
	if q.Folder != "" {
		where += " AND cwd = " + pb.add(q.Folder)
	}
	if q.Outcome != "" {
		where += " AND outcome = " + pb.add(q.Outcome)
	}
	rows, err := s.pg.QueryContext(ctx, `SELECT id, LEFT(COALESCE(NULLIF(display_name,''),NULLIF(session_name,''),NULLIF(first_message,''),NULLIF(project,''),id),160), project, cwd, agent, `+pgDateCol+`, outcome FROM sessions WHERE `+where, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("querying issue review sessions: %w", err)
	}
	defer rows.Close()
	loc := analyticsLocation(f)
	var out []db.IssueReviewSession
	for rows.Next() {
		var row db.IssueReviewSession
		var ts *time.Time
		if err := rows.Scan(&row.ID, &row.Name, &row.Project, &row.CWD, &row.Agent, &ts, &row.Outcome); err != nil {
			return nil, fmt.Errorf("scanning issue review session: %w", err)
		}
		row.Date = localDate(scanDateCol(ts), loc)
		row.Incomplete = row.Outcome == "errored" || row.Outcome == "abandoned"
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) issueReviewRows(ctx context.Context, sessions []db.IssueReviewSession) ([]db.IssueReviewMessage, []db.IssueReviewToolCall, error) {
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.ID
	}
	var messages []db.IssueReviewMessage
	var calls []db.IssueReviewToolCall
	err := pgQueryChunked(ids, func(chunk []string) error {
		pb := &paramBuilder{}
		in := pgInPlaceholders(chunk, pb)
		limit := pb.add(db.IssueReviewMessageScanLimit)
		rows, err := s.pg.QueryContext(ctx, `SELECT session_id, ordinal, role, LEFT(content, `+limit+`), COALESCE(to_char(timestamp AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),''), is_system, source_type, source_subtype, COALESCE(NULLIF(source_uuid,''),NULLIF(claude_message_id,''),'') FROM messages WHERE session_id IN `+in+` AND NOT is_system AND `+db.IssueReviewMessagePredicate("role", "content")+` ORDER BY session_id,ordinal`, pb.args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var row db.IssueReviewMessage
			if err := rows.Scan(&row.SessionID, &row.Ordinal, &row.Role, &row.Content, &row.Timestamp, &row.IsSystem, &row.SourceType, &row.SourceSubtype, &row.StableID); err != nil {
				rows.Close()
				return err
			}
			messages = append(messages, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		pb = &paramBuilder{}
		in = pgInPlaceholders(chunk, pb)
		inputLimit := pb.add(db.IssueReviewInputLimit)
		resultLimit := pb.add(db.IssueReviewResultEdgeLimit)
		result := "COALESCE(es.content,tc.result_content,'')"
		rows, err = s.pg.QueryContext(ctx, `WITH events AS (
			SELECT tre.*,
				ROW_NUMBER() OVER (PARTITION BY tre.session_id,tre.tool_call_message_ordinal,tre.call_index ORDER BY tre.event_index DESC,tre.id DESC) AS latest_rank,
				MIN(CASE WHEN tre.source='tool_execution' AND tre.status='started' THEN tre.timestamp END) OVER (PARTITION BY tre.session_id,tre.tool_call_message_ordinal,tre.call_index) AS started,
				MAX(CASE WHEN tre.source='tool_execution' AND tre.status IN ('completed','errored') THEN tre.timestamp END) OVER (PARTITION BY tre.session_id,tre.tool_call_message_ordinal,tre.call_index) AS ended
			FROM tool_result_events tre WHERE tre.session_id IN `+in+`
		), event_summary AS (
			SELECT session_id,tool_call_message_ordinal,call_index,content,status,source,started,ended FROM events WHERE latest_rank=1
		)
		SELECT tc.session_id,tc.message_ordinal,tc.call_index,tc.tool_name,tc.category,tc.tool_use_id,LEFT(COALESCE(tc.input_json,''),`+inputLimit+`),LEFT(`+result+`,`+resultLimit+`),CASE WHEN `+db.IssueReviewTailPredicate("es.status", result)+` THEN RIGHT(`+result+`,`+fmt.Sprint(db.IssueReviewResultEdgeLimit)+`) ELSE '' END,COALESCE(es.status,''),COALESCE(es.source,''),COALESCE(to_char(m.timestamp AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),''),es.started,es.ended
		FROM tool_calls tc LEFT JOIN messages m ON m.session_id=tc.session_id AND m.ordinal=tc.message_ordinal
		LEFT JOIN event_summary es ON es.session_id=tc.session_id AND es.tool_call_message_ordinal=tc.message_ordinal AND es.call_index=tc.call_index
		WHERE tc.session_id IN `+in+` ORDER BY tc.session_id,tc.message_ordinal,tc.call_index`, pb.args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var row db.IssueReviewToolCall
			var resultHead, resultTail string
			var started, ended *time.Time
			if err := rows.Scan(&row.SessionID, &row.MessageOrdinal, &row.CallIndex, &row.Tool, &row.Category, &row.ToolUseID, &row.Input, &resultHead, &resultTail, &row.EventStatus, &row.EventSource, &row.Timestamp, &started, &ended); err != nil {
				rows.Close()
				return err
			}
			row.Result = db.JoinIssueReviewResult(resultHead, resultTail)
			if started != nil && ended != nil && !ended.Before(*started) {
				value := ended.Sub(*started).Milliseconds()
				row.DurationMS = &value
				row.DurationSource = "tool_execution"
			}
			calls = append(calls, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		return rows.Close()
	})
	return messages, calls, err
}
