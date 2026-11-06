//go:build !(windows && arm64)

package duckdb

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// seedPinnedArchive writes sessions, messages, and pins into a fresh
// mirror file with set-based SQL, then opens it the way serve does.
// extra runs after the seed so a test can add rows of its own.
func seedPinnedArchive(tb testing.TB, archive pinnedArchive, extra ...string) *Store {
	tb.Helper()
	ctx := tb.Context()
	path := filepath.Join(tb.TempDir(), "mirror.duckdb")
	conn, err := Open(ctx, path)
	require.NoError(tb, err)
	require.NoError(tb, EnsureSchema(ctx, conn))
	statements := append([]string{
		fmt.Sprintf(`
		INSERT INTO sessions (
			id, project, agent, first_message, started_at, created_at,
			message_count, user_message_count, relationship_type, data_version
		)
		SELECT 'pin-' || i, 'project-' || (i %% 50), 'claude', 'first ' || i,
			TIMESTAMP '2026-01-01 00:00:00' + to_seconds(i),
			TIMESTAMP '2026-01-01 00:00:00' + to_seconds(i),
			%d, 1, 'root', 1
		FROM range(%d) t(i)`, archive.messagesPerSession, archive.sessions),
		fmt.Sprintf(`
		INSERT INTO messages (
			id, session_id, ordinal, role, content, content_length, timestamp
		)
		SELECT n + 1, 'pin-' || (n // %[1]d), n %% %[1]d,
			CASE WHEN n %% 2 = 0 THEN 'user' ELSE 'assistant' END,
			body, length(body),
			TIMESTAMP '2026-01-01 00:00:00' + to_seconds(n)
		FROM (
			SELECT i AS n, 'message pin-' || (i // %[1]d) || ' #' || (i %% %[1]d)
				|| ' ' || repeat('payload ', %[2]d) AS body
			FROM range(%[3]d) t(i)
		)`, archive.messagesPerSession, archive.payloadRepeats,
			archive.sessions*archive.messagesPerSession),
		fmt.Sprintf(`
		INSERT INTO pinned_messages (
			id, session_id, message_id, ordinal, note, created_at
		)
		SELECT i + 1, 'pin-' || (i * %[1]d // %[2]d),
			(i * %[1]d // %[2]d) * %[3]d + i %% %[3]d + 1, i %% %[3]d,
			'note ' || i, TIMESTAMP '2026-02-01 00:00:00' + to_seconds(i)
		FROM range(%[2]d) t(i)`, archive.sessions, archive.pins,
			archive.messagesPerSession),
	}, extra...)
	for _, stmt := range statements {
		_, err = conn.ExecContext(ctx, stmt)
		require.NoError(tb, err, stmt)
	}
	require.NoError(tb, conn.Close())
	store, err := NewStore(ctx, path)
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, store.Close()) })
	return store
}

// TestListPinnedMessagesAllSessionsJoinsMessages checks that the all-pins
// view returns each pin's own message content and role across sessions,
// and keeps a pin whose message is absent from the mirror.
func TestListPinnedMessagesAllSessionsJoinsMessages(t *testing.T) {
	archive := pinnedArchive{sessions: 4, messagesPerSession: 3, pins: 2, payloadRepeats: 1}
	store := seedPinnedArchive(t, archive, `
		INSERT INTO pinned_messages (id, session_id, message_id, ordinal, created_at)
		VALUES (3, 'pin-1', 999, 9, TIMESTAMP '2026-03-01 00:00:00')`)

	pins, err := store.ListPinnedMessages(t.Context(), "", "")
	require.NoError(t, err)

	type pinView struct {
		ID        int64
		SessionID string
		MessageID int64
		Content   *string
		Role      *string
	}
	got := make([]pinView, 0, len(pins))
	for _, p := range pins {
		got = append(got, pinView{p.ID, p.SessionID, p.MessageID, p.Content, p.Role})
	}
	text := func(s string) *string { return &s }
	assert.Equal(t, []pinView{
		{3, "pin-1", 999, nil, nil},
		{2, "pin-2", 8, text(archive.content(2, 1)), text("assistant")},
		{1, "pin-0", 1, text(archive.content(0, 0)), text("user")},
	}, got)
}

// BenchmarkDuckDBListPinnedMessages measures the Pinned view, which lists
// every pin with its message, over a 10,000-session mirror of 500,000
// messages with 20 pins.
func BenchmarkDuckDBListPinnedMessages(b *testing.B) {
	archive := pinnedArchive{
		sessions: 10_000, messagesPerSession: 50, pins: 20, payloadRepeats: 40,
	}
	store := seedPinnedArchive(b, archive)
	b.ReportAllocs()
	for b.Loop() {
		pins, err := store.ListPinnedMessages(b.Context(), "", "")
		require.NoError(b, err)
		require.Len(b, pins, archive.pins)
	}
}
