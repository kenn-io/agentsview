package server

import gosync "sync"

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
type Broadcaster struct {
	mu   gosync.Mutex
	subs map[chan Event]struct{}
}

// NewBroadcaster creates an empty broadcaster.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[chan Event]struct{})}
}

// Emit sends an event to every current subscriber. Delivery is
// non-blocking: if a subscriber's buffer is full, the event is
// dropped for that subscriber. The engine never blocks on slow
// clients.
func (b *Broadcaster) Emit(scope string) {
	ev := Event{Scope: scope}
	b.mu.Lock()
	defer b.mu.Unlock()
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
