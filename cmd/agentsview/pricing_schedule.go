package main

import (
	"context"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/poller"
	"go.kenn.io/agentsview/internal/pricingrefresh"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

const (
	pricingRefreshJobName  = "pricing-refresh"
	pricingRefreshInterval = 24 * time.Hour
	pricingRefreshJitter   = 5 * time.Minute
)

func pricingRefreshJob(database *db.DB, engine *agentsync.Engine) poller.Job {
	return poller.Job{
		Name:       pricingRefreshJobName,
		Interval:   pricingRefreshInterval,
		Jitter:     pricingRefreshJitter,
		Cooldown:   pricingrefresh.RefreshCooldown,
		RunAtStart: true,
		Run: func(ctx context.Context) error {
			return runPricingRefresh(ctx, func(ctx context.Context) error {
				return pricingrefresh.RefreshCurrent(ctx, database, func(ctx context.Context, work func() error) error {
					return runPricingExclusive(ctx, engine, work)
				})
			}, database)
		},
	}
}

type usageCacheRewarmer interface {
	UsagePricingDigest(context.Context) (string, error)
	RewarmUsageCache() error
}

// runPricingRefresh re-warms usage rollups once the refresh has committed any pricing write, even a partial one.
func runPricingRefresh(
	ctx context.Context,
	refresh func(context.Context) error, rewarmer usageCacheRewarmer,
) error {
	before, beforeErr := rewarmer.UsagePricingDigest(ctx)
	refreshErr := refresh(ctx)
	after, afterErr := rewarmer.UsagePricingDigest(ctx)
	// An unreadable digest can't prove nothing committed, so re-warm anyway.
	if ctx.Err() == nil && (beforeErr != nil || afterErr != nil || before != after) {
		if beforeErr != nil {
			log.Printf("reading usage pricing digest before pricing refresh: %v", beforeErr)
		}
		if afterErr != nil {
			log.Printf("reading usage pricing digest after pricing refresh: %v", afterErr)
		}
		// A failed re-warm must not back off the pricing job; the next Usage request rebuilds instead.
		if err := rewarmer.RewarmUsageCache(); err != nil {
			log.Printf("usage cache re-warm after pricing refresh: %v", err)
		}
	}
	return refreshErr
}

func runPricingExclusive(
	ctx context.Context,
	engine *agentsync.Engine,
	work func() error,
) error {
	if engine == nil {
		return work()
	}
	return engine.RunExclusiveCtx(ctx, work)
}
