//go:build chtest

package clickhouse

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
)

// pinnedArchive sizes a synthetic mirror. Session s is "pin-s"; message
// ordinal o of that session has id s*messagesPerSession+o+1. Pin i sits on
// session i*sessions/pins at ordinal i%messagesPerSession, so pins spread
// evenly across the archive and most sessions have none.
type pinnedArchive struct {
	sessions           int
	messagesPerSession int
	pins               int
	payloadRepeats     int
}

func (a pinnedArchive) content(session, ordinal int) string {
	return fmt.Sprintf("message pin-%d #%d ", session, ordinal) +
		strings.Repeat("payload ", a.payloadRepeats)
}

// seedPinnedArchive writes sessions, messages, and pins straight into a
// fresh mirror with set-based SQL so a multi-million-message archive seeds
// in seconds.
func seedPinnedArchive(tb testing.TB, archive pinnedArchive) *Store {
	tb.Helper()
	ctx := tb.Context()
	dsn, database := chtest.FreshDatabase(tb)
	target := Target{URL: dsn, Database: database}
	require.NoError(tb, EnsureSchema(ctx, target))
	store, err := NewStore(ctx, target)
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, store.Close()) })
	conn := store.DB()

	_, err = conn.ExecContext(ctx, `
		INSERT INTO sessions (
			id, project, agent, first_message, started_at, created_at,
			message_count, user_message_count, relationship_type, data_version
		)
		SELECT concat('pin-', toString(number)),
			concat('project-', toString(number % 50)), 'claude',
			concat('first ', toString(number)),
			addSeconds(toDateTime64('2026-01-01 00:00:00', 6, 'UTC'), number),
			addSeconds(toDateTime64('2026-01-01 00:00:00', 6, 'UTC'), number),
			?, 1, 'root', 1
		FROM numbers(?)`,
		archive.messagesPerSession, archive.sessions)
	require.NoError(tb, err, "seed sessions")
	_, err = conn.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, ordinal, role, content, content_length, timestamp
		)
		SELECT number + 1,
			concat('pin-', toString(intDiv(number, ?))),
			number % ?,
			if(number % 2 = 0, 'user', 'assistant'),
			concat('message pin-', toString(intDiv(number, ?)), ' #',
				toString(number % ?), ' ', repeat('payload ', ?)) AS body,
			length(body),
			addSeconds(toDateTime64('2026-01-01 00:00:00', 6, 'UTC'), number)
		FROM numbers(?)`,
		archive.messagesPerSession, archive.messagesPerSession,
		archive.messagesPerSession, archive.messagesPerSession,
		archive.payloadRepeats, archive.sessions*archive.messagesPerSession)
	require.NoError(tb, err, "seed messages")
	_, err = conn.ExecContext(ctx, `
		INSERT INTO pinned_messages (
			id, session_id, message_id, ordinal, note, created_at
		)
		SELECT number + 1,
			concat('pin-', toString(intDiv(number * ?, ?))),
			intDiv(number * ?, ?) * ? + number % ? + 1,
			number % ?,
			concat('note ', toString(number)),
			addSeconds(toDateTime64('2026-02-01 00:00:00', 6, 'UTC'), number)
		FROM numbers(?)`,
		archive.sessions, archive.pins,
		archive.sessions, archive.pins, archive.messagesPerSession,
		archive.messagesPerSession, archive.messagesPerSession, archive.pins)
	require.NoError(tb, err, "seed pins")
	// One part per table keeps FINAL reads and background merges from
	// varying between benchmark runs.
	for _, table := range []string{"sessions", "messages", "pinned_messages"} {
		_, err = conn.ExecContext(ctx, "OPTIMIZE TABLE "+table+" FINAL")
		require.NoError(tb, err, "optimize %s", table)
	}
	return store
}

// TestListPinnedMessagesAllSessionsJoinsMessages checks that the all-pins
// view returns each pin's own message content and role across sessions,
// and keeps a pin whose message is absent from the mirror.
func TestListPinnedMessagesAllSessionsJoinsMessages(t *testing.T) {
	t.Parallel()
	archive := pinnedArchive{sessions: 4, messagesPerSession: 3, pins: 2, payloadRepeats: 1}
	store := seedPinnedArchive(t, archive)
	_, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO pinned_messages (id, session_id, message_id, ordinal, created_at)
		VALUES (3, 'pin-1', 999, 9, toDateTime64('2026-03-01 00:00:00', 6, 'UTC'))`)
	require.NoError(t, err)

	pins, err := store.ListPinnedMessages(t.Context(), "", "")
	require.NoError(t, err)

	type pinView struct {
		ID        int64
		SessionID string
		MessageID int64
		Content   string
		Role      string
	}
	got := make([]pinView, 0, len(pins))
	for _, p := range pins {
		require.NotNil(t, p.Content)
		require.NotNil(t, p.Role)
		got = append(got, pinView{p.ID, p.SessionID, p.MessageID, *p.Content, *p.Role})
	}
	// ClickHouse fills an unmatched LEFT JOIN row with column defaults, so
	// the missing message reads as empty content and role.
	assert.Equal(t, []pinView{
		{3, "pin-1", 999, "", ""},
		{2, "pin-2", 8, archive.content(2, 1), "assistant"},
		{1, "pin-0", 1, archive.content(0, 0), "user"},
	}, got)
}

// BenchmarkClickHouseListPinnedMessages measures the Pinned view, which
// lists every pin with its message, over a 20,000-session archive of
// 2,000,000 messages with 20 pins.
func BenchmarkClickHouseListPinnedMessages(b *testing.B) {
	archive := pinnedArchive{
		sessions: 20_000, messagesPerSession: 100, pins: 20, payloadRepeats: 60,
	}
	store := seedPinnedArchive(b, archive)
	b.ReportAllocs()
	for b.Loop() {
		pins, err := store.ListPinnedMessages(b.Context(), "", "")
		require.NoError(b, err)
		require.Len(b, pins, archive.pins)
	}
}
