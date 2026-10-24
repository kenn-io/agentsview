package review

import (
	"context"
	"time"

	"go.kenn.io/agentsview/internal/poller"
)

// JobName is the poller job that keeps digests caught up (spec §8.1).
const JobName = "friction-review"

// Job runs CatchUp hourly. exclusive serializes each day with sync and resync on
// SQLite (engine.RunExclusive) or with manual runs on pg serve; nil runs
// the work directly.
func Job(r *Runner, exclusive func(func() error) error) poller.Job {
	return poller.Job{
		Name:       JobName,
		Interval:   time.Hour,
		Jitter:     5 * time.Minute,
		Cooldown:   10 * time.Minute,
		RunAtStart: true,
		Run: func(ctx context.Context) error {
			return r.catchUp(ctx, exclusive, nil)
		},
	}
}
