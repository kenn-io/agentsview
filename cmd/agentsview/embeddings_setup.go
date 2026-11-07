package main

import (
	"context"
	"errors"
	"fmt"
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
	return v, nil
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
	status.Action = ""
	if missingManager && cfg.DeploymentEmbeddings == nil {
		status.Action = "turn-on"
		var err error
		cfg.Vector.Embeddings, err = cfg.VectorSetupEmbeddings()
		if err != nil {
			return status, setupRequestError(err)
		}
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

func (v *liveVectorServing) Setup(approvedBytes int64) error {
	status, err := v.SetupStatus(v.ctx)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.setup.Busy {
		return vector.ErrBuildRunning
	}
	embeddings := v.cfg.Vector.Embeddings
	if v.serving.manager == nil {
		embeddings, err = v.cfg.VectorSetupEmbeddings()
		if err != nil {
			return setupRequestError(err)
		}
	}
	_, deployment, err := embeddings.Server("")
	if err != nil {
		return setupRequestError(err)
	}
	if status.Action != "turn-on" {
		return fmt.Errorf("%w: setup action is unavailable for the current embeddings provider", vector.ErrInvalidBuildRequest)
	}
	release, ok := v.idle.BeginWork()
	if !ok {
		return errors.New("server is shutting down")
	}
	v.setup = server.EmbeddingsSetupStatus{Busy: true, Action: "turn-on"}
	v.done = make(chan struct{})
	go func() {
		defer release()
		err := v.activate(deployment, approvedBytes)
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

func (v *liveVectorServing) activate(deployment config.VectorEmbeddingsServerConfig, approvedBytes int64) error {
	cfg := v.cfg
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
		if err := cfg.SaveVectorSetup(); err != nil {
			return err
		}
		cfg, err = v.reload()
		if err != nil {
			return err
		}
		// Opening can wait on another process's write lock; status stays available.
		serving, err := setupVectorServing(v.ctx, cfg, v.database, v.idle)
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
	err = v.serving.manager.StartBuild(vector.BuildRequest{IncludeAutomated: cfg.Vector.IncludeAutomated})
	if errors.Is(err, vector.ErrBuildRunning) {
		return nil
	}
	return err
}
