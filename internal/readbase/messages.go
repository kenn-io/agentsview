package readbase

import (
	"context"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

func (s Sessions) GetMessages(ctx context.Context, sessionID string, from, limit int, asc bool) ([]db.Message, error) {
	return s.GetMessagesWindow(ctx, sessionID, db.MessageWindow{From: &from, Limit: limit, Asc: asc})
}

func (s Sessions) GetMessagesWindow(ctx context.Context, sessionID string, w db.MessageWindow) ([]db.Message, error) {
	if w.Limit <= 0 || w.Limit > db.MaxMessageLimit {
		w.Limit = db.DefaultMessageLimit
	}
	query, args, revision := s.backend.MessageWindowSQL(sessionID, w)
	operation := QueryMessages
	if w.Around != nil {
		operation = QueryAroundMessages
	}
	var msgs []db.Message
	var err error
	if revision {
		msgs, err = db.QueryMessagesWithRevision(ctx, s.backend.QueryContext, s.backend.ScanMessages, w.ObservedRevision, query, args...)
		if err != nil {
			return nil, s.backend.ReadError(operation, err)
		}
	} else {
		rows, err := s.backend.QueryContext(ctx, query, args...) //nolint:kennlint // ScanMessages checks rows.Err().
		if err != nil {
			return nil, s.backend.ReadError(operation, err)
		}
		defer rows.Close()
		msgs, err = s.backend.ScanMessages(rows)
		if err != nil {
			return nil, err
		}
	}
	if err := s.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

func (s Sessions) GetAllMessages(ctx context.Context, sessionID string) ([]db.Message, error) {
	rows, err := s.backend.QueryContext(ctx, s.backend.AllMessagesSQL(), sessionID) //nolint:kennlint // ScanMessages checks rows.Err().
	if err != nil {
		return nil, s.backend.ReadError(QueryAllMessages, err)
	}
	defer rows.Close()
	msgs, err := s.backend.ScanMessages(rows)
	if err != nil {
		return nil, s.backend.ReadError(ScanAllMessages, err)
	}
	if err := s.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

func (s Sessions) attachToolCalls(ctx context.Context, msgs []db.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	index := make(map[int]int, len(msgs))
	sessionID := msgs[0].SessionID
	for i, msg := range msgs {
		index[msg.Ordinal] = i
	}
	rows, err := s.backend.QueryContext(ctx, s.backend.ToolCallsSQL(), sessionID)
	if err != nil {
		return s.backend.ReadError(QueryToolCalls, err)
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal, callIndex int
		var tc db.ToolCall
		if err := rows.Scan(&ordinal, &callIndex, &tc.ToolName,
			&tc.Category, &tc.ToolUseID, &tc.InputJSON,
			&tc.SkillName, &tc.ResultContentLength,
			&tc.ResultContent, &tc.SubagentSessionID, &tc.FilePath, &tc.ObservedOutcome, &tc.ObservedRepeat, &tc.SequenceEnding); err != nil {
			return s.backend.ReadError(ScanToolCall, err)
		}
		tc.CallIndex = callIndex
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
	if err := s.attachToolResultEvents(ctx, msgs, index, sessionID); err != nil {
		return err
	}
	// Restore summaries omitted when one result event already carries the content.
	db.RestoreMessageResultContent(msgs)
	return nil
}

func (s Sessions) attachToolResultEvents(
	ctx context.Context, msgs []db.Message, index map[int]int, sessionID string,
) error {
	rows, err := s.backend.QueryContext(ctx, s.backend.ToolResultEventsSQL(), sessionID)
	if err != nil {
		return s.backend.ReadError(QueryToolResultEvents, err)
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal, callIndex int
		var ev db.ToolResultEvent
		var ts any
		if err := rows.Scan(&ordinal, &callIndex, &ev.ToolUseID,
			&ev.AgentID, &ev.SubagentSessionID, &ev.Source, &ev.Status,
			&ev.Content, &ev.ContentLength, &ts, &ev.EventIndex); err != nil {
			return s.backend.ReadError(ScanToolResultEvent, err)
		}
		ev.Timestamp = s.backend.FormatTime(ts)
		if i, ok := index[ordinal]; ok && callIndex >= 0 && callIndex < len(msgs[i].ToolCalls) {
			msgs[i].ToolCalls[callIndex].ResultEvents = append(
				msgs[i].ToolCalls[callIndex].ResultEvents, ev)
		}
	}
	return rows.Err()
}

func (s Sessions) GetSessionActivity(ctx context.Context, sessionID string) (*db.SessionActivityResponse, error) {
	msgs, err := s.GetAllMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	visible := make([]db.Message, 0, len(msgs))
	var minTime, maxTime time.Time
	for _, msg := range msgs {
		if msg.IsSystem || db.IsSystemPrefixed(msg.Content, msg.Role) || msg.Timestamp == "" {
			continue
		}
		t, ok := s.backend.ParseTime(msg.Timestamp)
		if !ok {
			continue
		}
		if len(visible) == 0 || t.Before(minTime) {
			minTime = t
		}
		if len(visible) == 0 || t.After(maxTime) {
			maxTime = t
		}
		visible = append(visible, msg)
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
		t, _ := s.backend.ParseTime(msg.Timestamp)
		idx := int((t.Unix() - anchor) / interval)
		row := populated[idx]
		switch msg.Role {
		case "user":
			if msg.SourceSubtype != "tool_result" {
				row.userCount++
			}
		case "assistant":
			row.asstCount++
		}
		if row.firstOrdinal == nil || msg.Ordinal < *row.firstOrdinal {
			ord := msg.Ordinal
			row.firstOrdinal = &ord
		}
		populated[idx] = row
		maxIdx = max(maxIdx, idx)
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

func (s Sessions) GetSessionTiming(ctx context.Context, sessionID string) (*db.SessionTiming, error) {
	sess, err := s.GetSessionFull(ctx, sessionID)
	if err != nil || sess == nil {
		return nil, err
	}
	turnRows, err := s.backend.TurnRows(ctx, sess)
	if err != nil {
		return nil, err
	}
	callRows, err := s.backend.CallRows(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return db.AssembleTiming(sess, turnRows, callRows, time.Now().UTC()), nil
}

// MessageWindowSQL builds ordinal windows while preserving each driver's revision projection.
func MessageWindowSQL(columns, revision string, revisionArgs []any, sessionID string, w db.MessageWindow) (string, []any, bool) {
	var roleArgs []any
	roleClause := ""
	if len(w.Roles) > 0 {
		placeholders := make([]string, len(w.Roles))
		for i, role := range w.Roles {
			placeholders[i] = "?"
			roleArgs = append(roleArgs, role)
		}
		roleClause = " AND role IN (" + strings.Join(placeholders, ",") + ")"
	}
	args := append([]any(nil), revisionArgs...)
	if w.Around != nil {
		anchor := *w.Around
		args = append(args, sessionID, anchor)
		args = append(args, roleArgs...)
		args = append(args, max(w.Before, 0), sessionID, anchor, sessionID, anchor)
		args = append(args, roleArgs...)
		args = append(args, max(w.After, 0))
		projection := "w.*"
		if revision != "" {
			projection = revision + ", " + projection
		}
		return `SELECT ` + projection + ` FROM (
   SELECT * FROM (SELECT ` + columns + ` FROM messages
    WHERE session_id = ? AND ordinal < ?` + roleClause + ` ORDER BY ordinal DESC LIMIT ?) AS before_rows
   UNION ALL SELECT ` + columns + ` FROM messages WHERE session_id = ? AND ordinal = ?
   UNION ALL SELECT * FROM (SELECT ` + columns + ` FROM messages
    WHERE session_id = ? AND ordinal > ?` + roleClause + ` ORDER BY ordinal ASC LIMIT ?) AS after_rows
  ) AS w ORDER BY w.ordinal`, args, revision != ""
	}
	from := 0
	if w.From != nil {
		from = *w.From
	}
	dir, op := "ASC", ">="
	if !w.Asc {
		dir, op = "DESC", "<="
	}
	args = append(args, sessionID, from)
	args = append(args, roleArgs...)
	args = append(args, w.Limit)
	projection := columns
	if revision != "" {
		projection = revision + ", " + projection
	}
	return "SELECT " + projection + " FROM messages WHERE session_id = ? AND ordinal " + op + " ?" + roleClause + " ORDER BY ordinal " + dir + " LIMIT ?", args, revision != ""
}
