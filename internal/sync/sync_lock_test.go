package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEngineSyncLockYieldWithoutWaitersKeepsLock(t *testing.T) {
	var lock engineSyncLock
	lock.Lock()
	ran := false
	assert.False(t, lock.yield(func() { ran = true }))
	assert.False(t, ran, "beforeRelease runs only when the lock changes hands")
	assert.False(t, lock.TryLock(), "a yield with no waiters must keep the lock held")
	lock.Unlock()
}

// TestEngineSyncLockYieldHandsLockToBlockedWaiter pins the handoff that
// sync.Mutex alone does not give: the yielding holder resumes only after the
// goroutine already blocked in Lock has run its critical section.
func TestEngineSyncLockYieldHandsLockToBlockedWaiter(t *testing.T) {
	var lock engineSyncLock
	lock.Lock()
	order := make(chan string, 3)
	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		lock.Lock()
		order <- "waiter"
		lock.Unlock()
	}()
	require.Eventually(t, func() bool { return lock.waiting() == 1 },
		10*time.Second, time.Millisecond)

	assert.True(t, lock.yield(func() { order <- "before-release" }))
	order <- "holder"
	lock.Unlock()
	requireReceiveWithin(t, waiterDone, 10*time.Second)

	got := []string{<-order, <-order, <-order}
	assert.Equal(t, []string{"before-release", "waiter", "holder"}, got)
	assert.Zero(t, lock.waiting())
}

// TestEngineSyncLockYieldAdmitsEveryCallerWaitingAtTheBoundary covers a short
// maintenance call and a watcher sync that are both waiting when a poll pass
// yields. Both must run, oldest first, before the yielder resumes; the first
// one finishing must not let the yielder back in ahead of the second.
func TestEngineSyncLockYieldAdmitsEveryCallerWaitingAtTheBoundary(t *testing.T) {
	var lock engineSyncLock
	lock.Lock()
	order := make(chan string, 4)
	var done [2]chan struct{}
	for i, name := range []string{"maintenance", "watcher"} {
		done[i] = make(chan struct{})
		go func() {
			defer close(done[i])
			lock.Lock()
			order <- name
			lock.Unlock()
		}()
		// Queue them in a known order.
		require.Eventually(t, func() bool { return lock.waiting() == i+1 },
			10*time.Second, time.Millisecond)
	}

	assert.True(t, lock.yield(nil))
	order <- "holder"
	lock.Unlock()
	for _, ch := range done {
		requireReceiveWithin(t, ch, 10*time.Second)
	}

	got := []string{<-order, <-order, <-order}
	assert.Equal(t, []string{"maintenance", "watcher", "holder"}, got,
		"callers waiting at the yield run oldest first, before the yielder resumes")
}

// TestEngineSyncLockOverlappingYieldsAllResume covers two grouped passes on
// one engine that both yield while another sync waits. Each yield must hand
// the lock on and resume later; neither may strand the other.
func TestEngineSyncLockOverlappingYieldsAllResume(t *testing.T) {
	var lock engineSyncLock
	lock.Lock() // pass A
	order := make(chan string, 8)
	passBDone := make(chan struct{})
	syncDone := make(chan struct{})
	go func() {
		defer close(passBDone)
		lock.Lock()
		order <- "B-group-1"
		lock.yield(nil)
		order <- "B-group-2"
		lock.Unlock()
	}()
	require.Eventually(t, func() bool { return lock.waiting() == 1 },
		10*time.Second, time.Millisecond)
	go func() {
		defer close(syncDone)
		lock.Lock()
		order <- "sync"
		lock.Unlock()
	}()
	require.Eventually(t, func() bool { return lock.waiting() == 2 },
		10*time.Second, time.Millisecond)

	assert.True(t, lock.yield(nil))
	order <- "A-group-2"
	lock.Unlock()
	requireReceiveWithin(t, passBDone, 10*time.Second)
	requireReceiveWithin(t, syncDone, 10*time.Second)

	got := make([]string, 0, 4)
	for range 4 {
		got = append(got, <-order)
	}
	assert.Equal(t, []string{"B-group-1", "sync"}, got[:2],
		"the passes' next groups wait for the sync queued at the first yield")
	assert.ElementsMatch(t, []string{"A-group-2", "B-group-2"}, got[2:])
	assert.Zero(t, lock.waiting())
	assert.True(t, lock.TryLock(), "the lock is free once both passes finish")
	lock.Unlock()
}
