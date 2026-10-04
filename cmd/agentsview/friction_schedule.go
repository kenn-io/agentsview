package main

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/poller"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

// frictionRunnerStore is a review store that can report whether it is writable.
type frictionRunnerStore interface {
	review.Store
	ReadOnly() bool
}

type workTracker interface {
	BeginWork() (func(), bool)
}

func newFrictionRunner(cfg config.Config, store frictionRunnerStore, now func() time.Time) (*review.Runner, error) {
	if !cfg.Friction.Enabled || store == nil {
		return nil, nil
	}
	if capable, ok := store.(interface{ FrictionAvailable() bool }); ok {
		if !capable.FrictionAvailable() {
			log.Printf("friction review disabled: this store cannot write friction tables")
			return nil, nil
		}
	} else if store.ReadOnly() {
		return nil, nil
	}
	loc, err := friction.ResolveZone(os.Getenv(friction.ZoneEnvVar), cfg.Friction.Timezone)
	if err != nil {
		return nil, err
	}
	var filer review.Filer
	if links, ok := store.(filing.LinkStore); ok {
		filer = &filing.Filer{Store: links}
	}
	return &review.Runner{
		Store: store, Loc: loc, Now: now,
		BackfillDays: cfg.Friction.BackfillDays, PublicURL: cfg.PublicURL,
		Filer: filer,
	}, nil
}

// frictionExclusive keeps a detached daemon alive and serializes digest work
// with local sync and resync.
func frictionExclusive(tracker workTracker, runner remoteSyncExclusiveRunner) func(func() error) error {
	return func(work func() error) error {
		if tracker != nil {
			done, ok := tracker.BeginWork()
			if !ok {
				return nil
			}
			defer done()
		}
		if runner == nil {
			return work()
		}
		return runner.RunExclusive(work)
	}
}

// serialExclusive serializes digest work within one pg serve process.
func serialExclusive() func(func() error) error {
	var mu sync.Mutex
	return func(work func() error) error {
		mu.Lock()
		defer mu.Unlock()
		return work()
	}
}

// startFrictionReview starts the catch-up job when review writes are available.
// The caller cancels ctx before invoking the returned wait function.
func startFrictionReview(
	ctx context.Context, cfg config.Config, store frictionRunnerStore,
	exclusive func(func() error) error, attach ...func(*review.Runner),
) (*review.Runner, func()) {
	return startFrictionReviewWithBackfill(ctx, cfg, store, exclusive, nil, attach...)
}

func startFrictionReviewWithBackfill(
	ctx context.Context, cfg config.Config, store frictionRunnerStore,
	exclusive func(func() error) error, initialBackfill func(context.Context) error,
	attach ...func(*review.Runner),
) (*review.Runner, func()) {
	runner, err := newFrictionRunner(cfg, store, time.Now)
	if err != nil {
		log.Printf("friction review disabled: %v", err)
		return nil, func() {}
	}
	if runner == nil {
		return nil, func() {}
	}
	for _, hook := range attach {
		hook(runner)
	}
	job := review.Job(runner, exclusive)
	if initialBackfill != nil {
		runReview := job.Run
		var once sync.Once
		job.Run = func(jobCtx context.Context) error {
			once.Do(func() {
				if err := initialBackfill(jobCtx); err != nil && jobCtx.Err() == nil {
					log.Printf("friction backfill: %v", err)
				}
			})
			return runReview(jobCtx)
		}
	}
	scheduler := poller.Start(ctx, job)
	return runner, scheduler.Wait
}

func backfillFrictionAfterStartup(ctx context.Context, engine *agentsync.Engine) error {
	if engine == nil {
		return nil
	}
	// Wait for the startup sync and its archive swap, then let BackfillFriction
	// take the engine lock per session so later syncs can interleave safely.
	if err := engine.RunStartupMaintenance(ctx, func() error { return nil }); err != nil {
		return err
	}
	_, err := engine.BackfillFriction(ctx)
	return err
}
