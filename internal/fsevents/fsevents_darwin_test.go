//go:build darwin && cgo

package fsevents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const streamTestTimeout = 10 * time.Second

func TestStreamCallbackBoundsNativeDelivery(t *testing.T) {
	longPath := "/sessions/" + strings.Repeat("x", 300) + ".jsonl"
	tests := []struct {
		name       string
		count      int
		path       string
		wantCopies int
		wantBytes  int
	}{
		{
			name:  "event count",
			count: callbackMaxEvents + 1,
			path:  "/sessions/event.jsonl",
		},
		{
			name:       "path bytes",
			count:      callbackMaxEvents,
			path:       longPath,
			wantCopies: callbackMaxPathBytes / len(longPath),
			wantBytes:  (callbackMaxPathBytes / len(longPath)) * len(longPath),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []Event
			var copiedEvents int
			var copiedBytes int
			stream := &Stream{
				sink: func(events []Event) {
					got = append([]Event(nil), events...)
				},
				callbackStats: func(events, bytes int) {
					copiedEvents = events
					copiedBytes = bytes
				},
			}

			invokeTestCallback(stream, tc.count, tc.path, eventFlagItemCreated)

			assert.Equal(t, []Event{{Flags: eventFlagMustScanSubDirs}}, got)
			assert.Equal(t, tc.wantCopies, copiedEvents)
			assert.Equal(t, tc.wantBytes, copiedBytes)
			assert.LessOrEqual(t, copiedEvents, callbackMaxEvents)
			assert.LessOrEqual(t, copiedBytes, callbackMaxPathBytes)
		})
	}
}

func TestStreamDeliversFileEvent(t *testing.T) {
	root := t.TempDir()
	events := make(chan Event, 16)
	newTestStream(t, root, func(batch []Event) {
		for _, event := range batch {
			events <- event
		}
	})

	path := filepath.Join(root, "created.txt")
	require.NoError(t, os.WriteFile(path, []byte("created"), 0o600))
	event := waitForPath(t, events, filepath.Join(canonicalPath(t, root), "created.txt"))

	assert.NotZero(t, event.ID)
	assert.NotZero(t, event.Flags&eventFlagItemCreated)
}

func TestStreamCloseIsConcurrentAndIdempotent(t *testing.T) {
	stream := newTestStream(t, t.TempDir(), func([]Event) {})

	const callers = 16
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			errs <- stream.Close()
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.NoError(t, err)
	}
	assert.NoError(t, stream.Close())
}

func TestStreamCloseWaitsForCallback(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(canonicalPath(t, root), "blocked.txt")
	entered := make(chan struct{})
	release := make(chan struct{})
	stream := newTestStream(t, root, func(batch []Event) {
		for _, event := range batch {
			if event.Path == path {
				close(entered)
				<-release
				return
			}
		}
	})

	require.NoError(t, os.WriteFile(filepath.Join(root, "blocked.txt"), []byte("blocked"), 0o600))
	requireReceive(t, entered)

	closed := make(chan error, 1)
	go func() {
		closed <- stream.Close()
	}()
	assert.Never(t, func() bool {
		return len(closed) != 0
	}, 200*time.Millisecond, 10*time.Millisecond)

	close(release)
	require.NoError(t, requireReceive(t, closed))
}

func TestStreamCopiesCallbackData(t *testing.T) {
	root := t.TempDir()
	events := make(chan Event, 128)
	stream := newTestStream(t, root, func(batch []Event) {
		for _, event := range batch {
			events <- event
		}
	})

	firstPath := filepath.Join(root, "first.txt")
	require.NoError(t, os.WriteFile(firstPath, []byte("first"), 0o600))
	canonicalFirstPath := filepath.Join(canonicalPath(t, root), "first.txt")
	first := waitForPath(t, events, canonicalFirstPath)

	var lastPath string
	for i := range 32 {
		path := filepath.Join(root, fmt.Sprintf("reuse-%02d.txt", i))
		require.NoError(t, os.WriteFile(path, []byte("reuse"), 0o600))
		lastPath = path
	}
	waitForPath(t, events, filepath.Join(canonicalPath(t, root), filepath.Base(lastPath)))

	assert.Equal(t, canonicalFirstPath, first.Path)
	assert.NotZero(t, first.ID)
	assert.NotZero(t, first.Flags)
	assert.NoError(t, stream.Close())
}

func TestStreamHasNoCallbacksAfterCloseReturns(t *testing.T) {
	root := t.TempDir()
	events := make(chan Event, 16)
	stream := newTestStream(t, root, func(batch []Event) {
		for _, event := range batch {
			events <- event
		}
	})

	beforeClose := filepath.Join(root, "before-close.txt")
	require.NoError(t, os.WriteFile(beforeClose, []byte("before"), 0o600))
	waitForPath(t, events, filepath.Join(canonicalPath(t, root), "before-close.txt"))
	require.NoError(t, stream.Close())
	drainEvents(events)

	afterClose := filepath.Join(root, "after-close.txt")
	require.NoError(t, os.WriteFile(afterClose, []byte("after"), 0o600))
	assert.Never(t, func() bool {
		select {
		case <-events:
			return true
		default:
			return false
		}
	}, time.Second, 20*time.Millisecond)
}

func TestStreamClosingSharedQueueSiblingKeepsOtherStreamRunning(t *testing.T) {
	queue, err := NewQueue()
	require.NoError(t, err)

	firstRoot := t.TempDir()
	first, err := NewStream(queue, firstRoot, 500*time.Millisecond, func([]Event) {})
	require.NoError(t, err)
	require.NoError(t, first.Start())
	t.Cleanup(func() { assert.NoError(t, first.Close()) })

	secondRoot := t.TempDir()
	events := make(chan Event, 16)
	second, err := NewStream(queue, secondRoot, 500*time.Millisecond, func(batch []Event) {
		for _, event := range batch {
			events <- event
		}
	})
	require.NoError(t, err)
	require.NoError(t, second.Start())
	t.Cleanup(func() { assert.NoError(t, second.Close()) })

	require.NoError(t, first.Close())
	path := filepath.Join(secondRoot, "sibling.txt")
	require.NoError(t, os.WriteFile(path, []byte("sibling"), 0o600))
	waitForPath(t, events, filepath.Join(canonicalPath(t, secondRoot), "sibling.txt"))
}

func newTestStream(t *testing.T, root string, sink func([]Event)) *Stream {
	t.Helper()
	queue, err := NewQueue()
	require.NoError(t, err)
	stream, err := NewStream(queue, root, 500*time.Millisecond, sink)
	require.NoError(t, err)
	require.NoError(t, stream.Start())
	t.Cleanup(func() { assert.NoError(t, stream.Close()) })
	return stream
}

func waitForPath(t *testing.T, events <-chan Event, path string) Event {
	t.Helper()
	timer := time.NewTimer(streamTestTimeout)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Path == path {
				return event
			}
		case <-timer.C:
			require.FailNow(t, "timed out waiting for FSEvent", "path: %s", path)
		}
	}
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return canonical
}

func requireReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(streamTestTimeout):
		require.FailNow(t, "timed out waiting for channel value")
		var zero T
		return zero
	}
}

func drainEvents(events <-chan Event) {
	for {
		select {
		case <-events:
		default:
			return
		}
	}
}
