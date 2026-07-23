package artifact

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank"
)

func TestDocbankPhysicalMaintenanceRunsBoundedPhysicalStages(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	maintainer, ok := any(store).(artifactMaintainer)
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
	collected, err := maintainer.GarbageCollect(t.Context(), WorkBudget{MaxObjects: 1, MaxBytes: 1 << 20})
	require.NoError(t, err)
	assert.Positive(t, collected.Processed)
	_, err = maintainer.Repack(t.Context(), WorkBudget{MaxObjects: 1, MaxBytes: 1 << 20})
	require.NoError(t, err)
}

func TestDocbankPhysicalMaintenanceResumesStages(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	maintainer := any(store).(artifactMaintainer)
	for i := range 3 {
		body := fmt.Appendf(nil, "trash-%d", i)
		ref := createRetentionRef(t, store, Ref{
			Origin: retentionOrigin, Kind: KindRaw, Name: hashHex(body),
		}, body)
		require.NoError(t, store.Trash(t.Context(), ref))
	}
	trash, err := maintainer.EmptyTrash(t.Context(), 0, WorkBudget{MaxObjects: 1})
	require.NoError(t, err)
	passes := 1
	for trash.More {
		trash, err = maintainer.EmptyTrash(t.Context(), 0, WorkBudget{
			MaxObjects: 1, Cursor: trash.NextCursor,
		})
		require.NoError(t, err)
		passes++
	}
	assert.Equal(t, 3, passes)

	collected, err := maintainer.GarbageCollect(t.Context(), WorkBudget{MaxObjects: 1})
	require.NoError(t, err)
	for collected.More {
		collected, err = maintainer.GarbageCollect(t.Context(), WorkBudget{
			MaxObjects: 1, Cursor: collected.NextCursor,
		})
		require.NoError(t, err)
	}
}

func TestDocbankPhysicalMaintenanceRejectsInvalidBudget(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	maintainer := any(store).(artifactMaintainer)
	_, err := maintainer.GarbageCollect(t.Context(), WorkBudget{
		MaxObjects: docbank.MaxMaintenanceObjects + 1,
	})
	assert.ErrorIs(t, err, ErrArtifactInvalid)
	assert.ErrorIs(t, mapDocbankError(docbank.ErrStaleRevision), ErrArtifactConflict)
}

func TestDocbankRepackProcessedIncludesEveryMaintenanceAction(t *testing.T) {
	report := docbank.RepackReport{
		MappingsPruned: 2, PacksSelected: 3, PacksRewritten: 5,
		PacksSealed: 7, PacksRemoved: 11, PacksDeferredOversized: 13,
		BlobsRepacked: 17,
	}
	assert.Equal(t, 58, docbankRepackProcessed(report))
}

func TestPackSchedulerCoalescesBatchNotifications(t *testing.T) {
	packer := newBlockingPacker()
	scheduler := newPackScheduler(packer, packSchedulerOptions{RetryDelay: time.Hour})
	t.Cleanup(scheduler.Close)

	returned := make(chan struct{})
	go func() {
		scheduler.Notify(t.Context())
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		require.Fail(t, "Notify blocked on physical packing")
	}
	require.Eventually(t, func() bool { return packer.calls.Load() == 1 }, time.Second, time.Millisecond)
	for range 1_000 {
		scheduler.Notify(t.Context())
	}
	assert.Equal(t, int32(1), packer.maxConcurrent.Load())
	packer.release <- PackResult{}
	require.Eventually(t, func() bool { return packer.calls.Load() == 2 }, time.Second, time.Millisecond)
	packer.release <- PackResult{}
	assert.Never(t, func() bool { return packer.calls.Load() > 2 }, 20*time.Millisecond, time.Millisecond)
	assert.Equal(t, []int64{defaultPackPassBytes, defaultPackPassBytes}, packer.budgets())
}

func TestPackSchedulerRecoversIndexedBacklog(t *testing.T) {
	packer := newBlockingPacker()
	packer.setBacklog(LooseBacklog{EligibleObjects: 1, EligibleStoredBytes: 1})
	scheduler := newPackScheduler(packer, packSchedulerOptions{RetryDelay: time.Hour})
	t.Cleanup(scheduler.Close)
	require.NoError(t, scheduler.Recover(t.Context()))
	assert.Equal(t, int32(1), packer.backlogCalls.Load())
	require.Eventually(t, func() bool { return packer.calls.Load() == 1 }, time.Second, time.Millisecond)
	packer.release <- PackResult{}
}

func TestPackSchedulerRetriesMoreAndCloseCancels(t *testing.T) {
	packer := newBlockingPacker()
	scheduler := newPackScheduler(packer, packSchedulerOptions{RetryDelay: 10 * time.Millisecond})
	scheduler.Notify(t.Context())
	require.Eventually(t, func() bool { return packer.calls.Load() == 1 }, time.Second, time.Millisecond)
	packer.release <- PackResult{More: true}
	require.Eventually(t, func() bool { return packer.calls.Load() == 2 }, time.Second, time.Millisecond)
	scheduler.Close()
	assert.Eventually(t, func() bool { return packer.canceled.Load() == 1 }, time.Second, time.Millisecond)
}

func TestPackSchedulerSuppressedContextDoesNotStartWork(t *testing.T) {
	packer := newBlockingPacker()
	scheduler := newPackScheduler(packer, packSchedulerOptions{RetryDelay: time.Hour})
	t.Cleanup(scheduler.Close)
	scheduler.Notify(SuppressArtifactMaintenance(t.Context()))
	assert.Never(t, func() bool { return packer.calls.Load() != 0 }, 20*time.Millisecond, time.Millisecond)
}

func TestExportNotificationWorkDoesNotScaleWithArchiveCardinality(t *testing.T) {
	small := exportNotificationCardinalityStats(t, 10)
	large := exportNotificationCardinalityStats(t, 1_000)
	want := artifactExportCallStats{claims: 1, sessions: 1, messages: 1, usage: 1}
	assert.Equal(t, want, small)
	assert.Equal(t, want, large)
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

func newBlockingPacker() *blockingPacker {
	return &blockingPacker{release: make(chan PackResult, 4)}
}

func (p *blockingPacker) Pack(ctx context.Context, maxBytes int64) (PackResult, error) {
	p.calls.Add(1)
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for maximum := p.maxConcurrent.Load(); active > maximum; maximum = p.maxConcurrent.Load() {
		if p.maxConcurrent.CompareAndSwap(maximum, active) {
			break
		}
	}
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

func (p *blockingPacker) LooseBacklog(context.Context) (LooseBacklog, error) {
	p.backlogCalls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.backlog, nil
}

func (p *blockingPacker) budgets() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.seenBudgets...)
}

func (p *blockingPacker) setBacklog(backlog LooseBacklog) {
	p.mu.Lock()
	p.backlog = backlog
	p.mu.Unlock()
}

type artifactExportCallStats struct {
	pending, claims, sessions, messages, usage, owned int
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

func (d *countingArtifactExportDB) GetSessionFull(ctx context.Context, id string) (*db.Session, error) {
	d.stats.sessions++
	return d.artifactExportStore.GetSessionFull(ctx, id)
}

func (d *countingArtifactExportDB) GetAllMessages(ctx context.Context, id string) ([]db.Message, error) {
	d.stats.messages++
	return d.artifactExportStore.GetAllMessages(ctx, id)
}

func (d *countingArtifactExportDB) GetUsageEvents(ctx context.Context, id string) ([]db.UsageEvent, error) {
	d.stats.usage++
	return d.artifactExportStore.GetUsageEvents(ctx, id)
}

func (d *countingArtifactExportDB) ListOwnedSessionIDsForExport(ctx context.Context) ([]string, error) {
	d.stats.owned++
	return d.artifactExportStore.ListOwnedSessionIDsForExport(ctx)
}

func exportNotificationCardinalityStats(t *testing.T, archiveSize int) artifactExportCallStats {
	t.Helper()
	database := testDB(t)
	for index := range archiveSize - 1 {
		_, err := database.WriteSessionBatch([]db.SessionBatchWrite{{
			Session: db.Session{
				ID: fmt.Sprintf("unrelated-%05d", index), Project: "archive",
				Machine: "remote-origin", Agent: "claude",
			},
			ReplaceMessages: true,
		}})
		require.NoError(t, err)
	}
	seedSession(t, database, "changed-session", "project")
	countingDB := &countingArtifactExportDB{artifactExportStore: database}
	_, err := ExportToStore(t.Context(), countingDB, newRetentionStore(t), ExportOptions{
		Origin: retentionOrigin, SessionIDs: []string{"changed-session"},
	})
	require.NoError(t, err)
	return countingDB.stats
}

func TestRunPhysicalMaintenanceStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	maintainer := &recordingMaintainer{cancel: cancel}
	result, err := runPhysicalMaintenance(ctx, maintainer, ArtifactMaintenanceOptions{
		TrashGrace: time.Hour,
		EmptyTrash: WorkBudget{MaxObjects: 2},
		GC:         WorkBudget{MaxObjects: 3, MaxBytes: 4},
		Repack:     WorkBudget{MaxObjects: 5, MaxBytes: 6},
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []string{"empty"}, maintainer.calls)
	assert.Equal(t, MaintenanceResult{Processed: 1}, result.EmptyTrash)
}

func TestRunPhysicalMaintenanceRejectsInvalidInputs(t *testing.T) {
	_, err := runPhysicalMaintenance(t.Context(), nil, ArtifactMaintenanceOptions{})
	assert.ErrorIs(t, err, ErrArtifactUnsupported)
	_, err = runPhysicalMaintenance(t.Context(), &recordingMaintainer{}, ArtifactMaintenanceOptions{
		TrashGrace: -time.Second,
	})
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

type recordingMaintainer struct {
	calls  []string
	cancel context.CancelFunc
}

func (m *recordingMaintainer) Verify(context.Context, WorkBudget) (MaintenanceResult, error) {
	return MaintenanceResult{}, errors.New("unexpected verify")
}
func (m *recordingMaintainer) EmptyTrash(
	context.Context, time.Duration, WorkBudget,
) (MaintenanceResult, error) {
	m.calls = append(m.calls, "empty")
	if m.cancel != nil {
		m.cancel()
	}
	return MaintenanceResult{Processed: 1}, nil
}
func (m *recordingMaintainer) GarbageCollect(context.Context, WorkBudget) (MaintenanceResult, error) {
	m.calls = append(m.calls, "gc")
	return MaintenanceResult{Processed: 2}, nil
}
func (m *recordingMaintainer) Repack(context.Context, WorkBudget) (MaintenanceResult, error) {
	m.calls = append(m.calls, "repack")
	return MaintenanceResult{Processed: 3}, nil
}
