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
	require.Eventually(t, func() bool { return lock.waiters.Load() == 1 },
		10*time.Second, time.Millisecond)

	assert.True(t, lock.yield(func() { order <- "before-release" }))
	order <- "holder"
	lock.Unlock()
	requireReceiveWithin(t, waiterDone, 10*time.Second)

	got := []string{<-order, <-order, <-order}
	assert.Equal(t, []string{"before-release", "waiter", "holder"}, got)
	assert.Zero(t, lock.waiters.Load())
}
