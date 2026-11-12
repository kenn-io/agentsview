package testutil

import (
	"runtime"
	"strings"
)

// GoroutineBlockedOnMutexIn reports whether some goroutine is waiting to take
// a lock inside a function whose name contains fn. It recognises a sync.Mutex
// wait and the sync engine's first-in, first-out lock, whose blocked callers
// wait on a channel inside (*engineSyncLock).Lock. Go has no API for that,
// and testing/synctest does not treat a mutex wait as blocked, so this reads
// the text of every goroutine's stack. Tests use it to act only once another
// goroutine is parked on a lock.
func GoroutineBlockedOnMutexIn(fn string) bool {
	buf := make([]byte, 1<<20)
	for g := range strings.SplitSeq(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		header, _, _ := strings.Cut(g, "\n")
		if !strings.Contains(g, fn) {
			continue
		}
		if strings.Contains(header, "[sync.Mutex.Lock") {
			return true
		}
		if strings.Contains(header, "[chan receive") &&
			strings.Contains(g, "(*engineSyncLock).Lock") {
			return true
		}
	}
	return false
}
