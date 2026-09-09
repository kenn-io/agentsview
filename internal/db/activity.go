package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SessionActivityBucket holds message counts for one time interval.
type SessionActivityBucket struct {
	StartTime      string `json:"start_time"`
	EndTime        string `json:"end_time"`
	UserCount      int    `json:"user_count"`
	AssistantCount int    `json:"assistant_count"`
	FirstOrdinal   *int   `json:"first_ordinal"` // nil for empty buckets
}

// SessionActivityResponse is the response for the activity endpoint.
type SessionActivityResponse struct {
	Buckets         []SessionActivityBucket `json:"buckets"`
	IntervalSeconds int64                   `json:"interval_seconds"`
	TotalMessages   int                     `json:"total_messages"`
}

// intervalSteps are preferred bucket widths in seconds.
// For sessions longer than the last step * maxBuckets, the
// interval scales beyond this list to keep bucket count bounded.
var intervalSteps = []int64{
	60, 120, 300, 600, 900, 1800, 3600, 7200,
}

const maxBuckets = 50

// SnapInterval picks a bucket interval targeting ~30 buckets.
// For very long sessions the interval scales beyond the fixed
// step list so the total bucket count never exceeds maxBuckets.
func SnapInterval(durationSec int64) int64 {
	if durationSec <= 0 {
		return intervalSteps[0]
	}
	target := durationSec / 30
	if target <= intervalSteps[0] {
		return intervalSteps[0]
	}

	// Try the fixed step list first.
	best := intervalSteps[0]
	bestDist := abs64(intervalSteps[0] - target)
	for _, step := range intervalSteps {
		d := abs64(step - target)
		if d < bestDist || (d == bestDist && step > best) {
			bestDist = d
			best = step
		}
	}

	// If the best fixed step would produce too many buckets,
	// scale up to keep the count bounded. Bucket count is
	// floor(duration/interval) + 1, so divide by maxBuckets-1.
	if durationSec/best+1 > maxBuckets {
		best = (durationSec + maxBuckets - 2) / (maxBuckets - 1)
	}
	return best
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// GetSessionActivity returns time-bucketed message counts for a
// session. Only visible messages are counted (system and
// prefix-detected injected messages excluded).
func (d *DB) GetSessionActivity(
	ctx context.Context, sessionID string,
) (*SessionActivityResponse, error) {
	return getSessionActivitySQLite(d, ctx, sessionID)
}

func getSessionActivitySQLite(
	d *DB, ctx context.Context, sessionID string,
) (*SessionActivityResponse, error) {
	// Count all messages, including system (for TotalMessages field).
	var total int
	err := d.getReader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`,
		sessionID,
	).Scan(&total)
	if err != nil {
		return nil, fmt.Errorf("counting messages: %w", err)
	}

	// Visible-message filter: exclude persisted system messages and
	// prefix-detected injected user messages.
	visibleFilter := "m.is_system = 0 AND " + SystemPrefixSQL("m.content", "m.role")

	// Get min and max timestamps from visible messages with valid timestamps.
	// Use julianday() for sub-second precision — strftime('%s') truncates.
	tsFilter := "m.timestamp IS NOT NULL AND m.timestamp != '' AND julianday(m.timestamp) IS NOT NULL"
	var minEpoch, maxEpoch sql.NullFloat64
	err = d.getReader().QueryRowContext(ctx, fmt.Sprintf(`
		SELECT
			MIN((julianday(m.timestamp) - 2440587.5) * 86400.0),
			MAX((julianday(m.timestamp) - 2440587.5) * 86400.0)
		FROM messages m
		WHERE m.session_id = ?
		  AND %s
		  AND %s`,
		visibleFilter, tsFilter,
	), sessionID).Scan(&minEpoch, &maxEpoch)
	if err != nil {
		return nil, fmt.Errorf("querying timestamp range: %w", err)
	}

	// If no timestamps, return empty buckets with total count.
	if !minEpoch.Valid || !maxEpoch.Valid {
		return &SessionActivityResponse{
			Buckets:       []SessionActivityBucket{},
			TotalMessages: total,
		}, nil
	}

	// Use floor of min epoch as anchor so bucket boundaries
	// align to whole seconds. Compute duration from the exact
	// float values to preserve sub-second precision.
	epochMin := int64(minEpoch.Float64)
	durationSec := int64(maxEpoch.Float64 - minEpoch.Float64)
	interval := SnapInterval(durationSec)

	// Query: group visible messages into buckets using float epoch
	// for sub-second precision. The anchor is truncated to whole
	// seconds so bucket boundaries align cleanly.
	rows, err := d.getReader().QueryContext(ctx, fmt.Sprintf(`
		SELECT
			CAST(((julianday(m.timestamp) - 2440587.5) * 86400.0 - ?) / ? AS INTEGER) AS bucket_idx,
			SUM(CASE WHEN m.role = 'user' AND COALESCE(m.source_subtype, '') <> 'tool_result' THEN 1 ELSE 0 END) AS user_count,
			SUM(CASE WHEN m.role = 'assistant' THEN 1 ELSE 0 END) AS asst_count,
			MIN(m.ordinal) AS first_ordinal
		FROM messages m
		WHERE m.session_id = ?
		  AND %s
		  AND %s
		GROUP BY bucket_idx
		ORDER BY bucket_idx`,
		visibleFilter, tsFilter,
	), epochMin, interval, sessionID)
	if err != nil {
		return nil, fmt.Errorf("querying activity buckets: %w", err)
	}
	defer rows.Close()

	type bucketRow struct {
		idx          int
		userCount    int
		asstCount    int
		firstOrdinal int
	}
	var populated []bucketRow
	maxIdx := 0
	for rows.Next() {
		var br bucketRow
		if err := rows.Scan(
			&br.idx, &br.userCount, &br.asstCount, &br.firstOrdinal,
		); err != nil {
			return nil, fmt.Errorf("scanning bucket row: %w", err)
		}
		populated = append(populated, br)
		if br.idx > maxIdx {
			maxIdx = br.idx
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating bucket rows: %w", err)
	}

	// Build the full bucket array including empty gaps.
	bucketCount := maxIdx + 1
	buckets := make([]SessionActivityBucket, bucketCount)

	// Precompute a lookup from bucket index to populated row.
	popMap := make(map[int]bucketRow, len(populated))
	for _, br := range populated {
		popMap[br.idx] = br
	}

	for i := range buckets {
		startSec := epochMin + int64(i)*interval
		endSec := startSec + interval
		startTime := time.Unix(startSec, 0).UTC()
		endTime := time.Unix(endSec, 0).UTC()
		bucket := SessionActivityBucket{
			StartTime: startTime.Format(time.RFC3339),
			EndTime:   endTime.Format(time.RFC3339),
		}
		if br, ok := popMap[i]; ok {
			bucket.UserCount = br.userCount
			bucket.AssistantCount = br.asstCount
			ord := br.firstOrdinal
			bucket.FirstOrdinal = &ord
		}
		buckets[i] = bucket
	}

	return &SessionActivityResponse{
		Buckets:         buckets,
		IntervalSeconds: interval,
		TotalMessages:   total,
	}, nil
}
