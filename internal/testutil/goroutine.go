package testutil

import (
	"runtime"
	"strings"
)

// GoroutineBlockedOnMutexIn reports whether some goroutine is waiting to lock
// a sync.Mutex inside a function whose name contains fn. Go has no API for
// that, and testing/synctest does not treat a mutex wait as blocked, so this
// reads the text of every goroutine's stack. Tests use it to act only once
// another goroutine is parked on a lock.
func GoroutineBlockedOnMutexIn(fn string) bool {
	buf := make([]byte, 1<<20)
	for g := range strings.SplitSeq(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		header, _, _ := strings.Cut(g, "\n")
		if strings.Contains(header, "[sync.Mutex.Lock") && strings.Contains(g, fn) {
			return true
		}
	}
	return false
}
