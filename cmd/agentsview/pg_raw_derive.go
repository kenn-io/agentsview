package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/server"
)

func rawProcessingVersion() string { return fmt.Sprintf("parser-data-%d", db.CurrentDataVersion()) }

// pgRawRuntime owns one bounded sequential worker/maintenance loop. Stop joins
// all materialization/parser work before custody and database owners may close.
type pgRawRuntime struct {
	mu               sync.Mutex
	stopped, started bool
	cancel           context.CancelFunc
	done             chan struct{}
	ctx              context.Context
	batch            func(context.Context)
	interval         time.Duration
}

type pgRuntimeStopper interface{ Stop() }
type pgRuntimeCloser interface{ Close() error }

// pgHostedRawShutdown owns the production shutdown order. Parser work must
// join before the bound parser image and shared custody repository are closed.
type pgHostedRawShutdown struct {
	parityRuntime, rawRuntime, embeddingRuntime pgRuntimeStopper
	parityParser                                pgRuntimeCloser
	closeUploads                                func() error
	custody, store                              pgRuntimeCloser
}

func (s *pgHostedRawShutdown) Close() {
	if s.parityRuntime != nil {
		s.parityRuntime.Stop()
	}
	if s.rawRuntime != nil {
		s.rawRuntime.Stop()
	}
	if s.embeddingRuntime != nil {
		s.embeddingRuntime.Stop()
	}
	if s.parityParser != nil {
		if err := s.parityParser.Close(); err != nil {
			log.Print("migration parity parser cleanup failed")
		}
	}
	if s.closeUploads != nil {
		if err := s.closeUploads(); err != nil {
			log.Print("hosted raw upload cleanup failed")
		}
	}
	if s.custody != nil {
		if err := s.custody.Close(); err != nil {
			log.Print("hosted raw custody cleanup failed")
		}
	}
	if s.store != nil {
		if err := s.store.Close(); err != nil {
			log.Print("hosted raw database cleanup failed")
		}
	}
}

func newPGRawRuntime(parent context.Context, interval time.Duration, batch func(context.Context)) *pgRawRuntime {
	ctx, cancel := context.WithCancel(parent)
	return &pgRawRuntime{ctx: ctx, cancel: cancel, done: make(chan struct{}), batch: batch, interval: interval}
}
func (r *pgRawRuntime) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || r.stopped {
		return
	}
	r.started = true
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			if r.ctx.Err() != nil {
				return
			}
			r.batch(r.ctx)
			select {
			case <-r.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (r *pgRawRuntime) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.cancel()
	started := r.started
	r.mu.Unlock()
	if started {
		<-r.done
	}
}

func prepareHostedPGServe(app config.Config, pg config.PGConfig, basePath string) (pgServeStartup, error) {
	if err := pg.ValidateRawDerivation(app.RequireAuth); err != nil {
		return pgServeStartup{}, err
	}
	if err := pg.ValidateHostedEmbeddings(app.RequireAuth); err != nil {
		return pgServeStartup{}, err
	}
	if err := pg.ValidateParity(app.RequireAuth); err != nil {
		return pgServeStartup{}, err
	}
	applyClassifierConfig(app)
	store, err := postgres.NewHostedStore(pg.URL, pg.Schema, pg.RawTenant, pg.AllowInsecure)
	if err != nil {
		return pgServeStartup{}, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var runtime *pgRawRuntime
	var parityRuntime *pgRawRuntime
	var parityOwner *pgMigrationParityOwner
	var embeddingRuntime *hostedEmbeddingRuntime
	var closeUploads func() error
	custody := &pgRawSyncCustody{dataDir: app.DataDir, tenant: pg.RawTenant, limits: rawsync.DefaultManifestLimits(), version: rawProcessingVersion()}
	shutdown := &pgHostedRawShutdown{custody: custody, store: store}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			stop()
			shutdown.Close()
		})
	}
	fail := func(err error) (pgServeStartup, error) { cleanup(); return pgServeStartup{}, err }
	if err = applyRequiredCursorSecret(store, app); err != nil {
		return fail(err)
	}
	store.SetCustomPricing(app.CustomModelPricing)
	if err = postgres.CheckHostedRuntimeWritable(ctx, store.DB(), pg.Schema, app.ArchiveContent); err != nil {
		return fail(err)
	}
	poll, attempt, maxAttempts, concurrency := pg.HostedEmbeddingWorkerBounds()
	embeddings, embeddingErr := postgres.NewHostedEmbeddingStore(ctx, store.DB(), postgres.HostedEmbeddingOptions{Schema: pg.Schema, Tenant: pg.RawTenant, MaxAttempts: maxAttempts})
	if embeddingErr != nil && !errors.Is(embeddingErr, postgres.ErrHostedEmbeddingUnprovisioned) {
		return fail(embeddingErr)
	}
	if embeddings == nil {
		store.SetSemanticUnavailableReason("hosted embeddings are not provisioned")
		if pg.HostedEmbeddingsEnabled {
			return fail(errors.New("hosted embeddings are enabled but not provisioned"))
		}
	} else if app.ArchiveContent.UsageOnly() {
		if err = embeddings.CheckWritable(ctx); err != nil {
			return fail(err)
		}
		if err = embeddings.ClearContent(ctx); err != nil {
			return fail(err)
		}
		store.SetSemanticUnavailableReason("hosted semantic search is unavailable for usage-only archives")
	} else {
		resolver := newHostedEmbeddingResolver(app.HostedEmbeddings)
		store.SetVectorSearcher(newHostedEmbeddingSearcher(embeddings, resolver))
		if pg.HostedEmbeddingsEnabled {
			if err = embeddings.CheckWritable(ctx); err != nil {
				return fail(err)
			}
			embeddingRuntime = newHostedEmbeddingRuntime(ctx, embeddings, resolver, hostedEmbeddingRuntimeOptions{
				PollInterval: time.Duration(poll) * time.Second, AttemptTimeout: time.Duration(attempt) * time.Second,
				SourceConcurrency: concurrency, LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second,
			})
			shutdown.embeddingRuntime = embeddingRuntime
		}
	}
	metadata, err := postgres.NewHostedRawIngestStore(store.DB(), pg.RawTenant, custody.version)
	if err != nil {
		return fail(err)
	}
	custody.metadata = metadata
	authStore, err := postgres.NewTenantRawDeviceAuthStore(store.DB(), pg.RawTenant)
	if err != nil {
		return fail(err)
	}
	auth, err := rawsync.NewDeviceAuthService(authStore, pgRawSyncTokenTTL)
	if err != nil {
		return fail(err)
	}
	if pg.ParityEnabled {
		parityOwner, err = newPGMigrationParityOwner(ctx, app, pg, store.DB(), custody)
		if err != nil {
			return fail(err)
		}
		shutdown.parityParser = parityOwner.parser
		poll, _, _ := pg.ParityWorkerBounds()
		parityRuntime = newPGRawRuntime(ctx, time.Duration(poll)*time.Second, func(ctx context.Context) {
			if err := parityOwner.runOne(ctx); err != nil && ctx.Err() == nil {
				log.Print("migration parity worker batch failed")
			}
		})
		shutdown.parityRuntime = parityRuntime
	}
	if pg.RawDerivation {
		poll, attempt, maxAttempts := pg.RawWorkerBounds()
		isolated, err := rawderive.NewSubprocessParser(time.Duration(attempt) * time.Second)
		if err != nil {
			return fail(err)
		}
		if err = isolated.Preflight(ctx); err != nil {
			return fail(err)
		}
		retry := rawderive.RetryPolicy{Base: time.Second, Maximum: time.Minute, MaxAttempts: maxAttempts}
		sink, err := postgres.NewRawProjectionStore(store.DB(), hostedRawProjectionOptions(app, pg.RawTenant, retry))
		if err != nil {
			return fail(err)
		}
		worker, err := rawderive.NewWorker(rawderive.WorkerConfig{
			Queue: metadata, Manifests: rawderive.ManifestLoader{Store: custody, Limits: custody.limits},
			Materializer: rawderive.Materializer{Store: custody, BaseDir: os.TempDir(), MaxTotalBytes: 512 << 20}, Parser: isolated, Projection: sink,
			Owner: fmt.Sprintf("hosted-%d", os.Getpid()), BatchSize: 1, LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second, AttemptTimeout: time.Duration(attempt) * time.Second,
			RetryBase: retry.Base, RetryMax: retry.Maximum, MaxAttempts: retry.MaxAttempts,
		})
		if err != nil {
			return fail(err)
		}
		runtime = newPGRawRuntime(ctx, time.Duration(poll)*time.Second, func(ctx context.Context) {
			result, err := worker.RunBatch(ctx)
			if err != nil && ctx.Err() == nil {
				log.Printf("hosted raw worker: claimed=%d succeeded=%d retried=%d failed=%d; batch failed", result.Claimed, result.Succeeded, result.Retried, result.Failed)
			}
			if ctx.Err() != nil {
				return
			}
			maintenance, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if _, err = sink.SettlePendingSignals(maintenance, 64); err != nil && ctx.Err() == nil {
				log.Print("hosted raw pending-signal maintenance failed")
			}
		})
		shutdown.rawRuntime = runtime
	}
	rtOpts := serveRuntimeOptions{Mode: "pg-serve", RequestedPort: app.Port}
	app, err = prepareServeRuntimeConfig(app, rtOpts)
	if err != nil {
		return fail(err)
	}
	uploadOption, closeUploadStore, err := preparePGRawSyncUploads(app.DataDir, store.DB(), custody)
	if err != nil {
		return fail(err)
	}
	closeUploads = closeUploadStore
	shutdown.closeUploads = closeUploads
	opts := []server.Option{server.WithVersion(server.VersionInfo{Version: version, Commit: commit, BuildDate: buildDate, ReadOnly: true}), server.WithDataDir(app.DataDir), server.WithBaseContext(ctx), server.WithRawSyncServices(auth, custody), server.WithRawSyncTenant(pg.RawTenant), uploadOption}
	if basePath != "" {
		opts = append(opts, server.WithBasePath(basePath))
	}
	startup := pgServeStartup{cfg: app, ctx: ctx, rtOpts: rtOpts, srv: server.New(app, store, nil, opts...), cleanup: cleanup}
	if runtime != nil || parityRuntime != nil || embeddingRuntime != nil {
		startup.startWorker = func() {
			if parityRuntime != nil {
				parityRuntime.Start()
			}
			if runtime != nil {
				runtime.Start()
			}
			if embeddingRuntime != nil {
				embeddingRuntime.Start()
			}
		}
	}
	return startup, nil
}

func newPGHostedProvisionCommand() *cobra.Command {
	return &cobra.Command{Use: "hosted-provision [target]", Short: "Explicitly provision one hosted tenant schema using the configured owner connection", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.LoadMinimal()
		if err != nil {
			return err
		}
		target := ""
		if len(args) > 0 {
			target = args[0]
		}
		pg, err := cfg.ResolvePGTarget(target)
		if err != nil {
			return err
		}
		if pg.RawTenant == "" {
			return errors.New("hosted-provision requires raw_tenant in the selected PG target")
		}
		applyClassifierConfig(cfg)
		database, err := postgres.Open(pg.URL, pg.Schema, pg.AllowInsecure)
		if err != nil {
			return errors.New("opening hosted owner connection failed")
		}
		defer database.Close()
		if err = postgres.EnsureHostedTenant(cmd.Context(), database, pg.Schema, pg.RawTenant); err != nil {
			return fmt.Errorf("hosted provisioning failed: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Hosted tenant schema provisioned. Use a restricted tenant runtime connection to serve.")
		return nil
	}}
}
func newPGRawReparseCommand() *cobra.Command {
	var runID string
	var batch int
	cmd := &cobra.Command{Use: "raw-reparse [target]", Short: "Schedule one bounded, resumable batch of current raw heads for this parser version", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if runID == "" {
			return errors.New("raw-reparse requires --run-id for its durable checkpoint")
		}
		cfg, err := config.LoadMinimal()
		if err != nil {
			return err
		}
		target := ""
		if len(args) > 0 {
			target = args[0]
		}
		pg, err := cfg.ResolvePGTarget(target)
		if err != nil {
			return err
		}
		if err = pg.ValidateRawDerivation(cfg.RequireAuth); err != nil {
			return err
		}
		if pg.RawTenant == "" {
			return errors.New("raw-reparse requires raw_tenant")
		}
		store, err := postgres.NewHostedStore(pg.URL, pg.Schema, pg.RawTenant, pg.AllowInsecure)
		if err != nil {
			return err
		}
		defer store.Close()
		sink, err := postgres.NewRawProjectionStore(store.DB(), postgres.RawProjectionOptions{Tenant: pg.RawTenant})
		if err != nil {
			return err
		}
		result, err := sink.ScheduleCurrentHeads(cmd.Context(), runID, rawProcessingVersion(), batch)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Selected %d current heads; complete=%t\n", result.Selected, result.Done)
		return nil
	}}
	cmd.Flags().StringVar(&runID, "run-id", "", "Durable rollout checkpoint ID; reuse to resume")
	cmd.Flags().IntVar(&batch, "batch-size", 64, "Maximum current heads in this invocation (1-256)")
	return cmd
}

func hostedRawProjectionOptions(app config.Config, tenant string, retry rawderive.RetryPolicy) postgres.RawProjectionOptions {
	blocked := make(map[string]bool, len(app.ResultContentBlockedCategories))
	for _, category := range app.ResultContentBlockedCategories {
		category = strings.TrimSpace(category)
		if category != "" {
			blocked[strings.ToUpper(category[:1])+strings.ToLower(category[1:])] = true
		}
	}
	return postgres.RawProjectionOptions{Tenant: tenant, RetryPolicy: retry, Content: ingest.ContentOptions{ArchiveContent: app.ArchiveContent, ToolResultImages: app.ToolResultImages, BlockedResultCategories: blocked}}
}
