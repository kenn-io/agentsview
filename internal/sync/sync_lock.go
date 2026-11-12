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
	// admitted counts Lock calls that took the lock after blocking.
	admitted atomic.Uint64
	// changed wakes a yielding holder after an admission. Capacity one
	// coalesces wakes; the yielder rechecks state after each one.
	changed chan struct{}
	once    gosync.Once
}

func (l *engineSyncLock) signal() chan struct{} {
	l.once.Do(func() { l.changed = make(chan struct{}, 1) })
	return l.changed
}

func (l *engineSyncLock) Lock() {
	if l.mu.TryLock() {
		return
	}
	l.waiters.Add(1)
	l.mu.Lock()
	l.waiters.Add(-1)
	l.admitted.Add(1)
	select {
	case l.signal() <- struct{}{}:
	default:
	}
}

func (l *engineSyncLock) TryLock() bool { return l.mu.TryLock() }

func (l *engineSyncLock) Unlock() { l.mu.Unlock() }

// yield lets goroutines already blocked in Lock run before the caller, which
// must hold the lock, continues. It returns false without releasing the lock
// when nobody is waiting. Otherwise it releases the lock and waits until as
// many blocked callers as were waiting at that moment have taken it, or until
// nobody is left waiting, and then queues like any other Lock call. A short
// operation admitted first therefore cannot let the yielder take the lock back
// ahead of a watcher sync that was also waiting. beforeRelease, when set, runs
// while the lock is still held and only when the lock is about to change
// hands. sync.Mutex alone does not guarantee the handoff: an Unlock followed
// at once by Lock usually lets the releasing goroutine take the lock straight
// back.
func (l *engineSyncLock) yield(beforeRelease func()) bool {
	queued := l.waiters.Load()
	if queued <= 0 {
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
	target := l.admitted.Load() + uint64(queued)
	l.mu.Unlock()
	for l.admitted.Load() < target && l.waiters.Load() > 0 {
		<-changed
	}
	l.Lock()
	return true
}
