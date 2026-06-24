package server

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// IdleTracker tracks external client activity and internal write work for a
// daemon that should self-reap after a quiet period.
type IdleTracker struct {
	mu             sync.Mutex
	timeout        time.Duration
	onIdle         func()
	lastExternal   time.Time
	activeExternal int
	activeWork     int
	draining       bool
	notify         chan struct{}
}

func NewIdleTracker(timeout time.Duration, onIdle func()) *IdleTracker {
	return &IdleTracker{
		timeout:      timeout,
		onIdle:       onIdle,
		lastExternal: time.Now(),
		notify:       make(chan struct{}, 1),
	}
}

func (t *IdleTracker) Wrap(next http.Handler) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !t.beginExternal() {
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		defer t.endExternal()
		next.ServeHTTP(w, r)
	})
}

// BeginWork is nil-safe so foreground serve mode can share the same call sites
// as background daemons without enabling idle reaping.
func (t *IdleTracker) BeginWork() (func(), bool) {
	if t == nil {
		return func() {}, true
	}
	t.mu.Lock()
	if t.draining {
		t.mu.Unlock()
		return func() {}, false
	}
	t.activeWork++
	t.mu.Unlock()
	t.signal()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			if t.activeWork > 0 {
				t.activeWork--
			}
			t.mu.Unlock()
			t.signal()
		})
	}, true
}

// Do runs fn as tracked work, skipping it entirely when the daemon is already
// draining. It is nil-safe so foreground serve mode can share the same call
// sites without enabling idle reaping.
func (t *IdleTracker) Do(fn func()) {
	done, ok := t.BeginWork()
	if !ok {
		return
	}
	defer done()
	fn()
}

func (t *IdleTracker) Touch() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if !t.draining {
		t.lastExternal = time.Now()
	}
	t.mu.Unlock()
	t.signal()
}

func (t *IdleTracker) Run(ctx context.Context) {
	if t == nil || t.timeout <= 0 {
		return
	}
	for {
		wait, fired := t.nextWait()
		if fired {
			return
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-t.notify:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

func (t *IdleTracker) beginExternal() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return false
	}
	t.lastExternal = time.Now()
	t.activeExternal++
	return true
}

func (t *IdleTracker) endExternal() {
	t.mu.Lock()
	if t.activeExternal > 0 {
		t.activeExternal--
	}
	t.lastExternal = time.Now()
	t.mu.Unlock()
	t.signal()
}

func (t *IdleTracker) nextWait() (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return 0, true
	}
	if t.activeExternal > 0 || t.activeWork > 0 {
		return t.timeout, false
	}
	remaining := t.timeout - time.Since(t.lastExternal)
	if remaining > 0 {
		return remaining, false
	}
	t.draining = true
	go t.onIdle()
	return 0, true
}

func (t *IdleTracker) signal() {
	select {
	case t.notify <- struct{}{}:
	default:
	}
}
