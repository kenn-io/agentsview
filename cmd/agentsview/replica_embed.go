package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
)

// errEmbedNeedsLocalArchive rejects pg push --embed when a daemon owns the
// archive: the build runs in this process, and a daemon route is not built.
var errEmbedNeedsLocalArchive = errors.New(
	"pg push --embed builds embeddings in this process and cannot run while " +
		"a daemon owns the archive; run 'agentsview daemon stop' to build here")

// replicaEmbedder keeps the first resolved recipe for this process.
type replicaEmbedder struct {
	appCfg        config.Config
	backend       storage.Replica
	target        storage.ReplicaTarget
	source        *vectorPushSource
	emitterMu     sync.RWMutex
	scheduler     *embedScheduler
	buildOnExport bool
	buildOnce     sync.Once
	buildErr      error
	database      *db.DB
	serving       vectorServing
	cancel        context.CancelFunc
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
	e := &replicaEmbedder{
		appCfg: appCfg, backend: backend, target: target.Target, database: database,
	}
	if appCfg.Vector.Enabled {
		e.source = &vectorPushSource{cfg: appCfg}
	}
	return e, nil
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

func (e *replicaEmbedder) Emit(_ string) {
	e.emitterMu.RLock()
	scheduler := e.scheduler
	e.emitterMu.RUnlock()
	if scheduler != nil && e.source.cfg.Vector.Embed.RunAfterSyncEnabled() {
		scheduler.Notify()
	}
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
		if serving.Scheduler == nil {
			if serving.Close != nil {
				return serving.Close()
			}
			return nil
		}
		e.serving = serving
		buildCtx, cancel := context.WithCancel(ctx)
		e.cancel = cancel
		e.emitterMu.Lock()
		e.scheduler = serving.Scheduler
		e.emitterMu.Unlock()
		go serving.Scheduler.Run(buildCtx)
		serving.Scheduler.Notify()
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

// BeginExport builds once for a one-shot push and exports ready vectors for watch.
func (e *replicaEmbedder) BeginExport(
	ctx context.Context, sessionIDs []string,
) (storage.VectorExport, bool, error) {
	if e.buildOnExport {
		e.buildOnce.Do(func() {
			if e.buildErr = e.resolve(ctx); e.buildErr == nil {
				e.buildErr = e.build(ctx)
			}
		})
		if e.buildErr != nil {
			return nil, false, e.buildErr
		}
	}
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
