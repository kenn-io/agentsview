package notify

import (
	"context"
	"log"
	gosync "sync"
	"time"
)

// Store is the persistence surface the Hub needs. Implemented by
// the archive DB layer; every method is best-effort from the
// notification path's point of view.
type Store interface {
	// NotificationCandidates returns recently changed sessions
	// worth evaluating. The store bounds the batch and applies the
	// since window; the Hub only passes its last check time.
	NotificationCandidates(
		ctx context.Context, since time.Time, readyAt time.Time, limit int,
	) ([]Snapshot, error)
	// NotificationState loads the persisted dedup cursor for one
	// session. A zero State (and nil error) means never notified.
	NotificationState(
		ctx context.Context, sessionID string,
	) (State, error)
	// SaveNotificationState persists the dedup cursor.
	SaveNotificationState(
		ctx context.Context, sessionID string, st State,
	) error
	// RecordNotificationEvent appends to the bounded notification
	// event log (diagnostics, not a delivery queue).
	RecordNotificationEvent(ctx context.Context, n Notification) error
}

// Hub debounces sync-engine refresh scopes, evaluates candidate
// sessions through the Decider, persists dedup state, and fans
// decided notifications out to SSE subscribers.
//
// The sync events feeding it are refresh hints, not a reliable
// event queue: correctness comes from the archive (termination
// status, next_ordinal cursors) and the persisted State, so a
// missed or duplicated scope event can at worst delay or re-check,
// never double-notify.
type Hub struct {
	store   Store
	decider *Decider
	now     func() time.Time
	// readyAt is the daemon readiness snapshot. Snapshots older
	// than it are initial sync / resync churn and stay silent.
	readyAt    time.Time
	debounce   time.Duration
	checkEvery time.Duration

	mu        gosync.Mutex
	subs      map[chan Notification]struct{}
	lastCheck time.Time
}

// NewHub builds a Hub. readyAt should be the time startup sync was
// observed complete; debounce is the trailing-edge coalesce window
// for scope bursts.
func NewHub(
	store Store, cfgFn func() Config, readyAt time.Time, debounce time.Duration,
) *Hub {
	return &Hub{
		store:      store,
		decider:    NewDecider(cfgFn, nil),
		now:        time.Now,
		readyAt:    readyAt,
		debounce:   debounce,
		checkEvery: 30 * time.Second,
		subs:       make(map[chan Notification]struct{}),
		lastCheck:  readyAt,
	}
}

// SetConfigFn replaces the policy source. cmd wiring installs a
// server-backed fn after construction so settings changes made
// through the config API apply without a restart. The decider is
// swapped atomically so in-flight checks keep a consistent view.
func (h *Hub) SetConfigFn(cfgFn func() Config) {
	h.mu.Lock()
	h.decider = NewDecider(cfgFn, h.now)
	h.mu.Unlock()
}

// MarkReady updates the readiness snapshot after startup sync
// (initial sync, worker reconciliation, or full resync) finished.
// Transcript changes observed before it stay silent.
func (h *Hub) MarkReady(t time.Time) {
	h.mu.Lock()
	h.readyAt = t
	if h.lastCheck.Before(t) {
		h.lastCheck = t
	}
	h.mu.Unlock()
}

// Subscribe returns a notification channel and an unsubscribe
// function. The channel is buffered; a subscriber that falls a
// full buffer behind loses events rather than blocking the Hub.
func (h *Hub) Subscribe() (<-chan Notification, func()) {
	ch := make(chan Notification, 8)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *Hub) fanOut(n Notification) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- n:
		default:
			// Drop for a slow subscriber: SSE clients refresh
			// their own state anyway, and dedup state is
			// persisted regardless.
		}
	}
}

// Run consumes scope strings (from the sync engine's emitter) and
// checks for notifiable changes on the trailing edge of each burst,
// plus a periodic sweep so a lost scope event self-heals within
// checkEvery.
func (h *Hub) Run(ctx context.Context, scopes <-chan string) {
	timer := time.NewTimer(h.debounce)
	defer timer.Stop()
	sweep := time.NewTicker(h.checkEvery)
	defer sweep.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-scopes:
			pending = true
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(h.debounce)
		case <-timer.C:
			if pending {
				pending = false
				h.Check(ctx)
			}
		case <-sweep.C:
			h.Check(ctx)
		}
	}
}

// Check evaluates candidates once. Safe to call concurrently; the
// lastCheck window and decider reference are taken under the mutex.
func (h *Hub) Check(ctx context.Context) {
	h.mu.Lock()
	since := h.lastCheck.Add(-2 * h.debounce)
	h.lastCheck = h.now()
	decider := h.decider
	readyAt := h.readyAt
	h.mu.Unlock()

	candidates, err := h.store.NotificationCandidates(
		ctx, since, readyAt, 64,
	)
	if err != nil {
		log.Printf("notify: candidate query: %v", err)
		return
	}
	for _, s := range candidates {
		st, err := h.store.NotificationState(ctx, s.SessionID)
		if err != nil {
			log.Printf("notify: state load %s: %v", s.SessionID, err)
			continue
		}
		d := decider.Decide(s, st, readyAt)
		if d == nil {
			continue
		}
		if err := h.store.SaveNotificationState(
			ctx, s.SessionID, d.State,
		); err != nil {
			log.Printf("notify: state save %s: %v", s.SessionID, err)
			continue
		}
		if err := h.store.RecordNotificationEvent(
			ctx, d.Notification,
		); err != nil {
			log.Printf("notify: event log: %v", err)
		}
		h.fanOut(d.Notification)
	}
}
