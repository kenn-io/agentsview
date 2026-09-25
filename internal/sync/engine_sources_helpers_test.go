package sync

// withTestSources installs a source snapshot on a hand-built test engine.
func withTestSources(e *Engine, sources *engineSources) *Engine {
	e.sourceSet.Store(sources)
	return e
}
