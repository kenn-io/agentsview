package db

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
