package kept

import (
	"context"
	"sync"
)

type sharedFlight[T any] struct {
	cancel    context.CancelFunc
	done      chan struct{}
	waiters   int
	artifacts T
	err       error
}

// Flights shares a load until it finishes or its last waiter leaves.
// Its zero value is ready to use.
type Flights[T any] struct {
	mu      sync.Mutex
	flights map[string]*sharedFlight[T]
}

func (group *Flights[T]) Do(
	ctx context.Context,
	key string,
	build func(context.Context) (T, error),
) (T, error) {
	group.mu.Lock()
	if group.flights == nil {
		group.flights = make(map[string]*sharedFlight[T])
	}
	flight := group.flights[key]
	if flight == nil {
		// The shared build belongs to no single waiter, so it carries none
		// of their request values. Release cancels it when the last waiter
		// leaves.
		buildCtx, cancel := context.WithCancel(context.Background())
		flight = &sharedFlight[T]{
			cancel: cancel, done: make(chan struct{}), waiters: 1,
		}
		group.flights[key] = flight
		go func(active *sharedFlight[T]) {
			defer cancel()
			active.artifacts, active.err = build(buildCtx)
			group.mu.Lock()
			if group.flights[key] == active {
				delete(group.flights, key)
			}
			close(active.done)
			group.mu.Unlock()
		}(flight)
	} else {
		flight.waiters++
	}
	group.mu.Unlock()

	select {
	case <-flight.done:
		group.release(key, flight, false)
		return flight.artifacts, flight.err
	case <-ctx.Done():
		group.release(key, flight, true)
		var zero T
		return zero, ctx.Err()
	}
}

func (group *Flights[T]) release(
	key string, flight *sharedFlight[T], canceled bool,
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
