package sync

import (
	"context"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const firstImportWaveSize = 100

type firstImportWave struct {
	limit    int
	selected map[firstImportSource]int
	partial  bool
	onWrite  func()
}

// SyncNewest imports a bounded first wave without recording a completed sync.
func (e *Engine) SyncNewest(ctx context.Context, n int, onProgress ProgressFunc) (stats SyncStats, complete bool) {
	return e.syncNewest(ctx, n, onProgress, nil)
}

func (e *Engine) syncNewest(ctx context.Context, n int, onProgress ProgressFunc, onWrite func()) (stats SyncStats, complete bool) {
	if e.refuseWriteInForceParse("SyncNewest") {
		return stats, false
	}
	wave := &firstImportWave{limit: n, onWrite: onWrite}
	e.syncMu.Lock()
	defer func() { e.notifyFirstImport(stats, onWrite) }()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	stats = e.syncAllLocked(ctx, onProgress, time.Time{}, nil, syncWriteFirstImport, false, false, wave)
	return stats, !wave.partial
}

func (e *Engine) notifyFirstImport(stats SyncStats, onWrite func()) {
	if stats.hasSessionChanges() {
		if onWrite != nil {
			onWrite()
		} else {
			e.emit("sessions")
		}
	}
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
	stats, ran, err := e.runStartupSyncFallback(ctx, onProgress, &firstImportWave{onWrite: onWrite})
	if ran && err == nil && ctx.Err() == nil && stats.AuthoritativeDiscoveryComplete() {
		err = e.db.SetSyncState(ctx, db.FirstImportCompletedKey, "1")
	}
	return stats, ran, err
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
		mtime, ok := parseDiffDiscoveryMtime(file)
		if !ok {
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
	wave.selected = make(map[firstImportSource]int, min(len(candidates), wave.limit))
	for _, candidate := range candidates {
		if len(wave.selected) >= wave.limit {
			break
		}
		if _, exists := wave.selected[candidate.source]; !exists {
			wave.selected[candidate.source] = len(wave.selected)
		}
	}
	return nil
}

func firstImportFiles(files []parser.DiscoveredFile, wave *firstImportWave) []parser.DiscoveredFile {
	if wave != nil && wave.limit > 0 {
		slices.SortStableFunc(files, func(a, b parser.DiscoveredFile) int {
			return wave.selected[firstImportSource{a.Agent, a.Path}] - wave.selected[firstImportSource{b.Agent, b.Path}]
		})
	}
	return files
}

func (wave *firstImportWave) earlyDBAgent() parser.AgentType {
	if wave == nil || wave.limit == 0 {
		return ""
	}
	for source, rank := range wave.selected {
		if rank == 0 {
			switch source.agent {
			case parser.AgentWarp, parser.AgentForge, parser.AgentPiebald, parser.AgentZCode, parser.AgentGoose, parser.AgentCrush:
				return source.agent
			}
			break
		}
	}
	return ""
}

func (wave *firstImportWave) runDBPhase(agent parser.AgentType) bool {
	return wave.earlyDBAgent() != agent
}

func (e *Engine) collectFirstImport(ctx context.Context, files []parser.DiscoveredFile, total int, onProgress ProgressFunc, mode syncWriteMode, scope *rootSyncScope, wave *firstImportWave) SyncStats {
	var early SyncStats
	if agent := wave.earlyDBAgent(); agent != "" {
		early.Aborted = e.syncProviderDBBackedAgent(ctx, agent, string(agent), mode, onProgress == nil, scope, &early, func(_ int, messages int) {
			early.messagesIndexed += messages
		}, wave)
	}
	results := e.startFirstImportWorkers(ctx, firstImportFiles(files, wave), wave)
	stats := e.collectAndBatchWithOptions(ctx, results, len(files), total, onProgress, mode, firstImportBatchOptions(wave))
	stats.Merge(early)
	stats.providerFailures += early.providerFailures
	return stats
}

func firstImportDiscoverEach(ctx context.Context, discoverer parser.StreamingDiscoverer, agent parser.AgentType, wave *firstImportWave, yield func(parser.SourceRef) error) error {
	if wave == nil || wave.limit == 0 {
		return discoverer.DiscoverEach(ctx, yield)
	}
	selected := make([]parser.SourceRef, 0, wave.limit)
	err := discoverer.DiscoverEach(ctx, func(source parser.SourceRef) error {
		if _, ok := wave.selected[firstImportSource{agent, providerDiscoveredPath(source)}]; !ok {
			return yield(source)
		}
		selected = append(selected, source)
		return nil
	})
	if err != nil {
		return err
	}
	slices.SortStableFunc(selected, func(a, b parser.SourceRef) int {
		return wave.selected[firstImportSource{agent, providerDiscoveredPath(a)}] - wave.selected[firstImportSource{agent, providerDiscoveredPath(b)}]
	})
	for _, source := range selected {
		if err := yield(source); err != nil {
			return err
		}
	}
	return nil
}
