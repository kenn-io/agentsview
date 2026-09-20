package main

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/config"
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
	return opts, closeRawSync, nil
}
