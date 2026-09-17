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

// cursorLookbackFloor is the smallest trailing window a check
// re-scans when it is not draining a backlog. The coalesce interval
// is a user-facing setting and may legitimately be zero, so this
// overlap cannot be derived from it: with a zero window, a
// candidate whose write commits between the candidate query and the
// cursor update falls permanently outside the cursor and is never
// notified. Re-scanning is free of duplicates because the persisted
// per-session State suppresses sessions already reported; it only
// costs one extra indexed read per check.
const cursorLookbackFloor = 5 * time.Second

// Cursor is the exclusive lower bound of the next candidate batch,
// in the store's (transcript_modified_at, session id) ordering. An empty
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
	// worth evaluating, in the store's (transcript_modified_at, id)
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
	// ready gates every check. It stays false until MarkReady, so no
	// notification can be decided while startup is still writing:
	// the readyAt snapshot alone only bounds which rows qualify, and
	// a check that runs before the fence is raised still compares
	// against the previous snapshot. readyWake carries the MarkReady
	// signal into Run so the first post-startup check happens at
	// once instead of on the next sweep.
	ready     bool
	readyWake chan struct{}
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

// NewHub builds a Hub. readyAt seeds the candidate lower bound and
// the initial cursor; callers normally pass the current time and
// let MarkReady replace it once startup sync is observed complete.
// debounce is the trailing-edge coalesce window for scope bursts.
//
// The hub starts closed and decides nothing at all until MarkReady
// is called: startup may still be writing when it is built, and a
// snapshot taken now would predate that sync rather than bound it.
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
		readyWake:  make(chan struct{}, 1),
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
// (initial sync, worker reconciliation, or full resync) finished,
// and opens the gate Check waits on. Transcript changes observed
// before it stay silent.
//
// Every caller must reach this, including the paths that run no
// startup sync at all: the gate starts closed, so a mode that never
// marks readiness would never notify.
//
// The first call also wakes Run, which would otherwise wait for its
// next sweep before noticing that startup is over.
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
	opened := !h.ready
	h.ready = true
	h.mu.Unlock()
	if opened {
		select {
		case h.readyWake <- struct{}{}:
		default:
			// A wake is already pending; one is enough.
		}
	}
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
//
// Scopes that arrive before MarkReady are collected and dropped by a
// check that decides nothing; the readiness wake below is what makes
// the first real check happen, so startup's own sync output cannot
// slip through as a notification.
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
		case <-h.readyWake:
			pending = false
			h.Check(ctx)
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
// (transcript_modified_at, id), and caps the batch at the requested
// limit. A full batch means the store may still hold newer
// candidates, so the cursor advances to the exact (timestamp, id)
// of the last candidate processed and the next check resumes
// strictly after that position in the same ordering. Carrying the
// id is what keeps a batch of rows sharing one timestamp from
// being skipped: a timestamp-only cursor would exclude every tied
// row that the full batch did not include. Re-scanning is harmless
// because the persisted per-session State suppresses sessions
// already notified.
//
// No branch moves the cursor past the batch's own snapshot. The
// empty-window branch closes at a watermark taken before the query
// ran rather than at "now" after it, so a candidate written while
// this check was in flight still sorts above the cursor and is
// picked up by the next one instead of being skipped forever. And
// no branch moves the cursor past a candidate whose state could not
// be read or written: a transient archive failure is not a decision,
// and advancing over it would suppress that notification for good.
func (h *Hub) Check(ctx context.Context) {
	h.mu.Lock()
	if !h.ready {
		// Startup is still writing. Deciding now would compare
		// against the readiness snapshot the hub was built with,
		// which predates the sync that is running.
		h.mu.Unlock()
		return
	}
	cursor := h.cursor
	if !h.backlog {
		// Look-back overlap: re-scan a trailing window so a
		// candidate stamped just before the last check is not
		// missed. A plain time bound, with no id tiebreak. The
		// window has a positive floor: the coalesce interval it
		// scales with is user-configurable and may be zero.
		cursor = Cursor{Since: h.cursor.Since.Add(-h.lookback())}
	}
	decider := h.decider
	readyAt := h.readyAt
	// Watermark for the batch about to be read. Taken before the
	// query so it can never sit past the store's snapshot.
	queriedAt := h.now()
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
	// failedAt is the index of the earliest candidate this check could
	// not carry to a persisted decision. It is the position the cursor
	// must not pass.
	failedAt := -1
	for i, s := range candidates {
		st, err := h.store.NotificationState(ctx, s.SessionID)
		if err != nil {
			// A corrupt or unreadable dedup row must not be
			// mistaken for "never notified": skip the session
			// this round and let the next check retry.
			log.Printf("notify: state load %s: %v", s.SessionID, err)
			if failedAt < 0 {
				failedAt = i
			}
			continue
		}
		d := decider.Decide(s, st, readyAt)
		if d == nil {
			continue
		}
		if err := h.store.SaveNotificationState(
			ctx, s.SessionID, d.State,
		); err != nil {
			// Without the persisted state the decision is not
			// durable: retrying must not depend on the cursor
			// having moved past it.
			log.Printf("notify: state save %s: %v", s.SessionID, err)
			if failedAt < 0 {
				failedAt = i
			}
			continue
		}
		if err := h.store.RecordNotificationEvent(
			ctx, d.Notification,
		); err != nil {
			// Diagnostics only, and the dedup state above is
			// already durable: this must not hold the cursor.
			log.Printf("notify: event log: %v", err)
		}
		h.fanOut(d.Notification)
	}

	h.mu.Lock()
	switch {
	case failedAt == 0:
		// The window opens with a candidate that was not decided.
		// Leave the cursor where it was and drop any backlog: the
		// next check re-reads this same window, which is the only
		// way the failed session is retried.
		h.backlog = false
	case failedAt > 0:
		// Resume strictly after the last candidate that was
		// carried to a persisted decision, so the failed one and
		// everything behind it are re-read.
		last := candidates[failedAt-1]
		h.cursor = Cursor{
			Since: last.TranscriptModifiedAt,
			ID:    last.SessionID,
		}
		h.backlog = true
	case len(candidates) == candidateBatchLimit:
		// Full batch: the store may still hold newer candidates.
		// Resume strictly after the last one processed in the
		// store's ordering, so rows sharing its timestamp are
		// not skipped.
		last := candidates[len(candidates)-1]
		h.cursor = Cursor{
			Since: last.TranscriptModifiedAt,
			ID:    last.SessionID,
		}
		h.backlog = true
	default:
		// The window is exhausted; close it at the watermark, not
		// at now: a candidate committed after the query but before
		// this line would otherwise land below the cursor and never
		// be seen again.
		h.backlog = false
		h.cursor = Cursor{Since: queriedAt}
	}
	h.mu.Unlock()
}

// lookback returns the trailing window a non-backlog check
// re-scans. It tracks the coalesce interval but never drops below
// cursorLookbackFloor, so a zero coalesce interval still leaves the
// overlap that keeps concurrent writes from being skipped.
func (h *Hub) lookback() time.Duration {
	if window := 2 * h.debounce; window > cursorLookbackFloor {
		return window
	}
	return cursorLookbackFloor
}
