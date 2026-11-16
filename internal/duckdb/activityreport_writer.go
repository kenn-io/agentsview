package duckdb

import (
	"sync"
	"time"
)

// activityWriter coalesces pending cache writes and drains them on close.
// One worker and a byte limit bound detached work while reports return.
type activityWriter struct {
	mu       sync.Mutex
	pending  map[string]activityWrite
	bytes    int64
	done     chan struct{}
	closed   bool
	wake     chan struct{}
	deadline time.Time
}

type activityWrite struct {
	bytes int64
	save  func()
}

func (w *activityWriter) put(key string, size int64, save func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	old := w.pending[key]
	if (old.save == nil && len(w.pending) >= 64) || w.bytes-old.bytes+size > 128<<20 {
		return
	}
	if w.pending == nil {
		w.pending = make(map[string]activityWrite)
	}
	w.pending[key] = activityWrite{size, save}
	w.bytes += size - old.bytes
	w.deadline = time.Now().Add(10 * time.Second)
	if w.wake == nil {
		w.wake = make(chan struct{}, 1)
	}
	if w.done == nil {
		w.done = make(chan struct{})
		go w.run(w.done)
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *activityWriter) run(done chan struct{}) {
	for {
		w.mu.Lock()
		if delay := time.Until(w.deadline); !w.closed && delay > 0 {
			w.mu.Unlock()
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-w.wake:
			}
			timer.Stop()
			continue
		}
		var next activityWrite
		for key, write := range w.pending {
			next = write
			delete(w.pending, key)
			w.bytes -= write.bytes
			break
		}
		if next.save == nil {
			w.done = nil
			close(done)
			w.mu.Unlock()
			return
		}
		w.mu.Unlock()
		next.save()
	}
}

func (w *activityWriter) close() {
	w.mu.Lock()
	w.closed = true
	if w.wake != nil {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
	done := w.done
	w.mu.Unlock()
	if done != nil {
		<-done
	}
}
