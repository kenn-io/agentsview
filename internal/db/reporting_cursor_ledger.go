package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/export"
)

type reportingCursorEventRow struct {
	at    time.Time
	event export.ReportingCursorEvent
}

type reportingCursorPollRow struct {
	at   time.Time
	poll export.ReportingCursorPoll
}

func loadReportingCursorPolls(ctx context.Context, tx *sql.Tx, start, end time.Time) ([]reportingCursorPollRow, error) {
	polls, err := tx.QueryContext(ctx, reportingEmptyCursorPollsQuery, start.Unix(), end.Unix())
	if err != nil {
		return nil, fmt.Errorf("querying Cursor reporting polls: %w", err)
	}
	defer polls.Close()
	var result []reportingCursorPollRow
	for polls.Next() {
		var row reportingCursorPollRow
		var seconds int64
		if err := polls.Scan(&row.poll.Account, &seconds, &row.poll.PolledAt); err != nil {
			return nil, err
		}
		row.at = time.Unix(seconds, 0).UTC()
		result = append(result, row)
	}
	if err := polls.Err(); err != nil {
		return nil, err
	}

	return result, nil
}
