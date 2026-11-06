package main

import (
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/vector"
)

// admittedManager starts builds under the owner's mutex, the one Setup checks
// for a running build under, so a build looked up before Setup cannot start
// once Setup begins; Wait and the other calls skip the mutex.
type admittedManager struct {
	*vector.Manager
	owner *liveVectorServing
}

// admit wraps m for the build routes and keeps a missing index nil.
func (v *liveVectorServing) admit(m *vector.Manager) server.EmbeddingsManager {
	if m == nil {
		return nil
	}
	return admittedManager{Manager: m, owner: v}
}

// StartBuild refuses a manager setup is replacing or has already replaced.
func (a admittedManager) StartBuild(req vector.BuildRequest) error {
	a.owner.mu.Lock()
	defer a.owner.mu.Unlock()
	if a.owner.setup.Busy || (a.Manager != a.owner.serving.manager && a.Manager != a.owner.serving.recallManager) {
		return server.ErrEmbeddingsSetupBusy
	}
	return a.Manager.StartBuild(req)
}
