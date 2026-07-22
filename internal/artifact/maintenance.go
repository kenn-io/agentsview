package artifact

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	defaultPackPassBytes       = int64(256 << 20)
	defaultPackObjectThreshold = int64(512)
	defaultPackByteThreshold   = int64(64 << 20)
	defaultPackRetryDelay      = 30 * time.Second
)

type artifactPacker interface {
	Pack(context.Context, int64) (PackResult, error)
	LooseBacklog(context.Context) (LooseBacklog, error)
}

type packSchedulerOptions struct {
	ObjectThreshold int64
	ByteThreshold   int64
	RetryDelay      time.Duration
	Logf            func(string, ...any)
}

// packScheduler owns one optional background worker. Notifications are
// constant-work and coalesce in a one-slot channel; packing never runs on the
// caller's goroutine.
type packScheduler struct {
	packer artifactPacker
	logf   func(string, ...any)
	retry  time.Duration

	objectThreshold int64
	byteThreshold   int64
	objects         int64
	bytes           int64
	mu              sync.Mutex
	refreshMu       sync.Mutex
	refreshEpoch    uint64
	refreshing      bool
	refreshObjects  int64
	refreshBytes    int64

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}
	once   sync.Once
}

func newPackScheduler(packer artifactPacker, opts packSchedulerOptions) *packScheduler {
	retry := opts.RetryDelay
	if retry <= 0 {
		retry = defaultPackRetryDelay
	}
	ctx, cancel := context.WithCancel(context.Background())
	objectThreshold := opts.ObjectThreshold
	if objectThreshold <= 0 {
		objectThreshold = defaultPackObjectThreshold
	}
	byteThreshold := opts.ByteThreshold
	if byteThreshold <= 0 {
		byteThreshold = defaultPackByteThreshold
	}
	scheduler := &packScheduler{
		packer:          packer,
		logf:            opts.Logf,
		retry:           retry,
		objectThreshold: objectThreshold,
		byteThreshold:   byteThreshold,
		ctx:             ctx,
		cancel:          cancel,
		wake:            make(chan struct{}, 1),
		done:            make(chan struct{}),
	}
	go scheduler.run()
	return scheduler
}

// ObserveWrite accounts for a newly created physical loose representation.
// Empty receipts represent immutable-create retries and are ignored. The
// counters are updated without querying or walking the vault.
func (s *packScheduler) ObserveWrite(ctx context.Context, write PhysicalWrite) {
	if s == nil || s.packer == nil || artifactMaintenanceSuppressed(ctx) ||
		ctx.Err() != nil || write.Kind != "loose" || !write.PackEligible || write.StoredBytes < 0 {
		return
	}
	s.mu.Lock()
	s.objects = saturatingAdd(s.objects, 1)
	s.bytes = saturatingAdd(s.bytes, write.StoredBytes)
	if s.refreshing {
		s.refreshObjects = saturatingAdd(s.refreshObjects, 1)
		s.refreshBytes = saturatingAdd(s.refreshBytes, write.StoredBytes)
	}
	shouldPack := s.objects >= s.objectThreshold || s.bytes >= s.byteThreshold
	s.mu.Unlock()
	if shouldPack {
		s.notify()
	}
}

// Recover seeds threshold counters from Docbank's single indexed backlog
// aggregate. It does not enumerate logical nodes or physical files.
func (s *packScheduler) Recover(ctx context.Context) error {
	if s == nil || s.packer == nil {
		return nil
	}
	shouldPack, err := s.refreshBacklog(ctx)
	if err != nil {
		return err
	}
	if shouldPack {
		s.notify()
	}
	return nil
}

func saturatingAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func (s *packScheduler) refreshBacklog(ctx context.Context) (bool, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	epoch := s.beginBacklogRefresh()
	backlog, err := s.packer.LooseBacklog(ctx)
	if err != nil {
		s.cancelBacklogRefresh(epoch)
		return false, err
	}
	return s.finishBacklogRefresh(epoch, backlog), nil
}

func (s *packScheduler) beginBacklogRefresh() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshEpoch++
	s.refreshing = true
	s.refreshObjects = 0
	s.refreshBytes = 0
	return s.refreshEpoch
}

func (s *packScheduler) cancelBacklogRefresh(epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.refreshing || s.refreshEpoch != epoch {
		return
	}
	s.refreshing = false
	s.refreshObjects = 0
	s.refreshBytes = 0
}

func (s *packScheduler) finishBacklogRefresh(epoch uint64, backlog LooseBacklog) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.refreshing || s.refreshEpoch != epoch {
		return s.objects >= s.objectThreshold || s.bytes >= s.byteThreshold
	}
	s.objects = saturatingAdd(max(backlog.EligibleObjects, 0), s.refreshObjects)
	s.bytes = saturatingAdd(max(backlog.EligibleStoredBytes, 0), s.refreshBytes)
	s.refreshing = false
	s.refreshObjects = 0
	s.refreshBytes = 0
	return s.objects >= s.objectThreshold || s.bytes >= s.byteThreshold
}

func (s *packScheduler) Notify(ctx context.Context) {
	if s == nil || s.packer == nil || artifactMaintenanceSuppressed(ctx) {
		return
	}
	s.notify()
}

func (s *packScheduler) notify() {
	select {
	case <-s.ctx.Done():
		return
	default:
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *packScheduler) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.cancel()
		<-s.done
	})
}

func (s *packScheduler) run() {
	defer close(s.done)
	var timer *time.Timer
	var retry <-chan time.Time
	for {
		select {
		case <-s.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-s.wake:
			if timer != nil {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				retry = nil
			}
		case <-retry:
			retry = nil
		}

		if err := s.ctx.Err(); err != nil {
			return
		}
		result, err := s.packer.Pack(s.ctx, defaultPackPassBytes)
		if err != nil {
			if !errors.Is(err, context.Canceled) && s.logf != nil {
				s.logf("artifact pack: %v", err)
			}
			continue
		}
		_, backlogErr := s.refreshBacklog(s.ctx)
		if backlogErr != nil {
			if !errors.Is(backlogErr, context.Canceled) && s.logf != nil {
				s.logf("artifact pack backlog: %v", backlogErr)
			}
		}
		if result.More {
			if timer == nil {
				timer = time.NewTimer(s.retry)
			} else {
				timer.Reset(s.retry)
			}
			retry = timer.C
		}
	}
}

type suppressArtifactMaintenanceKey struct{}

// ArtifactBatchNotifier is implemented by process-owned stores with an
// asynchronous end-of-batch pack scheduler.
type ArtifactBatchNotifier interface {
	NotifyArtifactBatch(context.Context)
}

type artifactPackingRecoverer interface {
	recoverArtifactPacking(context.Context) error
}

// RecoverArtifactPacking seeds the process-owned scheduler from the store's
// indexed physical backlog. Repository decorators are unwrapped without
// exposing their backend or broadening the ArtifactStore contract.
func RecoverArtifactPacking(ctx context.Context, store ArtifactStore) error {
	if store == nil {
		return fmt.Errorf("%w: artifact store is required", ErrArtifactInvalid)
	}
	if repository, ok := store.(*repositoryStore); ok {
		return RecoverArtifactPacking(ctx, repository.ArtifactStore)
	}
	recoverer, ok := store.(artifactPackingRecoverer)
	if !ok {
		return ErrArtifactUnsupported
	}
	return recoverer.recoverArtifactPacking(ctx)
}

// NotifyArtifactBatch performs only a constant-work scheduler notification.
func NotifyArtifactBatch(ctx context.Context, store ArtifactStore) {
	if store == nil || artifactMaintenanceSuppressed(ctx) {
		return
	}
	if notifier, ok := store.(ArtifactBatchNotifier); ok {
		notifier.NotifyArtifactBatch(ctx)
	}
}

// SuppressArtifactMaintenance marks a shutdown-flush context. Required export
// and exchange work proceeds, but successful completion cannot start optional
// physical packing after the flush budget has begun.
func SuppressArtifactMaintenance(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, suppressArtifactMaintenanceKey{}, true)
}

func artifactMaintenanceSuppressed(ctx context.Context) bool {
	return ctx != nil && ctx.Value(suppressArtifactMaintenanceKey{}) == true
}

// ArtifactMaintenanceOptions keeps semantic deletion separate from bounded
// Docbank reclamation. Each stage performs at most one resumable pass.
type ArtifactMaintenanceOptions struct {
	TrashGrace time.Duration
	EmptyTrash WorkBudget
	GC         WorkBudget
	Repack     WorkBudget
}

// PhysicalMaintenanceOptions is retained for source compatibility.
type PhysicalMaintenanceOptions = ArtifactMaintenanceOptions

// PhysicalMaintenanceResult reports independently resumable physical stages.
type PhysicalMaintenanceResult struct {
	EmptyTrash     MaintenanceResult
	GarbageCollect MaintenanceResult
	Repack         MaintenanceResult
}

// ValidateArtifactMaintenanceOptions validates every caller-controlled
// physical maintenance limit before semantic retention can mutate the store.
func ValidateArtifactMaintenanceOptions(opts ArtifactMaintenanceOptions) error {
	if opts.TrashGrace < 0 {
		return fmt.Errorf(
			"%w: physical trash grace must not be negative", ErrArtifactInvalid)
	}
	for _, budget := range []WorkBudget{opts.EmptyTrash, opts.GC, opts.Repack} {
		if err := validateArtifactWorkBudget(budget); err != nil {
			return err
		}
	}
	if opts.EmptyTrash.MaxBytes != 0 {
		return fmt.Errorf(
			"%w: trash emptying supports only an object budget", ErrArtifactInvalid)
	}
	return nil
}

// RunPhysicalMaintenance performs one bounded pass of each physical stage.
// It never decides logical liveness and stops between stages on cancellation.
func RunPhysicalMaintenance(
	ctx context.Context,
	maintainer ArtifactMaintainer,
	opts ArtifactMaintenanceOptions,
) (PhysicalMaintenanceResult, error) {
	if maintainer == nil {
		return PhysicalMaintenanceResult{}, ErrArtifactUnsupported
	}
	if err := ValidateArtifactMaintenanceOptions(opts); err != nil {
		return PhysicalMaintenanceResult{}, err
	}
	var result PhysicalMaintenanceResult
	var err error
	result.EmptyTrash, err = maintainer.EmptyTrash(ctx, opts.TrashGrace, opts.EmptyTrash)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.GarbageCollect, err = maintainer.GarbageCollect(ctx, opts.GC)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.Repack, err = maintainer.Repack(ctx, opts.Repack)
	return result, errors.Join(err, ctx.Err())
}
