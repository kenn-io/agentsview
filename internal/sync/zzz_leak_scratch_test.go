package sync

// TEMPORARY diagnostic: counts fsnotify reader goroutines still running.

import (
	"runtime"
	"strings"
	"testing"
)

func TestZZZLeakScratch(t *testing.T) {
	buf := make([]byte, 64<<20)
	n := runtime.Stack(buf, true)
	c := 0
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(g, "readEvents") {
			c++
		}
	}
	t.Logf("LEAKED_READERS %d", c)
}
