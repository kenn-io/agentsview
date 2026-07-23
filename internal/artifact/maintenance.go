package artifact

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	defaultPackPassBytes  = int64(256 << 20)
	defaultPackRetryDelay = 30 * time.Second
)

type artifactPacker interface {
	Pack(context.Context, int64) (PackResult, error)
	LooseBacklog(context.Context) (LooseBacklog, error)
}

type packSchedulerOptions struct {
	RetryDelay time.Duration
	Logf       func(string, ...any)
}

// packScheduler owns one optional background worker. Notifications are
// constant-work and coalesce in a one-slot channel; packing never runs on the
// caller's goroutine.
type packScheduler struct {
	packer artifactPacker
	logf   func(string, ...any)
	retry  time.Duration

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
	scheduler := &packScheduler{
		packer: packer,
		logf:   opts.Logf,
		retry:  retry,
		ctx:    ctx,
		cancel: cancel,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	go scheduler.run()
	return scheduler
}

// Recover schedules a pass when Docbank reports any pack-eligible backlog. It
// uses Docbank's aggregate and never enumerates logical nodes or loose files.
func (s *packScheduler) Recover(ctx context.Context) error {
	if s == nil || s.packer == nil {
		return nil
	}
	backlog, err := s.packer.LooseBacklog(ctx)
	if err != nil {
		return err
	}
	if backlog.EligibleObjects > 0 || backlog.EligibleStoredBytes > 0 {
		s.notify()
	}
	return nil
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

type artifactMaintainer interface {
	Verify(context.Context, WorkBudget) (MaintenanceResult, error)
	EmptyTrash(context.Context, time.Duration, WorkBudget) (MaintenanceResult, error)
	GarbageCollect(context.Context, WorkBudget) (MaintenanceResult, error)
	Repack(context.Context, WorkBudget) (MaintenanceResult, error)
}

// runPhysicalMaintenance performs one bounded pass of each physical stage.
// It never decides logical liveness and stops between stages on cancellation.
func runPhysicalMaintenance(
	ctx context.Context,
	maintainer artifactMaintainer,
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
