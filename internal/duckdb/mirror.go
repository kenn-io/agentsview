package duckdb

import (
	"context"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// Mirror is the DuckDB derived mirror. It adapts this package's push
// functions to the storage.Mirror contract; it holds no state.
type Mirror struct{}

var _ storage.Mirror = Mirror{}

func (Mirror) Name() string { return "duckdb" }

func (Mirror) DisplayName() string { return "DuckDB" }

func (Mirror) ValidatePushTarget(cfg config.DuckDBConfig) error {
	return ValidatePushTarget(cfg)
}

func (Mirror) Push(
	ctx context.Context, cfg config.DuckDBConfig, local *db.DB,
	opts storage.MirrorPushOptions, full bool,
	onProgress func(storage.MirrorPushProgress),
) (storage.MirrorPushResult, error) {
	return Push(
		ctx, cfg.Path, local, cfg.MachineName, opts, full, onProgress,
	)
}
