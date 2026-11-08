package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// IdleTracker tracks external client activity and internal write work for a
// server that suspends local ingestion and optionally exits after a quiet period.
type IdleTracker struct {
	mu             sync.Mutex
	timeout        time.Duration
	onIdle         func()
	lastExternal   time.Time
	activeExternal int
	activeWork     int
	draining       bool
	notify         chan struct{}

	lastSyncActivity time.Time
	syncTimeout      time.Duration
	onSyncIdle       func()
	syncIdle         bool
}

func NewIdleTracker(timeout time.Duration, onIdle func()) *IdleTracker {
	return &IdleTracker{
		timeout:          timeout,
		onIdle:           onIdle,
		lastExternal:     time.Now(),
		lastSyncActivity: time.Now(),
		notify:           make(chan struct{}, 1),
	}
}

// SetSyncIdle installs a repeatable suspension callback. It marks ingestion
// pending synchronously; resource teardown must run outside this callback.
func (t *IdleTracker) SetSyncIdle(timeout time.Duration, suspend func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.syncTimeout, t.onSyncIdle = timeout, suspend
}

func (t *IdleTracker) Wrap(next http.Handler, basePath string) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		syncActivity := strings.TrimPrefix(r.URL.Path, basePath) != "/api/ping"
		if !t.beginExternal(syncActivity) {
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		defer t.endExternal(syncActivity)
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

	return sync.OnceFunc(func() {
		t.mu.Lock()
		t.activeWork = max(t.activeWork-1, 0)
		// Retry a suspension that catch-up could not yet accept.
		t.syncIdle = false
		t.mu.Unlock()
		t.signal()
	}), true
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
		t.lastSyncActivity = t.lastExternal
	}
	t.mu.Unlock()
	t.signal()
}

func (t *IdleTracker) Run(ctx context.Context) {
	if t == nil {
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

func (t *IdleTracker) beginExternal(syncActivity bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return false
	}
	t.lastExternal = time.Now()
	if syncActivity {
		t.syncIdle = false
		t.lastSyncActivity = t.lastExternal
	}
	t.activeExternal++
	return true
}

func (t *IdleTracker) endExternal(syncActivity bool) {
	t.mu.Lock()
	if t.activeExternal > 0 {
		t.activeExternal--
	}
	t.lastExternal = time.Now()
	if syncActivity {
		t.syncIdle = false
		t.lastSyncActivity = t.lastExternal
	}
	t.mu.Unlock()
	t.signal()
}

func (t *IdleTracker) nextWait() (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return 0, true
	}
	wait := time.Hour
	// Internal work holds only the exit lease; local collection may suspend.
	if t.activeExternal == 0 && t.syncTimeout > 0 && !t.syncIdle {
		remaining := t.syncTimeout - time.Since(t.lastSyncActivity)
		if remaining <= 0 {
			t.syncIdle = true
			t.onSyncIdle()
		} else {
			wait = min(wait, remaining)
		}
	}
	if t.activeExternal == 0 && t.activeWork == 0 && t.timeout > 0 {
		remaining := t.timeout - time.Since(t.lastExternal)
		if remaining <= 0 {
			t.draining = true
			go t.onIdle()
			return 0, true
		}
		wait = min(wait, remaining)
	}
	return wait, false
}

func (t *IdleTracker) signal() {
	select {
	case t.notify <- struct{}{}:
	default:
	}
}

// DisableShutdown preserves sync suspension while keeping the server available.
func (t *IdleTracker) DisableShutdown() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.timeout = 0
	t.mu.Unlock()
	t.signal()
}
