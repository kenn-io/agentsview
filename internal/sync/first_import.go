package sync

import (
	"context"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

const firstImportWaveSize = 100

type firstImportWave struct {
	limit    int
	selected map[firstImportSource]struct{}
	partial  bool
	onWrite  func()
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
	stats = e.syncAllLocked(ctx, onProgress, time.Time{}, nil, syncWriteFirstImport, false, false, wave)
	return stats, !wave.partial
}

// RunFirstImport publishes recent sessions before completing startup reconciliation.
func (e *Engine) RunFirstImport(ctx context.Context, onProgress ProgressFunc) (SyncStats, bool, error) {
	if e.StartupReconciled() {
		return SyncStats{}, false, nil
	}
	stats, _ := e.SyncNewest(ctx, firstImportWaveSize, onProgress)
	if ctx.Err() != nil {
		return stats, true, ctx.Err()
	}

	refresh := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var lastRefresh time.Time
		for range refresh {
			// Leave time for clients to refresh between notifications.
			if delay := time.Until(lastRefresh.Add(time.Second)); delay > 0 {
				time.Sleep(delay)
			}
			e.emit("sessions")
			lastRefresh = time.Now()
		}
	}()
	stats, ran, err := e.runStartupSyncFallback(ctx, onProgress, &firstImportWave{onWrite: func() {
		select {
		case refresh <- struct{}{}:
		default:
		}
	}})
	close(refresh)
	<-done
	return stats, ran, err
}

type firstImportSource struct {
	agent parser.AgentType
	path  string
}

type firstImportCandidate struct {
	source firstImportSource
	mtime  int64
}

func (e *Engine) firstImportSelect(ctx context.Context, files []parser.DiscoveredFile, scope *rootSyncScope, wave *firstImportWave) error {
	candidates := make([]firstImportCandidate, 0, len(files))
	for _, file := range files {
		var mtime int64
		if file.ProviderSource != nil {
			mtime = file.ProviderSource.DiscoveryMTimeNS
		}
		if mtime == 0 {
			mtime, _ = discoveredFileMtime(ctx, file)
		}
		candidates = append(candidates, firstImportCandidate{firstImportSource{file.Agent, file.Path}, max(mtime, 0)})
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
			candidates = append(candidates, firstImportCandidate{firstImportSource{agent, providerDiscoveredPath(source)}, max(source.DiscoveryMTimeNS, 0)})
			return ctx.Err()
		}); err != nil {
			return err
		}
	}
	if len(candidates) <= wave.limit {
		return nil
	}
	wave.partial = true
	slices.SortFunc(candidates, func(a, b firstImportCandidate) int {
		if a.mtime != b.mtime {
			if a.mtime > b.mtime {
				return -1
			}
			return 1
		}
		if c := strings.Compare(string(a.source.agent), string(b.source.agent)); c != 0 {
			return c
		}
		return strings.Compare(a.source.path, b.source.path)
	})
	wave.selected = make(map[firstImportSource]struct{}, wave.limit)
	for _, candidate := range candidates {
		if len(wave.selected) >= wave.limit {
			break
		}
		wave.selected[candidate.source] = struct{}{}
	}
	return nil
}
