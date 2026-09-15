package notify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeTimestamp renders a time the way every writer of
// sessions.local_modified_at does: fixed width, always three
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
	ts, since := storeTimestamp(s.LocalModifiedAt), storeTimestamp(cursor.Since)
	if ts != since {
		return ts > since
	}
	return cursor.ID != "" && s.SessionID > cursor.ID
}

func byStoreOrder(x, y Snapshot) int {
	tx, ty := storeTimestamp(x.LocalModifiedAt), storeTimestamp(y.LocalModifiedAt)
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
		if !s.LocalModifiedAt.After(readyAt) {
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
		SessionID:         id,
		Project:           "proj",
		Agent:             "claude",
		TerminationStatus: "awaiting_user",
		NextOrdinal:       ordinal,
		LastRole:          "assistant",
		LocalModifiedAt:   readyAt.Add(time.Minute),
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

func TestHubTurnEndOncePerTurn(t *testing.T) {
	store := newFakeStore()
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	hub := NewHub(store, enabledCfg, readyAt, time.Millisecond)
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
	hub := NewHub(store, enabledCfg, readyAt, time.Millisecond)
	ch, unsub := hub.Subscribe()
	defer unsub()

	hub.Check(context.Background())
	collect(t, ch, 1)

	// Simulate a daemon restart: fresh Hub, same archive state.
	// The persisted cursor must suppress the repeat.
	hub2 := NewHub(store, enabledCfg, readyAt, time.Millisecond)
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

	hub := NewHub(store, enabledCfg, readyAt, time.Millisecond)
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
				s.LocalModifiedAt = modified
			}))
	}

	// Production debounce is EventsCoalesceInterval (10s); use it
	// so the -2*debounce look-back is realistic.
	hub := NewHub(store, enabledCfg, readyAt, 10*time.Second)
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
// sync pass stamps many sessions with one local_modified_at, and a
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
				s.LocalModifiedAt = tie
			}))
	}

	hub := NewHub(store, enabledCfg, readyAt, 10*time.Second)
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
	hub := NewHub(store, enabledCfg, readyAt, time.Millisecond)
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
	hub := NewHub(store, enabledCfg, readyAt, time.Millisecond)

	hub.Check(context.Background())
	require.Len(t, store.events, 1)
	assert.Equal(t, int64(10), store.states["s1"].TurnEndOrdinal)
}

func TestHubFanOutAndSlowSubscriber(t *testing.T) {
	store := newFakeStore()
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	hub := NewHub(store, enabledCfg, readyAt, time.Millisecond)
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
	hub := NewHub(store, func() Config { return cfg }, readyAt, time.Millisecond)
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

func TestHubRunCoalescesScopeBursts(t *testing.T) {
	store := newFakeStore()
	store.candidates = []Snapshot{hubTestSnapshot("s1", 10)}
	hub := NewHub(store, enabledCfg, readyAt, 30*time.Millisecond)
	ch, unsub := hub.Subscribe()
	defer unsub()

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

	// Later bursts re-check; the archive cursor keeps it silent.
	for range 5 {
		scopes <- "sessions"
	}
	select {
	case n := <-ch:
		t.Fatalf("burst re-notified: %+v", n)
	case <-time.After(200 * time.Millisecond):
	}
}
