package server

import (
	gosync "sync"
	"time"
)

// broadcasterBufferCap is the per-subscriber buffer size. A slow
// client can fall this many events behind before the broadcaster
// starts dropping events on its channel.
const broadcasterBufferCap = 8

// Event is a refresh signal sent by the sync engine after a pass
// that wrote data. Scope is advisory — subscribers may filter on
// it but are free to treat it as "refetch now".
type Event struct {
	Scope string
}

// Broadcaster fans out Event values from the sync engine to all
// connected SSE clients. It implements sync.Emitter.
//
// Broadcasts are rate-limited with leading+trailing edge semantics:
// the first emit in a quiet period fires immediately, further emits
// within minInterval are coalesced into a single trailing broadcast
// carrying the most recent scope. This keeps first-write latency
// low while capping refetch work during sustained sync bursts.
type Broadcaster struct {
	mu          gosync.Mutex
	subs        map[chan Event]struct{}
	minInterval time.Duration
	lastEmit    time.Time
	pending     *Event
	timer       *time.Timer
	// timerGen increments each time a leading-edge broadcast
	// invalidates the trailing state. A flushTrailing callback
	// captures the generation at schedule time and returns early
	// if the current generation no longer matches. Without this
	// token, a callback whose timer already fired but was still
	// waiting for b.mu could acquire the lock after a leading
	// emit and a subsequent rate-limited emit had installed a
	// new pending+timer, then consume that newer pending as if
	// it were its own and broadcast it immediately — violating
	// the rate limit and orphaning the newly scheduled timer.
	timerGen uint64
}

// NewBroadcaster creates an empty broadcaster. minInterval is the
// minimum wall-clock time between broadcasts; zero (or any
// non-positive value) disables coalescing so every Emit fans out
// immediately.
func NewBroadcaster(minInterval time.Duration) *Broadcaster {
	return &Broadcaster{
		subs:        make(map[chan Event]struct{}),
		minInterval: minInterval,
	}
}

// Emit delivers scope to every subscriber, subject to rate limiting.
// The first emit after a quiet gap of at least minInterval fans out
// immediately; emits within the window update the pending scope and
// schedule one trailing broadcast when the window ends.
//
// Delivery is non-blocking: if a subscriber's buffer is full, the
// event is dropped for that subscriber. The engine never blocks on
// slow clients.
func (b *Broadcaster) Emit(scope string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	if b.minInterval == 0 || b.lastEmit.IsZero() ||
		now.Sub(b.lastEmit) >= b.minInterval {
		// Leading edge. Invalidate any in-flight trailing state:
		// bumping timerGen makes a flushTrailing callback whose
		// timer already fired (but was still waiting for b.mu)
		// return without touching state. Clearing pending and
		// stopping the timer handle the common cases where the
		// callback has not yet started; the generation token
		// covers the narrower race where it has.
		b.pending = nil
		if b.timer != nil {
			b.timer.Stop()
			b.timer = nil
		}
		b.timerGen++
		b.lastEmit = now
		b.broadcastLocked(Event{Scope: scope})
		return
	}

	b.pending = &Event{Scope: scope}
	if b.timer == nil {
		gen := b.timerGen
		wait := b.minInterval - now.Sub(b.lastEmit)
		b.timer = time.AfterFunc(wait, func() {
			b.flushTrailing(gen)
		})
	}
}

// flushTrailing is invoked by the trailing-edge timer. It delivers
// the most recent coalesced scope (if any) and clears the timer so
// future emits can schedule a new one. gen is the generation the
// timer captured at schedule time; a mismatch means a leading-edge
// broadcast has since invalidated this callback, so it returns
// without touching any state.
func (b *Broadcaster) flushTrailing(gen uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if gen != b.timerGen {
		return
	}
	b.timer = nil
	if b.pending == nil {
		return
	}
	ev := *b.pending
	b.pending = nil
	b.lastEmit = time.Now()
	b.broadcastLocked(ev)
}

// broadcastLocked sends ev to every subscriber using a non-blocking
// select so a full buffer drops the event for that subscriber only.
// Must be called with b.mu held; holding the lock is safe because
// sends never block.
func (b *Broadcaster) broadcastLocked(ev Event) {
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe returns a receive channel for events and an unsubscribe
// function. Calling unsubscribe closes the channel and removes the
// subscription. It is safe to call unsubscribe multiple times.
func (b *Broadcaster) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, broadcasterBufferCap)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once gosync.Once
	unsub := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subs[ch]; ok {
				delete(b.subs, ch)
				close(ch)
			}
			b.mu.Unlock()
		})
	}
	return ch, unsub
}
