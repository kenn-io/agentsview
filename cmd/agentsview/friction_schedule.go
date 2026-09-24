package main

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/poller"
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
	runner := &review.Runner{
		Store: store, Loc: loc, Now: now,
		BackfillDays: cfg.Friction.BackfillDays, PublicURL: cfg.PublicURL,
	}
	if cfg.Ledger.Enabled {
		writer, ok := store.(ledger.WriterStore)
		if !ok {
			return nil, errors.New("friction ledger enabled: review store cannot append ledger segments")
		}
		querier, ok := store.(diagnosticsStore)
		if !ok {
			return nil, errors.New("friction ledger enabled: review store cannot query diagnostics")
		}
		parts := frictionLedgerWiring(cfg, writer, querier, nil)
		runner.Ledger = parts.JobSink
		runner.LedgerSource = parts.Source
		runner.Diagnostics = parts.Diagnostics
	}
	return runner, nil
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
