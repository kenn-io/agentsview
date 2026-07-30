package main

import (
	"context"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricingrefresh"
)

const periodicPricingRefreshInterval = 24 * time.Hour

type pricingRefreshExclusiveRunner interface {
	RunExclusive(func() error) error
}

func startPeriodicPricingRefresh(
	ctx context.Context,
	database *db.DB,
	runner pricingRefreshExclusiveRunner,
) {
	if err := runCurrentPricingRefresh(
		ctx, database, runner,
	); err != nil && ctx.Err() == nil {
		log.Printf("pricing refresh: %v", err)
	}
	ticker := time.NewTicker(periodicPricingRefreshInterval)
	defer ticker.Stop()
	runPeriodicPricingRefresh(ctx, ticker.C, database, runner)
}

func runPeriodicPricingRefresh(
	ctx context.Context,
	ticks <-chan time.Time,
	database *db.DB,
	runner pricingRefreshExclusiveRunner,
) {
	runPricingRefreshLoop(ctx, ticks, func(ctx context.Context) error {
		return runCurrentPricingRefresh(ctx, database, runner)
	})
}

func runCurrentPricingRefresh(
	ctx context.Context,
	database *db.DB,
	runner pricingRefreshExclusiveRunner,
) error {
	refresh := func() error {
		return pricingrefresh.RefreshCurrent(ctx, database)
	}
	return runPricingExclusive(runner, refresh)
}

func runPricingExclusive(
	runner pricingRefreshExclusiveRunner,
	work func() error,
) error {
	if runner == nil {
		return work()
	}
	return runner.RunExclusive(work)
}

func runPricingRefreshLoop(
	ctx context.Context,
	ticks <-chan time.Time,
	refresh func(context.Context) error,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if err := refresh(ctx); err != nil && ctx.Err() == nil {
				log.Printf("pricing refresh: %v", err)
			}
		}
	}
}
