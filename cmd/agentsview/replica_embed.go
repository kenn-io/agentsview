package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
)

// errEmbedNeedsLocalArchive rejects pg push --embed when a daemon owns the
// archive: the build runs in this process, and a daemon route is not built.
var errEmbedNeedsLocalArchive = errors.New(
	"pg push --embed builds embeddings in this process and cannot run while " +
		"a daemon owns the archive; run pg push without --embed so the daemon " +
		"builds and pushes embeddings itself, or stop the daemon " +
		"(agentsview daemon stop) to build here")

// replicaEmbedder keeps the first resolved recipe for this process.
type replicaEmbedder struct {
	appCfg   config.Config
	backend  storage.Replica
	target   storage.ReplicaTarget
	source   *vectorPushSource
	database *db.DB
	serving  vectorServing
	cancel   context.CancelFunc
}

// newReplicaEmbedder validates a pg push --embed request before any local
// work runs.
func newReplicaEmbedder(
	appCfg config.Config, backend storage.Replica,
	target storage.ConfiguredReplica, database *db.DB,
) (*replicaEmbedder, error) {
	if !target.Target.PushVectors {
		return nil, errors.New(
			"--embed pushes vectors; remove push_vectors = false")
	}
	if appCfg.ArchiveContent.UsageOnly() {
		return nil, errors.New("--embed is unavailable for usage-only archives")
	}
	if !appCfg.Vector.Enabled && appCfg.DeploymentEmbeddings == nil {
		return nil, errors.New(
			"--embed needs [vector] enabled in config.toml, or " +
				"AGENTSVIEW_EMBEDDINGS_ENDPOINT under AGENTSVIEW_MODE to adopt " +
				"a recipe published to PostgreSQL")
	}
	return &replicaEmbedder{
		appCfg: appCfg, backend: backend, target: target.Target, database: database,
	}, nil
}

func (e *replicaEmbedder) resolve(ctx context.Context) error {
	if e.source == nil {
		cfg, err := e.resolveRecipe(ctx)
		if err != nil {
			return err
		}
		e.source = &vectorPushSource{cfg: cfg, adopted: !e.appCfg.Vector.Enabled}
	}
	return nil
}

func (e *replicaEmbedder) startScheduler(ctx context.Context) error {
	if err := e.resolve(ctx); err != nil {
		return err
	}
	if e.cancel == nil {
		serving, err := setupVectorServing(ctx, e.source.cfg, e.database, nil)
		if err != nil {
			return err
		}
		e.serving = serving
		buildCtx, cancel := context.WithCancel(ctx)
		e.cancel = cancel
		if serving.Scheduler == nil {
			return nil
		}
		go serving.Scheduler.Run(buildCtx)
	}
	if e.serving.Scheduler != nil {
		e.serving.Scheduler.Notify()
	}
	return nil
}

func (e *replicaEmbedder) build(ctx context.Context) error {
	if err := runEmbeddingsBuildDirect(ctx, io.Discard, e.source.cfg, vector.BuildRequest{
		IncludeAutomated: e.source.cfg.Vector.IncludeAutomated,
	}); err != nil {
		return fmt.Errorf("building embeddings: %w", err)
	}
	return nil
}

func (e *replicaEmbedder) resolveRecipe(ctx context.Context) (config.Config, error) {
	if e.appCfg.Vector.Enabled {
		return e.appCfg, nil
	}
	applyClassifierConfig(e.appCfg)
	store, err := e.backend.OpenStore(e.target)
	if err != nil {
		return config.Config{}, fmt.Errorf("opening %s: %w", e.backend.DisplayName(), err)
	}
	defer func() { _ = store.Close() }()
	return adoptReplicaVectorConfig(ctx, e.appCfg, e.backend, store)
}

// BeginExport exports the pinned recipe's generation; before a recipe is
// pinned there is nothing to push and the vector phase is skipped.
func (e *replicaEmbedder) BeginExport(
	ctx context.Context, sessionIDs []string,
) (storage.VectorExport, bool, error) {
	if e.source == nil {
		return nil, false, nil
	}
	return e.source.BeginExport(ctx, sessionIDs)
}

func (e *replicaEmbedder) Close() error {
	if e.cancel != nil {
		e.cancel()
	}
	if e.serving.Scheduler != nil {
		e.serving.Scheduler.Stop()
	}
	var err error
	if e.serving.Close != nil {
		err = e.serving.Close()
	}
	if e.source == nil {
		return err
	}
	return errors.Join(err, e.source.Close())
}
