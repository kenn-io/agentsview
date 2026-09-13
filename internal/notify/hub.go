package notify

import (
	"context"
	"log"
	gosync "sync"
	"time"
)

// candidateBatchLimit is the per-check candidate batch size the Hub
// requests. A batch that comes back exactly this size means the
// store may hold newer candidates still to process. This relies on
// the store honouring the caller's limit rather than clamping it to
// a smaller internal ceiling: a silently short batch would look
// exhausted and strand the rest of a burst.
const candidateBatchLimit = 64

// Cursor is the exclusive lower bound of the next candidate batch,
// in the store's (local_modified_at, session id) ordering. An empty
// ID is a plain, strictly greater time bound (the look-back
// overlap); a set ID extends the bound to ties on Since. The store
// serialises Since in the column's own format. The Hub never
// formats timestamps itself: the ordering domain belongs to the
// store.
type Cursor struct {
	Since time.Time
	ID    string
}

// Store is the persistence surface the Hub needs. Implemented by
// the archive DB layer; every method is best-effort from the
// notification path's point of view.
type Store interface {
	// NotificationCandidates returns recently changed sessions
	// worth evaluating, in the store's (local_modified_at, id)
	// ascending order and bounded by limit. A row qualifies when
	// its timestamp is strictly newer than readyAt and strictly
	// greater than cursor in that ordering: newer than cursor.Since,
	// or — when cursor.ID is set — equal to it with a greater id.
	// The store owns the timestamp text format; the Hub passes
	// times only.
	NotificationCandidates(
		ctx context.Context, cursor Cursor, readyAt time.Time, limit int,
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

	mu   gosync.Mutex
	subs map[chan Notification]struct{}
	// cursor is the resume position in the store's ordering. Its
	// ID is set only while draining a backlog (a batch that filled
	// the cap); otherwise it is a plain time bound.
	cursor Cursor
	// backlog is set when the last candidate batch filled the cap,
	// meaning the store may still hold newer candidates. The next
	// check then resumes strictly after cursor in the store's
	// (timestamp, id) ordering instead of re-scanning the look-back
	// overlap, so a burst larger than the cap drains instead of
	// stalling.
	backlog bool
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
		cursor:     Cursor{Since: readyAt},
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
	if h.cursor.Since.Before(t) {
		// Advance to a plain time bound: the id half of the
		// cursor belongs to the superseded window.
		h.cursor = Cursor{Since: t}
	}
	// A new readiness window supersedes any in-flight backlog.
	h.backlog = false
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
// cursor and decider reference are taken under the mutex.
//
// The store returns the oldest candidates first, ordered by
// (local_modified_at, id), and caps the batch at the requested
// limit. A full batch means the store may still hold newer
// candidates, so the cursor advances to the exact (timestamp, id)
// of the last candidate processed and the next check resumes
// strictly after that position in the same ordering. Carrying the
// id is what keeps a batch of rows sharing one timestamp from
// being skipped: a timestamp-only cursor would exclude every tied
// row that the full batch did not include. Re-scanning is harmless
// because the persisted per-session State suppresses sessions
// already notified.
func (h *Hub) Check(ctx context.Context) {
	h.mu.Lock()
	cursor := h.cursor
	if !h.backlog {
		// Look-back overlap: re-scan a trailing window so a
		// candidate stamped just before the last check is not
		// missed. A plain time bound, with no id tiebreak.
		cursor = Cursor{Since: h.cursor.Since.Add(-2 * h.debounce)}
	}
	decider := h.decider
	readyAt := h.readyAt
	h.mu.Unlock()

	candidates, err := h.store.NotificationCandidates(
		ctx, cursor, readyAt, candidateBatchLimit,
	)
	if err != nil {
		// Leave the cursor untouched: the failed window is still
		// pending, so the next check re-reads it instead of
		// silently skipping every candidate in it.
		log.Printf("notify: candidate query: %v", err)
		return
	}
	for _, s := range candidates {
		st, err := h.store.NotificationState(ctx, s.SessionID)
		if err != nil {
			// A corrupt or unreadable dedup row must not be
			// mistaken for "never notified": skip the session
			// this round and let the next check retry.
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

	h.mu.Lock()
	if len(candidates) == candidateBatchLimit {
		// Full batch: the store may still hold newer candidates.
		// Resume strictly after the last one processed in the
		// store's ordering, so rows sharing its timestamp are
		// not skipped.
		last := candidates[len(candidates)-1]
		h.cursor = Cursor{Since: last.LocalModifiedAt, ID: last.SessionID}
		h.backlog = true
	} else {
		// The window is exhausted; close it.
		h.backlog = false
		h.cursor = Cursor{Since: h.now()}
	}
	h.mu.Unlock()
}
