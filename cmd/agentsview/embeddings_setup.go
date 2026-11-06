package main

import (
	"context"
	"errors"
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

// liveVectorServing owns activation and replacement over the daemon's open indexes.
type liveVectorServing struct {
	mu          sync.Mutex
	serving     vectorServing
	cfg         config.Config
	database    *db.DB
	ctx         context.Context
	idle        *server.IdleTracker
	cancel      context.CancelFunc
	schedCancel context.CancelFunc
	done        chan struct{}
	setup       server.EmbeddingsSetupStatus
	reload      func() (config.Config, error)
	push        *vectorPushSource
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
	schedulerCtx, cancel := context.WithCancel(v.ctx)
	v.schedCancel = cancel
	v.serving.Scheduler.onBattery = onBattery
	v.serving.RecallScheduler.onBattery = onBattery
	go v.serving.Scheduler.Run(schedulerCtx)
	go v.serving.RecallScheduler.Run(schedulerCtx)
}

func (v *liveVectorServing) stopBuilders() {
	if v.serving.Scheduler == nil {
		return
	}
	v.schedCancel()
	v.serving.manager.Shutdown()
	v.serving.recallManager.Shutdown()
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
	closeVectorPushSource(v.push)
	if v.serving.Close != nil {
		return v.serving.Close()
	}
	return nil
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
	status, cfg := v.setup, v.cfg
	running := (v.serving.manager != nil && v.serving.manager.Status().Running) ||
		(v.serving.recallManager != nil && v.serving.recallManager.Status().Running)
	v.mu.Unlock()
	if status.Busy || running {
		status.Enabled = cfg.Vector.Enabled
		return status, nil
	}
	return embeddingsSetupStatus(ctx, status, cfg)
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
	status.RebuildRequired = cfg.Vector.Embeddings.Model != "" && !cfg.Vector.Embeddings.IsNomic()
	status.Provider = "built-in"
	if deployment.Endpoint != "" {
		status.Provider = "configured"
		if u, err := url.Parse(deployment.Endpoint); (err == nil && u.Port() == "11434") || deployment.OllamaCPUFallback {
			status.Provider = "ollama"
		}
		if status.Enabled && status.Provider == "ollama" && status.Action == "" {
			status.Action = "use-built-in"
		}
		if status.Provider != "ollama" {
			return status, nil
		}
	}
	inspected, err := inspectLocalEmbeddings(ctx, embedlocal.Options{SkipOllama: deployment.Local || deployment.Endpoint != ""})
	if err != nil {
		return status, err
	}
	status.MissingBytes, status.Installing = inspected.DownloadBytes, inspected.Installing
	if inspected.OllamaAvailable {
		status.Provider = "ollama"
		if status.Enabled && status.Action == "" {
			status.Action = "use-built-in"
		}
	} else if inspected.DownloadBytes > 0 && status.Action == "" {
		status.Action = "turn-on"
	}
	return status, nil
}

func (v *liveVectorServing) Setup(action string) error {
	status, err := v.SetupStatus(v.ctx)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.setup.Busy {
		return vector.ErrBuildRunning
	}
	if (v.serving.manager != nil && v.serving.manager.Status().Running) ||
		(v.serving.recallManager != nil && v.serving.recallManager.Status().Running) {
		return vector.ErrBuildRunning
	}
	if action != status.Action {
		return errors.New("setup action is unavailable for the current embeddings provider")
	}
	release, ok := v.idle.BeginWork()
	if !ok {
		return errors.New("server is shutting down")
	}
	v.setup = server.EmbeddingsSetupStatus{Busy: true, Action: action}
	v.done = make(chan struct{})
	go func() {
		defer release()
		err := v.activate(action)
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

// closeServing releases the current indexes and records cfg; setup.Busy keeps
// Setup out and Close waits on done until activate installs the replacement.
func (v *liveVectorServing) closeServing(cfg config.Config) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stopBuilders()
	if v.serving.Close != nil {
		if err := v.serving.Close(); err != nil {
			return err
		}
	}
	v.serving, v.cfg = vectorServing{}, cfg
	v.database.SetVectorSearcher(nil)
	v.database.SetRecallVectorSearcher(nil)
	return nil
}

func (v *liveVectorServing) activate(action string) error {
	cfg := v.cfg
	_, deployment, err := cfg.Vector.Embeddings.Server("")
	if err != nil && cfg.Vector.Embeddings.Model != "" {
		return err
	}
	if action == "use-built-in" {
		deployment.Endpoint, deployment.Local, deployment.OllamaCPUFallback = "", true, false
	}
	_, err = resolveLocalEmbeddings(v.ctx, deployment,
		func(context.Context, int64) (bool, error) { return true, nil },
		func(done, total int64) {
			v.mu.Lock()
			v.setup.DownloadedBytes, v.setup.TotalBytes = done, total
			v.mu.Unlock()
		},
	)
	if err != nil {
		return err
	}
	if err := cfg.SaveVectorSetup(action == "use-built-in"); err != nil {
		return err
	}
	cfg, err = v.reload()
	if err != nil {
		return err
	}
	if err := v.closeServing(cfg); err != nil {
		return err
	}
	// Opening can wait on another process's write lock; v.mu stays free so status answers.
	serving, err := setupVectorServing(v.ctx, cfg, v.database, v.idle)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.serving = serving
	if serving.manager == nil {
		return errors.New("Another process may be using search. Close it and retry.") //nolint:staticcheck // ST1005: This recovery message appears directly in the UI.
	}
	v.startSchedulers()
	v.push.mu.Lock()
	v.push.cfg = cfg
	v.push.mu.Unlock()
	return serving.manager.StartBuild(vector.BuildRequest{IncludeAutomated: cfg.Vector.IncludeAutomated})
}
