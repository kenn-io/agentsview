package main

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/poller"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/timeutil"
)

// frictionRunnerStore is a review store that can report whether it is writable.
type frictionRunnerStore interface {
	review.Store
}

type workTracker interface {
	BeginWork() (func(), bool)
}

func newFrictionRunner(cfg config.Config, store frictionRunnerStore, now func() time.Time) (*review.Runner, error) {
	if !cfg.Friction.Enabled || store == nil {
		return nil, nil
	}
	if local, ok := store.(interface{ ArchiveContent() config.ArchiveContent }); ok && local.ArchiveContent().UsageOnly() {
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
	zone := strings.TrimSpace(os.Getenv("AGENTSVIEW_FRICTION_TZ"))
	if zone == "" {
		zone = strings.TrimSpace(cfg.Friction.Timezone)
	}
	loc := timeutil.LocalLocation()
	if store.ReadOnly() {
		// Multiple hub processes must agree even when their host timezones differ.
		loc = time.UTC
	}
	var err error
	if zone != "" {
		loc, err = time.LoadLocation(zone)
	}
	if err != nil {
		return nil, err
	}
	patterns, err := friction.CompileSeatPatterns(cfg.Friction.SeatPatterns)
	if err != nil {
		return nil, err
	}
	return &review.Runner{
		Store: store, Loc: loc, Now: now,
		BackfillDays: cfg.Friction.BackfillDays, SeatPatterns: patterns,
	}, nil
}

// frictionExclusive keeps a detached daemon alive and serializes digest work
// with local sync and resync.
func frictionExclusive(tracker workTracker, exclusive func(func() error) error) func(func() error) error {
	return func(work func() error) error {
		if tracker != nil {
			done, ok := tracker.BeginWork()
			if !ok {
				return nil
			}
			defer done()
		}
		if exclusive == nil {
			return work()
		}
		return exclusive(work)
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
	exclusive func(func() error) error,
) (*review.Runner, func()) {
	runner, err := newFrictionRunner(cfg, store, time.Now)
	if err != nil {
		log.Printf("friction review disabled: %v", err)
		return nil, func() {}
	}
	if runner == nil {
		return nil, func() {}
	}
	scheduler := poller.Start(ctx, review.Job(runner, exclusive))
	return runner, scheduler.Wait
}

// startLocalFrictionReview shares the server's manual-sync barrier even when
// --no-sync leaves the background engine disabled.
func startLocalFrictionReview(
	ctx context.Context, cfg config.Config, store frictionRunnerStore,
	srv *server.Server, tracker workTracker,
) func() {
	exclusive := frictionExclusive(tracker, func(work func() error) error {
		return srv.SerializeArchiveWrite(ctx, work)
	})
	runner, wait := startFrictionReview(ctx, cfg, store, exclusive)
	server.WithFriction(runner)(srv)
	return wait
}
