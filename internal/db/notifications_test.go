package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/notify"
)

func notifTestSession(
	t *testing.T, d *DB, id string, mods ...func(*Session),
) {
	t.Helper()
	s := Session{
		ID:           id,
		Project:      "proj",
		Machine:      defaultMachine,
		Agent:        defaultAgent,
		MessageCount: 1,
	}
	for _, m := range mods {
		m(&s)
	}
	require.NoError(t, d.UpsertSession(s), "upsert %s", id)
}

func withTermination(status string) func(*Session) {
	return func(s *Session) { s.TerminationStatus = &status }
}

func withNextOrdinal(n int) func(*Session) {
	return func(s *Session) { s.NextOrdinal = n }
}

// setModifiedAt stamps local_modified_at directly: the column is
// owned by the sync write path, not UpsertSession.
func setModifiedAt(t *testing.T, d *DB, id, ts string) {
	t.Helper()
	_, err := d.getWriter().Exec(
		`UPDATE sessions SET local_modified_at = ? WHERE id = ?`, ts, id)
	require.NoError(t, err)
}

func TestNotificationCandidates(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	readyAt := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	since := readyAt.Add(-time.Minute)
	post := readyAt.Add(time.Minute).Format(time.RFC3339Nano)
	pre := readyAt.Add(-time.Hour).Format(time.RFC3339Nano)

	notifTestSession(t, d, "turn-end",
		withTermination("awaiting_user"), withNextOrdinal(10))
	notifTestSession(t, d, "no-modified",
		withTermination("awaiting_user"), withNextOrdinal(10))
	notifTestSession(t, d, "pre-ready",
		withTermination("awaiting_user"), withNextOrdinal(10))
	notifTestSession(t, d, "unclassified",
		withNextOrdinal(10))
	notifTestSession(t, d, "pending",
		withTermination("tool_call_pending"), withNextOrdinal(8))

	setModifiedAt(t, d, "turn-end", post)
	setModifiedAt(t, d, "pre-ready", pre)
	setModifiedAt(t, d, "unclassified", post)
	setModifiedAt(t, d, "pending", post)
	// ended_with_role is a signals column: stamp it like the sync
	// engine's signal pass would.
	updateSignals(t, d, "turn-end", SessionSignalUpdate{EndedWithRole: "assistant"})
	updateSignals(t, d, "pending", SessionSignalUpdate{EndedWithRole: "assistant"})

	cands, err := d.NotificationCandidates(
		ctx, notify.Cursor{Since: since}, readyAt, 64)
	require.NoError(t, err)
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.SessionID)
	}
	assert.Contains(t, ids, "turn-end")
	assert.Contains(t, ids, "pending")
	assert.NotContains(t, ids, "no-modified")
	assert.NotContains(t, ids, "pre-ready")
	assert.NotContains(t, ids, "unclassified")

	for _, c := range cands {
		if c.SessionID == "turn-end" {
			assert.Equal(t, int64(10), c.NextOrdinal)
			assert.Equal(t, "awaiting_user", c.TerminationStatus)
			assert.Equal(t, "assistant", c.LastRole)
			assert.False(t, c.LocalModifiedAt.IsZero())
		}
	}
}

func TestNotificationCandidatesCarriesLastMessage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	readyAt := time.Now().Add(-time.Minute)
	post := time.Now().Format(time.RFC3339Nano)

	notifTestSession(t, d, "s1",
		withTermination("awaiting_user"), withNextOrdinal(2))
	insertMessages(t, d,
		userMsg("s1", 0, "please fix"),
		asstMsg("s1", 1, "All done, tests pass."))
	setModifiedAt(t, d, "s1", post)
	updateSignals(t, d, "s1", SessionSignalUpdate{EndedWithRole: "assistant"})

	cands, err := d.NotificationCandidates(
		ctx, notify.Cursor{Since: readyAt.Add(-time.Minute)}, readyAt, 64)
	require.NoError(t, err)
	require.Len(t, cands, 1)
	assert.Equal(t, "All done, tests pass.", cands[0].LastMessage)
}

func TestNotificationCandidatesOldestFirst(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	readyAt := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	since := readyAt.Add(-time.Minute)

	for _, id := range []string{"c-newest", "c-oldest", "c-middle"} {
		notifTestSession(t, d, id,
			withTermination("awaiting_user"), withNextOrdinal(10))
	}
	setModifiedAt(t, d, "c-oldest",
		readyAt.Add(time.Minute).Format(time.RFC3339Nano))
	setModifiedAt(t, d, "c-middle",
		readyAt.Add(2*time.Minute).Format(time.RFC3339Nano))
	setModifiedAt(t, d, "c-newest",
		readyAt.Add(3*time.Minute).Format(time.RFC3339Nano))

	cands, err := d.NotificationCandidates(
		ctx, notify.Cursor{Since: since}, readyAt, 64)
	require.NoError(t, err)
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.SessionID)
	}
	// Oldest first is load-bearing: the Hub drains a burst larger
	// than the cap by resuming after the last row it processed.
	assert.Equal(t, []string{"c-oldest", "c-middle", "c-newest"}, ids)
}

// TestNotificationCandidatesTiedBurstDrains drives the real cursor
// round-trip the Hub performs. A single sync pass stamps many
// sessions with the same strftime('now') value, so exact ties are
// normal; a burst larger than the cap must still drain without
// losing any session sharing the batch's newest timestamp.
func TestNotificationCandidatesTiedBurstDrains(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	readyAt := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	const total = 100
	// One shared timestamp, written in the column's own format.
	tie := readyAt.Add(time.Minute).Format(notificationTimestampLayout)
	for i := range total {
		id := fmt.Sprintf("tie-%03d", i)
		notifTestSession(t, d, id,
			withTermination("awaiting_user"), withNextOrdinal(10))
		setModifiedAt(t, d, id, tie)
	}

	cursor := notify.Cursor{Since: readyAt.Add(-time.Minute)}
	seen := map[string]int{}
	for round := 0; round < total && len(seen) < total; round++ {
		cands, err := d.NotificationCandidates(ctx, cursor, readyAt, 64)
		require.NoError(t, err)
		if len(cands) == 0 {
			break
		}
		for _, c := range cands {
			seen[c.SessionID]++
		}
		last := cands[len(cands)-1]
		cursor = notify.Cursor{Since: last.LocalModifiedAt, ID: last.SessionID}
	}
	require.Len(t, seen, total, "every tied session must drain")
	for id, n := range seen {
		assert.Equal(t, 1, n, "session %s returned %d times", id, n)
	}
}

// TestNotificationCandidatesTrailingZeroCursor is the mechanism-1
// regression: the column stores three fractional digits, so a
// cursor formatted with a layout that strips trailing zeros
// (".06") sorts above ".061"..".069" and silently excludes those
// rows. Both the cursor and readyAt parameters are checked.
func TestNotificationCandidatesTrailingZeroCursor(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	// .060 exactly: its fractional part ends in a zero, which
	// RFC3339Nano would render as ".06".
	base, err := time.Parse(time.RFC3339Nano, "2026-09-06T12:01:00.060Z")
	require.NoError(t, err)

	// m060..m070, strictly increasing by 1ms; only m060 is <= base.
	for i := 0; i <= 10; i++ {
		id := fmt.Sprintf("m%03d", 60+i)
		notifTestSession(t, d, id,
			withTermination("awaiting_user"), withNextOrdinal(10))
		ts := base.Add(time.Duration(i) * time.Millisecond).
			UTC().Format(notificationTimestampLayout)
		setModifiedAt(t, d, id, ts)
	}
	want := []string{"m061", "m062", "m063", "m064", "m065",
		"m066", "m067", "m068", "m069", "m070"}

	t.Run("cursor at trailing-zero instant", func(t *testing.T) {
		cands, err := d.NotificationCandidates(
			ctx, notify.Cursor{Since: base},
			base.Add(-time.Hour), 64)
		require.NoError(t, err)
		ids := candidateIDs(cands)
		assert.Equal(t, want, ids)
	})

	t.Run("readyAt at trailing-zero instant", func(t *testing.T) {
		cands, err := d.NotificationCandidates(
			ctx, notify.Cursor{Since: base.Add(-time.Hour)}, base, 64)
		require.NoError(t, err)
		ids := candidateIDs(cands)
		assert.Equal(t, want, ids)
	})
}

// TestNotificationTimestampLayoutMatchesStrftime pins the Go layout
// used for the query parameters to the column's own writers. If a
// future writer changes the stored format, this fails rather than
// silently re-introducing the trailing-zero text-comparison bug.
func TestNotificationTimestampLayoutMatchesStrftime(t *testing.T) {
	d := testDB(t)
	instants := []time.Time{
		time.Date(2026, 9, 6, 12, 1, 0, 0, time.UTC),
		time.Date(2026, 9, 6, 12, 1, 0, int(1*time.Millisecond), time.UTC),
		time.Date(2026, 9, 6, 12, 1, 0, int(10*time.Millisecond), time.UTC),
		time.Date(2026, 9, 6, 12, 1, 0, int(60*time.Millisecond), time.UTC),
		time.Date(2026, 9, 6, 12, 1, 0, int(100*time.Millisecond), time.UTC),
		time.Date(2026, 9, 6, 12, 1, 0, int(120*time.Millisecond), time.UTC),
		time.Date(2026, 9, 6, 12, 1, 0, int(500*time.Millisecond), time.UTC),
		time.Date(2026, 9, 6, 12, 1, 0, int(999*time.Millisecond), time.UTC),
	}
	for _, in := range instants {
		var stored string
		require.NoError(t, d.getReader().QueryRow(
			`SELECT strftime('%Y-%m-%dT%H:%M:%fZ', ?)`,
			in.UTC().Format(time.RFC3339Nano),
		).Scan(&stored))
		assert.Equal(t, stored, in.UTC().Format(notificationTimestampLayout),
			"layout must match the column writer at %s", in)
	}
}

func candidateIDs(cands []notify.Snapshot) []string {
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.SessionID)
	}
	return ids
}

func TestNotificationStateRoundtripAndRestart(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	st, err := d.NotificationState(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, notify.State{}, st, "unknown session has zero state")

	notifiedAt := time.Date(2026, 9, 6, 12, 1, 0, 0, time.UTC)
	want := notify.State{
		TurnEndOrdinal:     10,
		ReplyNotifyOrdinal: 10,
		ReplyNotifiedAt:    notifiedAt,
	}
	require.NoError(t, d.SaveNotificationState(ctx, "s1", want))

	got, err := d.NotificationState(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// Overwrite advances the cursor.
	want.TurnEndOrdinal = 14
	require.NoError(t, d.SaveNotificationState(ctx, "s1", want))
	got, err = d.NotificationState(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, int64(14), got.TurnEndOrdinal)
}

func TestNotificationStateCorruptRowReturnsError(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// The row exists but is not valid JSON. Reporting a zero State
	// here would be indistinguishable from "never notified" and
	// would re-fire a notification for a session already reported.
	_, err := d.getWriter().Exec(
		`INSERT INTO archive_metadata (key, value) VALUES (?, ?)`,
		notificationStateKeyPrefix+"corrupt", "{not json")
	require.NoError(t, err)

	_, err = d.NotificationState(ctx, "corrupt")
	require.Error(t, err,
		"corrupt dedup state must error, not read as a zero State")
}

func TestNotificationEventsBoundedRing(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	events, err := d.NotificationEvents(ctx, 0)
	require.NoError(t, err)
	assert.Empty(t, events)

	for i := range 120 {
		n := notify.Notification{
			Kind:      notify.KindTurnEnd,
			SessionID: "s1",
			Project:   "proj",
			Agent:     "claude",
			Excerpt:   "t",
			CreatedAt: time.Unix(int64(i), 0).UTC().Format(time.RFC3339Nano),
		}
		require.NoError(t, d.RecordNotificationEvent(ctx, n))
	}

	events, err = d.NotificationEvents(ctx, 0)
	require.NoError(t, err)
	require.Len(t, events, 100, "ring stays bounded")
	// Newest first.
	assert.Equal(t, "s1", events[0].SessionID)

	recent, err := d.NotificationEvents(ctx, 5)
	require.NoError(t, err)
	assert.Len(t, recent, 5)
}
