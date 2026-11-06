package main

import (
	"context"

	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/vector"
)

// admittedManager runs builds and generation calls under the owner's mutex,
// the one Setup checks for a running build under and closes the indexes
// under, so a manager looked up before Setup cannot start a build once Setup
// begins or reach an index Setup has closed; Status skips the mutex.
type admittedManager struct {
	*vector.Manager
	owner *liveVectorServing
}

// admit wraps m for the embeddings routes and keeps a missing index nil.
func (v *liveVectorServing) admit(m *vector.Manager) server.EmbeddingsManager {
	if m == nil {
		return nil
	}
	return admittedManager{Manager: m, owner: v}
}

// admitted runs call unless setup is replacing or has already replaced the
// manager.
func (a admittedManager) admitted(call func() error) error {
	a.owner.mu.Lock()
	defer a.owner.mu.Unlock()
	if a.owner.setup.Busy || (a.Manager != a.owner.serving.manager && a.Manager != a.owner.serving.recallManager) {
		return server.ErrEmbeddingsSetupBusy
	}
	return call()
}

func (a admittedManager) StartBuild(req vector.BuildRequest) error {
	return a.admitted(func() error { return a.Manager.StartBuild(req) })
}

// Wait runs after the build route starts a build; once that build activates an
// index, the setup failure it recovered from is cleared so status recomputes
// the action from the installed runtime.
func (a admittedManager) Wait() {
	a.Manager.Wait()
	if result := a.Status().LastResult; result != nil && result.Activated {
		_ = a.admitted(func() error { a.owner.setup = server.EmbeddingsSetupStatus{}; return nil })
	}
}

func (a admittedManager) Generations(ctx context.Context) ([]vector.GenerationInfo, error) {
	var gens []vector.GenerationInfo
	err := a.admitted(func() (err error) {
		gens, err = a.Manager.Generations(ctx)
		return err
	})
	return gens, err
}

func (a admittedManager) Activate(ctx context.Context, id int64, force bool) error {
	return a.admitted(func() error { return a.Manager.Activate(ctx, id, force) })
}

func (a admittedManager) Retire(ctx context.Context, id int64, force bool) error {
	return a.admitted(func() error { return a.Manager.Retire(ctx, id, force) })
}
