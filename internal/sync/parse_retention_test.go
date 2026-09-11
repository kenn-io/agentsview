package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
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
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprintf("staged_%t", staged), func(t *testing.T) {
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
				if staged {
					sink, err := newCodexStagingSink(t.TempDir(), nil)
					require.NoError(t, err)
					runtime.SetFinalizer(sink, func(*codexStagingSink) { close(finalized) })
					results <- syncJob{
						staged: sink,
						results: []parser.ParseResult{{Session: parser.ParsedSession{
							ID: "first", Agent: parser.AgentCodex,
						}}},
					}
					return
				}
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

			assert.Eventually(t, func() bool {
				runtime.GC()
				runtime.Gosched()
				select {
				case <-finalized:
					return true
				default:
					return false
				}
			}, 2*time.Second, 10*time.Millisecond,
				"flushed parsed batch remained live while the collector awaited more results")

			results <- syncJob{err: errors.New("finish")}
			close(results)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("collector did not finish")
			}
		})
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
	synctest.Test(t, func(t *testing.T) {
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

		synctest.Wait()
		assert.True(t, budget.underPressure(),
			"second bulk parse must wait for retained capacity")
		select {
		case second := <-acquired:
			second.Release()
			t.Fatal("second bulk parse admitted above the retention limit")
		default:
		}

		first.Release()
		synctest.Wait()
		select {
		case second := <-acquired:
			second.Release()
		default:
			t.Fatal("second bulk parse was not admitted after capacity was released")
		}
		assert.Equal(t, int64(2), budget.acquired.Load())
	})
}

func TestBulkParseRetentionBudgetAdmitsWorkerPoolForMediumSources(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := newBulkParseRetentionBudget(defaultBulkParseRetentionBytes)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		for range maxWorkers {
			lease, err := budget.acquire(ctx, 6<<20)
			require.NoError(t, err,
				"bulk admission must preserve the full worker pool for medium sources")
			t.Cleanup(lease.Release)
		}
	})
}

func TestBulkParseRetentionBudgetScavengesOncePerParseBearingPass(t *testing.T) {
	budget := newBulkParseRetentionBudget(defaultBulkParseRetentionBytes)
	var scavenges int
	budget.scavenge = func() { scavenges++ }

	budget.scavengeIfNeeded()
	assert.Zero(t, scavenges, "a pass with no parses must not scavenge")

	lease, err := budget.acquire(
		t.Context(), parseRetentionScavengeThreshold,
	)
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

func TestCollectAndBatchFlushesOnByteCap(t *testing.T) {
	engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	var batchLengths []int
	engine.writeBatchOverride = func(
		batch []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		batchLengths = append(batchLengths, len(batch))
		return len(batch), 0, 0, 0
	}
	results := make(chan syncJob, 2)
	for i := range 2 {
		results <- syncJob{
			path:        fmt.Sprintf("/sessions/large-%d.jsonl", i),
			sourceBytes: parseBatchBytesLimit,
			results: []parser.ParseResult{{
				Session: parser.ParsedSession{
					ID:    fmt.Sprintf("byte-cap-%d", i),
					Agent: parser.AgentClaude,
				},
			}},
		}
	}
	close(results)

	stats := engine.collectAndBatch(
		t.Context(), results, 2, 2, nil, syncWriteDefault,
	)

	assert.Equal(t, []int{1, 1}, batchLengths,
		"each oversized pending result must flush its own batch")
	assert.Equal(t, 2, stats.Synced)
}

func TestParseRetentionBudgetBoundsConcurrentSourceWeight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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

		synctest.Wait()
		select {
		case lease := <-acquired:
			lease.Release()
			t.Fatal("third parse admitted above the weighted retention limit")
		default:
		}

		first.Release()
		synctest.Wait()
		select {
		case lease := <-acquired:
			lease.Release()
		default:
			t.Fatal("third parse was not admitted after capacity was released")
		}
	})
}

func TestParseRetentionBudgetRunsOversizedSourceExclusively(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := newParseRetentionBudget(defaultParseRetentionBytes)
		oversized, err := budget.acquire(t.Context(), defaultParseRetentionBytes)
		require.NoError(t, err)
		t.Cleanup(oversized.Release)

		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		acquired := make(chan error, 1)
		go func() {
			_, acquireErr := budget.acquire(ctx, 1)
			acquired <- acquireErr
		}()
		synctest.Sleep(50 * time.Millisecond)
		synctest.Wait()
		select {
		case err = <-acquired:
		default:
			require.FailNow(t, "oversized source admission did not reach its deadline")
		}
		assert.ErrorIs(t, err, context.DeadlineExceeded)

		oversized.Release()
		lease, err := budget.acquire(t.Context(), 1)
		require.NoError(t, err)
		lease.Release()
	})
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
	synctest.Test(t, func(t *testing.T) {
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
		synctest.Wait()
		select {
		case <-writeEntered:
		default:
			require.FailNow(t, "collector did not enter write")
		}

		secondAcquired := make(chan *parseRetentionLease, 1)
		go func() {
			second, acquireErr := budget.acquire(t.Context(), 1)
			if acquireErr == nil {
				secondAcquired <- second
			}
		}()
		synctest.Wait()
		select {
		case second := <-secondAcquired:
			second.Release()
			t.Fatal("next parse admitted while the prior parse payload was being written")
		default:
		}

		close(allowWrite)
		synctest.Wait()
		select {
		case second := <-secondAcquired:
			second.Release()
		default:
			t.Fatal("parse lease was not released after the database write completed")
		}
		<-done
	})
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
			synctest.Test(t, func(t *testing.T) {
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
				var releaseWriteOnce sync.Once
				releaseWrite := func() {
					releaseWriteOnce.Do(func() { close(allowWrite) })
				}
				t.Cleanup(releaseWrite)
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
				synctest.Wait()
				select {
				case <-writeEntered:
				default:
					require.FailNow(t, "collector did not enter write")
				}

				acquireCtx, cancel := context.WithTimeout(
					t.Context(), time.Second,
				)
				t.Cleanup(cancel)
				acquired := make(chan struct {
					next *parseRetentionLease
					err  error
				}, 1)
				go func() {
					next, acquireErr := budget.acquire(acquireCtx, 1)
					acquired <- struct {
						next *parseRetentionLease
						err  error
					}{next: next, err: acquireErr}
				}()
				synctest.Wait()
				var acquireResult struct {
					next *parseRetentionLease
					err  error
				}
				select {
				case acquireResult = <-acquired:
				default:
					require.FailNow(t,
						"archive writes must not retain active parse admission")
				}
				cancel()
				if acquireResult.next != nil {
					acquireResult.next.Release()
				}
				releaseWrite()
				synctest.Wait()
				select {
				case <-done:
				default:
					require.FailNow(t, "collector did not finish")
				}
				assert.NoError(t, acquireResult.err,
					"archive writes must not retain active parse admission")
			})
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
			synctest.Test(t, func(t *testing.T) {
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
				synctest.Wait()
				select {
				case <-writeEntered:
				default:
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
				synctest.Wait()
				select {
				case <-done:
				default:
					require.FailNow(t, "collector did not finish")
				}
			})
		})
	}
}

func TestCollectAndBatchReportsOrderedBulkFinalization(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
		t.Cleanup(engine.Close)
		restore := engine.beginBulkRetentionPass()
		defer restore()
		budget := engine.retentionBudget()
		// Parsing a source arms the end-of-pass bulk memory scavenge.
		lease, err := budget.acquire(t.Context(), parseRetentionScavengeThreshold)
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
		synctest.Wait()
		select {
		case <-scavengeEntered:
		default:
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
		synctest.Wait()
		select {
		case <-done:
		default:
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
	})
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
	synctest.Test(t, func(t *testing.T) {
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
	})
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
	synctest.Test(t, func(t *testing.T) {
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
	})
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
			synctest.Test(t, func(t *testing.T) {
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
		})
	}
}

func TestStartWorkersCancellationReleasesAdmissionWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		synctest.Wait()
		assert.True(t, budget.underPressure(),
			"workers must reach the admission wait before cancellation")
		cancel()
		synctest.Wait()

		for range files {
			job := <-results
			assert.ErrorIs(t, job.err, context.Canceled)
			job.releaseRetention()
		}
		holder.Release()
		next, err := budget.acquire(t.Context(), defaultParseRetentionBytes)
		require.NoError(t, err, "canceled waiters must not leak weighted capacity")
		next.Release()
	})
}

// newSQLiteContainerMemberFixture builds a container-shaped fixture: one real
// file named for an OpenCode-family container, truncated to containerBytes,
// fanned into members virtual sources registered with a live container pass.
func newSQLiteContainerMemberFixture(
	t *testing.T, containerBytes int64, members int,
) (*Engine, []parser.DiscoveredFile, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	handle, err := os.Create(dbPath)
	require.NoError(t, err)
	require.NoError(t, handle.Truncate(containerBytes))
	require.NoError(t, handle.Close())

	engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)

	files := make([]parser.DiscoveredFile, members)
	engine.beginStreamingSQLiteContainerPass(nil)
	for i := range files {
		files[i] = parser.DiscoveredFile{
			Path:  parser.VirtualSourcePath(dbPath, fmt.Sprintf("ses-%03d", i)),
			Agent: parser.AgentMiMoCode,
		}
		engine.noteSQLiteContainerDiscovery(files[i])
	}
	engine.finishStreamingSQLiteContainerDiscovery()
	return engine, files, dbPath
}

func TestBulkAdmissionAdmitsWorkerPoolForSQLiteContainerMembers(t *testing.T) {
	engine, files, _ := newSQLiteContainerMemberFixture(t, 64<<20, 64)
	synctest.Test(t, func(t *testing.T) {
		budget := newBulkParseRetentionBudget(defaultBulkParseRetentionBytes)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		admitted := 0
		for i := range maxWorkers {
			lease, err := budget.acquire(
				ctx, engine.parseRetentionSourceBytes(files[i]),
			)
			if err != nil {
				break
			}
			admitted++
			t.Cleanup(lease.Release)
		}
		assert.Equal(t, maxWorkers, admitted,
			"members of one shared SQLite container must not serialize bulk admission")
	})
}

func TestParseRetentionChargesContainerMemberItsShare(t *testing.T) {
	engine, files, _ := newSQLiteContainerMemberFixture(t, 64<<20, 64)

	assert.Equal(t, int64(1048576), engine.parseRetentionSourceBytes(files[0]),
		"a member must be charged its share of the container, not the whole file")

	var total int64
	for _, file := range files {
		total += engine.parseRetentionSourceBytes(file)
	}
	assert.Equal(t, int64(67108864), total)
	assert.LessOrEqual(t, total, int64(67108864),
		"member shares must sum back to the container they partition")
}

func TestParseRetentionKeepsWholeFileSourceExclusive(t *testing.T) {
	engine, _, dbPath := newSQLiteContainerMemberFixture(t, 64<<20, 64)
	plainPath := filepath.Join(filepath.Dir(dbPath), "whole.jsonl")
	handle, err := os.Create(plainPath)
	require.NoError(t, err)
	require.NoError(t, handle.Truncate(67108864))
	require.NoError(t, handle.Close())
	plain := parser.DiscoveredFile{Path: plainPath, Agent: parser.AgentClaude}

	sourceBytes := engine.parseRetentionSourceBytes(plain)
	assert.Equal(t, int64(67108864), sourceBytes,
		"a plain source must keep its whole-file size")

	synctest.Test(t, func(t *testing.T) {
		budget := newBulkParseRetentionBudget(defaultBulkParseRetentionBytes)
		assert.Equal(t, defaultBulkParseRetentionBytes, budget.weight(sourceBytes))

		first, acquireErr := budget.acquire(t.Context(), sourceBytes)
		require.NoError(t, acquireErr)
		t.Cleanup(first.Release)

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, acquireErr = budget.acquire(ctx, sourceBytes)
		assert.ErrorIs(t, acquireErr, context.DeadlineExceeded,
			"a second whole-file source must still block on the bulk budget")
	})
}

func TestParseRetentionIgnoresNonFamilyVirtualPath(t *testing.T) {
	engine, _, dbPath := newSQLiteContainerMemberFixture(t, 64<<20, 64)
	otherPath := filepath.Join(filepath.Dir(dbPath), "other.db")
	handle, err := os.Create(otherPath)
	require.NoError(t, err)
	require.NoError(t, handle.Truncate(67108864))
	require.NoError(t, handle.Close())

	assert.Equal(t, int64(67108864), engine.parseRetentionSourceBytes(
		parser.DiscoveredFile{
			Path:  parser.VirtualSourcePath(otherPath, "ses-1"),
			Agent: parser.AgentMiMoCode,
		}),
		"a virtual path over a non-family container base must keep the stat size")
}

func TestParseRetentionKeepsCodexSourceBytesForStagingThreshold(t *testing.T) {
	engine, _, dbPath := newSQLiteContainerMemberFixture(t, 64<<20, 64)
	codexPath := filepath.Join(filepath.Dir(dbPath), "rollout.jsonl")
	handle, err := os.Create(codexPath)
	require.NoError(t, err)
	require.NoError(t, handle.Truncate(67108864))
	require.NoError(t, handle.Close())

	assert.Equal(t, int64(67108864), engine.parseRetentionSourceBytes(
		parser.DiscoveredFile{Path: codexPath, Agent: parser.AgentCodex}),
		"the Codex staging threshold input must keep the whole-file size")
}

func TestParseRetentionFallsBackToContainerSizeWithoutPass(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mimocode.db")
	handle, err := os.Create(dbPath)
	require.NoError(t, err)
	require.NoError(t, handle.Truncate(67108864))
	require.NoError(t, handle.Close())

	engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	require.Nil(t, engine.containerPass)

	assert.Equal(t, int64(67108864), engine.parseRetentionSourceBytes(
		parser.DiscoveredFile{
			Path:  parser.VirtualSourcePath(dbPath, "ses-001"),
			Agent: parser.AgentMiMoCode,
		}),
		"a pass tracking no membership must keep the whole-container size")
}

func TestParseRetentionBudgetAdmissionWeights(t *testing.T) {
	bulk := newBulkParseRetentionBudget(defaultBulkParseRetentionBytes)
	daemon := newParseRetentionBudget(defaultParseRetentionBytes)
	for _, tc := range []struct {
		name         string
		budget       *parseRetentionBudget
		sourceBytes  int64
		wantWeight   int64
		wantRetained int64
	}{
		{"bulk_one_byte", bulk, 1, 65540, 65540},
		{"bulk_six_mib", bulk, 6291456, 25231360, 25231360},
		{"bulk_below_clamp", bulk, 67092479, 268435452, 268435452},
		{"bulk_at_clamp", bulk, 67092480, 268435456, 268435456},
		{"bulk_saturated", bulk, 134217728, 268435456, 536870912},
		{"bulk_unknown", bulk, 0, 268435456, 536870912},
		{"bulk_negative", bulk, -1, 268435456, 536870912},
		{"daemon_sixty_four_mib", daemon, 67108864, 67108864, 67108864},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantWeight, tc.budget.weight(tc.sourceBytes))
			assert.Equal(t, tc.wantRetained, tc.budget.retainedBytes(tc.sourceBytes))
		})
	}
}
