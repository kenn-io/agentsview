package sync

import (
	"context"
	"log"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

const firstImportWaveSize = 1

type firstImportWave struct {
	limit    int
	selected map[firstImportSource]struct{}
	partial  bool
	onWrite  func()
}

type firstImportContextKey struct{}

func firstImportFromContext(ctx context.Context) *firstImportWave {
	wave, _ := ctx.Value(firstImportContextKey{}).(*firstImportWave)
	return wave
}

func (e *Engine) syncNewest(ctx context.Context, n int, onProgress ProgressFunc, onWrite func()) (stats SyncStats, complete bool) {
	if e.refuseWriteInForceParse("SyncNewest") {
		return stats, false
	}
	wave := &firstImportWave{limit: n, onWrite: onWrite}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	ctx = context.WithValue(ctx, firstImportContextKey{}, wave)
	stats = e.syncAllLocked(ctx, onProgress, time.Time{}, nil, syncWriteFirstImport, false, false)
	return stats, !wave.partial
}

// RunFirstImport publishes recent sessions before completing startup reconciliation.
func (e *Engine) RunFirstImport(ctx context.Context, onProgress ProgressFunc) (SyncStats, bool, error) {
	if e.StartupReconciled() {
		return SyncStats{}, false, nil
	}
	refresh := make(chan struct{}, 1)
	done := make(chan struct{})
	go e.emitFirstImportRefreshes(ctx, refresh, done)
	defer func() { close(refresh); <-done }()
	onWrite := func() {
		select {
		case refresh <- struct{}{}:
		default:
		}
	}
	stats, _ := e.syncNewest(ctx, firstImportWaveSize, onProgress, onWrite)
	if ctx.Err() != nil {
		return stats, true, ctx.Err()
	}
	ctx = context.WithValue(ctx, firstImportContextKey{}, &firstImportWave{onWrite: onWrite})
	return e.RunStartupSyncFallback(ctx, onProgress)
}

func (e *Engine) emitFirstImportRefreshes(ctx context.Context, refresh <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	var lastRefresh time.Time
	for range refresh {
		if delay := time.Until(lastRefresh.Add(time.Second)); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		e.emit("sessions")
		lastRefresh = time.Now()
	}
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
		mtime := discoveredFileOrderingMtime(ctx, file)
		candidates = append(candidates, firstImportCandidate{firstImportSource{file.Agent, file.Path}, max(mtime, 0)})
	}
	for _, agent := range []parser.AgentType{parser.AgentWarp, parser.AgentForge, parser.AgentPiebald, parser.AgentGoose, parser.AgentCrush} {
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
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("sync %s first import discovery: %v", agent, err)
		}
	}
	wave.partial = len(candidates) > wave.limit
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
	wave.selected = make(map[firstImportSource]struct{}, min(len(candidates), wave.limit))
	for _, candidate := range candidates {
		if len(wave.selected) >= wave.limit {
			break
		}
		wave.selected[candidate.source] = struct{}{}
	}
	return nil
}
