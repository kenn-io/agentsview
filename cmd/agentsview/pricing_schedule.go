package main

import (
	"context"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/poller"
	"go.kenn.io/agentsview/internal/pricingrefresh"
)

const (
	pricingRefreshJobName  = "pricing-refresh"
	pricingRefreshInterval = 24 * time.Hour
	pricingRefreshJitter   = 5 * time.Minute
)

func pricingRefreshJob(database *db.DB, runner remoteSyncExclusiveRunner) poller.Job {
	return poller.Job{
		Name:       pricingRefreshJobName,
		Interval:   pricingRefreshInterval,
		Jitter:     pricingRefreshJitter,
		Cooldown:   pricingrefresh.RefreshCooldown,
		RunAtStart: true,
		Run: func(ctx context.Context) error {
			// RunExclusive cannot cancel its sync/resync lock wait, so shutdown
			// waits for the lock before this job can observe cancellation.
			return runPricingExclusive(runner, func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return pricingrefresh.RefreshCurrent(ctx, database)
			})
		},
	}
}

func runPricingExclusive(
	runner remoteSyncExclusiveRunner,
	work func() error,
) error {
	if runner == nil {
		return work()
	}
	return runner.RunExclusive(work)
}
