package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestLoop wires a pushLoop with caller-controlled timers.
func newTestLoop(push func(context.Context, pushReason) error) (
	*pushLoop, chan time.Time, chan time.Time,
) {
	fire := make(chan time.Time, 1)
	floor := make(chan time.Time, 1)
	l := &pushLoop{
		debounce: time.Minute, // irrelevant; after is stubbed
		dirty:    make(chan struct{}, 1),
		floor:    floor,
		after:    func(time.Duration) <-chan time.Time { return fire },
		push:     push,
	}
	return l, fire, floor
}

func TestPushLoop_DirtyTriggersOnePush(t *testing.T) {
	pushed := make(chan pushReason, 4)
	l, fire, _ := newTestLoop(func(_ context.Context, r pushReason) error {
		pushed <- r
		return nil
	})
	ctx := t.Context()
	go l.Run(ctx)

	l.NotifyDirty()
	fire <- time.Now()

	select {
	case r := <-pushed:
		if r != reasonChange {
			t.Fatalf("reason = %q, want %q", r, reasonChange)
		}
	case <-time.After(time.Second):
		t.Fatal("expected a push")
	}
}

func TestPushLoop_BurstCoalesces(t *testing.T) {
	pushed := make(chan pushReason, 8)
	l, fire, _ := newTestLoop(func(_ context.Context, r pushReason) error {
		pushed <- r
		return nil
	})
	ctx := t.Context()
	go l.Run(ctx)

	// Many dirty signals before the timer fires -> one push.
	for range 5 {
		l.NotifyDirty()
	}
	fire <- time.Now()

	select {
	case <-pushed:
	case <-time.After(time.Second):
		t.Fatal("expected a push")
	}
	select {
	case <-pushed:
		t.Fatal("expected exactly one push for a burst")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPushLoop_FloorPushesWithoutDirty(t *testing.T) {
	pushed := make(chan pushReason, 4)
	l, _, floor := newTestLoop(func(_ context.Context, r pushReason) error {
		pushed <- r
		return nil
	})
	ctx := t.Context()
	go l.Run(ctx)

	floor <- time.Now()

	select {
	case r := <-pushed:
		if r != reasonInterval {
			t.Fatalf("reason = %q, want %q", r, reasonInterval)
		}
	case <-time.After(time.Second):
		t.Fatal("expected an interval push")
	}
}

func TestPushLoop_ErrorDoesNotStopLoop(t *testing.T) {
	pushed := make(chan pushReason, 4)
	calls := 0
	l, fire, _ := newTestLoop(func(_ context.Context, r pushReason) error {
		calls++
		pushed <- r
		if calls == 1 {
			return errors.New("pg down")
		}
		return nil
	})
	ctx := t.Context()
	go l.Run(ctx)

	l.NotifyDirty()
	fire <- time.Now()
	<-pushed // first (errored); also synchronizes the loop draining fire before the next send

	l.NotifyDirty()
	fire <- time.Now()
	select {
	case <-pushed: // second succeeds -> loop survived the error
	case <-time.After(time.Second):
		t.Fatal("loop did not survive a push error")
	}
}

func TestPushLoop_NotifyDirtyWithAckWaitsForSuccessfulRetry(t *testing.T) {
	attempts := make(chan pushReason, 2)
	pushErr := errors.New("target unavailable")
	call := 0
	l, fire, _ := newTestLoop(func(_ context.Context, reason pushReason) error {
		call++
		attempts <- reason
		if call == 1 {
			return pushErr
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)

	ack := l.NotifyDirtyWithAck()
	fire <- time.Now()
	require.Equal(t, reasonChange, <-attempts)
	select {
	case err := <-ack:
		require.Fail(t, "failed push acknowledged dirty generation", "%v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Failure retains the dirty generation and rearms debounce without a
	// second producer notification.
	fire <- time.Now()
	require.Equal(t, reasonChange, <-attempts)
	require.NoError(t, <-ack)
}

func TestPushLoop_NotifyDirtyWithAckIsNonBlockingAndCoalescesWaiters(t *testing.T) {
	l, fire, _ := newTestLoop(func(context.Context, pushReason) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)

	first := l.NotifyDirtyWithAck()
	second := l.NotifyDirtyWithAck()
	assert.NotNil(t, first)
	assert.NotNil(t, second)
	fire <- time.Now()
	require.NoError(t, <-first)
	require.NoError(t, <-second)
}

func TestPushWatchFallbackCoverageMarksLoopDirty(t *testing.T) {
	loop, _, _ := newTestLoop(func(context.Context, pushReason) error { return nil })

	require.NoError(t,
		loop.NotifyCoverageDegraded([]string{"/root-a", "/root-a", "/root-b"}))
	pending, waiters := func() (bool, int) {
		loop.pendingMu.Lock()
		defer loop.pendingMu.Unlock()
		return loop.pending, len(loop.waiters)
	}()
	assert.True(t, pending,
		"coverage degradation must mark the loop dirty for the next push")
	assert.Zero(t, waiters,
		"coverage degradation must not enqueue ack waiters")
}

func TestPushLoop_ShutdownFlushes(t *testing.T) {
	pushed := make(chan pushReason, 4)
	l, _, _ := newTestLoop(func(_ context.Context, r pushReason) error {
		pushed <- r
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	go l.Run(ctx)

	cancel()
	select {
	case r := <-pushed:
		if r != reasonShutdown {
			t.Fatalf("reason = %q, want %q", r, reasonShutdown)
		}
	case <-time.After(time.Second):
		t.Fatal("expected a shutdown flush push")
	}
}

func TestPushLoop_ShutdownFlushHonorsTimeout(t *testing.T) {
	gotDeadline := make(chan bool, 1)
	l, _, _ := newTestLoop(func(ctx context.Context, _ pushReason) error {
		_, ok := ctx.Deadline()
		gotDeadline <- ok
		return nil
	})
	l.flushTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go l.Run(ctx)
	cancel()

	select {
	case ok := <-gotDeadline:
		if !ok {
			t.Fatal("shutdown flush ctx should carry a deadline when flushTimeout > 0")
		}
	case <-time.After(time.Second):
		t.Fatal("expected a shutdown flush push")
	}
}
