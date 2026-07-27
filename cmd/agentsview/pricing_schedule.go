package main

import (
	"context"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricingrefresh"
)

const periodicPricingRefreshInterval = 24 * time.Hour

func startPeriodicPricingRefresh(ctx context.Context, database *db.DB) {
	ticker := time.NewTicker(periodicPricingRefreshInterval)
	defer ticker.Stop()
	runPeriodicPricingRefresh(ctx, ticker.C, func(ctx context.Context) error {
		return pricingrefresh.EnsureCurrent(ctx, database)
	})
}

func runPeriodicPricingRefresh(
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
