package server

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBroadcaster_EmitFansOutToAllSubscribers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := NewBroadcaster(10 * time.Second)
		sub1, unsub1 := b.Subscribe()
		defer unsub1()
		sub2, unsub2 := b.Subscribe()
		defer unsub2()

		b.Emit("messages")

		for i, sub := range []<-chan Event{sub1, sub2} {
			ev := <-sub
			assert.Equal(t, "messages", ev.Scope, "sub %d", i)
		}
	})
}

func TestBroadcaster_EmitIsNonBlockingOnSlowSubscriber(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Disable rate limiting so every Emit attempts a broadcast, which
		// is what exercises the non-blocking select-default path against
		// a slow subscriber.
		b := NewBroadcaster(0)
		slow, unsub := b.Subscribe()
		defer unsub()

		// Don't read from slow. Fill its buffer + one extra; Emit must not block.
		const extra = 5
		done := make(chan struct{})
		go func() {
			for range broadcasterBufferCap + extra {
				b.Emit("messages")
			}
			close(done)
		}()
		synctest.Wait()
		<-done

		// Drain what we can — drop count >= extra, exact count not guaranteed.
		drained := 0
	drain:
		for {
			select {
			case <-slow:
				drained++
			default:
				break drain
			}
		}
		require.NotZero(t, drained, "slow subscriber received nothing")
	})
}

func TestBroadcaster_UnsubscribeStopsDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := NewBroadcaster(10 * time.Second)
		sub, unsub := b.Subscribe()
		unsub()

		b.Emit("messages")

		ev, ok := <-sub
		require.False(t, ok, "got event after unsubscribe: %v", ev)
	})
}

func TestBroadcaster_ConcurrentSubscribeAndEmit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Disable rate limiting so each subscriber's Emit reliably
		// produces a broadcast during the race.
		b := NewBroadcaster(0)
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				sub, unsub := b.Subscribe()
				defer unsub()
				b.Emit("sessions")
				<-sub
			})
		}
		wg.Wait()
	})
}

func TestBroadcaster_LeadingEdgeEmitsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := NewBroadcaster(time.Second)
		sub, unsub := b.Subscribe()
		defer unsub()

		b.Emit("messages")

		ev := <-sub
		assert.Equal(t, "messages", ev.Scope)
	})
}

func TestBroadcaster_CoalescesWithinWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 100 * time.Millisecond
		b := NewBroadcaster(interval)
		sub, unsub := b.Subscribe()
		defer unsub()

		// Leading-edge broadcast drains the first emit.
		b.Emit("sessions")
		<-sub

		// Bursts within the window are coalesced; no broadcast yet.
		b.Emit("messages")
		b.Emit("sync")
		b.Emit("sessions")

		synctest.Sleep(interval / 2)
		select {
		case ev := <-sub:
			require.Fail(t, "got early broadcast during rate-limit window", "ev=%v", ev)
		default:
		}

		// After the window elapses a single trailing broadcast arrives
		// carrying the most recent scope.
		ev := <-sub
		assert.Equal(t, "sessions", ev.Scope, "trailing scope")

		// The three coalesced emits produce exactly one trailing broadcast.
		synctest.Sleep(interval)
		select {
		case ev := <-sub:
			require.Fail(t, "got duplicate broadcast after trailing fire", "ev=%v", ev)
		default:
		}
	})
}

func TestBroadcaster_LeadingEdgeCancelsPendingTrailing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 50 * time.Millisecond
		b := NewBroadcaster(interval)
		sub, unsub := b.Subscribe()
		defer unsub()

		// Leading broadcast fills the window.
		b.Emit("a")
		<-sub

		// Rate-limited emit schedules a trailing broadcast of "b".
		b.Emit("b")

		// Simulate the race: another Emit arrives just after the window
		// boundary but before the in-flight trailing timer can acquire
		// the lock. Backdating lastEmit forces the next Emit to take the
		// leading branch while pending is still set and the timer is
		// still armed.
		b.mu.Lock()
		b.lastEmit = time.Now().Add(-2 * interval)
		b.mu.Unlock()

		b.Emit("c")
		ev := <-sub
		assert.Equal(t, "c", ev.Scope, "leading broadcast scope")

		// The pre-existing trailing timer for "b" may still fire. If the
		// leading branch did not cancel pending/timer, flushTrailing
		// would now deliver a stale "b" broadcast. Wait past the
		// original deadline and assert no extra event arrives.
		synctest.Sleep(2 * interval)
		select {
		case ev := <-sub:
			require.Fail(t, "stale trailing broadcast after leading edge", "ev=%v", ev)
		default:
		}
	})
}

func TestBroadcaster_StaleTrailingCallbackDoesNotConsumeNewerPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Narrow race: a trailing callback whose timer already fired is
		// waiting for b.mu; a leading-edge Emit runs first and a follow-up
		// rate-limited Emit installs a new pending+timer. Without a
		// generation token, the stale callback clobbers b.timer and
		// broadcasts the newer pending event immediately, violating the
		// rate limit.
		const interval = 50 * time.Millisecond
		b := NewBroadcaster(interval)
		sub, unsub := b.Subscribe()
		defer unsub()

		b.Emit("a")
		<-sub

		// Rate-limited emit schedules a timer; capture the generation
		// the scheduled callback will check against when it runs.
		b.Emit("b")
		b.mu.Lock()
		staleGen := b.timerGen
		b.mu.Unlock()

		// Force the next Emit into the leading branch, invalidating the
		// prior timer's generation.
		b.mu.Lock()
		b.lastEmit = time.Now().Add(-2 * interval)
		b.mu.Unlock()
		b.Emit("c")
		<-sub

		// Rate-limited emit after the leading edge installs a fresh
		// pending+timer under the new generation.
		b.Emit("d")

		// Simulate the stale callback from the "b" timer finally acquiring
		// the lock after being blocked. With the generation check it must
		// return without touching state; without it, the stale callback
		// would clear b.timer and broadcast "d" prematurely.
		b.flushTrailing(staleGen)

		// No premature broadcast in the window right after the stale
		// callback supposedly ran.
		synctest.Sleep(interval / 2)
		select {
		case ev := <-sub:
			require.Fail(t, "stale callback consumed newer pending", "ev=%v", ev)
		default:
		}

		// The new timer scheduled for "d" must still be live and deliver
		// "d" on its original schedule. A bug that lets the stale callback
		// null out b.timer would orphan the new timer here — in which case
		// the callback still fires, finds pending == nil, and no event
		// arrives.
		ev := <-sub
		assert.Equal(t, "d", ev.Scope)
	})
}

func TestBroadcaster_EmitAfterIntervalBroadcastsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 50 * time.Millisecond
		b := NewBroadcaster(interval)
		sub, unsub := b.Subscribe()
		defer unsub()

		b.Emit("first")
		<-sub

		synctest.Sleep(interval * 2)

		b.Emit("second")
		ev := <-sub
		assert.Equal(t, "second", ev.Scope)
	})
}
