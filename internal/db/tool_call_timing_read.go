package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ToolCallDurationQueries supplies the backend's selected occurrence query.
type ToolCallDurationQueries interface {
	QueryToolCallDurationRows(context.Context, string, []ToolCallPosition) (*sql.Rows, error)
}

// ToolCallTimingReadBase shares presence, scanning, and measured interval rules.
type ToolCallTimingReadBase struct {
	SessionLookup func(context.Context, string) (*Session, error)
	Queries       ToolCallDurationQueries
}

func (b ToolCallTimingReadBase) GetToolCallDurations(ctx context.Context, id string, positions []ToolCallPosition) (map[ToolCallPosition]*int64, error) {
	session, err := b.SessionLookup(ctx, id)
	if err != nil || session == nil {
		return nil, err
	}
	out := make(map[ToolCallPosition]*int64, len(positions))
	if len(positions) == 0 {
		return out, nil
	}
	rows, err := b.Queries.QueryToolCallDurationRows(ctx, id, positions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var position ToolCallPosition
		var start, end, childStart, childEnd any
		var child sql.NullString
		if err := rows.Scan(&position.MessageOrdinal, &position.CallIndex, &start, &end, &childStart, &childEnd, &child); err != nil {
			return nil, err
		}
		call := CallRow{ExecutionStart: toolCallTimestamp(start), ExecutionEnd: toolCallTimestamp(end), SubagentStart: toolCallTimestamp(childStart), SubagentEnd: toolCallTimestamp(childEnd)}
		if child.Valid && child.String != "" {
			call.SubagentSessionID = &child.String
		}
		out[position] = nil
		if interval, ok := measuredCallInterval(call); ok {
			duration := interval.end - interval.start
			out[position] = &duration
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func toolCallTimestamp(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}
