package poller

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startTestScheduler(t *testing.T, jobs ...Job) *Scheduler {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	s := Start(ctx, jobs...)
	t.Cleanup(func() {
		cancel()
		s.Wait()
	})
	return s
}

func TestBackoffCapsAndRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := time.Now()
		var calls atomic.Int32
		s := startTestScheduler(t, Job{
			Name: "catalog", Interval: 16 * time.Minute, RunAtStart: true,
			Run: func(context.Context) error {
				if calls.Add(1) <= 6 {
					return errors.New("network unavailable")
				}
				return nil
			},
		})
		for i, step := range []struct{ at, next, failures int }{
			{0, 1, 1},
			{1, 3, 2},
			{3, 7, 3},
			{7, 15, 4},
			{15, 31, 5},
			{31, 47, 6},
			{47, 63, 0},
		} {
			time.Sleep(time.Until(base.Add(time.Duration(step.at) * time.Minute)))
			synctest.Wait()
			assert.Equal(t, int32(i+1), calls.Load())
			status := s.Status()[0]
			assert.Equal(t, step.failures, status.ConsecutiveFailures)
			assert.Equal(t, base.Add(time.Duration(step.at)*time.Minute), status.LastAttempt)
			assert.Equal(t, base.Add(time.Duration(step.next)*time.Minute), status.NextRun)
		}
		assert.Empty(t, s.Status()[0].LastError)
		assert.Equal(t, base.Add(47*time.Minute), s.Status()[0].LastSuccess)
	})
}

func TestCooldownAndManualOverride(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := time.Now()
		var calls atomic.Int32
		s := startTestScheduler(t, Job{
			Name: "catalog", Interval: time.Minute,
			Cooldown: 10 * time.Minute, RunAtStart: true,
			Run: func(context.Context) error {
				if calls.Add(1) == 1 {
					return errors.New("network unavailable")
				}
				return nil
			},
		})
		synctest.Wait()
		time.Sleep(9 * time.Minute)
		synctest.Wait()
		assert.Equal(t, int32(1), calls.Load(), "failed attempts still start cooldown")
		assert.Equal(t, base.Add(10*time.Minute), s.Status()[0].NextRun)

		require.NoError(t, s.TriggerNow("catalog"))
		assert.Equal(t, int32(2), calls.Load())
		time.Sleep(9 * time.Minute)
		synctest.Wait()
		assert.Equal(t, int32(2), calls.Load(), "successful manual attempts also start cooldown")
		time.Sleep(time.Minute)
		synctest.Wait()
		assert.Equal(t, int32(3), calls.Load())
	})
}

func TestRetryAfterOverridesBackoffAndJitter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := time.Now()
		var calls atomic.Int32
		retry := &RetryAfterError{RetryAfter: 3 * time.Minute, Err: errors.New("rate limited")}
		s := startTestScheduler(t, Job{
			Name: "vendor", Interval: time.Hour, Jitter: 30 * time.Second,
			Run: func(context.Context) error {
				calls.Add(1)
				return fmt.Errorf("vendor response: %w", retry)
			},
		})
		require.ErrorIs(t, s.TriggerNow("vendor"), retry)
		assert.Equal(t, base.Add(3*time.Minute), s.Status()[0].NextRun)
		time.Sleep(3*time.Minute - time.Nanosecond)
		synctest.Wait()
		assert.Equal(t, int32(1), calls.Load())
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		assert.Equal(t, int32(2), calls.Load())
	})
}

func TestJitterAndIndependentJobs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := time.Now()
		var scheduledCalls atomic.Int32
		s := startTestScheduler(t,
			Job{Name: "scheduled", Interval: time.Hour, Jitter: 30 * time.Second,
				Run: func(context.Context) error {
					scheduledCalls.Add(1)
					return nil
				}},
			Job{Name: "manual", Interval: time.Hour, Run: func(context.Context) error { return nil }},
		)
		synctest.Wait()
		statuses := s.Status()
		require.Len(t, statuses, 2)
		assert.Equal(t, "scheduled", statuses[0].Name)
		assert.Equal(t, "manual", statuses[1].Name)
		next := statuses[0].NextRun
		assert.GreaterOrEqual(t, next.Sub(base), time.Hour)
		assert.Less(t, next.Sub(base), time.Hour+30*time.Second)
		require.NoError(t, s.TriggerNow("manual"))
		assert.Equal(t, base, s.Status()[1].LastSuccess)
		assert.Zero(t, scheduledCalls.Load())

		time.Sleep(time.Until(next))
		synctest.Wait()
		assert.Equal(t, int32(1), scheduledCalls.Load())
		assert.Equal(t, next, s.Status()[0].LastSuccess)
		delay := s.Status()[0].NextRun.Sub(next)
		assert.GreaterOrEqual(t, delay, time.Hour)
		assert.Less(t, delay, time.Hour+30*time.Second)
	})
}

func TestTriggersSerializeAndStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		release := make(chan struct{})
		var calls atomic.Int32
		s := Start(ctx, Job{
			Name: "vendor", Interval: time.Hour, Cooldown: time.Hour,
			Run: func(ctx context.Context) error {
				calls.Add(1)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		})
		t.Cleanup(func() {
			cancel()
			s.Wait()
		})
		results := make(chan error, 2)
		for range 2 {
			go func() { results <- s.TriggerNow("vendor") }()
			synctest.Wait()
		}
		assert.Equal(t, time.Now(), s.Status()[0].LastAttempt)
		assert.Equal(t, int32(1), calls.Load(), "pending trigger must not overlap Run")
		require.ErrorContains(t, s.TriggerNow("vendor"), "trigger pending")
		require.ErrorIs(t, s.TriggerNow("missing"), ErrUnknownJob)

		release <- struct{}{}
		synctest.Wait()
		require.NoError(t, <-results)
		assert.Equal(t, int32(2), calls.Load(), "queued call starts after the active call finishes")
		go func() { results <- s.TriggerNow("vendor") }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		for range 2 {
			require.ErrorIs(t, <-results, context.Canceled)
		}
		s.Wait()
		assert.Equal(t, int32(2), calls.Load(), "shutdown must not start the queued call")
		assert.ErrorIs(t, s.TriggerNow("vendor"), context.Canceled)
	})
}
