package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"net/url"
	"sync"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/power"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/vector"
	"go.kenn.io/kit/embedlocal"
)

var (
	inspectLocalEmbeddings = embedlocal.Inspect
	onBattery              = power.OnBattery
	// Kit discovers Ollama at this fixed endpoint.
	ollamaEmbeddingEndpoint = "http://127.0.0.1:11434/v1"
)

// liveVectorServing enables browser setup over the daemon's open indexes.
type liveVectorServing struct {
	mu       sync.Mutex
	serving  vectorServing
	cfg      config.Config
	database *db.DB
	ctx      context.Context
	idle     *server.IdleTracker
	cancel   context.CancelFunc
	done     chan struct{}
	setup    server.EmbeddingsSetupStatus
	reload   func() (config.Config, error)
	push     *vectorPushSource
}

func newLiveVectorServing(ctx context.Context, cfg config.Config, database *db.DB, idle *server.IdleTracker, reload func() (config.Config, error)) (*liveVectorServing, error) {
	ctx, cancel := context.WithCancel(ctx)
	serving, err := setupVectorServing(ctx, cfg, database, idle)
	if err != nil {
		cancel()
		return nil, err
	}
	v := &liveVectorServing{
		serving: serving, cfg: cfg, database: database,
		ctx: ctx, cancel: cancel, idle: idle, reload: reload,
		push: &vectorPushSource{cfg: cfg},
	}
	v.startSchedulers()
	v.startSharedChoice()
	return v, nil
}

func (v *liveVectorServing) startSharedChoice() {
	if v.cfg.ArchiveContent.UsageOnly() || v.database.ArchiveContent().UsageOnly() || v.cfg.DeploymentEmbeddings != nil {
		return
	}
	var approvedBytes int64
	embeddings := v.cfg.Vector.Embeddings
	if !v.cfg.Vector.Enabled {
		chosen, err := embedlocal.ChoiceRecorded()
		if err != nil {
			log.Printf("search by meaning: read shared choice: %v", err)
		}
		if !chosen {
			return
		}
		approvedBytes = math.MaxInt64
		// SetupStatus reports candidate errors other than an explicit choice.
		if embeddings, err = v.cfg.VectorSetupEmbeddings(true); err != nil {
			return
		}
	} else if v.serving.manager == nil {
		return
	}
	_, deployment, err := embeddings.Server("")
	if err != nil || deployment.Endpoint != "" || (v.cfg.Vector.Enabled && !deployment.Local) {
		return
	}
	deployment.Local = true
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.startSetup(deployment, approvedBytes, true, ""); err != nil {
		log.Printf("search by meaning: start setup: %v", err)
	}
}

func (v *liveVectorServing) startSchedulers() {
	if v.serving.Scheduler == nil {
		return
	}
	v.serving.Scheduler.onBattery = onBattery
	v.serving.RecallScheduler.onBattery = onBattery
	go v.serving.Scheduler.Run(v.ctx)
	go v.serving.RecallScheduler.Run(v.ctx)
}

func (v *liveVectorServing) stopBuilders() {
	if v.serving.Scheduler == nil {
		return
	}
	v.serving.Scheduler.Stop()
	v.serving.RecallScheduler.Stop()
}

func (v *liveVectorServing) Close() error {
	v.cancel()
	v.mu.Lock()
	done := v.done
	v.mu.Unlock()
	if done != nil {
		<-done
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stopBuilders()
	var err error
	if v.serving.Close != nil {
		err = v.serving.Close()
	}
	return errors.Join(err, v.push.Close())
}

func (v *liveVectorServing) Notify(recall bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if recall {
		if v.serving.RecallMutationNotify != nil {
			v.serving.RecallMutationNotify()
		}
	} else if v.serving.Scheduler != nil && v.cfg.Vector.Embed.RunAfterSyncEnabled() {
		v.serving.Scheduler.Notify()
	}
}

func (v *liveVectorServing) Manager(store string) server.EmbeddingsManager {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.serving.manager == nil {
		return nil
	}
	if store == "" || store == vector.MessageIndexSpec().Name {
		return v.serving.manager
	}
	if store == vector.RecallIndexSpec().Name {
		return v.serving.recallManager
	}
	return nil
}

func (v *liveVectorServing) ServerOptions() []server.Option {
	v.mu.Lock()
	defer v.mu.Unlock()
	options := []server.Option{
		server.WithEmbeddingsRuntime(v),
		server.WithVectorPushSource(v.push),
		server.WithSessionMutationNotifier(func() { v.Notify(false); v.Notify(true) }),
		server.WithRecallCorpusMutationNotifier(func() { v.Notify(true) }),
	}
	if v.serving.manager == nil {
		options = append(options, v.serving.ServerOpts...)
	}
	return options
}

func (v *liveVectorServing) IncludeAutomated() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.cfg.Vector.IncludeAutomated
}

func (v *liveVectorServing) SetupStatus(ctx context.Context) (server.EmbeddingsSetupStatus, error) {
	v.mu.Lock()
	status, cfg, missingManager := v.setup, v.cfg, v.serving.manager == nil
	v.mu.Unlock()
	if status.Busy {
		status.Enabled = cfg.Vector.Enabled
		return status, nil
	}
	if missingManager && cfg.DeploymentEmbeddings == nil {
		var err error
		cfg.Vector.Embeddings, err = cfg.VectorSetupEmbeddings(false)
		if err != nil {
			return status, setupRequestError(err)
		}
	}
	return setupStatusForConfig(ctx, status, cfg, missingManager)
}

func setupStatusForConfig(ctx context.Context, status server.EmbeddingsSetupStatus, cfg config.Config, missingManager bool) (server.EmbeddingsSetupStatus, error) {
	status.Action = ""
	if (missingManager || status.Error != "") && cfg.DeploymentEmbeddings == nil {
		status.Action = "turn-on"
	}
	status, err := embeddingsSetupStatus(ctx, status, cfg)
	if status.Action == "" {
		status.Error = ""
	}
	return status, err
}

func embeddingsSetupStatus(ctx context.Context, status server.EmbeddingsSetupStatus, cfg config.Config) (server.EmbeddingsSetupStatus, error) {
	status.Enabled = cfg.Vector.Enabled
	if cfg.DeploymentEmbeddings != nil {
		status.Action = ""
		status.Provider = "configured"
		return status, nil
	}
	if status.Action == "" && !status.Enabled {
		status.Action = "turn-on"
	}
	_, deployment, err := cfg.Vector.Embeddings.Server("")
	if err != nil && cfg.Vector.Embeddings.Model != "" {
		return status, err
	}
	status.Provider = "built-in"
	if deployment.Endpoint != "" {
		status.Provider = "configured"
		if u, err := url.Parse(deployment.Endpoint); (err == nil && u.Port() == "11434") || deployment.OllamaCPUFallback {
			status.Provider = "ollama"
		}
		return status, nil
	}
	inspected, err := inspectLocalEmbeddings(ctx, embedlocal.Options{SkipOllama: deployment.Local})
	if err != nil {
		return status, err
	}
	status.MissingBytes = max(status.MissingBytes, inspected.DownloadBytes)
	if inspected.OllamaAvailable {
		status.Provider = "ollama"
	}
	status.Installing = inspected.Installing && status.Provider != "ollama"
	return status, nil
}

// setupRequestError reports rejected candidate settings as a bad request and
// leaves config read failures internal.
func setupRequestError(err error) error {
	if errors.Is(err, config.ErrInvalidVectorSetup) {
		return fmt.Errorf("%w: %w", vector.ErrInvalidBuildRequest, err)
	}
	return err
}

func (v *liveVectorServing) Setup(approvedBytes int64, expectedProvider string) error {
	v.mu.Lock()
	if v.setup.Busy {
		v.mu.Unlock()
		return vector.ErrBuildRunning
	}
	cfg, setup, missingManager, done := v.cfg, v.setup, v.serving.manager == nil, v.done
	v.mu.Unlock()
	var err error
	if missingManager {
		cfg.Vector.Embeddings, err = cfg.VectorSetupEmbeddings(false)
		if err != nil {
			return setupRequestError(err)
		}
	}
	status, err := setupStatusForConfig(v.ctx, setup, cfg, missingManager)
	if err != nil {
		return err
	}
	_, deployment, err := cfg.Vector.Embeddings.Server("")
	if err != nil {
		return setupRequestError(err)
	}
	if status.Action != "turn-on" {
		return fmt.Errorf("%w: setup action is unavailable for the current embeddings provider", vector.ErrInvalidBuildRequest)
	}
	if status.Provider != expectedProvider {
		return server.ErrEmbeddingsProviderChanged
	}
	if status.Provider == "built-in" && deployment.Endpoint == "" {
		deployment.Local = true
	}
	var pinnedEndpoint string
	if expectedProvider == "ollama" && deployment.Endpoint == "" {
		pinnedEndpoint, err = localEmbeddingEndpoint(v.ctx, embedlocal.Options{})
		if err != nil || pinnedEndpoint != ollamaEmbeddingEndpoint {
			return server.ErrEmbeddingsProviderChanged
		}
		deployment.Endpoint = pinnedEndpoint
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.setup.Busy {
		return vector.ErrBuildRunning
	}
	// A setup may have finished and replaced the configuration during the probe.
	if v.done != done {
		return server.ErrEmbeddingsProviderChanged
	}
	return v.startSetup(deployment, approvedBytes, false, pinnedEndpoint)
}

// startSetup starts the leased worker with v.mu held.
func (v *liveVectorServing) startSetup(deployment config.VectorEmbeddingsServerConfig, approvedBytes int64, automatic bool, pinnedEndpoint string) error {
	release, ok := v.idle.BeginWork()
	if !ok {
		return errors.New("server is shutting down")
	}
	v.setup = server.EmbeddingsSetupStatus{Busy: true, Action: "turn-on"}
	v.done = make(chan struct{})
	go func() {
		defer release()
		err := v.activate(deployment, approvedBytes, automatic, pinnedEndpoint)
		v.mu.Lock()
		v.setup.Busy = false
		if err != nil {
			v.setup.Error = err.Error()
		} else {
			v.setup.Action = ""
		}
		manager := v.serving.manager
		close(v.done)
		v.mu.Unlock()
		if err == nil && manager != nil {
			manager.Wait()
		}
	}()
	return nil
}

func (v *liveVectorServing) activate(deployment config.VectorEmbeddingsServerConfig, approvedBytes int64, automatic bool, pinnedEndpoint string) error {
	cfg := v.cfg
	builtin := deployment.Local && deployment.Endpoint == ""
	_, err := resolveLocalEmbeddings(v.ctx, deployment,
		func(_ context.Context, size int64) (bool, error) {
			if size > approvedBytes {
				v.mu.Lock()
				v.setup.MissingBytes = size
				v.mu.Unlock()
				return false, nil
			}
			return true, nil
		},
		func(done, total int64) {
			v.mu.Lock()
			v.setup.DownloadedBytes, v.setup.TotalBytes = done, total
			v.mu.Unlock()
		},
	)
	if err != nil {
		return err
	}
	if v.serving.manager == nil {
		if err := cfg.SaveVectorSetup(builtin, automatic, pinnedEndpoint); errors.Is(err, config.ErrVectorSetupIneligible) {
			return nil
		} else if err != nil {
			return err
		}
		cfg, err = v.reload()
		if err != nil {
			return err
		}
		// Pin this run while leaving the saved configuration open to discovery.
		servingCfg := cfg
		if pinnedEndpoint != "" {
			name, server, err := cfg.Vector.Embeddings.Server("")
			if err != nil {
				return err
			}
			server.Endpoint = pinnedEndpoint
			servingCfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{}
			maps.Copy(servingCfg.Vector.Embeddings.Servers, cfg.Vector.Embeddings.Servers)
			servingCfg.Vector.Embeddings.Servers[name] = server
		}
		// Opening can wait on another process's write lock; status stays available.
		serving, err := setupVectorServing(v.ctx, servingCfg, v.database, v.idle)
		if err != nil {
			return err
		}
		v.mu.Lock()
		v.serving, v.cfg = serving, cfg
		v.push.reconfigure(cfg)
		if serving.manager == nil {
			v.mu.Unlock()
			return errors.New("Another process may be using search. Close it and retry.") //nolint:staticcheck // ST1005: This recovery message appears directly in the UI.
		}
		v.startSchedulers()
		v.mu.Unlock()
	}
	if automatic {
		v.serving.Scheduler.Notify()
		return nil
	}
	err = v.serving.manager.StartBuild(vector.BuildRequest{IncludeAutomated: cfg.Vector.IncludeAutomated})
	if errors.Is(err, vector.ErrBuildRunning) {
		err = nil
	}
	if err == nil && builtin && !automatic {
		if err := embedlocal.RecordChoice(); err != nil {
			return fmt.Errorf("record shared choice: %w", err)
		}
	}
	return err
}
