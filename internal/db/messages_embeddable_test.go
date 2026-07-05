package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScanEmbeddableMessagesFiltersRolesAndPrefixes seeds one session with a
// mix of user/assistant/tool/system-role messages plus a system-prefixed user
// message, and asserts only the clean user/assistant rows stream back in
// ordinal order.
func TestScanEmbeddableMessagesFiltersRolesAndPrefixes(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "sess-1", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d,
		Message{
			SessionID: "sess-1", Ordinal: 0, Role: "user",
			Content: "hello there", ContentLength: len("hello there"),
			Timestamp: tsZero,
		},
		Message{
			SessionID: "sess-1", Ordinal: 1, Role: "assistant",
			Content: "hi back", ContentLength: len("hi back"),
			Timestamp: tsZeroS1,
		},
		Message{
			SessionID: "sess-1", Ordinal: 2, Role: "tool",
			Content: "tool output", ContentLength: len("tool output"),
			Timestamp: tsZeroS2,
		},
		Message{
			SessionID: "sess-1", Ordinal: 3, Role: "system",
			Content: "system note", ContentLength: len("system note"),
			Timestamp: tsHour1,
		},
		Message{
			SessionID: "sess-1", Ordinal: 4, Role: "user",
			Content:       "This session is being continued from a previous one",
			ContentLength: 10, Timestamp: tsHour1,
		},
		Message{
			SessionID: "sess-1", Ordinal: 5, Role: "user",
			Content: "is_system flag set", ContentLength: 19,
			Timestamp: tsHour1, IsSystem: true,
		},
	)

	var got []EmbeddableMessage
	maxEnded, err := d.ScanEmbeddableMessages(context.Background(), "",
		func(m EmbeddableMessage) error {
			got = append(got, m)
			return nil
		})
	require.NoError(t, err)

	require.Len(t, got, 2)
	assert.Equal(t, EmbeddableMessage{
		SessionID: "sess-1", SourceUUID: "", Ordinal: 0, Content: "hello there",
	}, got[0])
	assert.Equal(t, EmbeddableMessage{
		SessionID: "sess-1", SourceUUID: "", Ordinal: 1, Content: "hi back",
	}, got[1])
	assert.Equal(t, tsHour1, maxEnded)
}

// TestScanEmbeddableMessagesSinceFiltersOlderSessions asserts that since
// restricts the scan to sessions whose ended_at is >= since, excluding an
// older session entirely.
func TestScanEmbeddableMessagesSinceFiltersOlderSessions(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "old-sess", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsZero)
	})
	insertMessages(t, d, Message{
		SessionID: "old-sess", Ordinal: 0, Role: "user",
		Content: "old content", ContentLength: len("old content"),
		Timestamp: tsZero,
	})

	insertSession(t, d, "new-sess", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsMidYear)
	})
	insertMessages(t, d, Message{
		SessionID: "new-sess", Ordinal: 0, Role: "user",
		Content: "new content", ContentLength: len("new content"),
		Timestamp: tsMidYear,
	})

	var got []EmbeddableMessage
	maxEnded, err := d.ScanEmbeddableMessages(context.Background(), tsHour1,
		func(m EmbeddableMessage) error {
			got = append(got, m)
			return nil
		})
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, "new-sess", got[0].SessionID)
	assert.Equal(t, tsMidYear, maxEnded)
}

// TestScanEmbeddableMessagesEmptyReturnsEmptyWatermark asserts that scanning
// an archive with no embeddable messages returns an empty maxEnded and never
// invokes fn.
func TestScanEmbeddableMessagesEmptyReturnsEmptyWatermark(t *testing.T) {
	d := testDB(t)

	calls := 0
	maxEnded, err := d.ScanEmbeddableMessages(context.Background(), "",
		func(m EmbeddableMessage) error {
			calls++
			return nil
		})
	require.NoError(t, err)
	assert.Zero(t, calls)
	assert.Empty(t, maxEnded)
}

// TestScanEmbeddableMessagesOrdersBySessionThenOrdinal asserts the stream is
// ordered by (session_id, ordinal) across multiple sessions.
func TestScanEmbeddableMessagesOrdersBySessionThenOrdinal(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "sess-b", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertSession(t, d, "sess-a", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
	})
	insertMessages(t, d,
		Message{
			SessionID: "sess-b", Ordinal: 0, Role: "user",
			Content: "b0", ContentLength: 2, Timestamp: tsZero,
		},
		Message{
			SessionID: "sess-a", Ordinal: 1, Role: "assistant",
			Content: "a1", ContentLength: 2, Timestamp: tsZeroS1,
			SourceUUID: "uuid-a1",
		},
		Message{
			SessionID: "sess-a", Ordinal: 0, Role: "user",
			Content: "a0", ContentLength: 2, Timestamp: tsZero,
			SourceUUID: "uuid-a0",
		},
	)

	var got []EmbeddableMessage
	_, err := d.ScanEmbeddableMessages(context.Background(), "",
		func(m EmbeddableMessage) error {
			got = append(got, m)
			return nil
		})
	require.NoError(t, err)

	require.Len(t, got, 3)
	assert.Equal(t, "sess-a", got[0].SessionID)
	assert.Equal(t, 0, got[0].Ordinal)
	assert.Equal(t, "uuid-a0", got[0].SourceUUID)
	assert.Equal(t, "sess-a", got[1].SessionID)
	assert.Equal(t, 1, got[1].Ordinal)
	assert.Equal(t, "uuid-a1", got[1].SourceUUID)
	assert.Equal(t, "sess-b", got[2].SessionID)
	assert.Equal(t, 0, got[2].Ordinal)
}
