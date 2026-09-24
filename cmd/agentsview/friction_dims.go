package main

import (
	"context"
	"log"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/nanoclaw"
	"go.kenn.io/agentsview/internal/sync"
)

// newNanoClawResolver builds the resolver for [friction.nanoclaw], or nil
// when data_dir is unset. Config load already validated the section.
func newNanoClawResolver(cfg config.Config) *nanoclaw.Resolver {
	nc := cfg.Friction.NanoClaw
	if !nc.Active() {
		return nil
	}
	dataDir, dbPath, err := nc.ResolvedPaths()
	if err != nil {
		log.Printf("friction: NanoClaw dimensions disabled: %v", err)
		return nil
	}
	return nanoclaw.NewResolver(dataDir, dbPath, nanoclaw.Filter{Include: nc.Include, Exclude: nc.Exclude})
}

// newFrictionDimsFunc builds the fleet-dims hook friction compute uses
// (spec §11). It is nil when neither seat_patterns nor [friction.nanoclaw]
// data_dir is configured, so no dims row is written.
func newFrictionDimsFunc(cfg config.Config) sync.FrictionDimsFunc {
	var seats []friction.SeatPattern
	if len(cfg.Friction.SeatPatterns) > 0 {
		patterns, err := friction.CompileSeatPatterns(cfg.Friction.SeatPatterns)
		if err != nil {
			log.Printf("friction: seat_patterns disabled: %v", err)
		} else {
			seats = patterns
		}
	}
	return sync.NewFrictionDims(newNanoClawResolver(cfg), seats)
}

// refreshNanoClawFriction runs on the reconcile tick. It is best-effort like
// recomputeStaleFriction: the next tick retries failures.
func refreshNanoClawFriction(ctx context.Context, engine *sync.Engine, resolver *nanoclaw.Resolver) {
	n, err := engine.RefreshNanoClawFriction(ctx, resolver)
	if err != nil && ctx.Err() == nil {
		log.Printf("nanoclaw friction refresh: %v", err)
	}
	if n > 0 {
		log.Printf("recomputed friction for %d NanoClaw sessions after an agent map change", n)
	}
}
