package main

import (
	"context"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/telemetry"
)

func startSetupTelemetry(ctx context.Context, reporter *telemetry.Reporter, cfg config.Config, database *db.DB, broadcaster *server.Broadcaster) func() {
	if !reporter.Enabled() {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSetupTelemetry(ctx, telemetryDailyCheckInterval, cfg, database, broadcaster, func() error {
			return reporter.CaptureSetupFinished(ctx)
		})
	}()
	return func() {
		cancel()
		<-done
	}
}

func runSetupTelemetry(ctx context.Context, interval time.Duration, cfg config.Config, database *db.DB, broadcaster *server.Broadcaster, send func() error) {
	subscribed, unsubscribe := broadcaster.Subscribe()
	defer unsubscribe()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		claimed, err := cfg.SetupFinishedClaimed()
		if claimed {
			return
		}
		if err == nil {
			var visible bool
			visible, err = database.HasVisibleSession(ctx)
			if err == nil && visible {
				err = cfg.ClaimSetupFinished(send)
				if err == nil {
					return
				}
			}
		}
		// After a failure, retry only on the hourly ticker; a clean check
		// resumes waking on broadcasts.
		events := subscribed
		if err != nil && ctx.Err() == nil {
			log.Printf("capture setup telemetry: %v", err)
			events = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-events:
		case <-ticker.C:
		}
	}
}
