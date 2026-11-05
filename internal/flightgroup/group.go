package flightgroup

import (
	"context"
	"sync"
)

type flight[T any] struct {
	cancel  context.CancelFunc
	done    chan struct{}
	waiters int
	result  T
	err     error
}

// Group shares in-flight work and cancels it when its last waiter leaves.
// Its zero value is ready to use.
type Group[T any] struct {
	mu      sync.Mutex
	flights map[string]*flight[T]
}

// Do joins or starts a build for key. The optional joined hook runs after
// this caller attaches to the flight.
func (group *Group[T]) Do(
	ctx context.Context,
	key string,
	build func(context.Context) (T, error),
	joined func(),
) (T, error) {
	group.mu.Lock()
	if group.flights == nil {
		group.flights = make(map[string]*flight[T])
	}
	active := group.flights[key]
	if active == nil {
		// The shared build belongs to no single waiter, so it carries none
		// of their request values. Release cancels it when the last waiter
		// leaves.
		buildCtx, cancel := context.WithCancel(context.Background())
		active = &flight[T]{
			cancel: cancel, done: make(chan struct{}), waiters: 1,
		}
		group.flights[key] = active
		go func(active *flight[T]) {
			defer cancel()
			active.result, active.err = build(buildCtx)
			group.mu.Lock()
			if group.flights[key] == active {
				delete(group.flights, key)
			}
			close(active.done)
			group.mu.Unlock()
		}(active)
	} else {
		active.waiters++
	}
	group.mu.Unlock()
	if joined != nil {
		joined()
	}

	select {
	case <-active.done:
		group.release(key, active, false)
		return active.result, active.err
	case <-ctx.Done():
		group.release(key, active, true)
		var zero T
		return zero, ctx.Err()
	}
}

func (group *Group[T]) release(
	key string, flight *flight[T], canceled bool,
) {
	group.mu.Lock()
	defer group.mu.Unlock()
	flight.waiters--
	if canceled && flight.waiters == 0 {
		if group.flights[key] == flight {
			delete(group.flights, key)
		}
		flight.cancel()
	}
}
