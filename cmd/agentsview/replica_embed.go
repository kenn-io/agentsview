package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/agentsview/internal/config"
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

// replicaEmbedder builds embeddings before a push and is the push's vector
// source. The first recipe it resolves is pinned for the process; a failed
// resolution is retried on the next cycle. Pushes call it serially.
type replicaEmbedder struct {
	appCfg  config.Config
	backend storage.Replica
	target  storage.ReplicaTarget
	pinned  *config.Config
	source  *vectorPushSource
}

// newReplicaEmbedder validates a pg push --embed request before any local
// work runs.
func newReplicaEmbedder(
	appCfg config.Config, backend storage.Replica,
	target storage.ConfiguredReplica, cfg ReplicaPushConfig,
) (*replicaEmbedder, error) {
	if backend.Name() != "pg" {
		return nil, fmt.Errorf("--embed is supported only by pg push, not %s push", backend.Name())
	}
	if cfg.NoVectors || !target.Target.PushVectors {
		return nil, errors.New(
			"--embed pushes vectors; remove --no-vectors or push_vectors = false")
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
		appCfg: appCfg, backend: backend, target: target.Target,
	}, nil
}

// prepare resolves and pins the recipe on first success, then builds
// pending embeddings into vectors.db.
func (e *replicaEmbedder) prepare(ctx context.Context) error {
	if e.pinned == nil {
		cfg, err := e.resolveRecipe(ctx)
		if err != nil {
			return fmt.Errorf("embedding before push: %w", err)
		}
		e.pinned = &cfg
		e.source = &vectorPushSource{cfg: cfg}
	}
	if err := runEmbeddingsBuildDirect(ctx, io.Discard, *e.pinned, vector.BuildRequest{
		IncludeAutomated: e.pinned.Vector.IncludeAutomated,
	}); err != nil {
		return fmt.Errorf("embedding before push: %w", err)
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

// Close releases the pinned source's vectors.db handle.
func (e *replicaEmbedder) Close() error {
	if e.source == nil {
		return nil
	}
	return e.source.Close()
}
