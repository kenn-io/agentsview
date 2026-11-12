package sync

import (
	gosync "sync"
	"sync/atomic"
)

// engineSyncLock is the engine's exclusive sync mutex. It behaves like
// sync.Mutex and also counts goroutines blocked in Lock, so a long background
// pass can hand the lock to waiting live syncs at a safe boundary instead of
// holding it for its whole length.
type engineSyncLock struct {
	mu gosync.Mutex
	// waiters counts goroutines blocked in Lock.
	waiters atomic.Int64
	// acquisitions increases on every successful Lock or TryLock.
	acquisitions atomic.Uint64
	// changed wakes a yielding holder after an acquisition. Capacity one
	// coalesces wakes; the yielder rechecks state after each one.
	changed chan struct{}
	once    gosync.Once
}

func (l *engineSyncLock) signal() chan struct{} {
	l.once.Do(func() { l.changed = make(chan struct{}, 1) })
	return l.changed
}

func (l *engineSyncLock) notify() {
	select {
	case l.signal() <- struct{}{}:
	default:
	}
}

func (l *engineSyncLock) Lock() {
	if l.TryLock() {
		return
	}
	l.waiters.Add(1)
	l.mu.Lock()
	l.acquisitions.Add(1)
	l.waiters.Add(-1)
	l.notify()
}

func (l *engineSyncLock) TryLock() bool {
	if !l.mu.TryLock() {
		return false
	}
	l.acquisitions.Add(1)
	l.notify()
	return true
}

func (l *engineSyncLock) Unlock() { l.mu.Unlock() }

// yield lets goroutines already blocked in Lock run before the caller, which
// must hold the lock, continues. It returns false without releasing the lock
// when nobody is waiting. Otherwise it releases the lock, waits until another
// goroutine has taken it, and then queues behind that holder like any other
// Lock call. beforeRelease, when set, runs while the lock is still held and
// only when the lock is about to change hands. sync.Mutex alone does not
// guarantee the handoff: an Unlock followed at once by Lock usually lets the
// releasing goroutine take the lock straight back.
func (l *engineSyncLock) yield(beforeRelease func()) bool {
	if l.waiters.Load() == 0 {
		return false
	}
	if beforeRelease != nil {
		beforeRelease()
	}
	changed := l.signal()
	select {
	case <-changed:
	default:
	}
	before := l.acquisitions.Load()
	l.mu.Unlock()
	for l.waiters.Load() > 0 && l.acquisitions.Load() == before {
		<-changed
	}
	l.Lock()
	return true
}
