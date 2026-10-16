package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

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

// errEmbedWriteLockHeld reports that another process owns vectors.write.lock,
// so pg push --watch --embed cannot start building yet.
var errEmbedWriteLockHeld = errors.New(
	"vectors.write.lock is held by another process, typically a direct " +
		"'agentsview embeddings build'; embeddings start on the first push " +
		"after it exits")

// replicaEmbedder keeps the first resolved recipe for this process.
type replicaEmbedder struct {
	appCfg   config.Config
	backend  storage.Replica
	target   storage.ReplicaTarget
	database *db.DB
	source   *vectorPushSource

	// buildOnPrepare builds once in PrepareExport for a one-shot push, writing
	// build progress to out.
	buildOnPrepare bool
	out            io.Writer
	buildOnce      sync.Once
	buildErr       error

	// The watch push loop alone owns these fields. resolveRetry spaces out
	// recipe lookups while none is adopted; lastStartErr keeps a repeated
	// start failure from logging on every push.
	resolveRetry   time.Duration
	retryResolveAt time.Time
	lastStartErr   string
	serving        vectorServing
	cancel         context.CancelFunc

	emitterMu    sync.RWMutex
	scheduler    *embedScheduler
	recallNotify func()
}

// newReplicaEmbedder rejects a pg push --embed request that cannot work with
// this configuration, before any local work runs.
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
		out: io.Discard,
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
	scheduler, recallNotify := e.scheduler, e.recallNotify
	e.emitterMu.RUnlock()
	if scheduler != nil && e.source.cfg.Vector.Embed.RunAfterSyncEnabled() {
		scheduler.Notify()
	}
	if recallNotify != nil {
		recallNotify()
	}
}

// ensureScheduler starts background builds for the watch loop. It logs a
// start failure once until the failure changes, and logs the start that
// follows a failure.
func (e *replicaEmbedder) ensureScheduler(ctx context.Context) {
	if err := e.startScheduler(ctx); err != nil {
		if msg := err.Error(); msg != e.lastStartErr {
			e.lastStartErr = msg
			log.Printf("pg watch: embeddings not started: %v", err)
		}
		return
	}
	if e.cancel != nil && e.lastStartErr != "" {
		e.lastStartErr = ""
		log.Printf("pg watch: embeddings started")
	}
}

// startScheduler starts the message and recall schedulers once a recipe
// resolves and vectors.write.lock is free. While no recipe is adopted it
// looks one up at most once per resolveRetry and returns nil in between.
func (e *replicaEmbedder) startScheduler(ctx context.Context) error {
	if e.cancel != nil {
		return nil
	}
	if e.source == nil {
		if time.Now().Before(e.retryResolveAt) {
			return nil
		}
		if err := e.resolve(ctx); err != nil {
			e.retryResolveAt = time.Now().Add(e.resolveRetry)
			return err
		}
	}
	// Check the lock without waiting: setupVectorServing retries for
	// several seconds, which would stall every push while a direct build
	// runs.
	lock, err := tryAcquireNamedLock(e.source.cfg.DataDir, vectorsWriteLockFile)
	if _, held := errors.AsType[writeOwnerLockHeldError](err); held {
		return errEmbedWriteLockHeld
	}
	if err != nil {
		return err
	}
	if err := lock.Close(); err != nil {
		return err
	}
	serving, err := setupVectorServing(ctx, e.source.cfg, e.database, nil)
	if err != nil {
		return err
	}
	if serving.Scheduler == nil {
		// Another process took the lock after the check above.
		return errEmbedWriteLockHeld
	}
	e.serving = serving
	buildCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.emitterMu.Lock()
	e.scheduler = serving.Scheduler
	e.recallNotify = serving.RecallMutationNotify
	e.emitterMu.Unlock()
	go serving.Scheduler.Run(buildCtx)
	go serving.RecallScheduler.Run(buildCtx)
	serving.Scheduler.Notify()
	return nil
}

func (e *replicaEmbedder) build(ctx context.Context) error {
	fmt.Fprintln(e.out, "\nBuilding embeddings...")
	if err := runEmbeddingsBuildDirect(ctx, e.out, e.source.cfg, vector.BuildRequest{
		IncludeAutomated: e.source.cfg.Vector.IncludeAutomated,
	}); err != nil {
		return fmt.Errorf("building embeddings: %w", err)
	}
	return nil
}

func (e *replicaEmbedder) resolveRecipe(ctx context.Context) (config.Config, error) {
	applyClassifierConfig(e.appCfg)
	store, err := e.backend.OpenStore(e.target)
	if err != nil {
		return config.Config{}, fmt.Errorf("opening %s: %w", e.backend.DisplayName(), err)
	}
	defer func() { _ = store.Close() }()
	return adoptReplicaVectorConfig(ctx, e.appCfg, e.backend, store)
}

// PlannedGeneration reports the generation a one-shot push builds, so the
// replica can register it before the build. Watch pushes build in the
// background and plan nothing.
func (e *replicaEmbedder) PlannedGeneration(
	ctx context.Context,
) (storage.VectorGenerationInfo, bool, error) {
	if !e.buildOnPrepare {
		return storage.VectorGenerationInfo{}, false, nil
	}
	if err := e.resolve(ctx); err != nil {
		return storage.VectorGenerationInfo{}, false, err
	}
	return e.source.configuredGeneration(), true, nil
}

// PrepareExport builds once for a one-shot push. The replica calls it only
// after confirming it can store vectors, so a push that skips vectors does
// not build.
func (e *replicaEmbedder) PrepareExport(ctx context.Context) error {
	if !e.buildOnPrepare {
		return nil
	}
	e.buildOnce.Do(func() {
		if e.buildErr = e.resolve(ctx); e.buildErr == nil {
			e.buildErr = e.build(ctx)
		}
	})
	return e.buildErr
}

// BeginExport exports the ready vectors of the resolved recipe. An older
// generation still active while the recipe builds reports not ready, so the
// push skips vectors and a --full repair stays pending for the recipe.
func (e *replicaEmbedder) BeginExport(
	ctx context.Context, sessionIDs []string,
) (storage.VectorExport, bool, error) {
	if e.source == nil {
		return nil, false, nil
	}
	export, ok, err := e.source.BeginExport(ctx, sessionIDs)
	if err != nil || !ok || export == nil {
		return export, ok, err
	}
	active := export.Generation().Fingerprint
	recipe := e.source.configuredGeneration().Fingerprint
	if active == recipe {
		return export, true, nil
	}
	if err := export.Close(); err != nil {
		return nil, false, fmt.Errorf("closing vector export: %w", err)
	}
	return nil, false, fmt.Errorf(
		"%w: active generation %s is not the recipe's generation %s",
		storage.ErrVectorSourceNotReady, active, recipe)
}

func (e *replicaEmbedder) Close() error {
	if e.cancel != nil {
		e.cancel()
		e.serving.Scheduler.Stop()
		e.serving.RecallScheduler.Stop()
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
