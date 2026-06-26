package postgres

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// GetSessionActivity returns time-bucketed message counts for a
// session, using PostgreSQL-specific timestamp functions.
func (s *Store) GetSessionActivity(
	ctx context.Context, sessionID string,
) (*db.SessionActivityResponse, error) {
	// 1. Count total messages (ALL, including system).
	var totalMessages int
	err := s.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages WHERE session_id = $1",
		sessionID,
	).Scan(&totalMessages)
	if err != nil {
		return nil, fmt.Errorf("counting messages: %w", err)
	}

	// 2. Visible-message filter (same as SQLite).
	visFilter := "m.is_system = FALSE AND " +
		db.PostgresSystemPrefixSQL("m.content", "m.role")

	// 3. Get min/max timestamps from visible messages.
	// PG stores timestamp as TIMESTAMPTZ, so scan into *time.Time.
	var minTS, maxTS *time.Time
	err = s.pg.QueryRowContext(ctx, `
		SELECT MIN(m.timestamp), MAX(m.timestamp)
		FROM messages m
		WHERE m.session_id = $1
			AND m.timestamp IS NOT NULL
			AND `+visFilter,
		sessionID,
	).Scan(&minTS, &maxTS)
	if err != nil {
		return nil, fmt.Errorf("getting timestamp range: %w", err)
	}

	if minTS == nil || maxTS == nil {
		return &db.SessionActivityResponse{
			Buckets:       []db.SessionActivityBucket{},
			TotalMessages: totalMessages,
		}, nil
	}

	// Use floor of min timestamp as anchor so bucket boundaries
	// align to whole seconds. Compute duration from exact values
	// to preserve sub-second precision.
	epochMin := minTS.Unix()
	durationSec := int64(maxTS.Sub(*minTS).Seconds())
	interval := db.SnapInterval(durationSec)

	// 4. Bucket query with PG-specific EXTRACT(EPOCH FROM ...).
	// Use floor() on the float division to handle sub-second
	// timestamps correctly — integer truncation via ::bigint on
	// the difference can shift messages near bucket boundaries.
	rows, err := s.pg.QueryContext(ctx, `
		SELECT
			floor((EXTRACT(EPOCH FROM m.timestamp) - $1) / $2)::bigint
				AS bucket,
			SUM(CASE WHEN m.role = 'user'
				THEN 1 ELSE 0 END)::int,
			SUM(CASE WHEN m.role = 'assistant'
				THEN 1 ELSE 0 END)::int,
			MIN(m.ordinal)
		FROM messages m
		WHERE m.session_id = $3
			AND m.timestamp IS NOT NULL
			AND `+visFilter+`
		GROUP BY bucket
		ORDER BY bucket ASC`,
		epochMin, interval, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("bucketing activity: %w", err)
	}
	defer rows.Close()

	// 5. Collect populated buckets.
	type rawBucket struct {
		idx      int64
		userCt   int
		asstCt   int
		firstOrd int
	}
	populated := map[int64]rawBucket{}
	var maxIdx int64
	for rows.Next() {
		var rb rawBucket
		if err := rows.Scan(
			&rb.idx, &rb.userCt, &rb.asstCt, &rb.firstOrd,
		); err != nil {
			return nil, fmt.Errorf("scanning bucket: %w", err)
		}
		populated[rb.idx] = rb
		if rb.idx > maxIdx {
			maxIdx = rb.idx
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(populated) == 0 {
		return &db.SessionActivityResponse{
			Buckets:         []db.SessionActivityBucket{},
			IntervalSeconds: interval,
			TotalMessages:   totalMessages,
		}, nil
	}

	// 6. Build full bucket array with empty gaps.
	buckets := make(
		[]db.SessionActivityBucket, 0, maxIdx+1,
	)
	for i := int64(0); i <= maxIdx; i++ {
		start := time.Unix(epochMin+i*interval, 0).UTC()
		end := time.Unix(
			epochMin+(i+1)*interval, 0,
		).UTC()
		b := db.SessionActivityBucket{
			StartTime: start.Format(time.RFC3339),
			EndTime:   end.Format(time.RFC3339),
		}
		if rb, ok := populated[i]; ok {
			b.UserCount = rb.userCt
			b.AssistantCount = rb.asstCt
			ord := rb.firstOrd
			b.FirstOrdinal = &ord
		}
		buckets = append(buckets, b)
	}

	return &db.SessionActivityResponse{
		Buckets:         buckets,
		IntervalSeconds: interval,
		TotalMessages:   totalMessages,
	}, nil
}
