package server

import (
	"sync"
	"testing"
	"time"
)

func TestBroadcaster_EmitFansOutToAllSubscribers(t *testing.T) {
	b := NewBroadcaster()
	sub1, unsub1 := b.Subscribe()
	defer unsub1()
	sub2, unsub2 := b.Subscribe()
	defer unsub2()

	b.Emit("messages")

	for i, sub := range []<-chan Event{sub1, sub2} {
		select {
		case ev := <-sub:
			if ev.Scope != "messages" {
				t.Errorf("sub %d: got scope %q, want %q", i, ev.Scope, "messages")
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d: timed out waiting for event", i)
		}
	}
}

func TestBroadcaster_EmitIsNonBlockingOnSlowSubscriber(t *testing.T) {
	b := NewBroadcaster()
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
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on slow subscriber")
	}

	// Drain what we can — drop count >= extra, exact count not guaranteed.
	drained := 0
	for {
		select {
		case <-slow:
			drained++
		case <-time.After(50 * time.Millisecond):
			if drained == 0 {
				t.Fatalf("slow subscriber received nothing")
			}
			return
		}
	}
}

func TestBroadcaster_UnsubscribeStopsDelivery(t *testing.T) {
	b := NewBroadcaster()
	sub, unsub := b.Subscribe()
	unsub()

	b.Emit("messages")

	select {
	case ev, ok := <-sub:
		if ok {
			t.Fatalf("got event after unsubscribe: %v", ev)
		}
		// channel closed by unsubscribe — acceptable
	case <-time.After(100 * time.Millisecond):
		// no delivery — also acceptable
	}
}

func TestBroadcaster_ConcurrentSubscribeAndEmit(t *testing.T) {
	b := NewBroadcaster()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			sub, unsub := b.Subscribe()
			defer unsub()
			b.Emit("sessions")
			select {
			case <-sub:
			case <-time.After(time.Second):
				t.Errorf("concurrent subscriber did not receive event")
			}
		})
	}
	wg.Wait()
}
