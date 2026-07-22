package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank"
)

func TestDocbankArtifactMaintainerRunsBoundedPhysicalStages(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	maintainer, ok := any(store).(ArtifactMaintainer)
	require.True(t, ok)
	ref := createRetentionRef(t, store, Ref{
		Origin: retentionOrigin, Kind: KindRaw, Name: hashHex([]byte("physical")),
	}, []byte("physical"))

	verified, err := maintainer.Verify(t.Context(), WorkBudget{MaxObjects: 1, MaxBytes: 1 << 20})
	require.NoError(t, err)
	assert.Equal(t, 1, verified.Processed)

	require.NoError(t, store.Trash(t.Context(), ref))
	emptied, err := maintainer.EmptyTrash(t.Context(), 0, WorkBudget{MaxObjects: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, emptied.Processed)

	collected, err := maintainer.GarbageCollect(t.Context(), WorkBudget{
		MaxObjects: 1, MaxBytes: 1 << 20,
	})
	require.NoError(t, err)
	assert.Positive(t, collected.Processed)

	_, err = maintainer.Repack(t.Context(), WorkBudget{
		MaxObjects: 1, MaxBytes: 1 << 20,
	})
	require.NoError(t, err)
}

func TestDocbankArtifactMaintainerResumesTrashAndGarbageCollection(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	maintainer := any(store).(ArtifactMaintainer)
	for i := range 3 {
		body := fmt.Appendf(nil, "trash-%d", i)
		ref := createRetentionRef(t, store, Ref{
			Origin: retentionOrigin, Kind: KindRaw, Name: hashHex(body),
		}, body)
		require.NoError(t, store.Trash(t.Context(), ref))
	}

	trash, err := maintainer.EmptyTrash(t.Context(), 0, WorkBudget{MaxObjects: 1})
	require.NoError(t, err)
	assert.True(t, trash.More)
	require.NotEmpty(t, trash.NextCursor)
	trashPasses := 1
	for trash.More {
		trash, err = maintainer.EmptyTrash(t.Context(), 0, WorkBudget{
			MaxObjects: 1, Cursor: trash.NextCursor,
		})
		require.NoError(t, err)
		trashPasses++
	}
	assert.Equal(t, 3, trashPasses)
	assert.Empty(t, trash.NextCursor)

	collected, err := maintainer.GarbageCollect(t.Context(), WorkBudget{MaxObjects: 1})
	require.NoError(t, err)
	assert.True(t, collected.More)
	require.NotEmpty(t, collected.NextCursor)
	_, err = maintainer.Repack(t.Context(), WorkBudget{
		MaxObjects: 1, Cursor: collected.NextCursor,
	})
	assert.ErrorIs(t, err, ErrArtifactInvalid,
		"a GC continuation must not be accepted by the Repack stage")
	gcPasses := 1
	for collected.More {
		collected, err = maintainer.GarbageCollect(t.Context(), WorkBudget{
			MaxObjects: 1, Cursor: collected.NextCursor,
		})
		require.NoError(t, err)
		gcPasses++
	}
	assert.Equal(t, 3, gcPasses)
	assert.Empty(t, collected.NextCursor)
}

func TestDocbankArtifactMaintainerRejectsOversizedObjectBudget(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	maintainer := any(store).(ArtifactMaintainer)
	_, err := maintainer.GarbageCollect(t.Context(), WorkBudget{
		MaxObjects: docbank.MaxMaintenanceObjects + 1,
	})
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

func TestDocbankRepackProcessedIncludesEveryMaintenanceAction(t *testing.T) {
	report := docbank.RepackReport{
		MappingsPruned: 2, PacksSelected: 3, PacksRewritten: 5,
		PacksSealed: 7, PacksRemoved: 11, PacksDeferredOversized: 13,
		BlobsRepacked: 17,
	}

	assert.Equal(t, 58, docbankRepackProcessed(report))
}

func TestDocbankStaleRevisionMapsToArtifactConflict(t *testing.T) {
	assert.ErrorIs(t, mapDocbankError(docbank.ErrStaleRevision), ErrArtifactConflict)
}

func TestPackSchedulerCoalescesNotificationsAndBoundsEachPass(t *testing.T) {
	packer := newBlockingPacker()
	scheduler := newPackScheduler(packer, packSchedulerOptions{
		RetryDelay: time.Hour,
	})
	t.Cleanup(scheduler.Close)

	returned := make(chan struct{})
	go func() {
		scheduler.Notify(t.Context())
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Notify blocked on physical packing")
	}
	require.Eventually(t, func() bool { return packer.calls.Load() == 1 }, time.Second, time.Millisecond)
	for range 1_000 {
		scheduler.Notify(t.Context())
	}
	assert.Equal(t, int32(1), packer.maxConcurrent.Load())
	packer.release <- PackResult{}
	require.Eventually(t, func() bool { return packer.calls.Load() == 2 }, time.Second, time.Millisecond)
	packer.release <- PackResult{}
	require.Never(t, func() bool { return packer.calls.Load() > 2 }, 20*time.Millisecond, time.Millisecond)
	assert.Equal(t, []int64{defaultPackPassBytes, defaultPackPassBytes}, packer.budgets())
	assert.Equal(t, int32(1), packer.maxConcurrent.Load())
}

func TestPackThresholdCountsOnlyNewEligibleLoosePhysicalWrites(t *testing.T) {
	packer := newBlockingPacker()
	scheduler := newPackScheduler(packer, packSchedulerOptions{
		RetryDelay: time.Hour,
	})
	t.Cleanup(scheduler.Close)

	for range defaultPackObjectThreshold - 2 {
		scheduler.ObserveWrite(t.Context(), PhysicalWrite{
			Kind: "loose", Encoding: "raw", StoredBytes: 1, PackEligible: true,
		})
	}
	scheduler.ObserveWrite(t.Context(), PhysicalWrite{
		Kind: "loose", Encoding: "zstd", StoredBytes: 2, PackEligible: true,
	})
	scheduler.ObserveWrite(t.Context(), PhysicalWrite{
		Kind: "packed", Encoding: "zstd", StoredBytes: 1 << 20, PackEligible: true,
	})
	scheduler.ObserveWrite(t.Context(), PhysicalWrite{
		Kind: "loose", Encoding: "zstd", StoredBytes: defaultPackByteThreshold + 1,
		PackEligible: false,
	})
	scheduler.ObserveWrite(t.Context(), PhysicalWrite{})
	assert.Never(t, func() bool { return packer.calls.Load() != 0 },
		20*time.Millisecond, time.Millisecond)

	scheduler.ObserveWrite(t.Context(), PhysicalWrite{
		Kind: "loose", Encoding: "raw", StoredBytes: 3, PackEligible: true,
	})
	require.Eventually(t, func() bool { return packer.calls.Load() == 1 },
		time.Second, time.Millisecond)
	for range 1_000 {
		scheduler.ObserveWrite(t.Context(), PhysicalWrite{
			Kind: "loose", Encoding: "raw", StoredBytes: 1, PackEligible: true,
		})
	}
	assert.Equal(t, int32(1), packer.maxConcurrent.Load())
	packer.setBacklog(LooseBacklog{})
	packer.release <- PackResult{}
	require.Eventually(t, func() bool { return packer.calls.Load() >= 2 },
		time.Second, time.Millisecond)
	packer.release <- PackResult{}
	assert.Equal(t, int32(1), packer.maxConcurrent.Load())
}

func TestPackThresholdUsesStoredBytesInsteadOfLogicalBytes(t *testing.T) {
	packer := newBlockingPacker()
	scheduler := newPackScheduler(packer, packSchedulerOptions{RetryDelay: time.Hour})
	t.Cleanup(scheduler.Close)

	scheduler.ObserveWrite(t.Context(), PhysicalWrite{
		Kind:         "loose",
		Encoding:     "zstd",
		LogicalBytes: defaultPackByteThreshold * 4,
		StoredBytes:  defaultPackByteThreshold - 1,
		PackEligible: true,
	})
	assert.Never(t, func() bool { return packer.calls.Load() != 0 },
		20*time.Millisecond, time.Millisecond)
	scheduler.ObserveWrite(t.Context(), PhysicalWrite{
		Kind: "loose", Encoding: "zstd", StoredBytes: 1, PackEligible: true,
	})
	require.Eventually(t, func() bool { return packer.calls.Load() == 1 },
		time.Second, time.Millisecond)
	packer.setBacklog(LooseBacklog{})
	packer.release <- PackResult{}
}

func TestPackThresholdRefreshesIndexedBacklogAfterSuccessfulPass(t *testing.T) {
	packer := newBlockingPacker()
	scheduler := newPackScheduler(packer, packSchedulerOptions{
		ObjectThreshold: 3,
		ByteThreshold:   1 << 60,
		RetryDelay:      time.Hour,
	})
	t.Cleanup(scheduler.Close)
	for range 3 {
		scheduler.ObserveWrite(t.Context(), PhysicalWrite{
			Kind: "loose", Encoding: "raw", StoredBytes: 1, PackEligible: true,
		})
	}
	require.Eventually(t, func() bool { return packer.calls.Load() == 1 },
		time.Second, time.Millisecond)
	packer.setBacklog(LooseBacklog{})
	packer.release <- PackResult{PackedObjects: 3, LogicalBytes: 3}
	require.Eventually(t, func() bool { return packer.backlogCalls.Load() == 1 },
		time.Second, time.Millisecond)

	for range 2 {
		scheduler.ObserveWrite(t.Context(), PhysicalWrite{
			Kind: "loose", Encoding: "raw", StoredBytes: 1, PackEligible: true,
		})
	}
	assert.Never(t, func() bool { return packer.calls.Load() > 1 },
		20*time.Millisecond, time.Millisecond)
	scheduler.ObserveWrite(t.Context(), PhysicalWrite{
		Kind: "loose", Encoding: "raw", StoredBytes: 1, PackEligible: true,
	})
	require.Eventually(t, func() bool { return packer.calls.Load() == 2 },
		time.Second, time.Millisecond)
	packer.release <- PackResult{}
}

func TestPackThresholdPreservesReceiptObservedDuringBacklogRefresh(t *testing.T) {
	packer := newInterleavedBacklogPacker()
	scheduler := newPackScheduler(packer, packSchedulerOptions{
		ObjectThreshold: 2,
		ByteThreshold:   1 << 60,
		RetryDelay:      time.Hour,
	})
	t.Cleanup(scheduler.Close)

	scheduler.Notify(t.Context())
	select {
	case <-packer.queryStarted:
	case <-time.After(time.Second):
		t.Fatal("post-pack backlog query did not start")
	}
	scheduler.ObserveWrite(t.Context(), PhysicalWrite{
		Kind: "loose", Encoding: "zstd", StoredBytes: 1, PackEligible: true,
	})
	close(packer.releaseQuery)
	require.Eventually(t, func() bool {
		scheduler.mu.Lock()
		defer scheduler.mu.Unlock()
		return !scheduler.refreshing
	}, time.Second, time.Millisecond, "post-pack backlog refresh did not finish")

	scheduler.ObserveWrite(t.Context(), PhysicalWrite{
		Kind: "loose", Encoding: "zstd", StoredBytes: 1, PackEligible: true,
	})
	require.Eventually(t, func() bool { return packer.packCalls.Load() == 2 },
		time.Second, time.Millisecond,
		"the refresh must preserve the first receipt so the second crosses the threshold")
}

func TestStartupBacklogQueriesOnceAndSchedulesPhysicalThreshold(t *testing.T) {
	for _, test := range []struct {
		name    string
		backlog LooseBacklog
		wantRun bool
	}{
		{
			name: "below thresholds",
			backlog: LooseBacklog{
				EligibleObjects: 1, EligibleBytes: 1 << 30,
				EligibleStoredBytes: defaultPackByteThreshold - 1,
			},
		},
		{
			name: "object threshold",
			backlog: LooseBacklog{
				EligibleObjects:     defaultPackObjectThreshold,
				EligibleStoredBytes: 1,
			},
			wantRun: true,
		},
		{
			name: "physical byte threshold",
			backlog: LooseBacklog{
				EligibleObjects: 1, EligibleBytes: defaultPackByteThreshold * 8,
				EligibleStoredBytes: defaultPackByteThreshold,
			},
			wantRun: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			packer := newBlockingPacker()
			packer.setBacklog(test.backlog)
			scheduler := newPackScheduler(packer, packSchedulerOptions{
				RetryDelay: time.Hour,
			})
			t.Cleanup(scheduler.Close)

			require.NoError(t, scheduler.Recover(t.Context()))
			assert.Equal(t, int32(1), packer.backlogCalls.Load())
			if !test.wantRun {
				assert.Never(t, func() bool { return packer.calls.Load() != 0 },
					20*time.Millisecond, time.Millisecond)
				return
			}
			require.Eventually(t, func() bool { return packer.calls.Load() == 1 },
				time.Second, time.Millisecond)
			packer.setBacklog(LooseBacklog{})
			packer.release <- PackResult{}
		})
	}
}

func TestStartupBacklogRecoveryReachesRepositoryOwnedStore(t *testing.T) {
	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	backend := repository.backend.(*docbankStore)
	backend.packer.Close()
	packer := newBlockingPacker()
	packer.setBacklog(LooseBacklog{EligibleObjects: defaultPackObjectThreshold})
	backend.packer = newPackScheduler(packer, packSchedulerOptions{RetryDelay: time.Hour})

	require.NoError(t, RecoverArtifactPacking(t.Context(), repository.Store()))
	assert.Equal(t, int32(1), packer.backlogCalls.Load())
	require.Eventually(t, func() bool { return packer.calls.Load() == 1 },
		time.Second, time.Millisecond)
	packer.setBacklog(LooseBacklog{})
	packer.release <- PackResult{}
}

func TestPackCardinalityRealVaultWriteBatchStaysConstant(t *testing.T) {
	small := realDocbankPackCardinalityStats(t, 8)
	large := realDocbankPackCardinalityStats(t, 1_024)
	t.Logf("small=%+v large=%+v", small, large)

	assert.Equal(t, int32(1), small.backlogQueries)
	assert.Equal(t, small.backlogQueries, large.backlogQueries,
		"one startup aggregate must not become per-write archive queries")
	assert.Equal(t, int32(0), small.packCalls)
	assert.Equal(t, small.packCalls, large.packCalls)
	assert.Equal(t, int32(packCardinalityBatchSize), small.physicalWrites)
	assert.Equal(t, small.physicalWrites, large.physicalWrites)
	assert.Equal(t, small.receiptBytes, large.receiptBytes)
	assert.LessOrEqual(t, large.allocatedBytes, small.allocatedBytes+(1<<20),
		"an identical write batch must not allocate in proportion to archived objects")
}

func TestPackThresholdSuppressedWriteDoesNotStartShutdownMaintenance(t *testing.T) {
	packer := newBlockingPacker()
	scheduler := newPackScheduler(packer, packSchedulerOptions{
		ObjectThreshold: 1, RetryDelay: time.Hour,
	})
	t.Cleanup(scheduler.Close)
	scheduler.ObserveWrite(SuppressArtifactMaintenance(t.Context()), PhysicalWrite{
		Kind: "loose", Encoding: "zstd", StoredBytes: 1, PackEligible: true,
	})
	assert.Never(t, func() bool { return packer.calls.Load() != 0 },
		20*time.Millisecond, time.Millisecond)
}

func TestPackThresholdDocbankCreateCountsUniquePhysicalWrites(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	store.packer.Close()
	packer := &countingArtifactPacker{artifactPacker: store}
	store.packer = newPackScheduler(packer, packSchedulerOptions{
		ObjectThreshold: 2,
		ByteThreshold:   1 << 60,
		RetryDelay:      time.Hour,
	})

	body := []byte(`{"origin":"contract-a1b2c3","sequence":1}`)
	ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
	identity := identityForBytes(t, body)
	created, err := store.Create(t.Context(), ref, identity, "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	assert.True(t, created.Created)
	retry, err := store.Create(t.Context(), ref, identity, "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	assert.False(t, retry.Created)
	assert.Never(t, func() bool { return packer.calls.Load() != 0 },
		20*time.Millisecond, time.Millisecond)

	createCheckpointBody(t, store, 2, []byte("second unique physical object"))
	require.Eventually(t, func() bool { return packer.calls.Load() == 1 },
		time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		backlog, backlogErr := store.LooseBacklog(t.Context())
		return backlogErr == nil && backlog.EligibleObjects == 0
	}, time.Second, time.Millisecond)
}

func TestSustainedPackingBoundsLooseBacklogWithoutPackingPerObject(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{LooseCompression: docbank.LooseCompressionOptions{
		Enabled: true, MinBytes: 1, MinSavingsPercent: 1,
	}})
	store.packer.Close()
	packer := &countingArtifactPacker{artifactPacker: store}
	store.packer = newPackScheduler(packer, packSchedulerOptions{
		ObjectThreshold: 16,
		ByteThreshold:   1 << 60,
		RetryDelay:      time.Millisecond,
	})

	const objects = 160
	for sequence := 1; sequence <= objects; sequence++ {
		payload := bytes.Repeat(fmt.Appendf(nil, "compressible-%03d\n", sequence), 256)
		createCheckpointBody(t, store, sequence, payload)
	}
	store.NotifyArtifactBatch(t.Context())
	var finalBacklog LooseBacklog
	require.Eventually(t, func() bool {
		backlog, err := store.LooseBacklog(t.Context())
		finalBacklog = backlog
		return err == nil && backlog.EligibleObjects == 0 && backlog.EligibleStoredBytes == 0
	}, 10*time.Second, 5*time.Millisecond)

	assert.Positive(t, packer.calls.Load())
	assert.Less(t, packer.calls.Load(), int32(objects/2),
		"packing should coalesce sustained writes instead of packing each object")
	assert.Zero(t, finalBacklog.EligibleStoredBytes,
		"successful packing must bound physical loose bytes, not only object count")
}

const packCardinalityBatchSize = 32

type packCardinalityStats struct {
	backlogQueries int32
	packCalls      int32
	physicalWrites int32
	receiptBytes   int64
	allocatedBytes uint64
}

func realDocbankPackCardinalityStats(t *testing.T, archiveSize int) packCardinalityStats {
	t.Helper()
	_, store := newTestDocbankStore(t, docbank.Config{LooseCompression: docbank.LooseCompressionOptions{
		Enabled: true, MinBytes: 1, MinSavingsPercent: 1,
	}})
	store.packer.Close()
	packer := &countingArtifactPacker{artifactPacker: store}
	store.packer = newPackScheduler(packer, packSchedulerOptions{
		ObjectThreshold: 1 << 60,
		ByteThreshold:   1 << 60,
		RetryDelay:      time.Hour,
	})

	for sequence := 1; sequence <= archiveSize; sequence++ {
		body := bytes.Repeat(fmt.Appendf(nil, "seed-%08d\n", sequence), 8)
		createCheckpointBody(t, store, sequence, body)
	}
	require.NoError(t, store.packer.Recover(t.Context()))
	require.Equal(t, int32(1), packer.backlogCalls.Load())

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	stats := packCardinalityStats{}
	for offset := range packCardinalityBatchSize {
		body := bytes.Repeat(fmt.Appendf(nil, "batch-%08d\n", offset), 8)
		result := createCheckpointBody(t, store, archiveSize+offset+1, body)
		if result.Physical != (PhysicalWrite{}) {
			stats.physicalWrites++
			stats.receiptBytes += result.Physical.StoredBytes
		}
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	stats.allocatedBytes = after.TotalAlloc - before.TotalAlloc
	stats.backlogQueries = packer.backlogCalls.Load()
	stats.packCalls = packer.calls.Load()
	return stats
}

type countingArtifactPacker struct {
	artifactPacker
	calls        atomic.Int32
	backlogCalls atomic.Int32
}

func (p *countingArtifactPacker) Pack(ctx context.Context, maxBytes int64) (PackResult, error) {
	p.calls.Add(1)
	return p.artifactPacker.Pack(ctx, maxBytes)
}

func (p *countingArtifactPacker) LooseBacklog(
	ctx context.Context,
) (LooseBacklog, error) {
	p.backlogCalls.Add(1)
	return p.artifactPacker.LooseBacklog(ctx)
}

func TestPackSchedulerRetriesMoreAfterDelayAndCloseCancelsWork(t *testing.T) {
	packer := newBlockingPacker()
	scheduler := newPackScheduler(packer, packSchedulerOptions{RetryDelay: 10 * time.Millisecond})
	scheduler.Notify(t.Context())
	require.Eventually(t, func() bool { return packer.calls.Load() == 1 }, time.Second, time.Millisecond)
	packer.release <- PackResult{More: true}
	require.Eventually(t, func() bool { return packer.calls.Load() == 2 }, time.Second, time.Millisecond)
	scheduler.Close()
	select {
	case packer.release <- PackResult{}:
	default:
	}
	assert.Eventually(t, func() bool { return packer.canceled.Load() == 1 }, time.Second, time.Millisecond)
	scheduler.Notify(t.Context())
	assert.Never(t, func() bool { return packer.calls.Load() > 2 }, 20*time.Millisecond, time.Millisecond)
}

func TestExportNotificationWorkDoesNotScaleWithUnrelatedArchiveCardinality(t *testing.T) {
	small := exportNotificationCardinalityStats(t, 10)
	large := exportNotificationCardinalityStats(t, 10_000)

	want := artifactExportCallStats{
		claims: 1, sessions: 1, messages: 1, usage: 1,
	}
	assert.Equal(t, want, small)
	assert.Equal(t, want, large)
}

func TestPackSchedulerSuppressedContextDoesNotStartOptionalWork(t *testing.T) {
	packer := &cardinalityPacker{called: make(chan struct{}, 1)}
	scheduler := newPackScheduler(packer, packSchedulerOptions{RetryDelay: time.Hour})
	t.Cleanup(scheduler.Close)
	scheduler.Notify(SuppressArtifactMaintenance(t.Context()))
	assert.Never(t, func() bool { return packer.calls.Load() != 0 }, 20*time.Millisecond, time.Millisecond)
}

func TestExportToStoreNotifiesOnlyAfterSuccessfulNonShutdownCompletion(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "notify-session", "project")
	base := newRetentionStore(t)
	store := &notificationStore{ArtifactStore: base}

	_, err := ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: retentionOrigin, Full: true,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), store.notifications.Load(),
		"one full export is one completed batch even when it drains internal pages")

	require.NoError(t, database.ReplaceSessionMessages("notify-session", []db.Message{{
		SessionID: "notify-session", Ordinal: 0, Role: "user", Content: "shutdown", ContentLength: 8,
	}}))
	_, err = ExportToStore(SuppressArtifactMaintenance(t.Context()), database, store, ExportOptions{
		Origin: retentionOrigin,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), store.notifications.Load(),
		"shutdown flush completion must not start optional work")
}

func TestTransportExchangeNotifiesOnlyAfterSuccessfulNonShutdownCompletion(t *testing.T) {
	base := newRetentionStore(t)
	store := &notificationStore{ArtifactStore: base}
	transport, err := openFolderTransport(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })

	require.NoError(t, transport.Exchange(t.Context(), store))
	assert.Equal(t, int32(1), store.notifications.Load())
	require.NoError(t, transport.Exchange(SuppressArtifactMaintenance(t.Context()), store))
	assert.Equal(t, int32(1), store.notifications.Load())

	failed, err := openFolderTransport(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, failed.Close())
	require.Error(t, failed.Exchange(t.Context(), store))
	assert.Equal(t, int32(1), store.notifications.Load(),
		"failed exchanges must not start optional work")
}

type notificationStore struct {
	ArtifactStore
	notifications atomic.Int32
}

func (s *notificationStore) NotifyArtifactBatch(context.Context) {
	s.notifications.Add(1)
}

type blockingPacker struct {
	calls         atomic.Int32
	active        atomic.Int32
	maxConcurrent atomic.Int32
	canceled      atomic.Int32
	release       chan PackResult
	mu            sync.Mutex
	seenBudgets   []int64
	backlog       LooseBacklog
	backlogCalls  atomic.Int32
}

type interleavedBacklogPacker struct {
	queryStarted chan struct{}
	releaseQuery chan struct{}
	queries      atomic.Int32
	packCalls    atomic.Int32
}

func newInterleavedBacklogPacker() *interleavedBacklogPacker {
	return &interleavedBacklogPacker{
		queryStarted: make(chan struct{}),
		releaseQuery: make(chan struct{}),
	}
}

func (p *interleavedBacklogPacker) Pack(context.Context, int64) (PackResult, error) {
	p.packCalls.Add(1)
	return PackResult{}, nil
}

func (p *interleavedBacklogPacker) LooseBacklog(
	ctx context.Context,
) (LooseBacklog, error) {
	if p.queries.Add(1) == 1 {
		close(p.queryStarted)
		select {
		case <-p.releaseQuery:
		case <-ctx.Done():
			return LooseBacklog{}, ctx.Err()
		}
	}
	return LooseBacklog{}, nil
}

func newBlockingPacker() *blockingPacker {
	return &blockingPacker{release: make(chan PackResult, 4)}
}

func (p *blockingPacker) Pack(ctx context.Context, maxBytes int64) (PackResult, error) {
	p.calls.Add(1)
	active := p.active.Add(1)
	for {
		maximum := p.maxConcurrent.Load()
		if active <= maximum || p.maxConcurrent.CompareAndSwap(maximum, active) {
			break
		}
	}
	defer p.active.Add(-1)
	p.mu.Lock()
	p.seenBudgets = append(p.seenBudgets, maxBytes)
	p.mu.Unlock()
	select {
	case result := <-p.release:
		return result, nil
	case <-ctx.Done():
		p.canceled.Add(1)
		return PackResult{}, ctx.Err()
	}
}

func (p *blockingPacker) budgets() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.seenBudgets...)
}

func (p *blockingPacker) LooseBacklog(context.Context) (LooseBacklog, error) {
	p.backlogCalls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.backlog, nil
}

func (p *blockingPacker) setBacklog(backlog LooseBacklog) {
	p.mu.Lock()
	p.backlog = backlog
	p.mu.Unlock()
}

type cardinalityPacker struct {
	called   chan struct{}
	calls    atomic.Int32
	maxBytes atomic.Int64
}

type artifactExportCallStats struct {
	pending  int
	claims   int
	sessions int
	messages int
	usage    int
	owned    int
}

type countingArtifactExportDB struct {
	artifactExportStore
	stats artifactExportCallStats
}

func (d *countingArtifactExportDB) PendingArtifactExports(
	ctx context.Context, limit int,
) ([]db.ArtifactExportQueueItem, error) {
	d.stats.pending++
	return d.artifactExportStore.PendingArtifactExports(ctx, limit)
}

func (d *countingArtifactExportDB) ArtifactExportClaims(
	ctx context.Context, ids []string,
) ([]db.ArtifactExportQueueItem, error) {
	d.stats.claims++
	return d.artifactExportStore.ArtifactExportClaims(ctx, ids)
}

func (d *countingArtifactExportDB) GetSessionFull(
	ctx context.Context, id string,
) (*db.Session, error) {
	d.stats.sessions++
	return d.artifactExportStore.GetSessionFull(ctx, id)
}

func (d *countingArtifactExportDB) GetAllMessages(
	ctx context.Context, id string,
) ([]db.Message, error) {
	d.stats.messages++
	return d.artifactExportStore.GetAllMessages(ctx, id)
}

func (d *countingArtifactExportDB) GetUsageEvents(
	ctx context.Context, id string,
) ([]db.UsageEvent, error) {
	d.stats.usage++
	return d.artifactExportStore.GetUsageEvents(ctx, id)
}

func (d *countingArtifactExportDB) ListOwnedSessionIDsForExport(
	ctx context.Context,
) ([]string, error) {
	d.stats.owned++
	return d.artifactExportStore.ListOwnedSessionIDsForExport(ctx)
}

func exportNotificationCardinalityStats(t *testing.T, archiveSize int) artifactExportCallStats {
	t.Helper()
	database := testDB(t)
	const batchSize = 1_000
	for start := 0; start < archiveSize-1; start += batchSize {
		end := min(start+batchSize, archiveSize-1)
		writes := make([]db.SessionBatchWrite, 0, end-start)
		for index := start; index < end; index++ {
			writes = append(writes, db.SessionBatchWrite{
				Session: db.Session{
					ID: fmt.Sprintf("unrelated-%05d", index), Project: "archive",
					Machine: "remote-origin", Agent: "claude",
				},
				ReplaceMessages: true,
			})
		}
		result, err := database.WriteSessionBatch(writes)
		require.NoError(t, err)
		assert.Equal(t, len(writes), result.WrittenSessions)
	}
	seedSession(t, database, "changed-session", "project")
	countingDB := &countingArtifactExportDB{artifactExportStore: database}
	store := &notificationStore{ArtifactStore: newRetentionStore(t)}

	result, err := ExportToStore(t.Context(), countingDB, store, ExportOptions{
		Origin: retentionOrigin, SessionIDs: []string{"changed-session"},
	})

	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)
	assert.Equal(t, int32(1), store.notifications.Load())
	return countingDB.stats
}

func (p *cardinalityPacker) Pack(_ context.Context, maxBytes int64) (PackResult, error) {
	p.calls.Add(1)
	p.maxBytes.Store(maxBytes)
	select {
	case p.called <- struct{}{}:
	default:
	}
	return PackResult{}, nil
}

func (p *cardinalityPacker) LooseBacklog(context.Context) (LooseBacklog, error) {
	return LooseBacklog{}, nil
}

func TestRunPhysicalMaintenanceSeparatesStagesAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	maintainer := &recordingMaintainer{cancel: cancel}
	result, err := RunPhysicalMaintenance(ctx, maintainer, PhysicalMaintenanceOptions{
		TrashGrace: time.Hour,
		EmptyTrash: WorkBudget{MaxObjects: 2},
		GC:         WorkBudget{MaxObjects: 3, MaxBytes: 4},
		Repack:     WorkBudget{MaxObjects: 5, MaxBytes: 6},
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []string{"empty"}, maintainer.calls)
	assert.Equal(t, MaintenanceResult{Processed: 1}, result.EmptyTrash)
	assert.Zero(t, result.GarbageCollect.Processed)
	assert.Zero(t, result.Repack.Processed)
}

func TestRunPhysicalMaintenanceRejectsUnsupportedAndInvalidBudgets(t *testing.T) {
	_, err := RunPhysicalMaintenance(t.Context(), nil, PhysicalMaintenanceOptions{})
	assert.ErrorIs(t, err, ErrArtifactUnsupported)
	_, err = RunPhysicalMaintenance(t.Context(), &recordingMaintainer{}, PhysicalMaintenanceOptions{
		TrashGrace: -time.Second,
	})
	assert.ErrorIs(t, err, ErrArtifactInvalid)
	_, err = RunPhysicalMaintenance(t.Context(), &recordingMaintainer{}, PhysicalMaintenanceOptions{
		GC: WorkBudget{MaxObjects: -1},
	})
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

type recordingMaintainer struct {
	calls  []string
	cancel context.CancelFunc
	err    error
}

func (m *recordingMaintainer) Verify(context.Context, WorkBudget) (MaintenanceResult, error) {
	return MaintenanceResult{}, errors.New("unexpected verify")
}

func (m *recordingMaintainer) EmptyTrash(
	_ context.Context, grace time.Duration, budget WorkBudget,
) (MaintenanceResult, error) {
	m.calls = append(m.calls, "empty")
	if grace != time.Hour || budget.MaxObjects != 2 {
		return MaintenanceResult{}, errors.New("wrong empty-trash options")
	}
	if m.cancel != nil {
		m.cancel()
	}
	return MaintenanceResult{Processed: 1}, m.err
}

func (m *recordingMaintainer) GarbageCollect(
	context.Context, WorkBudget,
) (MaintenanceResult, error) {
	m.calls = append(m.calls, "gc")
	return MaintenanceResult{Processed: 2}, m.err
}

func (m *recordingMaintainer) Repack(
	context.Context, WorkBudget,
) (MaintenanceResult, error) {
	m.calls = append(m.calls, "repack")
	return MaintenanceResult{Processed: 3}, m.err
}
