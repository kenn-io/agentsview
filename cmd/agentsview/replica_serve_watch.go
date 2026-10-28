package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/storage"
)

func validateReplicaServeWatchFlags(cmd *cobra.Command) error {
	if cmd.Flags().Lookup("push-watch") == nil {
		return nil
	}
	watch, _ := cmd.Flags().GetBool("push-watch")
	if !watch {
		for _, flag := range []string{"embed", "debounce", "interval"} {
			if cmd.Flags().Changed(flag) {
				return fmt.Errorf("--%s requires --push-watch", flag)
			}
		}
		return nil
	}
	noSync, _ := cmd.Flags().GetBool("no-sync")
	if noSync {
		return errors.New("--push-watch cannot be combined with --no-sync")
	}
	return nil
}

func loadReplicaServeWatch(cmd *cobra.Command, cfg config.Config) (*ReplicaPushConfig, error) {
	watch, _ := cmd.Flags().GetBool("push-watch")
	if !watch {
		return nil, nil
	}
	pg, err := cfg.ResolvePG()
	if err != nil {
		return nil, err
	}
	if pg.RawTenant != "" || pg.RawDerivation {
		return nil, errors.New("--push-watch is unavailable with raw_tenant/raw_derivation")
	}
	embed, _ := cmd.Flags().GetBool("embed")
	debounce, _ := cmd.Flags().GetDuration("debounce")
	interval, _ := cmd.Flags().GetDuration("interval")
	return &ReplicaPushConfig{Embed: embed, Debounce: debounce, Interval: interval}, nil
}

// attachReplicaServeWatch prepares ownership now, starts ingestion after
// readiness, and joins its final flush before any serve resources are closed.
func attachReplicaServeWatch(startup *replicaServeStartup, stop context.CancelFunc,
	backend storage.Replica, store storage.ReplicaStore, watch *ReplicaPushConfig,
) error {
	watch.OnPushSuccess = func(ctx context.Context) {
		if store.HasSemantic() {
			return
		}
		if err := wireReplicaVectorSearch(ctx, startup.cfg, backend, store, "pg serve"); err != nil {
			log.Printf("pg serve: refreshing semantic search after push: %v", err)
		}
	}
	run, cleanup, err := prepareReplicaPushWatch(startup.ctx, backend, startup.cfg, *watch, "", true)
	if err != nil {
		return err
	}
	joinReplicaServeWorker(startup, stop, run, cleanup)
	return nil
}

func joinReplicaServeWorker(startup *replicaServeStartup, stop context.CancelFunc, run func() error, cleanup func()) {
	joined := make(chan struct{})
	var workerErr error
	started := false
	startup.startWorker = func() {
		started = true
		go func() {
			workerErr = run()
			close(joined)
			stop()
		}()
	}
	startup.joinWorker = func() error {
		stop()
		if started {
			<-joined
		}
		return workerErr
	}
	previousCleanup := startup.cleanup
	startup.cleanup = func() {
		_ = startup.joinWorker()
		cleanup()
		previousCleanup()
	}
}
