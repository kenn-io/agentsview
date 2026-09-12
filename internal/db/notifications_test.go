package db

import (
	"context"
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

	cands, err := d.NotificationCandidates(ctx, since, readyAt, 64)
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
		ctx, readyAt.Add(-time.Minute), readyAt, 64)
	require.NoError(t, err)
	require.Len(t, cands, 1)
	assert.Equal(t, "All done, tests pass.", cands[0].LastMessage)
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
			Title:     "t",
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
