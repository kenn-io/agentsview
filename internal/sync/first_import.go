package sync

import (
	"context"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

var firstImportWaves = []int{25, 250, 2500}

type firstImportWave struct {
	limit   int
	cutoff  int64
	partial bool
}

// SyncNewest imports a bounded first wave without recording a completed sync.
func (e *Engine) SyncNewest(ctx context.Context, n int, onProgress ProgressFunc) (stats SyncStats, complete bool) {
	if e.refuseWriteInForceParse("SyncNewest") {
		return stats, false
	}
	wave := &firstImportWave{limit: n}
	e.syncMu.Lock()
	defer func() {
		if stats.hasSessionChanges() {
			e.emit("sessions")
		}
	}()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	stats = e.syncAllLocked(ctx, onProgress, time.Time{}, nil, syncWriteDefault, false, false, wave)
	return stats, !wave.partial
}

// RunFirstImport publishes recent sessions before completing startup reconciliation.
func (e *Engine) RunFirstImport(ctx context.Context, onProgress ProgressFunc) (SyncStats, bool, error) {
	for _, n := range firstImportWaves {
		if e.StartupReconciled() {
			return SyncStats{}, false, nil
		}
		stats, complete := e.SyncNewest(ctx, n, onProgress)
		if ctx.Err() != nil {
			return stats, true, ctx.Err()
		}
		if stats.Aborted || complete {
			break
		}
	}
	return e.RunStartupSyncFallback(ctx, onProgress)
}

func (e *Engine) firstImportCutoff(ctx context.Context, files []parser.DiscoveredFile, scope *rootSyncScope, wave *firstImportWave) error {
	times := make([]int64, 0, len(files))
	for _, file := range files {
		mtime, err := e.discoveredFileEffectiveMtime(ctx, file)
		if err != nil {
			mtime = 0
		}
		times = append(times, mtime)
	}
	for _, agent := range []parser.AgentType{parser.AgentWarp, parser.AgentForge, parser.AgentPiebald, parser.AgentZCode, parser.AgentGoose, parser.AgentCrush} {
		var roots []string
		for _, root := range e.sources().agentDirs[agent] {
			if root != "" && scope.includes(root) {
				roots = append(roots, root)
			}
		}
		factory := e.sources().providerFactories[agent]
		if len(roots) == 0 || factory == nil {
			continue
		}
		provider := factory.NewProvider(parser.ProviderConfig{Roots: roots, Machine: e.machine, SourceMachines: e.sources().sourceMachines[agent]})
		discoverer, ok := provider.(parser.StreamingDiscoverer)
		if !ok || provider.Capabilities().Source.StreamingDiscovery != parser.CapabilitySupported {
			continue
		}
		if err := discoverer.DiscoverEach(ctx, func(source parser.SourceRef) error {
			times = append(times, source.DiscoveryMTimeNS)
			return ctx.Err()
		}); err != nil {
			return err
		}
	}
	if len(times) <= wave.limit {
		return nil
	}
	wave.partial = true
	slices.Sort(times)
	wave.cutoff = max(times[len(times)-wave.limit], 1)
	return nil
}
