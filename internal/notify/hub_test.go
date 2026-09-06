package notify

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	_ context.Context, _ time.Time, readyAt time.Time, limit int,
) ([]Snapshot, error) {
	// The since window is a store-side optimization; the fake
	// store returns every post-ready candidate and lets the
	// archive-backed dedup state decide.
	var out []Snapshot
	for _, s := range f.candidates {
		if s.LocalModifiedAt.After(readyAt) {
			out = append(out, s)
		}
	}
	if len(out) > limit {
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
	for i := 0; i < 10; i++ {
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scopes := make(chan string, 16)
	go hub.Run(ctx, scopes)

	// A burst of scopes (sustained sync output) must collapse
	// into one check during the debounce window.
	for i := 0; i < 10; i++ {
		scopes <- "messages"
	}
	collect(t, ch, 1)
	assert.Len(t, store.events, 1)

	// Later bursts re-check; the archive cursor keeps it silent.
	for i := 0; i < 5; i++ {
		scopes <- "sessions"
	}
	select {
	case n := <-ch:
		t.Fatalf("burst re-notified: %+v", n)
	case <-time.After(200 * time.Millisecond):
	}
}
