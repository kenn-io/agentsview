package notify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeTimestamp renders a time the way every writer of
// sessions.transcript_modified_at does: fixed width, always three
// fractional digits. The fakes must compare candidates in this
// text domain (not time.Time) or they would hide both the
// trailing-zero and the tie bugs the archive query is vulnerable
// to. notify cannot import db, so this mirrors the layout const.
func storeTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// afterCursor mirrors the store's predicate: strictly greater than
// the cursor in the (timestamp, id) ordering, with an empty id
// meaning a plain strictly-greater time bound.
func afterCursor(s Snapshot, cursor Cursor) bool {
	ts, since := storeTimestamp(s.TranscriptModifiedAt), storeTimestamp(cursor.Since)
	if ts != since {
		return ts > since
	}
	return cursor.ID != "" && s.SessionID > cursor.ID
}

func byStoreOrder(x, y Snapshot) int {
	tx, ty := storeTimestamp(x.TranscriptModifiedAt),
		storeTimestamp(y.TranscriptModifiedAt)
	if tx != ty {
		if tx < ty {
			return -1
		}
		return 1
	}
	if x.SessionID < y.SessionID {
		return -1
	}
	if x.SessionID > y.SessionID {
		return 1
	}
	return 0
}

// fakeStore is an in-memory Store double recording what the Hub
// persisted, so tests can assert restart dedup against the same
// state a real archive would carry.
type fakeStore struct {
	candidates []Snapshot
	states     map[string]State
	events     []Notification
}

func newFakeStore() *fakeStore {
	return &fakeStore{states: map[string]State{}}
}

func (f *fakeStore) NotificationCandidates(
	_ context.Context, cursor Cursor, readyAt time.Time, limit int,
) ([]Snapshot, error) {
	// Mirror the archive query honestly: apply the readyAt bound,
	// the (timestamp, id) cursor, the ordering, and the cap.
	var out []Snapshot
	for _, s := range f.candidates {
		if !s.TranscriptModifiedAt.After(readyAt) {
			continue
		}
		if !afterCursor(s, cursor) {
			continue
		}
		out = append(out, s)
	}
	slices.SortFunc(out, byStoreOrder)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) NotificationState(
	_ context.Context, sessionID string,
) (State, error) {
	return f.states[sessionID], nil
}

func (f *fakeStore) SaveNotificationState(
	_ context.Context, sessionID string, st State,
) error {
	f.states[sessionID] = st
	return nil
}

func (f *fakeStore) RecordNotificationEvent(
	_ context.Context, n Notification,
) error {
	f.events = append(f.events, n)
	return nil
}

// archiveLikeStore mirrors the archive's candidate query contract
// closely enough to exercise the Hub's cursor handling: it applies
// the readyAt bound, the (timestamp, id) cursor, the ordering, and
// the batch cap (via fakeStore), and it can inject a query error.
type archiveLikeStore struct {
	*fakeStore
	queryErr error
}

func (a *archiveLikeStore) NotificationCandidates(
	ctx context.Context, cursor Cursor, readyAt time.Time, limit int,
) ([]Snapshot, error) {
	if a.queryErr != nil {
		return nil, a.queryErr
	}
	return a.fakeStore.NotificationCandidates(ctx, cursor, readyAt, limit)
}

// corruptStateStore models a persisted dedup row that exists but
// cannot be read. The archive reports it as an error (distinct
// from a missing row, which is a legitimate zero State), so the
// Hub must skip the session rather than treat it as fresh.
type corruptStateStore struct {
	*fakeStore
	badSession string
}

func (s *corruptStateStore) NotificationState(
	ctx context.Context, sessionID string,
) (State, error) {
	if sessionID == s.badSession {
		return State{}, errors.New("notification state corrupt")
	}
	return s.fakeStore.NotificationState(ctx, sessionID)
}

func hubTestSnapshot(id string, ordinal int64, mods ...func(*Snapshot)) Snapshot {
	s := Snapshot{
		SessionID:            id,
		Project:              "proj",
		Agent:                "claude",
		TerminationStatus:    "awaiting_user",
		NextOrdinal:          ordinal,
		LastRole:             "assistant",
		TranscriptModifiedAt: readyAt.Add(time.Minute),
	}
	for _, m := range mods {
		m(&s)
	}
	return s
}

func collect(t *testing.T, ch <-chan Notification, n int) []Notification {
	t.Helper()
	var got []Notification
	deadline := time.After(2 * time.Second)
	for len(got) < n {
		select {
		case v := <-ch:
			got = append(got, v)
		case <-deadline:
			t.Fatalf("timed out waiting for %d notifications, got %d", n, len(got))
		}
	}
	return got
}

// newReadyHub builds a hub with the startup gate already open,
// which is the state every steady-state check runs in. Tests that
// exercise the gate itself build with NewHub and check before
// MarkReady.
func newReadyHub(
	store Store, cfgFn func() Config, readyAt time.Time, debounce time.Duration,
) *Hub {
	hub := NewHub(store, cfgFn, readyAt, debounce)
	hub.MarkReady(readyAt)
	return hub
}

// eventSessions lists the decided-notification log by session, in
// the order the Hub wrote it. The log is the authoritative record
// of what was decided: it is written even with no subscriber
// attached, unlike the lossy SSE fan-out.
func eventSessions(events []Notification) []string {
	out := make([]string, 0, len(events))
	for _, n := range events {
		out = append(out, n.SessionID)
	}
	return out
}

func TestHubTurnEndOncePerTurn(t *testing.T) {
	store := newFakeStore()
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	hub := newReadyHub(store, enabledCfg, readyAt, time.Millisecond)
	// Freeze the clock just past the candidate so the cursor is
	// deterministic: the store's look-back must still cover it.
	now := readyAt.Add(time.Minute)
	hub.now = func() time.Time { return now }
	ch, unsub := hub.Subscribe()
	defer unsub()

	hub.Check(context.Background())
	got := collect(t, ch, 1)
	require.Len(t, got, 1)
	assert.Equal(t, KindTurnEnd, got[0].Kind)
	require.Len(t, store.events, 1)

	// Repeated check (duplicate sync, SSE reconnect, periodic
	// sweep) must not re-notify for the same content.
	hub.Check(context.Background())
	select {
	case n := <-ch:
		t.Fatalf("unexpected re-notification: %+v", n)
	case <-time.After(50 * time.Millisecond):
	}

	// A new turn advances the ordinal: notify again.
	store.candidates = []Snapshot{hubTestSnapshot("s1", 14)}
	hub.Check(context.Background())
	got = collect(t, ch, 1)
	assert.Equal(t, KindTurnEnd, got[0].Kind)
	assert.Equal(t, int64(14), store.states["s1"].TurnEndOrdinal)
}

func TestHubRestartDedup(t *testing.T) {
	store := newFakeStore()
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	hub := newReadyHub(store, enabledCfg, readyAt, time.Millisecond)
	ch, unsub := hub.Subscribe()
	defer unsub()

	hub.Check(context.Background())
	collect(t, ch, 1)

	// Simulate a daemon restart: fresh Hub, same archive state.
	// The persisted cursor must suppress the repeat.
	hub2 := newReadyHub(store, enabledCfg, readyAt, time.Millisecond)
	ch2, unsub2 := hub2.Subscribe()
	defer unsub2()
	hub2.Check(context.Background())
	select {
	case n := <-ch2:
		t.Fatalf("restarted hub re-notified: %+v", n)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHubFailedCandidateQueryDoesNotAdvanceCursor(t *testing.T) {
	store := &archiveLikeStore{fakeStore: newFakeStore()}
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	store.queryErr = errors.New("archive unavailable")

	hub := newReadyHub(store, enabledCfg, readyAt, time.Millisecond)
	// A clock far past the candidate: if the failed check advanced
	// the cursor, the candidate falls outside the next window.
	hub.now = func() time.Time { return readyAt.Add(time.Hour) }
	ch, unsub := hub.Subscribe()
	defer unsub()

	hub.Check(context.Background())
	select {
	case n := <-ch:
		t.Fatalf("notified on a failed query: %+v", n)
	case <-time.After(20 * time.Millisecond):
	}

	// The archive recovers. The candidate must still be inside the
	// window, proving the failed check did not consume it.
	store.queryErr = nil
	hub.Check(context.Background())
	got := collect(t, ch, 1)
	require.Len(t, got, 1)
	assert.Equal(t, "s1", got[0].SessionID)
}

func TestHubBurstExceedingCandidateCapNotifiesEverySession(t *testing.T) {
	const total = 100
	store := &archiveLikeStore{fakeStore: newFakeStore()}
	base := readyAt.Add(time.Minute)
	for i := range total {
		id := fmt.Sprintf("burst-%03d", i)
		// A tight burst: every session lands well inside one
		// debounce window, so the next check's look-back overlap
		// still contains the whole batch.
		modified := base.Add(time.Duration(i) * time.Millisecond)
		store.candidates = append(store.candidates,
			hubTestSnapshot(id, 10, func(s *Snapshot) {
				s.TranscriptModifiedAt = modified
			}))
	}

	// Production debounce is EventsCoalesceInterval (10s); use it
	// so the -2*debounce look-back is realistic.
	hub := newReadyHub(store, enabledCfg, readyAt, 10*time.Second)
	// Freeze the clock just past the newest candidate so the
	// fallback cursor is deterministic.
	now := base.Add(time.Second)
	hub.now = func() time.Time { return now }

	// Successive checks must make forward progress until the whole
	// burst is covered. The decided-notification log is the
	// authoritative record: it is written for every decision even
	// with no subscriber attached (the SSE channel is lossy by
	// design and would drop a 64-event burst).
	seen := map[string]int{}
	for round := 0; round < total && len(seen) < total; round++ {
		hub.Check(context.Background())
		seen = map[string]int{}
		for _, n := range store.events {
			seen[n.SessionID]++
		}
	}

	require.Len(t, seen, total, "every eligible session must be notified")
	for id, count := range seen {
		assert.Equal(t, 1, count, "session %s notified %d times", id, count)
	}
}

// TestHubBurstWithTiedTimestampsNotifiesEverySession covers the
// tie case the distinct-timestamp burst above cannot see: a single
// sync pass stamps many sessions with one transcript_modified_at, and a
// timestamp-only resume cursor would skip every unprocessed row
// that ties with the full batch's newest. The clock advances past
// the look-back window between checks, matching production cadence.
func TestHubBurstWithTiedTimestampsNotifiesEverySession(t *testing.T) {
	const total = 100
	store := &archiveLikeStore{fakeStore: newFakeStore()}
	tie := readyAt.Add(time.Minute) // one shared timestamp
	for i := range total {
		id := fmt.Sprintf("tie-%03d", i)
		store.candidates = append(store.candidates,
			hubTestSnapshot(id, 10, func(s *Snapshot) {
				s.TranscriptModifiedAt = tie
			}))
	}

	hub := newReadyHub(store, enabledCfg, readyAt, 10*time.Second)
	now := tie.Add(10 * time.Second) // trailing edge of the burst
	hub.now = func() time.Time { return now }

	hub.Check(context.Background()) // full batch -> backlog
	now = tie.Add(30 * time.Second)
	hub.Check(context.Background()) // resumes strictly after the tie
	now = tie.Add(60 * time.Second)
	hub.Check(context.Background()) // recovery check, look-back past

	seen := map[string]int{}
	for _, n := range store.events {
		seen[n.SessionID]++
	}
	require.Len(t, seen, total, "every tied session must be notified")
	for id, count := range seen {
		assert.Equal(t, 1, count, "session %s notified %d times", id, count)
	}
}

// midCheckWriteStore models a transcript write that commits while a
// check is in flight: after the store's snapshot was read, before
// the Hub updates its cursor. That is the window a cursor closed at
// "now" would step over.
type midCheckWriteStore struct {
	*fakeStore
	onQuery func()
}

func (m *midCheckWriteStore) NotificationCandidates(
	ctx context.Context, cursor Cursor, readyAt time.Time, limit int,
) ([]Snapshot, error) {
	out, err := m.fakeStore.NotificationCandidates(
		ctx, cursor, readyAt, limit)
	if m.onQuery != nil {
		m.onQuery()
	}
	return out, err
}

// TestHubCursorNeverPassesTheQuerySnapshot is the zero-coalesce
// regression: events_coalesce_interval may be set to 0, which used
// to zero the look-back as well, and the window was then closed at
// "now" — after the query. A candidate that committed in between
// landed below the cursor and was never notified again.
func TestHubCursorNeverPassesTheQuerySnapshot(t *testing.T) {
	store := newFakeStore()
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	// A zero coalesce interval: the look-back must not collapse
	// with it, and the cursor must close at the query watermark.
	hub := newReadyHub(store, enabledCfg, readyAt, 0)
	now := readyAt.Add(time.Minute)
	hub.now = func() time.Time { return now }
	ch, unsub := hub.Subscribe()
	defer unsub()

	// s2 commits during the first check: its stamp sits between
	// the watermark that check took and the clock reading it would
	// otherwise have used to close the window.
	late := readyAt.Add(time.Minute + time.Second)
	hub.store = &midCheckWriteStore{fakeStore: store, onQuery: func() {
		now = now.Add(3 * time.Second)
		store.candidates = append(store.candidates,
			hubTestSnapshot("s2", 10, func(s *Snapshot) {
				s.TranscriptModifiedAt = late
			}))
	}}

	hub.Check(context.Background())
	got := collect(t, ch, 1)
	require.Equal(t, "s1", got[0].SessionID)

	// The window closed at the watermark taken before the query,
	// not at the clock reading after it (now moved forward by the
	// mid-check write).
	hub.mu.Lock()
	closed := hub.cursor.Since
	hub.mu.Unlock()
	assert.Equal(t, readyAt.Add(time.Minute), closed,
		"the cursor must close at the pre-query watermark")

	// The next check must still see the session that landed
	// mid-flight. Skipping it here is the permanent-loss bug.
	hub.Check(context.Background())
	got = collect(t, ch, 1)
	assert.Equal(t, "s2", got[0].SessionID,
		"a candidate written during a check must not be skipped")
}

// TestHubLookbackHasAPositiveFloor pins the other half of the same
// fix: the trailing window cannot be derived from a user-settable
// interval that is allowed to be zero.
func TestHubLookbackHasAPositiveFloor(t *testing.T) {
	for _, debounce := range []time.Duration{0, time.Millisecond, 10 * time.Second} {
		hub := NewHub(newFakeStore(), enabledCfg, readyAt, debounce)
		assert.GreaterOrEqual(t, hub.lookback(), cursorLookbackFloor,
			"debounce %s must not shrink the look-back below the floor",
			debounce)
	}
	// A long interval still scales the window.
	hub := NewHub(newFakeStore(), enabledCfg, readyAt, time.Minute)
	assert.Equal(t, 2*time.Minute, hub.lookback())
}

// TestHubNeverFormatsTimestamps guards the design rule that the
// Hub passes time.Time only and the store owns the ordering domain.
// A Format call here would re-introduce the trailing-zero mismatch
// this fix removed.
func TestHubNeverFormatsTimestamps(t *testing.T) {
	src, err := os.ReadFile("hub.go")
	require.NoError(t, err)
	assert.NotContains(t, string(src), ".Format(",
		"the Hub must not format timestamps; the store owns the ordering domain")
}

func TestHubSkipsSessionWithUnreadableDedupState(t *testing.T) {
	store := &corruptStateStore{
		fakeStore:  newFakeStore(),
		badSession: "corrupt",
	}
	store.candidates = []Snapshot{
		hubTestSnapshot("corrupt", 10),
		hubTestSnapshot("healthy", 10),
	}
	hub := newReadyHub(store, enabledCfg, readyAt, time.Millisecond)
	hub.Check(context.Background())

	// A corrupt dedup row must not read as "never notified": the
	// session is skipped, not re-notified, and its stored state is
	// left alone. Other sessions in the same batch still fire.
	assert.NotContains(t, store.states, "corrupt",
		"corrupt state must not be overwritten as if fresh")
	require.Len(t, store.events, 1)
	assert.Equal(t, "healthy", store.events[0].SessionID)
}

func TestHubWorksWithoutSubscribers(t *testing.T) {
	// The desktop window may be closed; the background monitor
	// still decides, persists dedup state, and logs the event.
	store := newFakeStore()
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	hub := newReadyHub(store, enabledCfg, readyAt, time.Millisecond)

	hub.Check(context.Background())
	require.Len(t, store.events, 1)
	assert.Equal(t, int64(10), store.states["s1"].TurnEndOrdinal)
}

func TestHubFanOutAndSlowSubscriber(t *testing.T) {
	store := newFakeStore()
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	hub := newReadyHub(store, enabledCfg, readyAt, time.Millisecond)
	// Freeze the clock just past the candidate so the second check's
	// look-back still covers the replacement batch below.
	now := readyAt.Add(time.Minute)
	hub.now = func() time.Time { return now }
	ch1, unsub1 := hub.Subscribe()
	defer unsub1()
	ch2, unsub2 := hub.Subscribe()
	defer unsub2()

	hub.Check(context.Background())
	got1 := collect(t, ch1, 1)
	got2 := collect(t, ch2, 1)
	assert.Equal(t, got1[0].SessionID, got2[0].SessionID)

	// A subscriber that never drains must not block the Hub: ten
	// notifications at once overflow its buffer, but the check
	// completes and the live subscriber still receives.
	slow, unsubSlow := hub.Subscribe()
	defer unsubSlow()
	_ = slow
	var many []Snapshot
	for i := range 10 {
		id := "burst-" + string(rune('a'+i))
		many = append(many, hubTestSnapshot(id, 3))
	}
	store.candidates = many
	hub.Check(context.Background())

	// If fanOut ever blocked on the slow subscriber, this collect
	// would time out.
	collect(t, ch1, 8)
}

func TestHubDisabledConfigIsSilent(t *testing.T) {
	store := newFakeStore()
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	cfg := enabledCfg()
	cfg.Enabled = false
	hub := newReadyHub(store, func() Config { return cfg }, readyAt, time.Millisecond)
	ch, unsub := hub.Subscribe()
	defer unsub()

	hub.Check(context.Background())
	select {
	case n := <-ch:
		t.Fatalf("disabled hub notified: %+v", n)
	case <-time.After(50 * time.Millisecond):
	}
	assert.Empty(t, store.events)
}

// countingStore counts candidate queries, so a test can tell a real
// check apart from a notification that arrived for some other
// reason. The counter is atomic because Run queries from its own
// goroutine while the test reads it.
type countingStore struct {
	*fakeStore
	queries atomic.Int64
}

func (c *countingStore) NotificationCandidates(
	ctx context.Context, cursor Cursor, readyAt time.Time, limit int,
) ([]Snapshot, error) {
	c.queries.Add(1)
	return c.fakeStore.NotificationCandidates(ctx, cursor, readyAt, limit)
}

func TestHubRunCoalescesScopeBursts(t *testing.T) {
	store := &countingStore{fakeStore: newFakeStore()}
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	// A window three orders of magnitude wider than the send loop
	// below, so the burst provably lands inside one coalesce
	// interval rather than racing the timer.
	hub := newReadyHub(store, enabledCfg, readyAt, 200*time.Millisecond)
	ch, unsub := hub.Subscribe()
	defer unsub()

	// newReadyHub leaves the readiness wake queued. Drain it before
	// Run starts: otherwise the wake's check would be the one that
	// notifies, and the count below would describe that check rather
	// than the burst.
	<-hub.readyWake

	ctx := t.Context()
	scopes := make(chan string, 16)
	go hub.Run(ctx, scopes)

	// A burst of scopes (sustained sync output) must collapse
	// into one check during the debounce window.
	for range 10 {
		scopes <- "messages"
	}
	collect(t, ch, 1)
	assert.Len(t, store.events, 1)
	assert.Equal(t, int64(1), store.queries.Load(),
		"ten scopes must collapse into one check, not ten")

	// Later bursts re-check; the archive cursor keeps it silent.
	for range 5 {
		scopes <- "sessions"
	}
	select {
	case n := <-ch:
		t.Fatalf("burst re-notified: %+v", n)
	case <-time.After(500 * time.Millisecond):
	}
	assert.Equal(t, int64(2), store.queries.Load(),
		"a later burst must actually re-check the archive")
}

// TestHubCheckBeforeMarkReadyDecidesNothing covers the startup gate.
// The hub is built before the startup sync runs, so a check that ran
// then would decide against a readiness snapshot that predates every
// write the sync is about to make — announcing work the user did not
// just do.
func TestHubCheckBeforeMarkReadyDecidesNothing(t *testing.T) {
	store := newFakeStore()
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	hub := NewHub(store, enabledCfg, readyAt, time.Millisecond)
	now := readyAt.Add(time.Minute)
	hub.now = func() time.Time { return now }
	ch, unsub := hub.Subscribe()
	defer unsub()

	// Startup sync output reaches Check through Run's debounce; a
	// direct call is the same path with less ceremony.
	hub.Check(context.Background())
	select {
	case n := <-ch:
		t.Fatalf("decided before startup finished: %+v", n)
	case <-time.After(20 * time.Millisecond):
	}
	assert.Empty(t, store.events)
	assert.Empty(t, store.states)

	// The gate returns before the cursor is even read, so nothing
	// about the window was consumed either.
	hub.mu.Lock()
	seeded := hub.cursor
	hub.mu.Unlock()
	assert.Equal(t, readyAt, seeded.Since)
	assert.Empty(t, seeded.ID)

	// Opening the gate makes the same window eligible.
	hub.MarkReady(readyAt)
	hub.Check(context.Background())
	got := collect(t, ch, 1)
	assert.Equal(t, "s1", got[0].SessionID)
}

// TestHubMarkReadyWakesRun pins the other half of the gate: Run must
// not wait for its next periodic sweep to notice that startup ended,
// or the first post-startup turn could sit silent for checkEvery.
func TestHubMarkReadyWakesRun(t *testing.T) {
	store := newFakeStore()
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	hub := NewHub(store, enabledCfg, readyAt, time.Millisecond)
	ch, unsub := hub.Subscribe()
	defer unsub()

	scopes := make(chan string)
	go hub.Run(t.Context(), scopes)

	// No scope ever arrives, so only the readiness wake can trigger
	// a check. The sweep cannot be the cause: checkEvery is 30s and
	// collect gives up after 2s.
	hub.MarkReady(readyAt)
	got := collect(t, ch, 1)
	assert.Equal(t, "s1", got[0].SessionID)
}

// failingSaveStore models an archive whose dedup-state write fails
// for one session. Without the persisted state a decision is not
// durable, so the cursor must not move past it.
type failingSaveStore struct {
	*fakeStore
	badSession string
	fail       bool
}

func (s *failingSaveStore) SaveNotificationState(
	ctx context.Context, sessionID string, st State,
) error {
	if s.fail && sessionID == s.badSession {
		return errors.New("notification state write failed")
	}
	return s.fakeStore.SaveNotificationState(ctx, sessionID, st)
}

// TestHubSaveFailureHoldsCursorForRetry is the failure-aware cursor
// regression: a candidate whose state could not be persisted has not
// been decided, and advancing the cursor over it would suppress that
// notification for good.
func TestHubSaveFailureHoldsCursorForRetry(t *testing.T) {
	store := &failingSaveStore{
		fakeStore:  newFakeStore(),
		badSession: "flaky",
		fail:       true,
	}
	// Store order is by id within a tied timestamp, so the failing
	// session opens the batch: the cursor cannot move at all.
	store.candidates = []Snapshot{
		hubTestSnapshot("flaky", 10),
		hubTestSnapshot("steady", 10),
	}
	hub := newReadyHub(store, enabledCfg, readyAt, time.Millisecond)
	// A clock far past the batch: a cursor that advanced would leave
	// the retry outside the next window.
	hub.now = func() time.Time { return readyAt.Add(time.Hour) }

	hub.Check(context.Background())
	// The healthy session in the same batch still fires; only the
	// undecidable one is held back.
	require.Len(t, store.events, 1)
	assert.Equal(t, "steady", store.events[0].SessionID)
	assert.NotContains(t, store.states, "flaky")

	hub.mu.Lock()
	held := hub.cursor
	hub.mu.Unlock()
	assert.Equal(t, readyAt, held.Since, "the cursor must not move")

	// The archive recovers. The held candidate is still inside the
	// window, which is the only reason it is retried.
	store.fail = false
	hub.Check(context.Background())
	require.Len(t, store.events, 2)
	assert.Equal(t, "flaky", store.events[1].SessionID)
	assert.Equal(t, int64(10), store.states["flaky"].TurnEndOrdinal)
}

// TestHubSaveFailureResumesBeforeTheFailedCandidate covers the
// partial case: the cursor may advance through the candidates that
// were decided, but not past the first one that was not.
func TestHubSaveFailureResumesBeforeTheFailedCandidate(t *testing.T) {
	store := &failingSaveStore{
		fakeStore:  newFakeStore(),
		badSession: "a-02",
		fail:       true,
	}
	for _, id := range []string{"a-00", "a-01", "a-02", "a-03"} {
		store.candidates = append(store.candidates, hubTestSnapshot(id, 10))
	}
	hub := newReadyHub(store, enabledCfg, readyAt, time.Millisecond)
	hub.now = func() time.Time { return readyAt.Add(time.Hour) }

	hub.Check(context.Background())
	// The batch is processed best-effort: a row whose state cannot
	// be written is held back, but the rows behind it are still
	// decided rather than being stalled with it.
	assert.Equal(t, []string{"a-00", "a-01", "a-03"}, eventSessions(store.events))

	// The resume position is the last decided candidate before the
	// failure, so the held row is re-read and the tail is not.
	hub.mu.Lock()
	resume, backlog := hub.cursor, hub.backlog
	hub.mu.Unlock()
	assert.Equal(t, readyAt.Add(time.Minute), resume.Since)
	assert.Equal(t, "a-01", resume.ID)
	assert.True(t, backlog, "a held candidate must keep the drain going")

	// The archive recovers: exactly the held session is retried, and
	// the candidates already decided in front of it are not
	// re-notified.
	store.fail = false
	hub.Check(context.Background())
	assert.Equal(t, []string{"a-00", "a-01", "a-03", "a-02"},
		eventSessions(store.events))
}

// failingEventStore models an archive whose notification event log
// cannot be appended to. The log is diagnostics, not a delivery
// queue, so it must not hold the cursor: the dedup state written
// alongside it is already durable.
type failingEventStore struct {
	*fakeStore
}

func (s *failingEventStore) RecordNotificationEvent(
	context.Context, Notification,
) error {
	return errors.New("event log unavailable")
}

func TestHubEventLogFailureDoesNotHoldCursor(t *testing.T) {
	store := &failingEventStore{fakeStore: newFakeStore()}
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	hub := newReadyHub(store, enabledCfg, readyAt, time.Millisecond)
	now := readyAt.Add(time.Minute)
	hub.now = func() time.Time { return now }

	hub.Check(context.Background())
	assert.Empty(t, store.events)
	// The decision is durable even though its log entry is not.
	assert.Equal(t, int64(10), store.states["s1"].TurnEndOrdinal)

	hub.mu.Lock()
	closed := hub.cursor
	hub.mu.Unlock()
	assert.Equal(t, readyAt.Add(time.Minute), closed.Since,
		"an event-log failure must not hold the cursor")
}
