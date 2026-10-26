// Package lockctx takes a mutex but stops waiting when a context ends.
package lockctx

import (
	"container/list"
	"context"
	"sync"
)

// Lock takes mu in the lock's normal queue, so the caller is not starved by
// back-to-back holders, and returns ctx.Err() if ctx ends first. On success
// the caller owns mu and must unlock it. A wait that gives up leaves a
// goroutine that unlocks mu once it is granted.
func Lock(ctx context.Context, mu sync.Locker) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	locked := make(chan struct{})
	go func() {
		mu.Lock()
		close(locked)
	}()
	select {
	case <-locked:
		return nil
	case <-ctx.Done():
		go func() {
			<-locked
			mu.Unlock()
		}()
		return ctx.Err()
	}
}

// Waiter shares one queued mutex acquisition across callers and retries.
// Each Waiter must always be used with the same mutex.
type Waiter struct {
	guard   sync.Mutex
	waiting list.List
	running bool
}

type lockRequest struct {
	ready   chan struct{}
	granted bool
}

// LockQueued preserves mutex queue ordering while canceled requests leave the
// queue. At most one acquisition goroutine remains while mu is held.
func LockQueued(ctx context.Context, mu sync.Locker, waiter *Waiter) error {
	waiter.guard.Lock()
	if err := ctx.Err(); err != nil {
		waiter.guard.Unlock()
		return err
	}
	request := &lockRequest{ready: make(chan struct{})}
	element := waiter.waiting.PushBack(request)
	if !waiter.running {
		waiter.running = true
		go func() {
			for {
				mu.Lock()
				waiter.guard.Lock()
				next := waiter.waiting.Front()
				if next == nil {
					waiter.running = false
					waiter.guard.Unlock()
					mu.Unlock()
					return
				}
				pending := next.Value.(*lockRequest)
				waiter.waiting.Remove(next)
				pending.granted = true
				close(pending.ready)
				waiter.guard.Unlock()
			}
		}()
	}
	waiter.guard.Unlock()
	select {
	case <-request.ready:
		if err := ctx.Err(); err != nil {
			mu.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		waiter.guard.Lock()
		granted := request.granted
		if !granted {
			waiter.waiting.Remove(element)
		}
		waiter.guard.Unlock()
		if granted {
			mu.Unlock()
		}
		return ctx.Err()
	}
}
