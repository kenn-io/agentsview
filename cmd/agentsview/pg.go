package main

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

// pgReplica is the PostgreSQL replica as this binary registers it: the
// storage.Replica contract from internal/postgres plus the PostgreSQL-only
// HTTP capabilities `pg serve` adds on top of db.Store.
type pgReplica struct {
	postgres.Backend
}

var _ replicaServeExtras = pgReplica{}

// serveOptions wires the raw-upload ingestion services when the connected
// role may write the raw sync schema. Semantic search is wired by the shared
// replica gate; PostgreSQL contributes through storage.VectorSearchProvider.
func (pgReplica) serveOptions(
	ctx context.Context, appCfg config.Config,
	target storage.ReplicaTarget, store storage.ReplicaStore,
) ([]server.Option, func() error, error) {
	pgStore, ok := store.(*postgres.Store)
	if !ok {
		return nil, nil, fmt.Errorf(
			"pg serve store is %T, not *postgres.Store", store,
		)
	}
	rawSyncWritable, err := postgres.CanWriteRawSyncSchema(
		ctx, pgStore.DB(), target.Schema,
	)
	if err != nil {
		return nil, nil, err
	}
	rawSyncOption, closeRawSync, err := preparePGRawSyncServicesIfWritable(
		ctx, appCfg.DataDir, pgStore.DB(), rawSyncWritable,
	)
	if err != nil {
		return nil, nil, err
	}
	var opts []server.Option
	if rawSyncOption != nil {
		opts = append(opts, rawSyncOption)
	}
	kataConn := kata.NewConn(kata.ConfigFrom(appCfg.Kata, filing.EligibleHost(true, false)))
	frictionExcl := serialExclusive()
	var frictionFiler *filing.Filer
	frictionRunner, waitFriction := startFrictionReview(ctx, appCfg, pgStore, frictionExcl, func(r *review.Runner) {
		frictionFiler = newFrictionFiler(frictionFilerDeps{Cfg: &appCfg, Store: pgStore, Conn: kataConn, Runner: r, IsPGServe: true})
		attachFiler(r, frictionFiler)
	})
	if frictionRunner != nil {
		opts = append(opts, server.WithFriction(frictionRunner, frictionExcl))
	}
	opts = append(opts, server.WithKataConn(kataConn), server.WithFrictionFiler(frictionFiler))
	closeAll := func() error {
		waitFriction()
		if closeRawSync != nil {
			return closeRawSync()
		}
		return nil
	}
	return opts, closeAll, nil
}
