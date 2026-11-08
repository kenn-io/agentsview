package sync

import (
	"context"
	"fmt"
	"log"
	"os"
	gosync "sync"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

const syncOperationHeartbeatInterval = 30 * time.Second

var syncOperationSequence atomic.Uint64

type syncOperationKind string

type syncOperationScope struct {
	kind       syncOperationKind
	agent      parser.AgentType
	roots      int
	paths      int
	lostEvents bool
	full       bool
}

type syncOperationDiagnostics struct {
	active atomic.Pointer[syncOperation]
}

type syncOperationCounts struct {
	updated   int
	unchanged int
	errors    int
	deferred  int
}

type syncOperationProgress struct {
	syncOperationCounts
	phase        string
	pass         int
	pages        int
	sourcesDone  int
	sourcesTotal int
	totalKnown   bool
	pageOffset   int
}

type syncOperation struct {
	owner   *syncOperationDiagnostics
	id      uint64
	scope   syncOperationScope
	started time.Time
	waiting time.Time
	done    chan struct{}
	stopped chan struct{}

	mu         gosync.Mutex
	acquired   time.Time
	progress   syncOperationProgress
	beforePass syncOperationCounts
}

func (e *Engine) beginSyncOperation(ctx context.Context, scope syncOperationScope) *syncOperation {
	op := &syncOperation{
		owner: &e.operationDiagnostics,
		id:    syncOperationSequence.Add(1), scope: scope, started: time.Now(),
		done: make(chan struct{}), stopped: make(chan struct{}),
		progress: syncOperationProgress{phase: "waiting"},
	}
	op.writeLog(ctx, "started", nil)
	op.waiting = time.Now()
	go op.run(ctx)
	return op
}

func (op *syncOperation) run(ctx context.Context) {
	defer close(op.stopped)
	ticker := time.NewTicker(syncOperationHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-op.done:
			return
		case <-ticker.C:
			op.writeLog(ctx, "heartbeat", nil)
		}
	}
}

func (op *syncOperation) acquiredLock() {
	op.mu.Lock()
	op.acquired = time.Now()
	op.progress.phase = "processing"
	op.mu.Unlock()
	op.owner.active.Store(op)
}

func (op *syncOperation) finish(ctx context.Context, err error) {
	// Stop the observer before releasing syncMu so it cannot report a later
	// operation's work or emit a heartbeat after this operation's final record.
	close(op.done)
	<-op.stopped
	op.owner.active.CompareAndSwap(op, nil)
	op.writeLog(ctx, "finished", err)
}

func (op *syncOperation) setPhase(phase string) {
	if op == nil {
		return
	}
	op.mu.Lock()
	op.progress.phase = phase
	op.mu.Unlock()
}

func (op *syncOperation) beginReconciliation() {
	if op == nil {
		return
	}
	op.mu.Lock()
	op.progress.pass++
	op.beforePass = op.progress.syncOperationCounts
	op.progress.totalKnown = false
	op.progress.phase = "discovery"
	op.mu.Unlock()
}

func (op *syncOperation) discovered(total int, known bool) {
	if op == nil {
		return
	}
	op.mu.Lock()
	op.progress.sourcesTotal += total
	op.progress.totalKnown = known
	op.mu.Unlock()
}

func (op *syncOperation) beginPage() {
	if op == nil {
		return
	}
	op.mu.Lock()
	op.progress.pages++
	op.progress.pageOffset = op.progress.sourcesDone
	op.progress.phase = "processing"
	op.mu.Unlock()
}

func (op *syncOperation) pageProgress(p Progress) {
	if op == nil || p.Phase != PhaseSyncing {
		return
	}
	op.mu.Lock()
	op.progress.sourcesDone = max(op.progress.sourcesDone, op.progress.pageOffset+p.SessionsDone)
	op.mu.Unlock()
}

func (op *syncOperation) recordStats(stats SyncStats) {
	if op == nil {
		return
	}
	op.mu.Lock()
	op.progress.updated += stats.Synced
	op.progress.unchanged += stats.Skipped
	op.progress.errors += stats.Failed
	op.progress.deferred += stats.Deferred
	op.mu.Unlock()
}

func (op *syncOperation) reconciled(stats SyncStats) {
	if op == nil {
		return
	}
	op.mu.Lock()
	op.progress.syncOperationCounts = syncOperationCounts{
		updated:   op.beforePass.updated + stats.Synced,
		unchanged: op.beforePass.unchanged + stats.Skipped,
		errors:    op.beforePass.errors + stats.Failed + stats.providerFailures,
		deferred:  op.beforePass.deferred + stats.Deferred,
	}
	op.mu.Unlock()
}

func (op *syncOperation) writeLog(ctx context.Context, state string, err error) {
	op.mu.Lock()
	progress, acquired := op.progress, op.acquired
	op.mu.Unlock()
	now := time.Now()
	agent := string(op.scope.agent)
	if agent == "" {
		agent = "all"
	}
	message := fmt.Sprintf(
		"sync operation: pid=%d operation=%d kind=%s agent=%s roots=%d paths=%d lost_events=%t full=%t state=%s phase=%s elapsed=%s",
		os.Getpid(), op.id, op.scope.kind, agent, op.scope.roots, op.scope.paths,
		op.scope.lostEvents, op.scope.full, state, progress.phase,
		now.Sub(op.started).Round(time.Millisecond),
	)
	if state == "started" {
		log.Print(message)
		return
	}
	waiting, running := now.Sub(op.waiting), time.Duration(0)
	if !acquired.IsZero() {
		waiting, running = acquired.Sub(op.waiting), now.Sub(acquired)
	}
	message += fmt.Sprintf(
		" lock_wait=%s running=%s canceled=%t error=%t pass=%d pages=%d sources_done=%d sources_total=%d total_known=%t updated=%d unchanged=%d errors=%d deferred=%d",
		waiting.Round(time.Millisecond), running.Round(time.Millisecond), ctx.Err() != nil, err != nil,
		progress.pass, progress.pages, progress.sourcesDone, progress.sourcesTotal,
		progress.totalKnown, progress.updated, progress.unchanged, progress.errors, progress.deferred,
	)
	if acquired.IsZero() {
		activeID, activeKind := uint64(0), syncOperationKind("untracked")
		if active := op.owner.active.Load(); active != nil {
			activeID, activeKind = active.id, active.scope.kind
		}
		message += fmt.Sprintf(" active_operation=%d active_kind=%s", activeID, activeKind)
	}
	log.Print(message)
}
