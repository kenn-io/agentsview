package sync

import gosync "sync"

// engineSyncLock is the engine's exclusive sync mutex. Blocked callers are
// served strictly first in, first out: Unlock hands ownership straight to the
// oldest waiter instead of letting any goroutine race for it. That order lets
// a long background pass give the lock to live syncs at a safe boundary with
// yield. sync.Mutex cannot do this: an Unlock followed at once by Lock usually
// lets the releasing goroutine take the lock straight back.
//
// The zero value is an unlocked lock.
type engineSyncLock struct {
	state  gosync.Mutex
	locked bool
	// queue holds one channel per blocked Lock call, oldest first. Unlock
	// transfers ownership by closing the head channel.
	queue []chan struct{}
}

func (l *engineSyncLock) Lock() {
	l.state.Lock()
	if !l.locked {
		l.locked = true
		l.state.Unlock()
		return
	}
	turn := make(chan struct{})
	l.queue = append(l.queue, turn)
	l.state.Unlock()
	<-turn
}

// TryLock takes the lock only when it is free and nobody is queued for it, so
// it never jumps ahead of a blocked caller.
func (l *engineSyncLock) TryLock() bool {
	l.state.Lock()
	defer l.state.Unlock()
	if l.locked {
		return false
	}
	l.locked = true
	return true
}

func (l *engineSyncLock) Unlock() {
	l.state.Lock()
	defer l.state.Unlock()
	if !l.locked {
		panic("sync: unlock of unlocked engineSyncLock")
	}
	if len(l.queue) == 0 {
		l.locked = false
		return
	}
	next := l.queue[0]
	l.queue[0] = nil
	l.queue = l.queue[1:]
	// Ownership passes to next; locked stays true.
	close(next)
}

// waiting reports how many callers are blocked in Lock.
func (l *engineSyncLock) waiting() int {
	l.state.Lock()
	defer l.state.Unlock()
	return len(l.queue)
}

// yield lets every caller already blocked in Lock run before the caller, which
// must hold the lock, continues. It returns false without releasing the lock
// when nobody is waiting. Otherwise it hands the lock to the oldest waiter and
// queues behind everyone waiting at that moment, so it resumes only after each
// of them has held and released the lock. beforeRelease, when set, runs while
// the lock is still held and only when the lock is about to change hands.
func (l *engineSyncLock) yield(beforeRelease func()) bool {
	if l.waiting() == 0 {
		return false
	}
	if beforeRelease != nil {
		beforeRelease()
	}
	l.Unlock()
	l.Lock()
	return true
}
