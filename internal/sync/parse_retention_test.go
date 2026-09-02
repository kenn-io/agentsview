package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type parseRetentionFinalizerMarker struct {
	value bool
}

// newWarmBenchEngine builds a small already-synced Claude archive and
// returns an engine watching it, mirroring the fixture shape
// BenchmarkSyncAllWarmNoop uses. Five sessions exercise the per-source
// skip gates a warm no-op pass runs.
func newWarmBenchEngine(t *testing.T) (*Engine, context.Context) {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "warm-project")
	require.NoError(t, os.MkdirAll(proj, 0o755))
	for s := range 5 {
		builder := testjsonl.NewSessionBuilder()
		for m := 0; m < 6; m += 2 {
			ts := fmt.Sprintf("2026-06-20T10:%02d:00Z", m)
			builder.AddClaudeUser(ts, fmt.Sprintf(
				"user message %d in session %d", m, s,
			))
			builder.AddClaudeAssistant(ts, fmt.Sprintf(
				"assistant reply %d in session %d", m, s,
			))
		}
		path := filepath.Join(proj, fmt.Sprintf("warm-%04d.jsonl", s))
		require.NoError(t, os.WriteFile(
			path, []byte(builder.String()), 0o644,
		))
	}
	engine := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {dir},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	return engine, context.Background()
}

func TestWarmNoopSyncAcquiresNoRetentionLeases(t *testing.T) {
	e, ctx := newWarmBenchEngine(t)
	e.SyncAll(ctx, nil) // cold pass parses, acquires leases
	require.NotNil(t, e.bulkRetentionBudget,
		"a full sync pass must run under the bulk retention budget")
	before := e.bulkRetentionBudget.acquired.Load()
	require.Positive(t, before, "cold pass must acquire bulk leases")
	stats := e.SyncAll(ctx, nil) // warm pass: everything skips
	require.Equal(t, 0, stats.Synced)
	assert.Equal(t, before, e.bulkRetentionBudget.acquired.Load(),
		"warm no-op pass must not acquire parse-retention leases")
}

func TestBulkCollectorReleasesFlushedParsedBatch(t *testing.T) {
	engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)

	flushed := make(chan struct{})
	engine.writeBatchOverride = func(
		pending []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		close(flushed)
		return len(pending), 0, 0, 0
	}

	results := make(chan syncJob, batchSize+1)
	finalized := make(chan struct{})
	func() {
		marker := &parseRetentionFinalizerMarker{}
		runtime.SetFinalizer(marker, func(*parseRetentionFinalizerMarker) {
			close(finalized)
		})
		results <- syncJob{
			results: []parser.ParseResult{{
				Session: parser.ParsedSession{
					ID: "first", Agent: parser.AgentClaude,
					ClaudeLinearParse: &marker.value,
				},
			}}}
	}()
	for i := 1; i < batchSize; i++ {
		results <- syncJob{
			results: []parser.ParseResult{{
				Session: parser.ParsedSession{
					ID:    fmt.Sprintf("session-%d", i),
					Agent: parser.AgentClaude,
				},
			}}}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.collectAndBatch(
			t.Context(), results, batchSize+1, batchSize+1,
			nil, syncWriteBulk,
		)
	}()
	select {
	case <-flushed:
	case <-time.After(5 * time.Second):
		t.Fatal("collector did not flush the full parsed batch")
	}

	for range 3 {
		runtime.GC()
		runtime.Gosched()
	}
	select {
	case <-finalized:
	case <-time.After(2 * time.Second):
		t.Error("flushed parsed batch remained live while the collector awaited more results")
	}

	results <- syncJob{err: errors.New("finish")}
	close(results)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("collector did not finish")
	}
}

func TestFullSyncPassUsesBoundedRetentionAndScavengesOnce(t *testing.T) {
	e, ctx := newWarmBenchEngine(t)
	var scavenges int
	e.bulkRetentionBudget = newBulkParseRetentionBudget(
		defaultBulkParseRetentionBytes,
	)
	e.bulkRetentionBudget.scavenge = func() { scavenges++ }

	e.SyncAll(ctx, nil) // cold pass parses every source
	acquired := e.bulkRetentionBudget.acquired.Load()
	require.Positive(t, acquired,
		"full pass must admit parses through the bulk budget")
	require.NotNil(t, e.bulkRetentionBudget.weighted,
		"full pass must use byte-weighted parse admission")
	assert.Equal(t, defaultBulkParseRetentionBytes, e.bulkRetentionBudget.capacity)
	if e.parseRetentionBudget != nil {
		assert.Zero(t, e.parseRetentionBudget.acquired.Load(),
			"full pass must not consume the bounded daemon budget")
	}
	assert.Equal(t, 1, scavenges,
		"a parse-bearing bulk pass must release memory once at the end")

	stats := e.SyncAll(ctx, nil) // warm pass: everything skips
	require.Equal(t, 0, stats.Synced)
	assert.Equal(t, 1, scavenges,
		"a warm no-op pass must not force another scavenge")
	assert.Nil(t, e.activeRetention.Load(),
		"bulk budget must be uninstalled after the pass")
}

func TestScopedSyncKeepsBoundedRetentionBudget(t *testing.T) {
	e, ctx := newWarmBenchEngine(t)
	cutoff := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	stats := e.SyncAllSince(ctx, cutoff, nil) // cutoff-scoped daemon churn
	require.Positive(t, stats.Synced)
	require.NotNil(t, e.parseRetentionBudget,
		"a cutoff-scoped pass must use the bounded budget")
	assert.Positive(t, e.parseRetentionBudget.acquired.Load(),
		"scoped pass parses must be admitted by the bounded budget")
	assert.Nil(t, e.bulkRetentionBudget,
		"a cutoff-scoped pass must not create the bulk budget")
}

func TestBulkParseRetentionBudgetBoundsConcurrentSourceWeight(t *testing.T) {
	budget := newBulkParseRetentionBudget(defaultBulkParseRetentionBytes)
	first, err := budget.acquire(t.Context(), defaultParseRetentionBytes)
	require.NoError(t, err)
	t.Cleanup(first.Release)

	acquired := make(chan *parseRetentionLease, 1)
	go func() {
		second, acquireErr := budget.acquire(
			t.Context(), defaultParseRetentionBytes,
		)
		if acquireErr == nil {
			acquired <- second
		}
	}()

	require.Eventually(t, budget.underPressure, time.Second, time.Millisecond,
		"second bulk parse must wait for retained capacity")
	select {
	case second := <-acquired:
		second.Release()
		t.Fatal("second bulk parse admitted above the retention limit")
	default:
	}

	first.Release()
	select {
	case second := <-acquired:
		second.Release()
	case <-time.After(time.Second):
		t.Fatal("second bulk parse was not admitted after capacity was released")
	}
	assert.Equal(t, int64(2), budget.acquired.Load())
}

func TestBulkParseRetentionBudgetScavengesOncePerParseBearingPass(t *testing.T) {
	budget := newBulkParseRetentionBudget(defaultBulkParseRetentionBytes)
	var scavenges int
	budget.scavenge = func() { scavenges++ }

	budget.scavengeIfNeeded()
	assert.Zero(t, scavenges, "a pass with no parses must not scavenge")

	lease, err := budget.acquire(t.Context(), 1)
	require.NoError(t, err)
	lease.Release()
	budget.scavengeIfNeeded()
	budget.scavengeIfNeeded()
	assert.Equal(t, 1, scavenges,
		"one parse-bearing pass needs exactly one end-of-pass scavenge")
}

func TestBulkParseRetentionBudgetCountsUnknownSourceAtPendingLimit(t *testing.T) {
	budget := newBulkParseRetentionBudget(defaultBulkParseRetentionBytes)
	lease, err := budget.acquire(t.Context(), 0)
	require.NoError(t, err)
	t.Cleanup(lease.Release)

	assert.Equal(t, defaultBulkPendingRetentionBytes, lease.retainedBytes,
		"an unknown source must not undercount the pending parsed payload")
}

func TestParseRetentionBudgetBoundsConcurrentSourceWeight(t *testing.T) {
	budget := newParseRetentionBudget(defaultParseRetentionBytes)
	first, err := budget.acquire(t.Context(), 7<<20)
	require.NoError(t, err)
	second, err := budget.acquire(t.Context(), 7<<20)
	require.NoError(t, err)
	t.Cleanup(first.Release)
	t.Cleanup(second.Release)

	acquired := make(chan *parseRetentionLease, 1)
	go func() {
		lease, acquireErr := budget.acquire(t.Context(), 7<<20)
		if acquireErr == nil {
			acquired <- lease
		}
	}()

	select {
	case lease := <-acquired:
		lease.Release()
		t.Fatal("third parse admitted above the weighted retention limit")
	case <-time.After(50 * time.Millisecond):
	}

	first.Release()
	select {
	case lease := <-acquired:
		lease.Release()
	case <-time.After(time.Second):
		t.Fatal("third parse was not admitted after capacity was released")
	}
}

func TestParseRetentionBudgetRunsOversizedSourceExclusively(t *testing.T) {
	budget := newParseRetentionBudget(defaultParseRetentionBytes)
	oversized, err := budget.acquire(t.Context(), defaultParseRetentionBytes)
	require.NoError(t, err)
	t.Cleanup(oversized.Release)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = budget.acquire(ctx, 1)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	oversized.Release()
	lease, err := budget.acquire(t.Context(), 1)
	require.NoError(t, err)
	lease.Release()
}

func TestParseRetentionBudgetScavengesOnceAfterKnownLargeSource(t *testing.T) {
	budget := newParseRetentionBudget(defaultParseRetentionBytes)
	var scavenges int
	budget.scavenge = func() { scavenges++ }

	unknown, err := budget.acquire(t.Context(), 0)
	require.NoError(t, err)
	unknown.Release()
	budget.scavengeIfNeeded()
	assert.Zero(t, scavenges, "unknown sources must not force GC on every virtual event")

	large, err := budget.acquire(t.Context(), parseRetentionScavengeThreshold)
	require.NoError(t, err)
	large.Release()
	budget.scavengeIfNeeded()
	budget.scavengeIfNeeded()
	assert.Equal(t, 1, scavenges, "one large batch needs one post-write scavenge")
}

func TestCollectAndBatchRetainsParseLeaseThroughWrite(t *testing.T) {
	engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	budget := newParseRetentionBudget(defaultParseRetentionBytes)
	lease, err := budget.acquire(t.Context(), defaultParseRetentionBytes)
	require.NoError(t, err)

	writeEntered := make(chan struct{})
	allowWrite := make(chan struct{})
	engine.writeBatchOverride = func(
		batch []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		assert.Len(t, batch, 1)
		close(writeEntered)
		<-allowWrite
		return len(batch), 0, 0, 0
	}
	results := make(chan syncJob, 1)
	results <- syncJob{
		path:           "/sessions/one.jsonl",
		retentionLease: lease,
		results: []parser.ParseResult{{
			Session: parser.ParsedSession{ID: "one", Agent: parser.AgentClaude},
		}},
	}
	close(results)
	done := make(chan struct{})
	go func() {
		engine.collectAndBatch(
			t.Context(), results, 1, 1, nil, syncWriteDefault,
		)
		close(done)
	}()
	<-writeEntered

	secondAcquired := make(chan *parseRetentionLease, 1)
	go func() {
		second, acquireErr := budget.acquire(t.Context(), 1)
		if acquireErr == nil {
			secondAcquired <- second
		}
	}()
	select {
	case second := <-secondAcquired:
		second.Release()
		t.Fatal("next parse admitted while the prior parse payload was being written")
	case <-time.After(50 * time.Millisecond):
	}

	close(allowWrite)
	select {
	case second := <-secondAcquired:
		second.Release()
	case <-time.After(time.Second):
		t.Fatal("parse lease was not released after the database write completed")
	}
	<-done
}

func TestArchiveCollectorReleasesParseLeaseBeforeWrite(t *testing.T) {
	tests := []struct {
		name string
		mode syncWriteMode
	}{
		{name: "default-write", mode: syncWriteDefault},
		{name: "bulk-write", mode: syncWriteBulk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
			t.Cleanup(engine.Close)
			restore := engine.beginBulkRetentionPass()
			t.Cleanup(restore)
			budget := engine.retentionBudget()
			lease, err := budget.acquire(
				t.Context(), defaultBulkParseRetentionBytes,
			)
			require.NoError(t, err)

			writeEntered := make(chan struct{})
			allowWrite := make(chan struct{})
			engine.writeBatchOverride = func(
				batch []pendingWrite, _ syncWriteMode, _ bool,
			) (int, int, int, int) {
				assert.Len(t, batch, 1)
				close(writeEntered)
				<-allowWrite
				return len(batch), 0, 0, 0
			}
			results := make(chan syncJob, 1)
			results <- syncJob{
				path:           "/sessions/one.jsonl",
				retentionLease: lease,
				results: []parser.ParseResult{{
					Session: parser.ParsedSession{
						ID: "one", Agent: parser.AgentClaude,
					},
				}},
			}
			close(results)
			done := make(chan struct{})
			go func() {
				engine.collectAndBatch(
					t.Context(), results, 1, 1, nil, tt.mode,
				)
				close(done)
			}()
			<-writeEntered

			acquireCtx, cancel := context.WithTimeout(
				t.Context(), time.Second,
			)
			next, acquireErr := budget.acquire(acquireCtx, 1)
			cancel()
			if next != nil {
				next.Release()
			}
			close(allowWrite)
			<-done
			assert.NoError(t, acquireErr,
				"archive writes must not retain active parse admission")
		})
	}
}

func TestBulkCollectorBoundsPendingParsedBytesBetweenWrites(t *testing.T) {
	engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	engine.bulkRetentionBudget = newBulkParseRetentionBudget(128 << 20)
	engine.bulkRetentionBudget.pendingCapacity = 100 << 20
	restore := engine.beginBulkRetentionPass()
	t.Cleanup(restore)
	budget := engine.retentionBudget()

	const sourceBytes = 15 << 20
	first, err := budget.acquire(t.Context(), sourceBytes)
	require.NoError(t, err)
	second, err := budget.acquire(t.Context(), sourceBytes)
	require.NoError(t, err)
	results := make(chan syncJob, 2)
	for i, lease := range []*parseRetentionLease{first, second} {
		results <- syncJob{
			path:           fmt.Sprintf("/sessions/%d.jsonl", i),
			retentionLease: lease,
			results: []parser.ParseResult{{Session: parser.ParsedSession{
				ID: fmt.Sprintf("session-%d", i), Agent: parser.AgentClaude,
			}}},
		}
	}
	close(results)

	var batchLengths []int
	engine.writeBatchOverride = func(
		batch []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		batchLengths = append(batchLengths, len(batch))
		return len(batch), 0, 0, 0
	}
	stats := engine.collectAndBatch(
		t.Context(), results, 2, 2, nil, syncWriteBulk,
	)

	assert.Equal(t, []int{1, 1}, batchLengths)
	assert.Equal(t, 2, stats.Synced)
}

func TestCollectAndBatchReportsFinalizingOnlyBeforeBulkTerminalFlush(t *testing.T) {
	tests := []struct {
		name       string
		mode       syncWriteMode
		jobs       int
		wantPhase  Phase
		wantDetail string
	}{
		{
			name: "bulk terminal flush", mode: syncWriteBulk, jobs: 1,
			wantPhase:  PhaseFinalizing,
			wantDetail: "Finalizing sync: committing session writes",
		},
		{
			name: "bulk mid-loop flush", mode: syncWriteBulk, jobs: batchSize,
			wantPhase: PhaseSyncing,
		},
		{
			name: "default terminal flush", mode: syncWriteDefault, jobs: 1,
			wantPhase: PhaseSyncing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
			t.Cleanup(engine.Close)
			writeEntered := make(chan struct{})
			allowWrite := make(chan struct{})
			engine.writeBatchOverride = func(
				batch []pendingWrite, _ syncWriteMode, _ bool,
			) (int, int, int, int) {
				close(writeEntered)
				<-allowWrite
				return len(batch), 0, 0, 0
			}
			results := make(chan syncJob, tt.jobs)
			for i := range tt.jobs {
				results <- syncJob{
					path: fmt.Sprintf("/sessions/%03d.jsonl", i),
					results: []parser.ParseResult{{Session: parser.ParsedSession{
						ID: fmt.Sprintf("session-%03d", i), Agent: parser.AgentClaude,
					}}},
				}
			}
			close(results)
			progress := make(chan Progress, tt.jobs+8)
			done := make(chan struct{})
			go func() {
				engine.collectAndBatch(
					t.Context(), results, tt.jobs, tt.jobs,
					func(p Progress) { progress <- p }, tt.mode,
				)
				close(done)
			}()
			select {
			case <-writeEntered:
			case <-time.After(time.Second):
				require.FailNow(t, "collector did not enter write")
			}
			var last Progress
		drain:
			for {
				select {
				case last = <-progress:
				default:
					break drain
				}
			}
			assert.Equal(t, tt.wantPhase, last.Phase)
			assert.Equal(t, tt.wantDetail, last.Detail)
			close(allowWrite)
			select {
			case <-done:
			case <-time.After(time.Second):
				require.FailNow(t, "collector did not finish")
			}
		})
	}
}

func TestCollectAndBatchReportsOrderedBulkFinalization(t *testing.T) {
	engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	restore := engine.beginBulkRetentionPass()
	defer restore()
	budget := engine.retentionBudget()
	lease, err := budget.acquire(t.Context(), 1)
	require.NoError(t, err)

	scavengeEntered := make(chan struct{})
	allowScavenge := make(chan struct{})
	budget.scavenge = func() {
		close(scavengeEntered)
		<-allowScavenge
	}
	engine.writeBatchOverride = func(
		batch []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		return len(batch), 0, 0, 0
	}
	results := make(chan syncJob, 1)
	results <- syncJob{
		path: "/sessions/one.jsonl", retentionLease: lease,
		results: []parser.ParseResult{{Session: parser.ParsedSession{
			ID: "one", Agent: parser.AgentClaude,
		}}},
	}
	close(results)
	progress := make(chan Progress, 16)
	done := make(chan struct{})
	go func() {
		engine.collectAndBatch(
			t.Context(), results, 1, 1,
			func(p Progress) { progress <- p }, syncWriteBulk,
		)
		close(done)
	}()
	select {
	case <-scavengeEntered:
	case <-time.After(time.Second):
		require.FailNow(t, "collector did not enter memory scavenge")
	}
	var events []Progress
drain:
	for {
		select {
		case event := <-progress:
			events = append(events, event)
		default:
			break drain
		}
	}
	require.NotEmpty(t, events)
	assert.Equal(t, "Finalizing sync: releasing parsed-session memory",
		events[len(events)-1].Detail)
	close(allowScavenge)
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "collector did not finish after memory scavenge")
	}
	var details []string
	for _, event := range events {
		if event.Phase == PhaseFinalizing {
			details = append(details, event.Detail)
		}
	}
	assert.Equal(t, []string{
		"Finalizing sync: committing session writes",
		"Finalizing sync: saving session source state",
		"Finalizing sync: linking file-backed subagent sessions",
		"Finalizing sync: repairing subagent relationships",
		"Finalizing sync: releasing parsed-session memory",
	}, details)
}

func TestCollectAndBatchDiscardsPendingResultAfterCancellation(t *testing.T) {
	engine := NewEngine(openTestDB(t), EngineConfig{
		Machine:                      "local",
		DiscardPendingWritesOnCancel: true,
	})
	t.Cleanup(engine.Close)
	budget := newParseRetentionBudget(defaultParseRetentionBytes)
	lease, err := budget.acquire(t.Context(), defaultParseRetentionBytes)
	require.NoError(t, err)

	results := make(chan syncJob, 2)
	results <- syncJob{
		results: []parser.ParseResult{{
			Session: parser.ParsedSession{ID: "one", Agent: parser.AgentClaude},
		}},
		path:           "/sessions/one.jsonl",
		retentionLease: lease,
	}
	results <- syncJob{
		results: []parser.ParseResult{{
			Session: parser.ParsedSession{ID: "two", Agent: parser.AgentClaude},
		}},
		path: "/sessions/two.jsonl",
	}
	close(results)
	ctx, cancel := context.WithCancel(t.Context())
	writes := 0
	engine.writeBatchOverride = func(
		[]pendingWrite, syncWriteMode, bool,
	) (int, int, int, int) {
		writes++
		return 1, 0, 0, 0
	}

	observed := 0
	stats := engine.collectAndBatchWithOptions(
		ctx, results, 2, 2, nil, syncWriteDefault,
		collectAndBatchOptions{observeResult: func(syncJob) {
			observed++
			if observed == 2 {
				cancel()
			}
		}},
	)

	assert.True(t, stats.Aborted)
	assert.Zero(t, writes, "cancellation must not flush parsed scratch rows")
	next, err := budget.acquire(t.Context(), defaultParseRetentionBytes)
	require.NoError(t, err, "discarded parse data must release its retention lease")
	next.Release()
}

func TestDrainResultsReleasesParseLeases(t *testing.T) {
	budget := newParseRetentionBudget(defaultParseRetentionBytes)
	lease, err := budget.acquire(t.Context(), defaultParseRetentionBytes)
	require.NoError(t, err)
	results := make(chan syncJob, 1)
	results <- syncJob{retentionLease: lease}
	close(results)

	drainResults(results, 1)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	next, err := budget.acquire(ctx, defaultParseRetentionBytes)
	require.NoError(t, err)
	next.Release()
}

func TestCollectAndBatchPromotesClaudeSourceAfterShutdownCancellation(t *testing.T) {
	database := openTestDB(t)
	for _, id := range []string{"root", "fork"} {
		require.NoError(t, database.UpsertSession(db.Session{
			ID: id, Project: "project-a", Machine: "local", Agent: "claude",
		}))
		require.NoError(t, database.SetSessionDataVersion(
			id, db.CurrentDataVersion(),
		))
	}
	engine := NewEngine(database, EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	ctx, cancel := context.WithCancel(t.Context())
	engine.writeBatchOverride = func(
		batch []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		cancel()
		return len(batch), 0, 0, 0
	}
	results := make(chan syncJob, 1)
	results <- syncJob{
		results: []parser.ParseResult{
			{Session: parser.ParsedSession{ID: "root", Agent: parser.AgentClaude}},
			{Session: parser.ParsedSession{ID: "fork", Agent: parser.AgentClaude}},
		},
		agent: parser.AgentClaude,
		path:  "/sessions/root.jsonl",
	}
	close(results)

	engine.collectAndBatch(ctx, results, 1, 2, nil, syncWriteDefault)

	assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion("root"))
	assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion("fork"))
}

func TestCollectAndBatchKeepsFanoutUnderOneLeaseUntilOneWrite(t *testing.T) {
	engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	budget := newParseRetentionBudget(defaultParseRetentionBytes)
	lease, err := budget.acquire(t.Context(), defaultParseRetentionBytes)
	require.NoError(t, err)

	parsed := make([]parser.ParseResult, batchSize+1)
	for i := range parsed {
		parsed[i].Session = parser.ParsedSession{
			ID: fmt.Sprintf("fanout-%03d", i), Agent: parser.AgentKiro,
		}
	}
	var batchLengths []int
	engine.writeBatchOverride = func(
		batch []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		batchLengths = append(batchLengths, len(batch))
		return len(batch), 0, 0, 0
	}
	results := make(chan syncJob, 1)
	results <- syncJob{
		path:           "/sessions/data.sqlite3",
		retentionLease: lease,
		results:        parsed,
	}
	close(results)

	stats := engine.collectAndBatch(
		t.Context(), results, 1, 1, nil, syncWriteDefault,
	)

	assert.Equal(t, []int{batchSize + 1}, batchLengths)
	assert.Equal(t, batchSize+1, stats.Synced)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	next, err := budget.acquire(ctx, defaultParseRetentionBytes)
	require.NoError(t, err)
	next.Release()
}

func TestStartWorkersKeepsBulkBatchingIndependentOfParseAdmission(t *testing.T) {
	tests := []struct {
		name       string
		mode       syncWriteMode
		wantWrites int
	}{
		{name: "default", mode: syncWriteDefault, wantWrites: 2},
		{name: "bulk", mode: syncWriteBulk, wantWrites: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const agent parser.AgentType = "retention-test"
			provider := &directStreamingProvider{
				Def: parser.AgentDef{Type: agent},
				parseOutcome: parser.ParseOutcome{
					Results: []parser.ParseResultOutcome{{
						Result: parser.ParseResult{Session: parser.ParsedSession{
							ID: "retention-test:session", Agent: agent,
						}},
					}},
					ResultSetComplete: true,
				},
			}
			engine := NewEngine(openTestDB(t), EngineConfig{
				Machine: "local",
				ProviderFactories: []parser.ProviderFactory{
					directStreamingFactory{provider},
				},
				ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
					agent: parser.ProviderMigrationProviderAuthoritative,
				},
			})
			t.Cleanup(engine.Close)
			if tt.mode == syncWriteBulk {
				restore := engine.beginBulkRetentionPass()
				t.Cleanup(restore)
			}
			engine.workerCountOverride = 2
			var writes int
			engine.writeBatchOverride = func(
				batch []pendingWrite, _ syncWriteMode, _ bool,
			) (int, int, int, int) {
				writes++
				return len(batch), 0, 0, 0
			}

			files := make([]parser.DiscoveredFile, 2)
			for i := range files {
				path := filepath.Join(t.TempDir(), fmt.Sprintf("large-%d.jsonl", i))
				file, err := os.Create(path)
				require.NoError(t, err)
				require.NoError(t, file.Truncate(20<<20))
				require.NoError(t, file.Close())
				source := parser.SourceRef{
					Provider: agent, Key: path, DisplayPath: path, FingerprintKey: path,
				}
				files[i] = parser.DiscoveredFile{
					Path: path, Agent: agent, ProviderSource: &source,
					ProviderProcess: true, ForceParse: true,
				}
			}

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			stats := engine.collectAndBatch(
				ctx, engine.startWorkers(ctx, files), len(files), len(files), nil,
				tt.mode,
			)

			assert.False(t, stats.Aborted)
			assert.Equal(t, 2, stats.Synced)
			assert.Equal(t, tt.wantWrites, writes,
				"bulk parse admission must not fragment the database batch")
		})
	}
}

func TestStartWorkersCancellationReleasesAdmissionWaiters(t *testing.T) {
	const agent parser.AgentType = "retention-cancel-test"
	provider := &directStreamingProvider{
		Def: parser.AgentDef{Type: agent},
		parseOutcome: parser.ParseOutcome{
			Results: []parser.ParseResultOutcome{{
				Result: parser.ParseResult{Session: parser.ParsedSession{
					ID: "retention-cancel-test:session", Agent: agent,
				}},
			}},
			ResultSetComplete: true,
		},
	}
	engine := NewEngine(openTestDB(t), EngineConfig{
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{directStreamingFactory{provider}},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			agent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	engine.workerCountOverride = 2
	budget := newParseRetentionBudget(defaultParseRetentionBytes)
	engine.parseRetentionBudget = budget

	holder, err := budget.acquire(t.Context(), defaultParseRetentionBytes)
	require.NoError(t, err)
	files := make([]parser.DiscoveredFile, 2)
	for i := range files {
		// A provider-authoritative, force-parsed source reaches the provider
		// parse seam where the lease is acquired; a trivial file would return
		// through a skip gate before the admission wait now that acquisition
		// follows the gates.
		path := filepath.Join(t.TempDir(), fmt.Sprintf("waiting-%d.jsonl", i))
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
		source := parser.SourceRef{
			Provider: agent, Key: path, DisplayPath: path, FingerprintKey: path,
		}
		files[i] = parser.DiscoveredFile{
			Path: path, Agent: agent, ProviderSource: &source,
			ProviderProcess: true, ForceParse: true,
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	results := engine.startWorkers(ctx, files)
	require.Eventually(t, budget.underPressure, time.Second, time.Millisecond,
		"workers must reach the admission wait before cancellation")
	cancel()

	for range files {
		job := <-results
		assert.ErrorIs(t, job.err, context.Canceled)
		job.releaseRetention()
	}
	holder.Release()
	next, err := budget.acquire(t.Context(), defaultParseRetentionBytes)
	require.NoError(t, err, "canceled waiters must not leak weighted capacity")
	next.Release()
}
